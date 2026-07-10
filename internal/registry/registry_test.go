package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildFakeProviderZip returns zip bytes containing a single flat-root file
// named "terraform-provider-<name>" holding content. This mirrors the real
// OpenTofu registry archive layout confirmed in research: an unversioned
// binary name, no subdirectories.
func buildFakeProviderZip(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("terraform-provider-" + name)
	if err != nil {
		t.Fatalf("zip.Create: %v", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("zip Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

// fakeRegistry is a minimal in-process stand-in for registry.opentofu.org
// serving exactly one namespace/name/version, with a correct checksum chain.
type fakeRegistry struct {
	srv      *httptest.Server
	ns       string
	name     string
	version  string
	filename string
	zipBytes []byte
	sha256   string // hex sha256 of zipBytes
}

func newFakeRegistry(t *testing.T, ns, name, version, binaryContent string) *fakeRegistry {
	t.Helper()
	zipBytes := buildFakeProviderZip(t, name, binaryContent)
	sum := sha256.Sum256(zipBytes)
	fr := &fakeRegistry{
		ns:       ns,
		name:     name,
		version:  version,
		filename: fmt.Sprintf("terraform-provider-%s_%s_%s_%s.zip", name, version, runtime.GOOS, runtime.GOARCH),
		zipBytes: zipBytes,
		sha256:   hex.EncodeToString(sum[:]),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/{ns}/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ns") != fr.ns || r.PathValue("name") != fr.name {
			http.NotFound(w, r)
			return
		}
		// Real registry serves JSON with this content-type; Install must
		// not gate parsing on it.
		w.Header().Set("Content-Type", "application/octet-stream")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{
				{"version": fr.version, "protocols": []string{"5.0"}},
			},
		})
	})
	mux.HandleFunc("GET /v1/providers/{ns}/{name}/{version}/download/{goos}/{goarch}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ns") != fr.ns || r.PathValue("name") != fr.name || r.PathValue("version") != fr.version {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"filename":     fr.filename,
			"download_url": fr.srv.URL + "/dl/" + fr.filename,
			"shasums_url":  fr.srv.URL + "/dl/SHA256SUMS",
			"shasum":       fr.sha256,
		})
	})
	mux.HandleFunc("GET /dl/"+fr.filename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fr.zipBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", fr.sha256, fr.filename)
	})

	fr.srv = httptest.NewServer(mux)
	t.Cleanup(fr.srv.Close)
	return fr
}

func TestInstall_Success(t *testing.T) {
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "#!/bin/sh\necho fake-provider\n")
	cacheDir := t.TempDir()

	path, err := Install(context.Background(), "opentofu/widget", "1.2.3", fr.srv.URL, cacheDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	wantDir := filepath.Join(cacheDir, "opentofu", "widget", "1.2.3", runtime.GOOS+"_"+runtime.GOARCH)
	if filepath.Dir(path) != wantDir {
		t.Errorf("path dir = %s, want %s", filepath.Dir(path), wantDir)
	}
	if !strings.HasPrefix(filepath.Base(path), "terraform-provider-") {
		t.Errorf("binary name = %s, want terraform-provider- prefix", filepath.Base(path))
	}

	got, err := os.ReadFile(path) //nolint:gosec // G304: path is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "#!/bin/sh\necho fake-provider\n" {
		t.Errorf("extracted content = %q, want the fake binary content", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want executable bits set (chmod 0755)", info.Mode())
	}
}

func TestInstall_VersionNotFound(t *testing.T) {
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "content")
	cacheDir := t.TempDir()

	_, err := Install(context.Background(), "opentofu/widget", "9.9.9", fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want error for a version the registry does not offer, got nil")
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error = %q, want it to mention the missing version", err)
	}
}

func TestInstall_ChecksumMismatch(t *testing.T) {
	ns, name, version := "opentofu", "widget", "1.2.3"
	zipBytes := buildFakeProviderZip(t, name, "content")
	filename := fmt.Sprintf("terraform-provider-%s_%s_%s_%s.zip", name, version, runtime.GOOS, runtime.GOARCH)
	wrongHash := strings.Repeat("0", 64)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/{ns}/{name}/versions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{{"version": version, "protocols": []string{"5.0"}}},
		})
	})
	mux.HandleFunc("GET /v1/providers/{ns}/{name}/{version}/download/{goos}/{goarch}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"filename":     filename,
			"download_url": srv.URL + "/dl/" + filename,
			"shasums_url":  srv.URL + "/dl/SHA256SUMS",
			"shasum":       wrongHash,
		})
	})
	mux.HandleFunc("GET /dl/"+filename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zipBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately wrong: the SHA256SUMS document does not match the
		// zip Install is about to download.
		_, _ = fmt.Fprintf(w, "%s  %s\n", wrongHash, filename)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cacheDir := t.TempDir()
	_, err := Install(context.Background(), ns+"/"+name, version, srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want checksum mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want it to report a checksum mismatch", err)
	}

	// Nothing should have been left behind in the cache: checksum
	// verification happens before extraction.
	if _, statErr := os.Stat(filepath.Join(cacheDir, ns)); !os.IsNotExist(statErr) {
		t.Errorf("cache dir %s should not exist after a checksum failure", filepath.Join(cacheDir, ns))
	}
}

func TestDiscover_PluginDirPrecedence(t *testing.T) {
	cacheDir := t.TempDir()
	pluginDir := t.TempDir()

	osArch := runtime.GOOS + "_" + runtime.GOARCH
	cacheBinDir := filepath.Join(cacheDir, "opentofu", "widget", "1.2.3", osArch)
	if err := os.MkdirAll(cacheBinDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cachePath := filepath.Join(cacheBinDir, "terraform-provider-widget")
	if err := os.WriteFile(cachePath, []byte("from-cache"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	pluginPath := filepath.Join(pluginDir, "terraform-provider-widget_v1.2.3_x5")
	if err := os.WriteFile(pluginPath, []byte("from-plugin-dir"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Both a plugin dir and a cache entry exist: pluginDir must win.
	got, err := Discover(cacheDir, pluginDir, "opentofu/widget", "1.2.3")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != pluginPath {
		t.Errorf("Discover = %s, want plugin dir match %s", got, pluginPath)
	}

	// With no plugin dir, Discover must fall back to the cache layout.
	got, err = Discover(cacheDir, "", "opentofu/widget", "1.2.3")
	if err != nil {
		t.Fatalf("Discover (cache fallback): %v", err)
	}
	if got != cachePath {
		t.Errorf("Discover = %s, want cache match %s", got, cachePath)
	}

	// A plugin dir with no matching file must also fall back to the cache.
	emptyPluginDir := t.TempDir()
	got, err = Discover(cacheDir, emptyPluginDir, "opentofu/widget", "1.2.3")
	if err != nil {
		t.Fatalf("Discover (empty plugin dir fallback): %v", err)
	}
	if got != cachePath {
		t.Errorf("Discover = %s, want cache match %s", got, cachePath)
	}

	// Neither location has a match: error.
	if _, err := Discover(cacheDir, "", "opentofu/missing", "9.9.9"); err == nil {
		t.Error("Discover: want error when no binary exists anywhere, got nil")
	}
}

func TestList(t *testing.T) {
	cacheDir := t.TempDir()

	// A fully installed provider: has a binary under its os_arch dir.
	widgetDir := filepath.Join(cacheDir, "opentofu", "widget", "1.2.3", "linux_amd64")
	if err := os.MkdirAll(widgetDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	widgetBin := filepath.Join(widgetDir, "terraform-provider-widget")
	if err := os.WriteFile(widgetBin, []byte("bin"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A second version of the same provider, different platform directory.
	widgetDir2 := filepath.Join(cacheDir, "opentofu", "widget", "1.3.0", "darwin_arm64")
	if err := os.MkdirAll(widgetDir2, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	widgetBin2 := filepath.Join(widgetDir2, "terraform-provider-widget")
	if err := os.WriteFile(widgetBin2, []byte("bin2"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A version directory with no binary inside (interrupted/partial
	// install) must be skipped, not reported as installed.
	emptyDir := filepath.Join(cacheDir, "opentofu", "empty", "0.1.0", "linux_amd64")
	if err := os.MkdirAll(emptyDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := List(cacheDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := map[string]string{ // "source@version" -> expected path
		"opentofu/widget@1.2.3": widgetBin,
		"opentofu/widget@1.3.0": widgetBin2,
	}
	if len(got) != len(want) {
		t.Fatalf("List returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for _, inst := range got {
		key := inst.Source + "@" + inst.Version
		wantPath, ok := want[key]
		if !ok {
			t.Errorf("unexpected entry %+v", inst)
			continue
		}
		if inst.Path != wantPath {
			t.Errorf("%s: Path = %s, want %s", key, inst.Path, wantPath)
		}
	}
}

func TestList_MissingCacheDir(t *testing.T) {
	got, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %+v, want empty for a missing cache dir", got)
	}
}

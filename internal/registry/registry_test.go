package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
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

// testSigner owns an ephemeral OpenPGP entity used only by local fixtures.
type testSigner struct {
	entity      *openpgp.Entity
	publicArmor string
	keyID       string
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	entity, err := openpgp.NewEntity("tchori-test", "provider signing test key", "test@example.com", nil)
	if err != nil {
		t.Fatalf("openpgp.NewEntity: %v", err)
	}

	var public bytes.Buffer
	armored, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := entity.Serialize(armored); err != nil {
		t.Fatalf("Serialize public key: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("close public-key armor: %v", err)
	}

	return &testSigner{
		entity:      entity,
		publicArmor: public.String(),
		keyID:       fmt.Sprintf("%016X", entity.PrimaryKey.KeyId),
	}
}

func (s *testSigner) detachSign(t *testing.T, message []byte) []byte {
	t.Helper()
	var signature bytes.Buffer
	if err := openpgp.DetachSign(&signature, s.entity, bytes.NewReader(message), nil); err != nil {
		t.Fatalf("openpgp.DetachSign: %v", err)
	}
	return signature.Bytes()
}

// fakeRegistry is a minimal in-process stand-in for registry.opentofu.org
// serving exactly one namespace/name/version with mutable, signed metadata.
type fakeRegistry struct {
	srv                 *httptest.Server
	ns                  string
	name                string
	version             string
	filename            string
	zipBytes            []byte
	descriptorShasum    string
	sumsBytes           []byte
	signature           []byte
	advertisedKeys      []gpgPublicKey
	includeSignatureURL bool
	versionsHandler     http.HandlerFunc
	descriptorHandler   http.HandlerFunc
	archiveHandler      http.HandlerFunc
	shasumsHandler      http.HandlerFunc
	signatureHandler    http.HandlerFunc
	signer              *testSigner
}

func (fr *fakeRegistry) setPackage(t *testing.T, zipBytes []byte) {
	t.Helper()
	fr.zipBytes = zipBytes
	sum := sha256.Sum256(zipBytes)
	fr.descriptorShasum = hex.EncodeToString(sum[:])
	fr.sumsBytes = []byte(fmt.Sprintf("%s  %s\n", fr.descriptorShasum, fr.filename))
	fr.signature = fr.signer.detachSign(t, fr.sumsBytes)
}

func (fr *fakeRegistry) setSignedSums(t *testing.T, sums []byte) {
	t.Helper()
	fr.sumsBytes = sums
	fr.signature = fr.signer.detachSign(t, sums)
}

func newFakeRegistry(t *testing.T, ns, name, version, binaryContent string) *fakeRegistry {
	t.Helper()
	signer := newTestSigner(t)
	fr := &fakeRegistry{
		ns:                  ns,
		name:                name,
		version:             version,
		filename:            fmt.Sprintf("terraform-provider-%s_%s_%s_%s.zip", name, version, runtime.GOOS, runtime.GOARCH),
		includeSignatureURL: true,
		signer:              signer,
		advertisedKeys: []gpgPublicKey{{
			KeyID:      signer.keyID,
			ASCIIArmor: signer.publicArmor,
		}},
	}
	fr.setPackage(t, buildFakeProviderZip(t, name, binaryContent))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/{ns}/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ns") != fr.ns || r.PathValue("name") != fr.name {
			http.NotFound(w, r)
			return
		}
		if fr.versionsHandler != nil {
			fr.versionsHandler(w, r)
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
		if fr.descriptorHandler != nil {
			fr.descriptorHandler(w, r)
			return
		}
		signatureURL := ""
		if fr.includeSignatureURL {
			signatureURL = fr.srv.URL + "/dl/SHA256SUMS.sig"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"filename":              fr.filename,
			"download_url":          fr.srv.URL + "/dl/" + fr.filename,
			"shasums_url":           fr.srv.URL + "/dl/SHA256SUMS",
			"shasums_signature_url": signatureURL,
			"shasum":                fr.descriptorShasum,
			"signing_keys": map[string]any{
				"gpg_public_keys": fr.advertisedKeys,
			},
		})
	})
	mux.HandleFunc("GET /dl/"+fr.filename, func(w http.ResponseWriter, r *http.Request) {
		if fr.archiveHandler != nil {
			fr.archiveHandler(w, r)
			return
		}
		_, _ = w.Write(fr.zipBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		if fr.shasumsHandler != nil {
			fr.shasumsHandler(w, r)
			return
		}
		_, _ = w.Write(fr.sumsBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS.sig", func(w http.ResponseWriter, r *http.Request) {
		if fr.signatureHandler != nil {
			fr.signatureHandler(w, r)
			return
		}
		_, _ = w.Write(fr.signature)
	})

	fr.srv = httptest.NewServer(mux)
	t.Cleanup(fr.srv.Close)
	return fr
}

func TestInstall_RejectsUnsafeProviderCoordinatesWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		version string
	}{
		{name: "parent namespace", source: "../name", version: "1.0.0"},
		{name: "parent name", source: "name/..", version: "1.0.0"},
		{name: "exact symptom parent name", source: "opentofu/..", version: "1.0.0"},
		{name: "multiple parent segments", source: "../../x/y", version: "1.0.0"},
		{name: "nested source", source: "a/b/c", version: "1.0.0"},
		{name: "backslash in name", source: `opentofu/na\me`, version: "1.0.0"},
		{name: "absolute source", source: "/absolute", version: "1.0.0"},
		{name: "windows absolute source", source: `C:\evil/name`, version: "1.0.0"},
		{name: "UNC source", source: `\\host\share/name`, version: "1.0.0"},
		{name: "NUL in name", source: "opentofu/na\x00me", version: "1.0.0"},
		{name: "encoded traversal name", source: "opentofu/%2e%2e", version: "1.0.0"},
		{name: "parent version", source: "opentofu/widget", version: ".."},
		{name: "traversing version", source: "opentofu/widget", version: "../../x"},
		{name: "embedded version traversal", source: "opentofu/widget", version: "1.0.0/../.."},
		{name: "backslash in version", source: "opentofu/widget", version: `1.0.0\x`},
		{name: "dot version", source: "opentofu/widget", version: "."},
		{name: "absolute version", source: "opentofu/widget", version: "/1.0.0"},
		{name: "windows absolute version", source: "opentofu/widget", version: `C:\1.0.0`},
		{name: "NUL in version", source: "opentofu/widget", version: "1.0.0\x00x"},
		{name: "encoded traversal version", source: "opentofu/widget", version: "%2e%2e"},
	}

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unsafe coordinates reached the registry", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootDir := t.TempDir()
			cacheDir := filepath.Join(rootDir, "cache")
			beforeRequests := requests.Load()

			_, err := Install(context.Background(), tt.source, tt.version, srv.URL, cacheDir)
			if err == nil {
				t.Fatalf("Install(%q, %q): want validation error, got nil", tt.source, tt.version)
			}
			if !strings.Contains(err.Error(), "registry: invalid") {
				t.Errorf("error = %q, want registry validation error", err)
			}
			if got := requests.Load(); got != beforeRequests {
				t.Errorf("registry received %d request(s), want zero", got-beforeRequests)
			}

			var written []string
			walkErr := filepath.Walk(rootDir, func(path string, _ os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if path != rootDir {
					written = append(written, path)
				}
				return nil
			})
			if walkErr != nil {
				t.Fatalf("Walk(%s): %v", rootDir, walkErr)
			}
			if len(written) != 0 {
				t.Errorf("unsafe Install wrote beneath %s: %v", rootDir, written)
			}
		})
	}
}

func TestDiscover_RejectsUnsafeProviderCoordinatesBeforeGlobbing(t *testing.T) {
	tests := []struct {
		name             string
		source           string
		version          string
		pluginBinaryName string
	}{
		{name: "parent namespace", source: "../name", version: "1.0.0", pluginBinaryName: "name"},
		{name: "parent name", source: "name/..", version: "1.0.0", pluginBinaryName: "..-trap"},
		{name: "exact symptom parent name", source: "opentofu/..", version: "1.0.0", pluginBinaryName: "..-trap"},
		{name: "multiple parent segments", source: "../../x/y", version: "1.0.0"},
		{name: "nested source", source: "a/b/c", version: "1.0.0"},
		{name: "backslash in name", source: `opentofu/na\me`, version: "1.0.0"},
		{name: "absolute source", source: "/absolute", version: "1.0.0"},
		{name: "windows absolute source", source: `C:\evil/name`, version: "1.0.0", pluginBinaryName: "name"},
		{name: "UNC source", source: `\\host\share/name`, version: "1.0.0", pluginBinaryName: "name"},
		{name: "NUL in name", source: "opentofu/na\x00me", version: "1.0.0"},
		{name: "encoded traversal name", source: "opentofu/%2e%2e", version: "1.0.0"},
		{name: "parent version", source: "opentofu/widget", version: "..", pluginBinaryName: "widget"},
		{name: "traversing version", source: "opentofu/widget", version: "../../x", pluginBinaryName: "widget"},
		{name: "embedded version traversal", source: "opentofu/widget", version: "1.0.0/../..", pluginBinaryName: "widget"},
		{name: "backslash in version", source: "opentofu/widget", version: `1.0.0\x`, pluginBinaryName: "widget"},
		{name: "dot version", source: "opentofu/widget", version: ".", pluginBinaryName: "widget"},
		{name: "encoded traversal version", source: "opentofu/widget", version: "%2e%2e", pluginBinaryName: "widget"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rootDir := t.TempDir()
			cacheDir := filepath.Join(rootDir, "cache")
			pluginDir := ""
			if tt.pluginBinaryName != "" {
				pluginDir = filepath.Join(rootDir, "plugins")
				if err := os.Mkdir(pluginDir, 0o750); err != nil {
					t.Fatalf("Mkdir plugin dir: %v", err)
				}
				trap := filepath.Join(pluginDir, "terraform-provider-"+tt.pluginBinaryName)
				if err := os.WriteFile(trap, []byte("must not be discovered"), 0o600); err != nil {
					t.Fatalf("WriteFile plugin trap: %v", err)
				}
			}

			_, err := Discover(cacheDir, pluginDir, tt.source, tt.version)
			if err == nil {
				t.Fatalf("Discover(%q, %q): want validation error, got nil", tt.source, tt.version)
			}
			if !strings.Contains(err.Error(), "registry: invalid") {
				t.Errorf("error = %q, want registry validation error", err)
			}
			if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
				t.Errorf("cache dir %s should not exist after rejected discovery", cacheDir)
			}
		})
	}
}

func TestInstallDiscover_ValidHyphenatedCoordinates(t *testing.T) {
	fr := newFakeRegistry(t, "tchori-labs", "metaads", "3.2.4", "valid provider")
	cacheDir := filepath.Join(t.TempDir(), "cache", "nested", "..", "providers")

	installed, err := Install(context.Background(), "tchori-labs/metaads", "3.2.4", fr.srv.URL, cacheDir)
	if err != nil {
		t.Fatalf("Install valid provider: %v", err)
	}
	if err := assertUnderRoot(cacheDir, installed); err != nil {
		t.Fatalf("installed path containment: %v", err)
	}

	discovered, err := Discover(cacheDir, "", "tchori-labs/metaads", "3.2.4")
	if err != nil {
		t.Fatalf("Discover valid provider: %v", err)
	}
	if discovered != installed {
		t.Errorf("Discover = %s, want installed path %s", discovered, installed)
	}
}

func TestAssertUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache", "nested", "..")
	inside := filepath.Join(root, "opentofu", "widget", "1.2.3", runtime.GOOS+"_"+runtime.GOARCH)
	if err := assertUnderRoot(root, inside); err != nil {
		t.Fatalf("assertUnderRoot(valid destination): %v", err)
	}

	outside := filepath.Join(root, "..", "outside")
	if err := assertUnderRoot(root, outside); err == nil {
		t.Fatal("assertUnderRoot(escaping destination): want error, got nil")
	}
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

func setMaxProviderArchiveBytes(t *testing.T, limit int64) {
	t.Helper()
	original := maxProviderArchiveBytes
	maxProviderArchiveBytes = limit
	t.Cleanup(func() { maxProviderArchiveBytes = original })
}

func assertNoArchiveInstallResidue(t *testing.T, cacheDir, tempDir, ns, name, version string) {
	t.Helper()

	nsDir := filepath.Join(cacheDir, ns)
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Errorf("cache namespace %s should not exist after oversized archive rejection", nsDir)
	}
	binaryPath := filepath.Join(cacheDir, ns, name, version, runtime.GOOS+"_"+runtime.GOARCH, "terraform-provider-"+name)
	if _, err := os.Stat(binaryPath); !os.IsNotExist(err) {
		t.Errorf("no binary should be left behind at %s after oversized archive rejection", binaryPath)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", tempDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary archive directory %s should be empty after rejection, found %v", tempDir, entries)
	}
}

func setRegistryHTTPTimeoutsForTest(t *testing.T, dial, tlsHandshake, responseHeader, metadataRead, archiveRead time.Duration) {
	t.Helper()
	oldDial := httpDialTimeout
	oldTLSHandshake := httpTLSHandshakeTimeout
	oldResponseHeader := httpResponseHeaderTimeout
	oldMetadataRead := httpMetadataReadTimeout
	oldArchiveRead := httpArchiveReadTimeout
	httpDialTimeout = dial
	httpTLSHandshakeTimeout = tlsHandshake
	httpResponseHeaderTimeout = responseHeader
	httpMetadataReadTimeout = metadataRead
	httpArchiveReadTimeout = archiveRead
	t.Cleanup(func() {
		httpDialTimeout = oldDial
		httpTLSHandshakeTimeout = oldTLSHandshake
		httpResponseHeaderTimeout = oldResponseHeader
		httpMetadataReadTimeout = oldMetadataRead
		httpArchiveReadTimeout = oldArchiveRead
	})
}

func requireTimeoutError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Install: want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !os.IsTimeout(err) {
		t.Fatalf("Install error = %v, want a deadline/timeout error", err)
	}
}

func assertNoProviderInstallResidue(t *testing.T, tempDir, cacheDir, ns, name, version string) {
	t.Helper()
	nsDir := filepath.Join(cacheDir, ns)
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Errorf("provider cache namespace %s should not exist after timeout", nsDir)
	}
	binaryPath := filepath.Join(cacheDir, ns, name, version, runtime.GOOS+"_"+runtime.GOARCH, "terraform-provider-"+name)
	if _, err := os.Stat(binaryPath); !os.IsNotExist(err) {
		t.Errorf("provider binary %s should not exist after timeout", binaryPath)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", tempDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary provider archive was not cleaned up after timeout: %v", entries)
	}
}

func TestNewRegistryClient_UsesDedicatedTimeoutPolicy(t *testing.T) {
	setRegistryHTTPTimeoutsForTest(t, 11*time.Millisecond, 12*time.Millisecond, 13*time.Millisecond, 14*time.Millisecond, 15*time.Millisecond)

	client := newRegistryClient()
	if client == http.DefaultClient {
		t.Fatal("newRegistryClient returned http.DefaultClient")
	}
	if client.Timeout != 0 {
		t.Errorf("client.Timeout = %s, want zero so request classes retain distinct deadlines", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", client.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("newRegistryClient reused http.DefaultTransport")
	}
	if transport.DialContext == nil {
		t.Error("transport.DialContext is nil, want a bounded net.Dialer")
	}
	if transport.TLSHandshakeTimeout != httpTLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %s, want %s", transport.TLSHandshakeTimeout, httpTLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != httpResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", transport.ResponseHeaderTimeout, httpResponseHeaderTimeout)
	}
}

func TestInstall_ResponseHeaderTimeoutAppliesToEveryFetch(t *testing.T) {
	tests := []struct {
		name       string
		setHandler func(*fakeRegistry, http.HandlerFunc)
	}{
		{name: "versions", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.versionsHandler = h }},
		{name: "download descriptor", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.descriptorHandler = h }},
		{name: "archive", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.archiveHandler = h }},
		{name: "SHA256SUMS", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.shasumsHandler = h }},
		{name: "signature", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.signatureHandler = h }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRegistryHTTPTimeoutsForTest(t, time.Second, time.Second, 25*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond)
			fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "provider")
			tt.setHandler(fr, func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(250 * time.Millisecond):
				}
			})
			tempDir := t.TempDir()
			t.Setenv("TMPDIR", tempDir)
			cacheDir := filepath.Join(t.TempDir(), "cache")

			started := time.Now()
			_, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
			requireTimeoutError(t, err)
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("Install took %s after response-header timeout, want prompt return", elapsed)
			}
			assertNoProviderInstallResidue(t, tempDir, cacheDir, fr.ns, fr.name, fr.version)
		})
	}
}

func TestInstall_MetadataBodyTimeoutAppliesToEveryMetadataFetch(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		setHandler func(*fakeRegistry, http.HandlerFunc)
	}{
		{name: "versions", prefix: "{", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.versionsHandler = h }},
		{name: "download descriptor", prefix: "{", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.descriptorHandler = h }},
		{name: "SHA256SUMS", prefix: "0", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.shasumsHandler = h }},
		{name: "signature", prefix: "0", setHandler: func(fr *fakeRegistry, h http.HandlerFunc) { fr.signatureHandler = h }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRegistryHTTPTimeoutsForTest(t, time.Second, time.Second, 500*time.Millisecond, 40*time.Millisecond, time.Second)
			fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "provider")
			tt.setHandler(fr, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tt.prefix)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-r.Context().Done()
			})
			tempDir := t.TempDir()
			t.Setenv("TMPDIR", tempDir)
			cacheDir := filepath.Join(t.TempDir(), "cache")

			_, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
			requireTimeoutError(t, err)
			assertNoProviderInstallResidue(t, tempDir, cacheDir, fr.ns, fr.name, fr.version)
		})
	}
}

func TestInstall_ArchiveBodyTimeoutCleansUp(t *testing.T) {
	setRegistryHTTPTimeoutsForTest(t, time.Second, time.Second, 500*time.Millisecond, time.Second, 60*time.Millisecond)
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "provider")
	fr.archiveHandler = func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		for _, b := range fr.zipBytes {
			if _, err := w.Write([]byte{b}); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	cacheDir := filepath.Join(t.TempDir(), "cache")

	_, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
	requireTimeoutError(t, err)
	assertNoProviderInstallResidue(t, tempDir, cacheDir, fr.ns, fr.name, fr.version)
}

func TestInstall_SuccessWithFiniteTimeoutPolicy(t *testing.T) {
	setRegistryHTTPTimeoutsForTest(t, time.Second, time.Second, time.Second, time.Second, time.Second)
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "responsive provider")
	cacheDir := t.TempDir()

	path, err := Install(context.Background(), "opentofu/widget", "1.2.3", fr.srv.URL, cacheDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // path is returned from an install rooted in t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "responsive provider" {
		t.Errorf("installed content = %q, want responsive provider", got)
	}
}

func TestInstall_ArchiveDeclaredLengthOverLimit(t *testing.T) {
	const limit int64 = 64
	setMaxProviderArchiveBytes(t, limit)

	fr := newFakeRegistry(t, "opentofu", "declared", "1.0.0", "unused")
	fr.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(limit+1, 10))
		w.WriteHeader(http.StatusOK)
	}

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	_, err := Install(context.Background(), "opentofu/declared", "1.0.0", fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want error for an archive declaring more than the size limit, got nil")
	}
	if !strings.Contains(err.Error(), "declares 65 bytes") || !strings.Contains(err.Error(), "64 byte limit") {
		t.Errorf("error = %q, want declared size and byte limit", err)
	}
	assertNoArchiveInstallResidue(t, cacheDir, tempDir, "opentofu", "declared", "1.0.0")
}

func TestInstall_ArchiveChunkedBodyOverLimit(t *testing.T) {
	const limit int64 = 64
	setMaxProviderArchiveBytes(t, limit)

	fr := newFakeRegistry(t, "opentofu", "chunked", "1.0.0", "unused")
	fr.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("archive response writer does not support flushing")
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("A"), 32))
		flusher.Flush() // Force chunked framing, leaving ContentLength unknown.
		_, _ = w.Write(bytes.Repeat([]byte("B"), 33))
	}

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	_, err := Install(context.Background(), "opentofu/chunked", "1.0.0", fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want error for a chunked archive exceeding the size limit, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds 64 byte limit") {
		t.Errorf("error = %q, want it to mention the byte limit", err)
	}
	assertNoArchiveInstallResidue(t, cacheDir, tempDir, "opentofu", "chunked", "1.0.0")
}

func TestInstall_ArchiveSizeBoundary(t *testing.T) {
	fr := newFakeRegistry(t, "opentofu", "boundary", "1.0.0", "#!/bin/sh\necho boundary\n")
	archiveSize := int64(len(fr.zipBytes))

	t.Run("exact limit succeeds", func(t *testing.T) {
		setMaxProviderArchiveBytes(t, archiveSize)
		cacheDir := t.TempDir()

		path, err := Install(context.Background(), "opentofu/boundary", "1.0.0", fr.srv.URL, cacheDir)
		if err != nil {
			t.Fatalf("Install with %d-byte archive at exact limit: %v", archiveSize, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Stat installed provider: %v", err)
		}
	})

	t.Run("one byte over limit is rejected", func(t *testing.T) {
		setMaxProviderArchiveBytes(t, archiveSize-1)
		tempDir := t.TempDir()
		t.Setenv("TMPDIR", tempDir)
		cacheDir := filepath.Join(t.TempDir(), "cache")

		_, err := Install(context.Background(), "opentofu/boundary", "1.0.0", fr.srv.URL, cacheDir)
		if err == nil {
			t.Fatal("Install: want error for an archive one byte over the size limit, got nil")
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d byte limit", archiveSize-1)) {
			t.Errorf("error = %q, want exact byte limit", err)
		}
		assertNoArchiveInstallResidue(t, cacheDir, tempDir, "opentofu", "boundary", "1.0.0")
	})
}
func providerInstallPath(cacheDir, namespace, name, version string) (string, string) {
	destDir := filepath.Join(cacheDir, namespace, name, version, runtime.GOOS+"_"+runtime.GOARCH)
	return destDir, filepath.Join(destDir, "terraform-provider-"+name)
}

func TestInstall_SymlinkDestinationTargetUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and POSIX mode assertions are not portable to Windows")
	}

	const sentinel = "do not overwrite this external file"
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "provider binary")
	cacheDir := t.TempDir()
	destDir, destPath := providerInstallPath(cacheDir, fr.ns, fr.name, fr.version)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	realTarget := filepath.Join(t.TempDir(), "external-target")
	if err := os.WriteFile(realTarget, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("WriteFile real target: %v", err)
	}
	before, err := os.Lstat(realTarget)
	if err != nil {
		t.Fatalf("Lstat real target before install: %v", err)
	}
	if err := os.Symlink(realTarget, destPath); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("creating symlink is not permitted: %v", err)
		}
		t.Fatalf("Symlink: %v", err)
	}

	_, installErr := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
	info, lstatErr := os.Lstat(destPath)
	if installErr == nil {
		if lstatErr != nil {
			t.Fatalf("Lstat destination after successful Install: %v", lstatErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("successful Install left destination mode %v, want a regular non-symlink", info.Mode())
		}
	} else if lstatErr != nil {
		t.Fatalf("Lstat rejected destination: %v", lstatErr)
	}

	got, err := os.ReadFile(realTarget) //nolint:gosec // realTarget is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile real target after install: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("real target content = %q, want unchanged sentinel %q", got, sentinel)
	}
	after, err := os.Lstat(realTarget)
	if err != nil {
		t.Fatalf("Lstat real target after install: %v", err)
	}
	if after.Mode() != before.Mode() {
		t.Errorf("real target mode = %v, want unchanged %v", after.Mode(), before.Mode())
	}
}

func TestInstall_ExtractionFailureLeavesNoPartialBinary(t *testing.T) {
	orig := maxZipEntryBytes
	maxZipEntryBytes = 16
	t.Cleanup(func() { maxZipEntryBytes = orig })

	fr := newFakeRegistry(t, "opentofu", "bigbin", "1.0.0", strings.Repeat("A", 17))
	cacheDir := t.TempDir()
	destDir, destPath := providerInstallPath(cacheDir, fr.ns, fr.name, fr.version)

	_, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want extraction error for oversized entry, got nil")
	}
	if _, statErr := os.Lstat(destPath); !os.IsNotExist(statErr) {
		t.Errorf("Lstat(%s) error = %v, want destination to remain absent", destPath, statErr)
	}
	temps, globErr := filepath.Glob(filepath.Join(destDir, ".terraform-provider-*.tmp"))
	if globErr != nil {
		t.Fatalf("Glob temporary provider files: %v", globErr)
	}
	if len(temps) != 0 {
		t.Errorf("temporary provider files remain after extraction failure: %v", temps)
	}
}

func TestInstall_EmptyBinaryContent(t *testing.T) {
	fr := newFakeRegistry(t, "opentofu", "emptybin", "1.0.0", "")
	path, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, t.TempDir())
	if err != nil {
		t.Fatalf("Install empty binary: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // path is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile empty binary: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty provider content length = %d, want 0", len(got))
	}
}

func TestInstall_SuccessfulInstallIsAtomicRegularExecutable(t *testing.T) {
	const content = "#!/bin/sh\necho complete-provider\n"
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", content)
	cacheDir := t.TempDir()

	path, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat installed provider: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("installed provider mode = %v, want regular non-symlink", info.Mode())
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o755 {
			t.Errorf("installed provider permissions = %#o, want 0755", got)
		}
		if got := info.Mode().Perm() & 0o022; got != 0 {
			t.Errorf("installed provider group/world write bits = %#o, want 0", got)
		}
	}
	got, err := os.ReadFile(path) //nolint:gosec // path is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile installed provider: %v", err)
	}
	if string(got) != content {
		t.Errorf("installed provider content = %q, want %q", got, content)
	}
}

func TestInstall_ConcurrentInstallsNoCorruptOutput(t *testing.T) {
	const (
		installers = 8
		content    = "#!/bin/sh\necho complete-concurrent-provider\n"
	)
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", content)
	cacheDir := t.TempDir()
	destDir, destPath := providerInstallPath(cacheDir, fr.ns, fr.name, fr.version)

	var wg sync.WaitGroup
	errs := make(chan error, installers)
	for range installers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Install: %v", err)
		}
	}

	got, err := os.ReadFile(destPath) //nolint:gosec // destPath is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile final provider: %v", err)
	}
	if string(got) != content {
		t.Errorf("final provider content = %q, want complete content %q", got, content)
	}
	info, err := os.Lstat(destPath)
	if err != nil {
		t.Fatalf("Lstat final provider: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("final provider mode = %v, want regular non-symlink", info.Mode())
	}
	temps, globErr := filepath.Glob(filepath.Join(destDir, ".terraform-provider-*.tmp"))
	if globErr != nil {
		t.Fatalf("Glob temporary provider files: %v", globErr)
	}
	if len(temps) != 0 {
		t.Errorf("temporary provider files remain after concurrent installs: %v", temps)
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

func installExpectFailure(t *testing.T, fr *fakeRegistry) error {
	t.Helper()
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	cacheDir := filepath.Join(t.TempDir(), "cache")

	_, err := Install(context.Background(), fr.ns+"/"+fr.name, fr.version, fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want verification error, got nil")
	}
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Errorf("cache dir %s should not exist after verification failure", cacheDir)
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil {
		t.Fatalf("ReadDir(%s): %v", tempDir, readErr)
	}
	if len(entries) != 0 {
		t.Errorf("temporary provider archive was not cleaned up: %v", entries)
	}
	return err
}

func TestInstall_ChecksumMismatch(t *testing.T) {
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "signed content")
	// Serve different archive bytes without changing the signed checksum or
	// descriptor. Authentic metadata must not make tampered bytes acceptable.
	fr.zipBytes = buildFakeProviderZip(t, fr.name, "tampered content")

	err := installExpectFailure(t, fr)
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want it to report a checksum mismatch", err)
	}
}

func TestInstall_SignatureAndChecksumMetadataFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, *fakeRegistry)
		wantErr string
	}{
		{
			name: "tampered SHA256SUMS",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.sumsBytes = append(append([]byte(nil), fr.sumsBytes...), []byte("# altered after signing\n")...)
			},
			wantErr: "signature verification failed",
		},
		{
			name: "malformed signature",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.signature = []byte("not a detached OpenPGP signature")
			},
			wantErr: "signature verification failed",
		},
		{
			name: "unadvertised signing key",
			mutate: func(t *testing.T, fr *fakeRegistry) {
				fr.signature = newTestSigner(t).detachSign(t, fr.sumsBytes)
			},
			wantErr: "signature verification failed",
		},
		{
			name: "missing signature URL",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.includeSignatureURL = false
			},
			wantErr: "missing required shasums_signature_url",
		},
		{
			name: "missing advertised keys",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.advertisedKeys = nil
			},
			wantErr: "no usable signing key advertised",
		},
		{
			name: "empty ascii armor",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.advertisedKeys = []gpgPublicKey{{ASCIIArmor: ""}}
			},
			wantErr: "no usable signing key advertised",
		},
		{
			name: "malformed ascii armor",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.advertisedKeys = []gpgPublicKey{{ASCIIArmor: "not an armored public key"}}
			},
			wantErr: "malformed advertised signing key",
		},
		{
			name: "one malformed key among valid keys",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.advertisedKeys = append(fr.advertisedKeys, gpgPublicKey{ASCIIArmor: "malformed"})
			},
			wantErr: "malformed advertised signing key",
		},
		{
			name: "descriptor disagrees with signed entry",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.descriptorShasum = strings.Repeat("0", 64)
			},
			wantErr: "descriptor shasum disagrees with signed SHA256SUMS",
		},
		{
			name: "short SHA256SUMS hash",
			mutate: func(t *testing.T, fr *fakeRegistry) {
				fr.setSignedSums(t, []byte(fmt.Sprintf("abc  %s\n", fr.filename)))
			},
			wantErr: "want 64 hexadecimal characters",
		},
		{
			name: "non-hex SHA256SUMS hash",
			mutate: func(t *testing.T, fr *fakeRegistry) {
				fr.setSignedSums(t, []byte(fmt.Sprintf("%s  %s\n", strings.Repeat("g", 64), fr.filename)))
			},
			wantErr: "invalid byte",
		},
		{
			name: "short descriptor hash",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.descriptorShasum = "abc"
			},
			wantErr: "want 64 hexadecimal characters",
		},
		{
			name: "non-hex descriptor hash",
			mutate: func(_ *testing.T, fr *fakeRegistry) {
				fr.descriptorShasum = strings.Repeat("g", 64)
			},
			wantErr: "invalid byte",
		},
		{
			name: "duplicate equal entries",
			mutate: func(t *testing.T, fr *fakeRegistry) {
				line := fmt.Sprintf("%s  %s\n", fr.descriptorShasum, fr.filename)
				fr.setSignedSums(t, []byte(line+line))
			},
			wantErr: "duplicate SHA256SUMS entries",
		},
		{
			name: "duplicate conflicting entries",
			mutate: func(t *testing.T, fr *fakeRegistry) {
				fr.setSignedSums(t, []byte(fmt.Sprintf(
					"%s  %s\n%s  %s\n",
					fr.descriptorShasum,
					fr.filename,
					strings.Repeat("0", 64),
					fr.filename,
				)))
			},
			wantErr: "duplicate SHA256SUMS entries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "authentic provider")
			tt.mutate(t, fr)
			err := installExpectFailure(t, fr)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestInstall_UppercaseHashesNormalizeConsistently(t *testing.T) {
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "authentic provider")
	fr.setSignedSums(t, []byte(fmt.Sprintf("%s  %s\n", strings.ToUpper(fr.descriptorShasum), fr.filename)))
	fr.descriptorShasum = strings.ToUpper(fr.descriptorShasum)

	path, err := Install(context.Background(), "opentofu/widget", "1.2.3", fr.srv.URL, t.TempDir())
	if err != nil {
		t.Fatalf("Install with uppercase hashes: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat installed provider: %v", err)
	}
}

func TestInstall_SignatureFetchHonorsContextCancellation(t *testing.T) {
	setRegistryHTTPTimeoutsForTest(t, time.Second, time.Second, time.Second, 500*time.Millisecond, time.Second)
	fr := newFakeRegistry(t, "opentofu", "widget", "1.2.3", "authentic provider")
	started := make(chan struct{})
	fr.signatureHandler = func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Install(ctx, "opentofu/widget", "1.2.3", fr.srv.URL, cacheDir)
		result <- err
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("signature request did not start")
	}

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Install error = %v, want context.Canceled", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Install error = %v, caller cancellation must not surface as deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Install did not return after context cancellation")
	}
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Errorf("cache dir %s should not exist after cancellation", cacheDir)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", tempDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary provider archive was not cleaned up after cancellation: %v", entries)
	}
}

// TestInstall_OversizedEntryRejected pins the bomb-guard behavior: an entry
// larger than maxZipEntryBytes must make Install fail hard, not silently
// truncate the binary and report success. maxZipEntryBytes is temporarily
// lowered so the test doesn't need to generate a real gigabyte-sized fixture.
func TestInstall_OversizedEntryRejected(t *testing.T) {
	orig := maxZipEntryBytes
	maxZipEntryBytes = 16
	t.Cleanup(func() { maxZipEntryBytes = orig })

	// Content well past the lowered cap: prior to the fix, io.CopyN would
	// silently stop at maxZipEntryBytes and Install would return success
	// with a truncated binary on disk.
	content := strings.Repeat("A", 64)
	fr := newFakeRegistry(t, "opentofu", "bigbin", "1.0.0", content)
	cacheDir := t.TempDir()

	_, err := Install(context.Background(), "opentofu/bigbin", "1.0.0", fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want error for a zip entry exceeding the size limit, got nil")
	}
	if !strings.Contains(err.Error(), "zip entry") || !strings.Contains(err.Error(), "exceeds 16 byte limit") {
		t.Errorf("error = %q, want it to identify the independently enforced zip-entry limit", err)
	}

	destDir := filepath.Join(cacheDir, "opentofu", "bigbin", "1.0.0", runtime.GOOS+"_"+runtime.GOARCH)
	binPath := filepath.Join(destDir, "terraform-provider-bigbin")
	if _, statErr := os.Stat(binPath); !os.IsNotExist(statErr) {
		t.Errorf("no (truncated) binary should be left behind at %s after an oversized-entry rejection", binPath)
	}
}

// buildZipWithEntry returns zip bytes containing a single entry with the
// given raw (untrusted) name and content. Unlike buildFakeProviderZip, the
// name is used exactly as given so tests can construct zip-slip attempts.
func buildZipWithEntry(t *testing.T, entryName, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entryName)
	if err != nil {
		t.Fatalf("zip.Create(%q): %v", entryName, err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("zip Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

// TestExtract_ZipSlipRejected pins the zip-slip guard: an entry name that
// escapes the destination directory via ".." traversal must be rejected
// outright, never sanitized-and-extracted. The entry name is given the
// required "terraform-provider-" prefix so it actually reaches
// safeZipEntryName instead of being skipped by the earlier prefix filter.
func TestExtract_ZipSlipRejected(t *testing.T) {
	ns, name, version := "opentofu", "evil", "1.0.0"
	entryName := "terraform-provider-x/../../evil"
	fr := newFakeRegistry(t, ns, name, version, "placeholder")
	fr.setPackage(t, buildZipWithEntry(t, entryName, "malicious payload"))

	rootDir := t.TempDir()
	cacheDir := filepath.Join(rootDir, "cache")

	_, err := Install(context.Background(), ns+"/"+name, version, fr.srv.URL, cacheDir)
	if err == nil {
		t.Fatal("Install: want error for a zip-slip entry name, got nil")
	}
	if !strings.Contains(err.Error(), "unsafe zip entry name") {
		t.Errorf("error = %q, want it to report an unsafe zip entry name", err)
	}

	// Nothing should have been written anywhere under rootDir, whether
	// inside cacheDir or via traversal outside it.
	var found []string
	walkErr := filepath.Walk(rootDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("Walk: %v", walkErr)
	}
	if len(found) != 0 {
		t.Errorf("expected no files written anywhere under %s, found %v", rootDir, found)
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

func TestList_IgnoresInvalidCoordinateDirectories(t *testing.T) {
	rootDir := t.TempDir()
	cacheRoot := filepath.Join(rootDir, "cache")
	cacheDir := filepath.Join(cacheRoot, "nested", "..")

	writeProvider := func(namespace, name, version string) string {
		t.Helper()
		binDir := filepath.Join(cacheRoot, namespace, name, version, "linux_amd64")
		if err := os.MkdirAll(binDir, 0o750); err != nil {
			t.Fatalf("MkdirAll(%s): %v", binDir, err)
		}
		binPath := filepath.Join(binDir, "terraform-provider-"+name)
		if err := os.WriteFile(binPath, []byte("bin"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", binPath, err)
		}
		return binPath
	}

	validPath := writeProvider("opentofu", "widget", "1.2.3")
	writeProvider("bad[namespace", "widget", "1.2.3")
	writeProvider("opentofu", "bad[name", "1.2.3")
	writeProvider("opentofu", "widget", "[")
	writeProvider("OpenTofu", "widget", "1.2.3")

	got, err := List(cacheDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d entries, want only the valid entry: %+v", len(got), got)
	}
	want := Installed{Source: "opentofu/widget", Version: "1.2.3", Path: validPath}
	if got[0] != want {
		t.Errorf("List entry = %+v, want %+v", got[0], want)
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

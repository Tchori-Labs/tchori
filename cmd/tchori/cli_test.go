package main_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The CLI is tested end to end: TestMain builds the real tchori binary and
// the Task 5 fake provider into a shared temp dir, and each test runs real
// subprocess invocations against a config directory, asserting the
// agent-facing exit-code contract (0 = ok/no changes, 2 = changes, 1 = error).
var (
	tchoriBin string // built tchori binary
	pluginDir string // directory containing terraform-provider-tchoritest
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

// testMain exists so deferred cleanup runs before os.Exit. TestMain has no
// *testing.T, hence os.MkdirTemp instead of t.TempDir.
func testMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "tchori-cli-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "MkdirTemp:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	tchoriBin = filepath.Join(tmp, "tchori")
	pluginDir = filepath.Join(tmp, "plugins")
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "MkdirAll:", err)
		return 1
	}

	builds := []struct{ target, pkg string }{
		{tchoriBin, "github.com/tchori-labs/tchori/cmd/tchori"},
		{filepath.Join(pluginDir, "terraform-provider-tchoritest"),
			"github.com/tchori-labs/tchori/internal/provider/testprovider"},
	}
	for _, b := range builds {
		cmd := exec.Command("go", "build", "-o", b.target, b.pkg) //nolint:gosec // fixed command; targets are temp-dir artifacts
		cmd.Dir = filepath.Join("..", "..")                       // module root; go test runs with cwd = cmd/tchori
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build %s: %v\n%s", b.pkg, err, out)
			return 1
		}
	}
	return m.Run()
}

// runCLI executes the built tchori binary in dir and returns stdout, stderr,
// and the exit code. exec.ExitError.ExitCode() parses the code portably
// (ProcessState.ExitCode works on both unix and windows).
func runCLI(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	return runCLIEnv(t, dir, nil, args...)
}

// runCLIEnv is runCLI with selected environment variables replaced. It is
// used for options whose contract is intentionally environment-based, while
// keeping all other inherited variables (including PATH) intact.
func runCLIEnv(t *testing.T, dir string, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(tchoriBin, args...) //nolint:gosec // binary built by TestMain into a temp dir
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for key, value := range env {
		prefix := key + "="
		filtered := cmd.Env[:0]
		for _, entry := range cmd.Env {
			if !strings.HasPrefix(entry, prefix) {
				filtered = append(filtered, entry)
			}
		}
		cmd.Env = append(filtered, prefix+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("tchori %v did not run: %v\nstderr: %s", args, err, stderr.String())
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// writeConfig writes a one-provider one-resource config whose provider is
// resolved from the type prefix (tchoritest_thing -> tchoritest).
func writeConfig(t *testing.T, dir, name string) {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest_thing.demo": {
      "config": {"name": %q}
    }
  }
}`, name)
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// newCLIRegistryFixture serves one provider release with the same metadata,
// archive, and SHA256SUMS chain as the OpenTofu registry protocol.
func newCLIRegistryFixture(t *testing.T, namespace, name, version string) *httptest.Server {
	t.Helper()

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("terraform-provider-" + name)
	if err != nil {
		t.Fatalf("creating fixture archive entry: %v", err)
	}
	if _, err := entry.Write([]byte("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatalf("writing fixture archive entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing fixture archive: %v", err)
	}

	archiveBytes := archive.Bytes()
	sum := sha256.Sum256(archiveBytes)
	sumHex := hex.EncodeToString(sum[:])
	filename := fmt.Sprintf("terraform-provider-%s_%s_%s_%s.zip", name, version, runtime.GOOS, runtime.GOARCH)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/{namespace}/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("namespace") != namespace || r.PathValue("name") != name {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{{"version": version, "protocols": []string{"6.0"}}},
		})
	})
	mux.HandleFunc("GET /v1/providers/{namespace}/{name}/{version}/download/{goos}/{goarch}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("namespace") != namespace || r.PathValue("name") != name ||
			r.PathValue("version") != version || r.PathValue("goos") != runtime.GOOS ||
			r.PathValue("goarch") != runtime.GOARCH {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"filename":     filename,
			"download_url": srv.URL + "/dl/" + filename,
			"shasums_url":  srv.URL + "/dl/SHA256SUMS",
			"shasum":       sumHex,
		})
	})
	mux.HandleFunc("GET /dl/"+filename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archiveBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", sumHex, filename)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProvidersInstallRegistryOverride(t *testing.T) {
	const (
		namespace = "example"
		name      = "fixture"
		version   = "1.2.3"
	)
	registry := newCLIRegistryFixture(t, namespace, name, version)
	home := t.TempDir()
	work := t.TempDir()
	env := map[string]string{
		"HOME":                home,
		"TCHORI_REGISTRY_URL": registry.URL,
	}

	if stdout, stderr, code := runCLIEnv(t, work, env, "providers", "install", namespace+"/"+name, version); code != 0 {
		t.Fatalf("providers install: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	stdout, stderr, code := runCLIEnv(t, work, env, "-json", "providers", "list")
	if code != 0 {
		t.Fatalf("providers list: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var installed []struct {
		Source  string `json:"source"`
		Version string `json:"version"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &installed); err != nil {
		t.Fatalf("providers list output is not JSON: %v\n%s", err, stdout)
	}
	if len(installed) != 1 {
		t.Fatalf("providers list returned %d entries, want 1: %+v", len(installed), installed)
	}

	got := installed[0]
	wantDir := filepath.Join(home, ".tchori", "providers", namespace, name, version, runtime.GOOS+"_"+runtime.GOARCH)
	if got.Source != namespace+"/"+name || got.Version != version {
		t.Errorf("installed provider = %s@%s, want %s@%s", got.Source, got.Version, namespace+"/"+name, version)
	}
	if filepath.Dir(got.Path) != wantDir {
		t.Errorf("installed path dir = %s, want %s", filepath.Dir(got.Path), wantDir)
	}
	if filepath.Base(got.Path) != "terraform-provider-"+name {
		t.Errorf("installed binary = %s, want terraform-provider-%s", filepath.Base(got.Path), name)
	}
	info, err := os.Stat(got.Path)
	if err != nil {
		t.Fatalf("stat installed provider: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode = %v, want executable bits", info.Mode())
	}
}

func TestCLILifecycle(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	pd := "--plugin-dir=" + pluginDir

	// validate: clean config -> exit 0.
	if _, stderr, code := runCLI(t, dir, "validate", pd); code != 0 {
		t.Fatalf("validate: exit %d, want 0\nstderr: %s", code, stderr)
	}

	// plan with a pending create -> exit 2, human summary on stdout.
	stdout, stderr, code := runCLI(t, dir, "plan", pd)
	if code != 2 {
		t.Fatalf("plan: exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 to create") {
		t.Errorf("plan stdout missing human summary: %q", stdout)
	}

	// plan -json (single-dash spelling) -> the plan document on stdout.
	stdout, _, code = runCLI(t, dir, "plan", pd, "-json")
	if code != 2 {
		t.Fatalf("plan -json: exit %d, want 2\nstdout: %s", code, stdout)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("plan -json stdout is not JSON: %v\n%s", err, stdout)
	}
	if doc["format_version"] != "1.0" {
		t.Errorf("plan -json format_version = %v, want %q", doc["format_version"], "1.0")
	}

	// plan -out -> plan file written, still exit 2.
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "plan.json"); code != 2 {
		t.Fatalf("plan -out: exit %d, want 2\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "plan.json")); err != nil {
		t.Fatalf("plan.json not written: %v", err)
	}

	// apply the saved plan -> exit 0.
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "plan.json"); code != 0 {
		t.Fatalf("apply: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// state list -> exactly the one managed address.
	stdout, _, code = runCLI(t, dir, "state", "list")
	if code != 0 {
		t.Fatalf("state list: exit %d, want 0", code)
	}
	if stdout != "tchoritest_thing.demo\n" {
		t.Errorf("state list = %q, want %q", stdout, "tchoritest_thing.demo\n")
	}

	// state show -> attributes include the apply-computed id ("t-" prefix
	// from provider config proves Configure ran with the composed config).
	stdout, _, code = runCLI(t, dir, "state", "show", "tchoritest_thing.demo")
	if code != 0 {
		t.Fatalf("state show: exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "t-id-demo") {
		t.Errorf("state show missing computed id t-id-demo: %q", stdout)
	}

	// plan after apply: no changes -> exit 0.
	stdout, stderr, code = runCLI(t, dir, "plan", pd)
	if code != 0 {
		t.Fatalf("plan (no changes): exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "No changes") {
		t.Errorf("plan (no changes) stdout = %q, want it to say No changes", stdout)
	}

	// destroy without -out and without a TTY must refuse -> exit 1.
	if _, stderr, code := runCLI(t, dir, "destroy", pd); code != 1 {
		t.Fatalf("destroy without TTY: exit %d, want 1\nstderr: %s", code, stderr)
	}

	// destroy -out writes a delete plan -> exit 2; applying it empties state.
	if _, stderr, code := runCLI(t, dir, "destroy", pd, "-out", "destroy.json"); code != 2 {
		t.Fatalf("destroy -out: exit %d, want 2\nstderr: %s", code, stderr)
	}
	if _, stderr, code := runCLI(t, dir, "apply", pd, "destroy.json"); code != 0 {
		t.Fatalf("apply destroy.json: exit %d, want 0\nstderr: %s", code, stderr)
	}
	stdout, _, code = runCLI(t, dir, "state", "list")
	if code != 0 || stdout != "" {
		t.Fatalf("state list after destroy: exit %d, stdout %q; want 0 and empty", code, stdout)
	}
}

func TestValidateInvalidName(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "invalid") // fake provider rejects name == "invalid"

	_, stderr, code := runCLI(t, dir, "validate", "--plugin-dir="+pluginDir)
	if code != 1 {
		t.Fatalf("validate: exit %d, want 1\nstderr: %s", code, stderr)
	}
	// stderr is a pipe (not a TTY), so diagnostics must be JSON lines.
	if !strings.Contains(stderr, `"severity":"error"`) {
		t.Errorf("stderr carries no structured JSON error diagnostic: %q", stderr)
	}
	if !strings.Contains(stderr, "invalid name") {
		t.Errorf("stderr does not carry the provider's diagnostic summary: %q", stderr)
	}
}

// TestValidateUnsupportedResourceType guards the fix for issue #5: a config
// referencing tchoritest_broken_thing (a resource type whose schema tchori
// cannot convert — see testprovider's brokenThingSchema, a nested_type
// attribute) must fail validate with a diagnostic naming the stored
// conversion detail, not crash or report a generic/misleading error. Other
// tests in this file (e.g. TestCLILifecycle) prove that a config touching
// only fully-supported flat resources still validates and plans cleanly even
// though the provider now also exposes this unsupported type.
func TestValidateUnsupportedResourceType(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest_broken_thing.boom": {
      "config": {"name": "boom"}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, stderr, code := runCLI(t, dir, "validate", "--plugin-dir="+pluginDir)
	if code != 1 {
		t.Fatalf("validate: exit %d, want 1\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, `"severity":"error"`) {
		t.Errorf("stderr carries no structured JSON error diagnostic: %q", stderr)
	}
	if !strings.Contains(stderr, "unsupported schema") {
		t.Errorf("stderr missing \"unsupported schema\": %q", stderr)
	}
	if !strings.Contains(stderr, "nested_type") {
		t.Errorf("stderr missing the stored nested_type detail: %q", stderr)
	}
}

func TestChdirGlobalFlag(t *testing.T) {
	cfgDir := t.TempDir()
	writeConfig(t, cfgDir, "demo")
	elsewhere := t.TempDir() // deliberately NOT the config dir

	_, stderr, code := runCLI(t, elsewhere, "-chdir="+cfgDir, "validate", "--plugin-dir="+pluginDir)
	if code != 0 {
		t.Fatalf("-chdir validate: exit %d, want 0\nstderr: %s", code, stderr)
	}
}

func TestVersion(t *testing.T) {
	stdout, _, code := runCLI(t, t.TempDir(), "version")
	if code != 0 {
		t.Fatalf("version: exit %d, want 0", code)
	}
	if stdout != "0.1.0-dev\n" {
		t.Errorf("version = %q, want %q", stdout, "0.1.0-dev\n")
	}
}

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
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
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
		{filepath.Join(pluginDir, "terraform-provider-tchoritest5"),
			"github.com/tchori-labs/tchori/internal/provider/testprovider5"},
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
func writeNamedConfig(t *testing.T, dir, address, name, prefix string) {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": %q}
    }
  },
  "resources": {
    %q: {
      "config": {"name": %q}
    }
  }
}`, prefix, address, name)
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

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

// writeConfig5 writes a one-provider one-resource config against the
// protocol-5 fake provider (tchoritest5_thing -> tchoritest5), proving the
// tfplugin5 adapter composes with the full command surface.
func decodeDiagnosticLines(t *testing.T, stderr string) []struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	Address  string `json:"address"`
} {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	out := make([]struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		Address  string `json:"address"`
	}, 0, len(lines))
	for _, line := range lines {
		// terraform-plugin-go writes its own timestamped provider log lines to
		// the inherited stderr when a fake RPC returns an error diagnostic.
		// The engine diagnostics themselves remain one compact JSON object per
		// line; ignore those provider-subprocess logs here.
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var d struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
			Address  string `json:"address"`
		}
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("stderr line is not one compact JSON diagnostic: %v\nline: %s\nstderr: %s", err, line, stderr)
		}
		out = append(out, d)
	}
	return out
}

func writeConfig5(t *testing.T, dir, name string) {
	t.Helper()
	cfg := fmt.Sprintf(`{
  "providers": {
    "tchoritest5": {
      "source": "tchori-labs/tchoritest5",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest5_thing.demo": {
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

	// Ephemeral signing key: authenticates the fixture's SHA256SUMS the same
	// way registry.Install requires of a real registry (detached OpenPGP
	// signature over shasums_url bytes, key advertised via signing_keys).
	entity, err := openpgp.NewEntity("tchori-test", "cli fixture signing key", "test@example.com", nil)
	if err != nil {
		t.Fatalf("openpgp.NewEntity: %v", err)
	}
	var publicArmor bytes.Buffer
	armorWriter, err := armor.Encode(&publicArmor, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := entity.Serialize(armorWriter); err != nil {
		t.Fatalf("serialize public key: %v", err)
	}
	if err := armorWriter.Close(); err != nil {
		t.Fatalf("close public-key armor: %v", err)
	}
	keyID := fmt.Sprintf("%016X", entity.PrimaryKey.KeyId)

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
	sumsBytes := []byte(fmt.Sprintf("%s  %s\n", sumHex, filename))
	var signature bytes.Buffer
	if err := openpgp.DetachSign(&signature, entity, bytes.NewReader(sumsBytes), nil); err != nil {
		t.Fatalf("openpgp.DetachSign: %v", err)
	}

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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"filename":              filename,
			"download_url":          srv.URL + "/dl/" + filename,
			"shasums_url":           srv.URL + "/dl/SHA256SUMS",
			"shasums_signature_url": srv.URL + "/dl/SHA256SUMS.sig",
			"shasum":                sumHex,
			"signing_keys": map[string]any{
				"gpg_public_keys": []map[string]string{{
					"key_id":      keyID,
					"ascii_armor": publicArmor.String(),
				}},
			},
		})
	})
	mux.HandleFunc("GET /dl/"+filename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archiveBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sumsBytes)
	})
	mux.HandleFunc("GET /dl/SHA256SUMS.sig", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(signature.Bytes())
	})

	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
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

func TestCLIApplyReportsInconsistentProviderResult(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "providers": {"tchoritest": {"source":"tchori-labs/tchoritest","version":"0.0.1","config":{}}},
  "resources": {"tchoritest_lossy.svc": {"config":{"name":"svc","flag":true,"secret":"do-not-print"}}}
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	pd := "--plugin-dir=" + pluginDir
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "plan.json"); code != 2 {
		t.Fatalf("plan: exit %d, stderr %s", code, stderr)
	}
	_, stderr, code := runCLI(t, dir, "apply", pd, "-json", "plan.json")
	if code != 1 {
		t.Fatalf("apply: exit %d, want 1; stderr %s", code, stderr)
	}
	ds := decodeDiagnosticLines(t, stderr)
	if len(ds) != 3 {
		t.Fatalf("diagnostics = %#v, want inconsistent-result error, abort accounting, and incomplete-state warning", ds)
	}
	d := ds[0]
	if d.Severity != "error" || d.Address != "tchoritest_lossy.svc" || !strings.Contains(d.Detail, "flag: planned true, applied false") || strings.Contains(d.Detail, "do-not-print") {
		t.Fatalf("diagnostic = %#v", d)
	}
	stateBytes, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // dir is a test-owned t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stateBytes, []byte(`"flag": false`)) {
		t.Fatalf("state does not record provider's false: %s", stateBytes)
	}
}

func assertAPIFailureDiagnostics(t *testing.T, stderr, address string) {
	t.Helper()
	diagnostics := decodeDiagnosticLines(t, stderr)
	counts := map[string]int{}
	for _, d := range diagnostics {
		counts[d.Summary]++
		switch d.Summary {
		case "Error updating service":
			if d.Severity != "error" || d.Detail != "api error (status 400): Invalid request" || d.Address != address {
				t.Fatalf("provider diagnostic = %#v", d)
			}
		case "apply aborted", "attempted change":
			if d.Severity != "warning" || d.Address != address || d.Detail == "" {
				t.Fatalf("accounting diagnostic = %#v", d)
			}
			var pretty bytes.Buffer
			diag.Emit(&pretty, diag.Diagnostics{{Severity: diag.Warning, Summary: d.Summary, Detail: d.Detail, Address: d.Address}}, true)
			prettyText := pretty.String()
			if !strings.HasPrefix(prettyText, "Warning: "+d.Summary+" ("+address+")\n") || strings.HasSuffix(prettyText, "\n\n") {
				t.Fatalf("pretty diagnostic = %q", prettyText)
			}
			for _, line := range strings.Split(d.Detail, "\n") {
				if !strings.Contains(prettyText, "  "+line+"\n") {
					t.Fatalf("pretty detail did not indent source line %q: %q", line, prettyText)
				}
			}
		}
	}
	for _, summary := range []string{"Error updating service", "apply aborted", "attempted change"} {
		if counts[summary] != 1 {
			t.Fatalf("%s count = %d, want 1; diagnostics = %#v", summary, counts[summary], diagnostics)
		}
	}
}

func TestCLIApplyReportsProvider400WithAbortAccounting(t *testing.T) {
	dir := t.TempDir()
	pd := "--plugin-dir=" + pluginDir
	writeConfig(t, dir, "before")
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "seed.json"); code != 2 {
		t.Fatalf("seed plan: exit %d, stderr %s", code, stderr)
	}
	if _, stderr, code := runCLI(t, dir, "apply", pd, "seed.json"); code != 0 {
		t.Fatalf("seed apply: exit %d, stderr %s", code, stderr)
	}
	writeConfig(t, dir, "api_400")
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "failure.json"); code != 2 {
		t.Fatalf("failure plan: exit %d, stderr %s", code, stderr)
	}
	_, stderr, code := runCLI(t, dir, "apply", pd, "-json", "failure.json")
	if code != 1 {
		t.Fatalf("apply: exit %d, want 1; stderr %s", code, stderr)
	}
	assertAPIFailureDiagnostics(t, stderr, "tchoritest_thing.demo")
}

func TestCLIProtocol5ApplyReportsProvider400WithAbortAccounting(t *testing.T) {
	dir := t.TempDir()
	pd := "--plugin-dir=" + pluginDir
	writeConfig5(t, dir, "before")
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "seed.json"); code != 2 {
		t.Fatalf("seed plan: exit %d, stderr %s", code, stderr)
	}
	if _, stderr, code := runCLI(t, dir, "apply", pd, "seed.json"); code != 0 {
		t.Fatalf("seed apply: exit %d, stderr %s", code, stderr)
	}
	writeConfig5(t, dir, "api_400")
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "failure.json"); code != 2 {
		t.Fatalf("failure plan: exit %d, stderr %s", code, stderr)
	}
	_, stderr, code := runCLI(t, dir, "apply", pd, "-json", "failure.json")
	if code != 1 {
		t.Fatalf("apply: exit %d, want 1; stderr %s", code, stderr)
	}
	assertAPIFailureDiagnostics(t, stderr, "tchoritest5_thing.demo")
}

func TestCLIRefreshDriftRenderedOnBothProtocols(t *testing.T) {
	protocols := []struct {
		name    string
		write   func(*testing.T, string, string)
		address string
	}{
		{name: "protocol6", write: writeConfig, address: "tchoritest_thing.demo"},
		{name: "protocol5", write: writeConfig5, address: "tchoritest5_thing.demo"},
	}
	for _, protocol := range protocols {
		protocol := protocol
		t.Run(protocol.name+"_pending_update", func(t *testing.T) {
			dir := t.TempDir()
			seedAppliedResource(t, dir, protocol.write, "drift-a")
			protocol.write(t, dir, "drift-b")
			stdout, stderr, code := runCLI(t, dir, "plan", "--plugin-dir="+pluginDir, "-out", "pending.json")
			if code != 2 {
				t.Fatalf("plan: exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			for _, text := range []string{"echo = \"degraded:unhealthy\" -> (known after apply)", "Note: objects have changed outside tchori"} {
				if !strings.Contains(stdout, text) {
					t.Errorf("plan stdout missing %q:\n%s", text, stdout)
				}
			}
			assertSavedDrift(t, filepath.Join(dir, "pending.json"), protocol.address, "echo")
		})

		t.Run(protocol.name+"_no_pending_change", func(t *testing.T) {
			dir := t.TempDir()
			seedAppliedResource(t, dir, protocol.write, "drift-a")
			stdout, stderr, code := runCLI(t, dir, "plan", "--plugin-dir="+pluginDir, "-out", "drift.json")
			if code != 0 {
				t.Fatalf("plan: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			for _, text := range []string{"Note: objects have changed outside tchori", "degraded:unhealthy", "No changes. Configuration matches state."} {
				if !strings.Contains(stdout, text) {
					t.Errorf("plan stdout missing %q:\n%s", text, stdout)
				}
			}
			assertSavedDrift(t, filepath.Join(dir, "drift.json"), protocol.address, "echo")
		})
	}
}

func TestCLIProtocol5VanishedObjectDrift(t *testing.T) {
	dir := t.TempDir()
	seedAppliedResource(t, dir, writeConfig5, "vanish")
	stdout, stderr, code := runCLI(t, dir, "plan", "--plugin-dir="+pluginDir, "-out", "vanished.json")
	if code != 2 {
		t.Fatalf("plan: exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "tchoritest5_thing.demo (object no longer exists)") || !strings.Contains(stdout, "+ tchoritest5_thing.demo") {
		t.Fatalf("vanished plan output missing drift/create markers:\n%s", stdout)
	}
	var pl plan.Plan
	readJSONFile(t, filepath.Join(dir, "vanished.json"), &pl)
	if len(pl.Drift) != 1 || pl.Drift[0].Address != "tchoritest5_thing.demo" || string(pl.Drift[0].After) != "null" {
		t.Fatalf("vanished drift = %#v", pl.Drift)
	}
}

func seedAppliedResource(t *testing.T, dir string, write func(*testing.T, string, string), name string) {
	t.Helper()
	write(t, dir, name)
	pd := "--plugin-dir=" + pluginDir
	if stdout, stderr, code := runCLI(t, dir, "plan", pd, "-out", "seed.json"); code != 2 {
		t.Fatalf("seed plan: exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "seed.json"); code != 0 {
		t.Fatalf("seed apply: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

func assertSavedDrift(t *testing.T, path, address, changedPath string) {
	t.Helper()
	var pl plan.Plan
	readJSONFile(t, path, &pl)
	if pl.FormatVersion != "1.0" {
		t.Fatalf("format_version = %q, want 1.0", pl.FormatVersion)
	}
	if len(pl.Drift) != 1 || pl.Drift[0].Address != address || !slices.Contains(pl.Drift[0].Paths, changedPath) {
		t.Fatalf("drift = %#v, want one %s entry at %s", pl.Drift, changedPath, address)
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test-only path is always rooted in t.TempDir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, b)
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
	if _, exists := doc["drift"]; exists {
		t.Errorf("drift-free plan -json unexpectedly contains drift: %s", stdout)
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

// TestCLILifecycleProtocol5 proves the tfplugin5 adapter composes with the
// full CLI command surface, not just package-level RPCs: validate -> plan
// (exit 2) -> apply (exit 0) -> plan (exit 0, no changes) -> import against
// the protocol-5-only fake provider (testprovider5), launched via
// --plugin-dir exactly like any protocol-6 provider.
func TestCLILifecycleProtocol5(t *testing.T) {
	dir := t.TempDir()
	writeConfig5(t, dir, "demo")
	pd := "--plugin-dir=" + pluginDir

	// validate: clean config -> exit 0.
	if _, stderr, code := runCLI(t, dir, "validate", pd); code != 0 {
		t.Fatalf("validate: exit %d, want 0\nstderr: %s", code, stderr)
	}

	// plan with a pending create -> exit 2.
	stdout, stderr, code := runCLI(t, dir, "plan", pd)
	if code != 2 {
		t.Fatalf("plan: exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 to create") {
		t.Errorf("plan stdout missing human summary: %q", stdout)
	}

	// plan -out -> plan file written, still exit 2.
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "plan.json"); code != 2 {
		t.Fatalf("plan -out: exit %d, want 2\nstderr: %s", code, stderr)
	}

	// apply the saved plan -> exit 0.
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "plan.json"); code != 0 {
		t.Fatalf("apply: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// state show -> attributes include the apply-computed id ("t-" prefix
	// from provider config proves ConfigureProvider ran through the adapter).
	stdout, _, code = runCLI(t, dir, "state", "show", "tchoritest5_thing.demo")
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

	// destroy -out writes a delete plan -> exit 2; applying it empties state.
	if _, stderr, code := runCLI(t, dir, "destroy", pd, "-out", "destroy.json"); code != 2 {
		t.Fatalf("destroy -out: exit %d, want 2\nstderr: %s", code, stderr)
	}
	if _, stderr, code := runCLI(t, dir, "apply", pd, "destroy.json"); code != 0 {
		t.Fatalf("apply destroy.json: exit %d, want 0\nstderr: %s", code, stderr)
	}

	// import the just-destroyed resource back by id -> exit 0. Proves
	// ImportResourceState composes with the adapter through the full CLI
	// import command, not just the package-level RPC.
	stdout, stderr, code = runCLI(t, dir, "import", pd, "tchoritest5_thing.demo", "t-id-demo")
	if code != 0 {
		t.Fatalf("import: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "tchoritest5_thing.demo") {
		t.Errorf("import stdout missing confirmation: %q", stdout)
	}

	// Idempotence: config's name ("demo") matches the id-derived imported
	// name, so plan reports no changes.
	stdout, stderr, code = runCLI(t, dir, "plan", pd)
	if code != 0 {
		t.Fatalf("plan after import: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "No changes") {
		t.Errorf("plan after import stdout = %q, want it to say No changes", stdout)
	}
}

func TestPlanGatewayHTMLDiagnosticsAreAttributedJSONLines(t *testing.T) {
	const addr = "tchoritest_thing.web"
	dir := t.TempDir()
	writeNamedConfig(t, dir, addr, "gateway_html", "t-")
	stateDoc := `{
  "format_version": "1.0",
  "serial": 1,
  "resources": {
    "tchoritest_thing.web": {
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "attributes": {"echo":"gateway_html","id":"id-gateway_html","name":"gateway_html","replace_me":null,"rules":null,"tags":null}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, dir, "plan", "--plugin-dir="+pluginDir, "-json")
	if code != 1 {
		t.Fatalf("plan: exit %d, want 1\nstderr: %s", code, stderr)
	}
	ds := decodeDiagnosticLines(t, stderr)
	if len(ds) != 2 {
		t.Fatalf("diagnostics = %#v, want error and one hint", ds)
	}
	if ds[0].Severity != "error" || ds[0].Address != addr || ds[0].Summary != "Error reading project" || ds[0].Detail != "decoding response: invalid character '<' looking for beginning of value" {
		t.Fatalf("provider diagnostic = %#v", ds[0])
	}
	if ds[1].Severity != "warning" || ds[1].Address != addr || ds[1].Summary != "provider received a non-JSON response (HTML)" {
		t.Fatalf("hint diagnostic = %#v", ds[1])
	}
}

func TestConfigureGatewayHTMLDiagnosticHasProviderAddress(t *testing.T) {
	dir := t.TempDir()
	writeNamedConfig(t, dir, "tchoritest_thing.web", "demo", "gateway_html")
	_, stderr, code := runCLI(t, dir, "validate", "--plugin-dir="+pluginDir)
	if code != 1 {
		t.Fatalf("validate: exit %d, want 1\nstderr: %s", code, stderr)
	}
	ds := decodeDiagnosticLines(t, stderr)
	if len(ds) != 2 || ds[0].Address != "provider.tchoritest" || ds[1].Address != "provider.tchoritest" || ds[1].Severity != "warning" {
		t.Fatalf("configure diagnostics = %#v", ds)
	}
}

func TestImportAndPostImportReadDiagnosticsHaveResourceAddress(t *testing.T) {
	const addr = "tchoritest_thing.web"
	t.Run("ImportResource", func(t *testing.T) {
		dir := t.TempDir()
		writeNamedConfig(t, dir, addr, "demo", "t-")
		_, stderr, code := runCLI(t, dir, "import", "--plugin-dir="+pluginDir, addr, "missing")
		if code != 1 {
			t.Fatalf("import: exit %d, want 1\nstderr: %s", code, stderr)
		}
		ds := decodeDiagnosticLines(t, stderr)
		if len(ds) != 1 || ds[0].Summary != "resource does not exist" || ds[0].Address != addr {
			t.Fatalf("import diagnostics = %#v", ds)
		}
	})
	t.Run("post-import ReadResource", func(t *testing.T) {
		dir := t.TempDir()
		writeNamedConfig(t, dir, addr, "gateway_html", "t-")
		_, stderr, code := runCLI(t, dir, "import", "--plugin-dir="+pluginDir, addr, "t-id-gateway_html")
		if code != 1 {
			t.Fatalf("import refresh: exit %d, want 1\nstderr: %s", code, stderr)
		}
		ds := decodeDiagnosticLines(t, stderr)
		if len(ds) != 2 || ds[0].Summary != "Error reading project" || ds[0].Address != addr || ds[1].Severity != "warning" || ds[1].Address != addr {
			t.Fatalf("post-import refresh diagnostics = %#v", ds)
		}
	})
}

func TestCLIStateStatusAfterFailedApply(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "explode")
	pd := "--plugin-dir=" + pluginDir
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "failed.json"); code != 2 {
		t.Fatalf("plan: exit %d, want 2: %s", code, stderr)
	}
	if _, stderr, code := runCLI(t, dir, "apply", pd, "failed.json"); code != 1 || !strings.Contains(stderr, "apply exploded") {
		t.Fatalf("failed apply: exit %d stderr=%s", code, stderr)
	}

	stdout, _, code := runCLI(t, dir, "state", "status")
	if code != 1 || !strings.Contains(stdout, "tchoritest_thing.demo") {
		t.Fatalf("state status: exit %d stdout=%q", code, stdout)
	}
	if stdout, stderr, code := runCLI(t, dir, "state", "list"); code != 0 || stdout != "" {
		t.Fatalf("state list after failure: exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, _, code = runCLI(t, dir, "-json", "state", "status")
	if code != 1 {
		t.Fatalf("json state status: exit %d stdout=%s", code, stdout)
	}
	var status struct {
		Converged bool `json:"converged"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil || status.Converged {
		t.Fatalf("json status = %q parsed=%+v err=%v", stdout, status, err)
	}

	// Recovery planning remains available and warns instead of changing the
	// established 0/2 plan exit contract.
	_, stderr, code := runCLI(t, dir, "plan", pd, "-out", "recovery.json")
	if code != 2 || !strings.Contains(stderr, `"severity":"warning"`) || !strings.Contains(stderr, "incomplete apply") {
		t.Fatalf("recovery plan: exit %d stderr=%s", code, stderr)
	}
	writeConfig(t, dir, "recovered")
	if _, stderr, code = runCLI(t, dir, "plan", pd, "-out", "recovery.json"); code != 2 {
		t.Fatalf("recovery plan after config fix: exit %d stderr=%s", code, stderr)
	}
	if _, stderr, code = runCLI(t, dir, "apply", pd, "recovery.json"); code != 0 {
		t.Fatalf("recovery apply: exit %d stderr=%s", code, stderr)
	}
	stdout, _, code = runCLI(t, dir, "state", "status")
	if code != 0 || !strings.Contains(stdout, "converged") {
		t.Fatalf("recovered state status: exit %d stdout=%q", code, stdout)
	}
}

func TestCLIRejectsEmbeddedReference(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {}
    }
  },
  "resources": {
    "tchoritest_thing.tunnel": {
      "config": {"name": "tunnel"}
    },
    "tchoritest_thing.wh": {
      "config": {
        "name": "wh",
        "tags": {"content": "${tchoritest_thing.tunnel.id}.cfargotunnel.com"}
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	pd := "--plugin-dir=" + pluginDir

	for _, command := range []string{"validate", "plan"} {
		_, stderr, code := runCLI(t, dir, command, pd)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1\nstderr: %s", command, code, stderr)
		}
		if !strings.Contains(stderr, `"summary":"unresolved reference"`) || !strings.Contains(stderr, "tags.content") {
			t.Errorf("%s stderr missing unresolved-reference summary/path: %q", command, stderr)
		}
	}
}

func TestCLIResourceConfigEnvWrapperLifecycle(t *testing.T) {
	const envName = "TCHORI_TEST_NAME"
	t.Setenv(envName, "alpha")
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
    "tchoritest_thing.demo": {
      "config": {"name": {"env": "TCHORI_TEST_NAME"}}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	pd := "--plugin-dir=" + pluginDir
	assertNoProviderOnlyError := func(command, stderr string) {
		t.Helper()
		if strings.Contains(stderr, "only allowed"+" in provider config") {
			t.Errorf("%s stderr retains resource env-wrapper rejection: %q", command, stderr)
		}
	}

	_, stderr, code := runCLI(t, dir, "validate", pd)
	assertNoProviderOnlyError("validate", stderr)
	if code != 0 {
		t.Fatalf("validate: exit %d, want 0\nstderr: %s", code, stderr)
	}

	_, stderr, code = runCLI(t, dir, "plan", pd, "-out", "plan.json")
	assertNoProviderOnlyError("plan", stderr)
	if code != 2 {
		t.Fatalf("plan -out: exit %d, want 2\nstderr: %s", code, stderr)
	}

	_, stderr, code = runCLI(t, dir, "apply", pd, "plan.json")
	assertNoProviderOnlyError("apply", stderr)
	if code != 0 {
		t.Fatalf("apply: exit %d, want 0\nstderr: %s", code, stderr)
	}

	stateBytes, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test temp directory
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	var stateDoc struct {
		Resources map[string]struct {
			Attributes map[string]any `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(stateBytes, &stateDoc); err != nil {
		t.Fatalf("decode state.json: %v", err)
	}
	if got := stateDoc.Resources["tchoritest_thing.demo"].Attributes["name"]; got != "alpha" {
		t.Errorf("state name = %#v, want environment value %q", got, "alpha")
	}

	stdout, stderr, code := runCLI(t, dir, "plan", pd)
	assertNoProviderOnlyError("follow-up plan", stderr)
	if code != 0 || !strings.Contains(stdout, "No changes") {
		t.Fatalf("follow-up plan: exit %d, want 0 and No changes\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	if err := os.Unsetenv(envName); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	_, stderr, code = runCLI(t, dir, "validate", pd)
	assertNoProviderOnlyError("validate with unset variable", stderr)
	if code != 0 {
		t.Fatalf("validate with unset variable: exit %d, want 0\nstderr: %s", code, stderr)
	}

	_, stderr, code = runCLI(t, dir, "destroy", pd, "-out", "destroy.json")
	assertNoProviderOnlyError("destroy", stderr)
	if code != 2 {
		t.Fatalf("destroy -out with unset variable: exit %d, want 2\nstderr: %s", code, stderr)
	}
	_, stderr, code = runCLI(t, dir, "apply", pd, "destroy.json")
	assertNoProviderOnlyError("apply destroy", stderr)
	if code != 0 {
		t.Fatalf("apply destroy with unset variable: exit %d, want 0\nstderr: %s", code, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "state", "list")
	if code != 0 || stdout != "" {
		t.Fatalf("state list after destroy: exit %d, stdout %q, stderr %q; want empty", code, stdout, stderr)
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
	if !strings.Contains(stderr, `"address":"tchoritest_thing.demo"`) {
		t.Errorf("stderr does not attribute the provider diagnostic: %q", stderr)
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

func TestValidateChdirEnvCandidateFallback(t *testing.T) {
	const (
		baseURL  = "TCHORI_TEST_BASE_URL"
		endpoint = "TCHORI_TEST_ENDPOINT"
		resolved = "candidate-prefix-do-not-emit-"
	)
	for _, name := range []string{baseURL, endpoint} {
		t.Setenv(name, "restore-for-cleanup")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("Unsetenv(%q): %v", name, err)
		}
	}

	cfgDir := t.TempDir()
	cfg := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": {"env": ["TCHORI_TEST_BASE_URL", "TCHORI_TEST_ENDPOINT"]}}
    }
  },
  "resources": {
    "tchoritest_thing.demo": {"config": {"name": "demo"}}
  }
}`
	if err := os.WriteFile(filepath.Join(cfgDir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	elsewhere := t.TempDir() // exercise the reported command from outside the config directory
	args := []string{"-chdir=" + cfgDir, "validate", "--plugin-dir=" + pluginDir}

	_, stderr, code := runCLI(t, elsewhere, args...)
	if code != 1 {
		t.Fatalf("validate with all candidates unset: exit %d, want 1\nstderr: %s", code, stderr)
	}
	diags := decodeDiagnosticLines(t, stderr)
	if len(diags) != 1 || diags[0].Summary != "environment variable not set" {
		t.Fatalf("diagnostics = %+v, want one environment variable not set error\nstderr: %s", diags, stderr)
	}
	detail := diags[0].Detail
	baseAt := strings.Index(detail, `"`+baseURL+`"`)
	endpointAt := strings.Index(detail, `"`+endpoint+`"`)
	if baseAt < 0 || endpointAt < baseAt {
		t.Errorf("diagnostic does not name candidates in config order: %q", detail)
	}
	for _, want := range []string{`{"env": ...}`, "*.tchori.json", "no built-in or provider-specific", "add the name"} {
		if !strings.Contains(detail, want) {
			t.Errorf("diagnostic %q does not contain guidance %q", detail, want)
		}
	}
	if strings.Contains(stderr, resolved) {
		t.Errorf("unset diagnostic leaked a resolved environment value: %s", stderr)
	}

	t.Setenv(baseURL, resolved)
	stdout, stderr, code := runCLI(t, elsewhere, args...)
	if code != 0 {
		t.Fatalf("validate with fallback candidate set: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Configuration is valid.") {
		t.Errorf("stdout = %q, want validation success", stdout)
	}
	if strings.Contains(stderr, resolved) {
		t.Errorf("successful validation leaked the resolved environment value to stderr: %s", stderr)
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

// readStateFile parses dir/state.json into serial and resource keys, for
// asserting state-file integrity (no serial bump, no partial writes) on
// import error paths. A missing file reads as serial 0, no resources.
func readStateFile(t *testing.T, dir string) (serial uint64, resources map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // G304: dir is a t.TempDir() test fixture, not attacker-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		t.Fatalf("read state.json: %v", err)
	}
	var doc struct {
		Serial    uint64                     `json:"serial"`
		Resources map[string]json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse state.json: %v\n%s", err, data)
	}
	return doc.Serial, doc.Resources
}

// TestImportAdoptsResourceIntoState covers the success path: import writes
// the resource into state.json under the declared address with the
// provider-returned type/attributes, and a subsequent plan is a clean no-op
// (import -> plan idempotence).
func TestImportAdoptsResourceIntoState(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	pd := "--plugin-dir=" + pluginDir

	stdout, stderr, code := runCLI(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo")
	if code != 0 {
		t.Fatalf("import: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "tchoritest_thing.demo") {
		t.Errorf("import stdout missing confirmation: %q", stdout)
	}

	stdout, _, code = runCLI(t, dir, "state", "show", "tchoritest_thing.demo")
	if code != 0 {
		t.Fatalf("state show: exit %d, want 0", code)
	}
	var rs struct {
		Type       string `json:"type"`
		Provider   string `json:"provider"`
		Attributes struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(stdout), &rs); err != nil {
		t.Fatalf("state show output is not JSON: %v\n%s", err, stdout)
	}
	if rs.Type != "tchoritest_thing" {
		t.Errorf("imported type = %q, want %q", rs.Type, "tchoritest_thing")
	}
	if rs.Provider != "tchoritest" {
		t.Errorf("imported provider = %q, want %q", rs.Provider, "tchoritest")
	}
	if rs.Attributes.ID != "t-id-demo" {
		t.Errorf("imported id = %q, want %q", rs.Attributes.ID, "t-id-demo")
	}
	if rs.Attributes.Name != "demo" {
		t.Errorf("imported name = %q, want %q", rs.Attributes.Name, "demo")
	}

	// Idempotence: config's name ("demo") matches the id-derived imported
	// name, so plan reports no changes.
	stdout, stderr, code = runCLI(t, dir, "plan", pd)
	if code != 0 {
		t.Fatalf("plan after import: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "No changes") {
		t.Errorf("plan after import stdout = %q, want it to say No changes", stdout)
	}
}

// TestImportErrorPaths covers import's error contract: undeclared address,
// already-in-state address, and a nonexistent provider ID all exit 1
// without mutating state.json (serial unchanged, no partial resource entry).
func TestImportErrorPaths(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	pd := "--plugin-dir=" + pluginDir

	t.Run("address not declared in config", func(t *testing.T) {
		serialBefore, resBefore := readStateFile(t, dir)
		_, stderr, code := runCLI(t, dir, "import", pd, "tchoritest_thing.nope", "t-id-demo")
		if code != 1 {
			t.Fatalf("import undeclared address: exit %d, want 1\nstderr: %s", code, stderr)
		}
		serialAfter, resAfter := readStateFile(t, dir)
		if serialAfter != serialBefore || len(resAfter) != len(resBefore) {
			t.Fatalf("import undeclared address mutated state: serial %d->%d, resources %d->%d",
				serialBefore, serialAfter, len(resBefore), len(resAfter))
		}
	})

	t.Run("nonexistent provider id", func(t *testing.T) {
		serialBefore, resBefore := readStateFile(t, dir)
		_, stderr, code := runCLI(t, dir, "import", pd, "tchoritest_thing.demo", "no-marker-here")
		if code != 1 {
			t.Fatalf("import nonexistent id: exit %d, want 1\nstderr: %s", code, stderr)
		}
		serialAfter, resAfter := readStateFile(t, dir)
		if serialAfter != serialBefore || len(resAfter) != len(resBefore) {
			t.Fatalf("import nonexistent id mutated state: serial %d->%d, resources %d->%d",
				serialBefore, serialAfter, len(resBefore), len(resAfter))
		}
	})

	// Successful import, then a second import of the same address must
	// refuse to overwrite.
	if _, stderr, code := runCLI(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo"); code != 0 {
		t.Fatalf("import (setup): exit %d, want 0\nstderr: %s", code, stderr)
	}

	t.Run("address already in state", func(t *testing.T) {
		serialBefore, resBefore := readStateFile(t, dir)
		_, stderr, code := runCLI(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo")
		if code != 1 {
			t.Fatalf("import already-in-state: exit %d, want 1\nstderr: %s", code, stderr)
		}
		serialAfter, resAfter := readStateFile(t, dir)
		if serialAfter != serialBefore || len(resAfter) != len(resBefore) {
			t.Fatalf("import already-in-state mutated state: serial %d->%d, resources %d->%d",
				serialBefore, serialAfter, len(resBefore), len(resAfter))
		}
	})
}

func TestStateShowMasksRecordedSensitivityAndNotesOnlyUnscanned(t *testing.T) {
	dir := t.TempDir()
	const sentinel = "tchori-e2e-super-secret-value"
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{` +
		`"secret.masked":{"type":"secret","provider":"test","attributes":{"client_secret":"` + sentinel + `"},"sensitive_paths":["client_secret"]},` +
		`"thing.scanned":{"type":"thing","provider":"test","attributes":{"value":"ok"},"sensitive_scanned":true},` +
		`"thing.legacy":{"type":"thing","provider":"test","attributes":{"value":"legacy"}}}}`
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
	stdout, stderr, code := runCLI(t, dir, "state", "show", "secret.masked")
	if code != 0 || strings.Contains(stdout+stderr, sentinel) || strings.Contains(stderr, "not checked") {
		t.Fatalf("masked show: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "state", "show", "thing.scanned")
	if code != 0 || strings.Contains(stderr, "not checked") {
		t.Fatalf("scanned show: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	_, stderr, code = runCLI(t, dir, "state", "show", "thing.legacy")
	if code != 0 || !strings.Contains(stderr, "not checked") {
		t.Fatalf("legacy note: code=%d stderr=%s", code, stderr)
	}
	after, _ := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
	if !bytes.Equal(before, after) {
		t.Fatal("state show modified state.json")
	}
}

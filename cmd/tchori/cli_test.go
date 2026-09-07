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
	"github.com/rogpeppe/go-internal/testscript"

	tchoricli "github.com/tchori-labs/tchori/cmd/tchori"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/privateblob"
	"github.com/tchori-labs/tchori/internal/sensitive"
	"github.com/tchori-labs/tchori/internal/state"
)

// The CLI is tested end to end: TestMain builds the real tchori binary and
// the Task 5 fake provider into a shared temp dir, and each test runs real
// subprocess invocations against a config directory, asserting the
// agent-facing exit-code contract (0 = ok/no changes, 2 = changes, 1 = error).
var (
	tchoriBin string // built tchori binary
	pluginDir string // directory containing terraform-provider-tchoritest
)

// testArtifactKey is an explicitly non-production key used only to cross the
// environment-only artifact-key boundary in CLI acceptance tests.
const testArtifactKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// TestMain both registers the CLI as testscript's "tchori" command (the txtar
// scripts in testdata/script re-exec this binary under that name; see
// script_test.go) and runs the suite through testMain, which builds the real
// binary and the provider fixtures the subprocess tests need.
// testscript.Main dispatches on argv[0] and always exits, so a re-exec as
// "tchori" never pays for the fixture builds.
func TestMain(m *testing.M) {
	testscript.Main(fixtureM{m}, map[string]func(){
		"tchori": tchoricli.ScriptCmdMain,
	})
}

// fixtureM adapts *testing.M to testscript.TestingM so that testscript runs
// the suite through testMain's fixture builds rather than calling m.Run
// directly.
type fixtureM struct{ m *testing.M }

func (f fixtureM) Run() int { return testMain(f.m) }

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

func runCLIWithArtifactKey(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	return runCLIEnv(t, dir, map[string]string{"TCHORI_ARTIFACT_KEY": testArtifactKey}, args...)
}

// runCLIEnv is runCLI with a deterministic test-only artifact key and selected
// environment variables replaced. Callers testing missing or malformed keys
// override TCHORI_ARTIFACT_KEY explicitly.
func runCLIEnv(t *testing.T, dir string, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(tchoriBin, args...) //nolint:gosec // binary built by TestMain into a temp dir
	cmd.Dir = dir
	cmd.Env = os.Environ()
	overrides := map[string]string{"TCHORI_ARTIFACT_KEY": testArtifactKey}
	for key, value := range env {
		overrides[key] = value
	}
	for key, value := range overrides {
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

func writeSensitiveSetConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest_set_thing.imported": {
      "config": {
        "name": "imported",
        "attribute_members": [
          {"label":"same","token":"imported-attribute-token-one","details":[{"kind":"same","secret":"imported-attribute-detail-one"}]},
          {"label":"same","token":"imported-attribute-token-two","details":[{"kind":"same","secret":"imported-attribute-detail-two"}]}
        ],
        "block_members": [
          {"label":"same","token":"imported-block-token-one","details":[{"kind":"same","secret":"imported-block-detail-one"}]},
          {"label":"same","token":"imported-block-token-two","details":[{"kind":"same","secret":"imported-block-detail-two"}]}
        ]
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func writeRefreshSensitiveConfig(t *testing.T, dir string) {
	t.Helper()
	const cfg = `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest_refresh_sensitive.demo": {
      "config": {"name": "omit-sensitive"}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}
func writeThingResources(t *testing.T, dir string, resources map[string]string) {
	t.Helper()
	writeProtocolThingResources(t, dir, "tchoritest", resources)
}

func writeThingResources5(t *testing.T, dir string, resources map[string]string) {
	t.Helper()
	writeProtocolThingResources(t, dir, "tchoritest5", resources)
}

func writeProtocolThingResources(t *testing.T, dir, providerName string, resources map[string]string) {
	t.Helper()
	var body strings.Builder
	addresses := make([]string, 0, len(resources))
	for address := range resources {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	for i, address := range addresses {
		if i > 0 {
			body.WriteString(",\n")
		}
		_, _ = fmt.Fprintf(&body, "    %q: {\"config\": {\"name\": %q}}", address, resources[address])
	}
	cfg := fmt.Sprintf(`{
  "providers": {
    %q: {
      "source": "tchori-labs/%s",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
%s
  }
}`, providerName, providerName, body.String())
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
	stdout, stderr, code := runCLI(t, dir, "apply", pd, "-json", "plan.json")
	if code != 1 {
		t.Fatalf("apply: exit %d, want 1; stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "Apply incomplete: 1 created, 0 updated, 0 deleted, 0 replaced; 0 changes not executed.") {
		t.Fatalf("apply stdout = %q, want executed create accounting", stdout)
	}
	ds := decodeDiagnosticLines(t, stderr)
	if len(ds) != 3 {
		t.Fatalf("diagnostic count = %d: %#v, want sensitive-state warning, inconsistent-result error, and incomplete-state warning", len(ds), ds)
	}
	foundInconsistent := false
	for _, d := range ds {
		if strings.Contains(d.Detail, "do-not-print") {
			t.Fatalf("diagnostic leaked sensitive value: %#v", d)
		}
		if d.Summary == "provider produced inconsistent result after apply" {
			foundInconsistent = d.Severity == "error" &&
				d.Address == "tchoritest_lossy.svc" &&
				strings.Contains(d.Detail, "flag: planned true, applied false")
		}
	}
	if !foundInconsistent {
		t.Fatalf("diagnostics = %#v, want attributed inconsistent-result error", ds)
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
		case "attempted change":
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
	for _, summary := range []string{"Error updating service", "attempted change"} {
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
	stdout, stderr, code := runCLI(t, dir, "apply", pd, "-json", "failure.json")
	if code != 1 {
		t.Fatalf("apply: exit %d, want 1; stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "Apply incomplete: 0 created, 0 updated, 0 deleted, 0 replaced; 0 changes not executed.") {
		t.Fatalf("apply stdout = %q, want zero executed changes", stdout)
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
	stdout, stderr, code := runCLI(t, dir, "apply", pd, "-json", "failure.json")
	if code != 1 {
		t.Fatalf("apply: exit %d, want 1; stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "Apply incomplete: 0 created, 0 updated, 0 deleted, 0 replaced; 0 changes not executed.") {
		t.Fatalf("apply stdout = %q, want zero executed changes", stdout)
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
	if pl.FormatVersion != plan.FormatVersion {
		t.Fatalf("format_version = %q, want %q", pl.FormatVersion, plan.FormatVersion)
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

func TestCLIApplyFailureDoesNotStarveRemovedResourceDelete(t *testing.T) {
	dir := t.TempDir()
	pd := "--plugin-dir=" + pluginDir
	const drop = "tchoritest_thing.drop"

	writeThingResources(t, dir, map[string]string{drop: "drop"})
	if stdout, stderr, code := runCLI(t, dir, "plan", pd, "-out", "seed.json"); code != 2 {
		t.Fatalf("seed plan: exit %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "seed.json"); code != 0 {
		t.Fatalf("seed apply: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	writeThingResources(t, dir, map[string]string{"tchoritest_thing.boom": "explode"})
	stdout, stderr, code := runCLI(t, dir, "plan", pd, "-out", "mixed.json")
	if code != 2 || !strings.Contains(stdout, "+ tchoritest_thing.boom") || !strings.Contains(stdout, "- "+drop) {
		t.Fatalf("mixed plan: exit %d, want create and delete\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "apply", pd, "mixed.json")
	if code != 1 {
		t.Fatalf("mixed apply: exit %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Apply incomplete: 0 created, 0 updated, 1 deleted, 0 replaced; 0 changes not executed.") {
		t.Fatalf("mixed apply stdout does not report the executed delete: %q", stdout)
	}
	diagnostics := decodeDiagnosticLines(t, stderr)
	if !slices.ContainsFunc(diagnostics, func(d struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		Address  string `json:"address"`
	}) bool {
		return d.Severity == "error" && d.Address == "tchoritest_thing.boom" && d.Summary == "apply exploded"
	}) {
		t.Fatalf("stderr does not name the failing resource: %s", stderr)
	}

	stdout, stderr, code = runCLI(t, dir, "state", "list")
	if code != 0 || strings.Contains(stdout, drop) {
		t.Fatalf("state list after partial apply: exit %d, drop still present=%v\nstdout: %s\nstderr: %s", code, strings.Contains(stdout, drop), stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "plan", pd)
	if code != 2 || strings.Contains(stdout, "- "+drop) {
		t.Fatalf("follow-up plan: exit %d, removed resource delete repeated\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

func TestCLIProtocol5ApplyFailureDoesNotStarveRemovedResourceDelete(t *testing.T) {
	dir := t.TempDir()
	pd := "--plugin-dir=" + pluginDir
	const drop = "tchoritest5_thing.drop"
	const boom = "tchoritest5_thing.boom"

	writeThingResources5(t, dir, map[string]string{drop: "drop"})
	if _, stderr, code := runCLI(t, dir, "plan", pd, "-out", "seed.json"); code != 2 {
		t.Fatalf("seed plan: exit %d, stderr: %s", code, stderr)
	}
	if _, stderr, code := runCLI(t, dir, "apply", pd, "seed.json"); code != 0 {
		t.Fatalf("seed apply: exit %d, stderr: %s", code, stderr)
	}
	writeThingResources5(t, dir, map[string]string{boom: "explode"})
	stdout, stderr, code := runCLI(t, dir, "plan", pd, "-out", "mixed.json")
	if code != 2 || !strings.Contains(stdout, "+ "+boom) || !strings.Contains(stdout, "- "+drop) {
		t.Fatalf("mixed plan: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "apply", pd, "mixed.json")
	if code != 1 || !strings.Contains(stdout, "0 created, 0 updated, 1 deleted, 0 replaced; 0 changes not executed") {
		t.Fatalf("mixed apply: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !slices.ContainsFunc(decodeDiagnosticLines(t, stderr), func(d struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		Address  string `json:"address"`
	}) bool {
		return d.Severity == "error" && d.Address == boom && d.Summary == "apply exploded"
	}) {
		t.Fatalf("stderr does not name protocol-5 failure: %s", stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "state", "list")
	if code != 0 || strings.Contains(stdout, drop) {
		t.Fatalf("state list: exit %d, stdout: %s, stderr: %s", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "plan", pd)
	if code != 2 || strings.Contains(stdout, "- "+drop) {
		t.Fatalf("follow-up plan repeats delete: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
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
	if doc["format_version"] != plan.FormatVersion {
		t.Errorf("plan -json format_version = %v, want %q", doc["format_version"], plan.FormatVersion)
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

	// apply the saved plan -> exit 0 with counts from executed work.
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "plan.json"); code != 0 || !strings.Contains(stdout, "Apply complete: 1 created, 0 updated, 0 deleted, 0 replaced.") {
		t.Fatalf("apply: exit %d, want 0 with truthful counts\nstdout: %s\nstderr: %s", code, stdout, stderr)
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
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "destroy.json"); code != 0 || !strings.Contains(stdout, "Apply complete: 0 created, 0 updated, 1 deleted, 0 replaced.") {
		t.Fatalf("apply destroy.json: exit %d, want 0 with one executed delete\nstdout: %s\nstderr: %s", code, stdout, stderr)
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

	// apply the saved plan -> exit 0 with counts from executed work.
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "plan.json"); code != 0 || !strings.Contains(stdout, "Apply complete: 1 created, 0 updated, 0 deleted, 0 replaced.") {
		t.Fatalf("apply: exit %d, want 0 with truthful counts\nstdout: %s\nstderr: %s", code, stdout, stderr)
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
	if stdout, stderr, code := runCLI(t, dir, "apply", pd, "destroy.json"); code != 0 || !strings.Contains(stdout, "Apply complete: 0 created, 0 updated, 1 deleted, 0 replaced.") {
		t.Fatalf("apply destroy.json: exit %d, want 0 with one executed delete\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// import the just-destroyed resource back by id -> exit 0. Proves
	// ImportResourceState composes with the adapter through the full CLI
	// import command, not just the package-level RPC.
	stdout, stderr, code = runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest5_thing.demo", "t-id-demo")
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
  "format_version": "1.1",
  "serial": 1,
  "resources": {
    "tchoritest_thing.web": {
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "provider_source": "tchori-labs/tchoritest",
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
		_, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--plugin-dir="+pluginDir, addr, "missing")
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
		_, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--plugin-dir="+pluginDir, addr, "t-id-gateway_html")
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

	stdout, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo")
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

func TestImportRefreshReplacesOnlyNamedResource(t *testing.T) {
	dir := t.TempDir()
	writeThingResources(t, dir, map[string]string{
		"tchoritest_thing.one": "one",
		"tchoritest_thing.two": "two",
	})
	pd := "--plugin-dir=" + pluginDir

	for _, tc := range []struct {
		address string
		id      string
	}{
		{address: "tchoritest_thing.one", id: "t-id-one"},
		{address: "tchoritest_thing.two", id: "t-id-two"},
	} {
		if stdout, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, tc.address, tc.id); code != 0 {
			t.Fatalf("import %s: exit %d, want 0\nstdout: %s\nstderr: %s", tc.address, code, stdout, stderr)
		}
	}
	beforeState, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	beforeSerial, beforeResources := readStateFile(t, dir)

	stdout, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, "tchoritest_thing.one", "t-id-one-refreshed")
	if code != 0 {
		t.Fatalf("refresh import: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout != "Refreshed tchoritest_thing.one.\n" {
		t.Fatalf("refresh import stdout = %q, want address-only confirmation", stdout)
	}

	afterSerial, afterResources := readStateFile(t, dir)
	afterState, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(afterState, []byte(`"format_version": "1.3"`)) {
		t.Fatalf("refresh did not persist format 1.3 state: %s", afterState)
	}
	if afterSerial != beforeSerial+1 {
		t.Fatalf("refresh serial = %d, want exactly one increment from %d", afterSerial, beforeSerial)
	}
	if bytes.Equal(beforeResources["tchoritest_thing.one"], afterResources["tchoritest_thing.one"]) {
		t.Fatal("refresh did not replace the named resource")
	}
	if !bytes.Equal(beforeResources["tchoritest_thing.two"], afterResources["tchoritest_thing.two"]) {
		t.Fatal("refresh changed an unrelated resource")
	}
	backup, err := os.ReadFile(filepath.Join(dir, "state.json.backup")) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, beforeState) {
		t.Fatal("refresh backup does not contain the complete pre-refresh state")
	}
}

func TestImportRefreshPreservesOmittedSensitiveState(t *testing.T) {
	dir := t.TempDir()
	writeRefreshSensitiveConfig(t, dir)
	pd := "--plugin-dir=" + pluginDir
	const (
		address   = "tchoritest_refresh_sensitive.demo"
		initialID = "t-id-seeded"
		refreshID = "t-id-omit-sensitive"
	)
	if stdout, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, address, initialID); code != 0 {
		t.Fatalf("initial import: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	initialSerial, _ := readStateFile(t, dir)
	stdout, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, address, refreshID)
	if code != 0 || stdout != "Refreshed "+address+".\n" {
		t.Fatalf("sensitive refresh: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, secret := range []string{"refresh-sensitive-secret", "refresh-member-one", "refresh-member-two"} {
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("refresh output exposed %q", secret)
		}
	}
	for _, path := range []string{filepath.Join(dir, "state.json"), filepath.Join(dir, "state.json.backup")} {
		data, err := os.ReadFile(path) //nolint:gosec // test-controlled state artifact
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"refresh-sensitive-secret", "refresh-member-one", "refresh-member-two"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("%s exposed %q", filepath.Base(path), secret)
			}
		}
	}
	stateData, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Serial    uint64 `json:"serial"`
		Resources map[string]struct {
			Attributes           map[string]any `json:"attributes"`
			SensitiveRecovery    any            `json:"sensitive_set_recovery"`
			SensitiveRecoveryVer int            `json:"sensitive_recovery_version"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(stateData, &document); err != nil {
		t.Fatal(err)
	}
	if document.Serial != initialSerial+1 {
		t.Fatalf("refresh serial = %d, want exactly one increment from %d", document.Serial, initialSerial)
	}
	resource := document.Resources[address]
	if resource.Attributes["secret"] != nil || resource.Attributes["members"] != nil {
		t.Fatalf("refreshed sensitive attributes = %#v, want redacted nulls", resource.Attributes)
	}
	if resource.Attributes["note"] != "remote-note" {
		t.Fatalf("ordinary note = %#v, want provider refresh value", resource.Attributes["note"])
	}
	if resource.SensitiveRecovery == nil || resource.SensitiveRecoveryVer == 0 {
		t.Fatal("refreshed state omitted encrypted sensitive recovery")
	}

	stdout, stderr, code = runCLIWithArtifactKey(t, dir, "state", "show", address)
	if code != 0 || strings.Contains(stdout+stderr, "refresh-sensitive") {
		t.Fatalf("state show exposed refresh-sensitive data: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var shown struct {
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
		t.Fatalf("state show output: %v\n%s", err, stdout)
	}
	if shown.Attributes["secret"] != nil || shown.Attributes["members"] != nil {
		t.Fatalf("state show sensitive attributes = %#v, want nulls", shown.Attributes)
	}
	if shown.Attributes["note"] != "remote-note" {
		t.Fatalf("state show note = %#v, want remote value", shown.Attributes["note"])
	}

	stdout, stderr, code = runCLIWithArtifactKey(t, dir, "plan", pd)
	if code != 0 || !strings.Contains(stdout, "No changes") {
		t.Fatalf("plan after omitted-sensitive refresh: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, "refresh-sensitive") {
		t.Fatal("plan exposed a sensitive refresh value")
	}
}

func TestImportSensitiveSetsEncryptsIdentityAndPreservesProjection(t *testing.T) {
	dir := t.TempDir()
	writeSensitiveSetConfig(t, dir)
	pd := "--plugin-dir=" + pluginDir
	const address = "tchoritest_set_thing.imported"
	if stdout, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, address, "remote-set-id"); code != 0 {
		t.Fatalf("import: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled artifact
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"imported-attribute-token-one", "imported-attribute-token-two",
		"imported-attribute-detail-one", "imported-attribute-detail-two",
		"imported-block-token-one", "imported-block-token-two",
		"imported-block-detail-one", "imported-block-detail-two",
	} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("imported state exposed %q", secret)
		}
	}
	if !bytes.Contains(data, []byte(`"sensitive_set_recovery"`)) {
		t.Fatal("imported state omitted encrypted sensitive set recovery")
	}
	stdout, stderr, code := runCLIWithArtifactKey(t, dir, "state", "show", address)
	if code != 0 {
		t.Fatalf("state show: exit %d\nstderr: %s", code, stderr)
	}
	var shown struct {
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
		t.Fatalf("state show output: %v\n%s", err, stdout)
	}
	for _, field := range []string{"attribute_members", "block_members"} {
		members, ok := shown.Attributes[field].([]any)
		if !ok || len(members) != 2 {
			t.Fatalf("state show %s = %#v, want two projected members", field, shown.Attributes[field])
		}
		for _, raw := range members {
			member := raw.(map[string]any)
			if member["token"] != nil || member["details"].([]any)[0].(map[string]any)["secret"] != nil {
				t.Fatalf("state show exposed %s member: %#v", field, member)
			}
		}
	}
	stdout, stderr, code = runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, address, "remote-set-id")
	if code != 0 || stdout != "Refreshed "+address+".\n" {
		t.Fatalf("refresh sensitive import: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, secret := range []string{
		"imported-attribute-token-one", "imported-attribute-token-two",
		"imported-attribute-detail-one", "imported-attribute-detail-two",
		"imported-block-token-one", "imported-block-token-two",
		"imported-block-detail-one", "imported-block-detail-two",
	} {
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("refresh output exposed %q", secret)
		}
	}
	for _, path := range []string{filepath.Join(dir, "state.json"), filepath.Join(dir, "state.json.backup")} {
		data, err := os.ReadFile(path) //nolint:gosec // test-controlled artifact
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{
			"imported-attribute-token-one", "imported-attribute-token-two",
			"imported-attribute-detail-one", "imported-attribute-detail-two",
			"imported-block-token-one", "imported-block-token-two",
			"imported-block-detail-one", "imported-block-detail-two",
		} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("refresh exposed %q in %s", secret, path)
			}
		}
	}

	stdout, stderr, code = runCLIWithArtifactKey(t, dir, "plan", pd)
	if code != 0 || !strings.Contains(stdout, "No changes") {
		t.Fatalf("plan after sensitive set import: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

func TestImportRefreshScanFailureRollsBackState(t *testing.T) {
	dir := t.TempDir()
	config := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest_thing.one": {
      "sensitive_attributes": ["name"],
      "config": {"name": {"env": "TCHORI_ONE_NAME"}}
    },
    "tchoritest_thing.other": {
      "sensitive_attributes": ["name"],
      "config": {"name": {"env": "TCHORI_OTHER_NAME"}}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	pd := "--plugin-dir=" + pluginDir
	if _, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.one", "t-id-one"); code != 0 {
		t.Fatalf("import setup: exit %d\nstderr: %s", code, stderr)
	}

	statePath := filepath.Join(dir, "state.json")
	var document map[string]any
	stateBytes, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stateBytes, &document); err != nil {
		t.Fatal(err)
	}
	resources := document["resources"].(map[string]any)
	resources["tchoritest_thing.other"] = map[string]any{
		"type":                       "tchoritest_thing",
		"provider":                   "tchoritest",
		"provider_source":            "tchori-labs/tchoritest",
		"attributes":                 map[string]any{"echo": "other", "id": "t-id-other", "name": map[string]any{"unexpected": true}},
		"sensitive_recovery_version": sensitive.RecoveryVersion,
		"sensitive_paths":            []string{"name"},
		"sensitive_scanned":          true,
	}
	corrupt, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	corrupt = append(corrupt, '\n')
	if err := os.WriteFile(statePath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	backupPath := statePath + ".backup"
	beforeState, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	beforeBackup := []byte("pre-existing backup")
	if err := os.WriteFile(backupPath, beforeBackup, 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, "tchoritest_thing.one", "t-id-one-refreshed")
	if code != 1 || !strings.Contains(stderr, "sanitize backup attributes") {
		t.Fatalf("refresh scan failure: exit %d, want scan rejection\nstderr: %s", code, stderr)
	}
	afterState, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	afterBackup, err := os.ReadFile(backupPath) //nolint:gosec // test-controlled backup artifact
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeBackup, afterBackup) {
		t.Fatal("scan failure changed state or backup")
	}
}

func TestImportRefreshRejectsSensitiveValidationWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	pd := "--plugin-dir=" + pluginDir
	if _, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo"); code != 0 {
		t.Fatalf("import setup: exit %d\nstderr: %s", code, stderr)
	}
	statePath := filepath.Join(dir, "state.json")
	beforeState, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	config := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": "t-"}
    }
  },
  "resources": {
    "tchoritest_thing.demo": {
      "sensitive_attributes": ["not_in_schema"],
      "config": {"name": "demo"}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, "tchoritest_thing.demo", "t-id-demo")
	if code != 1 || !strings.Contains(stderr, "unknown sensitive attribute") {
		t.Fatalf("refresh sensitive validation: exit %d, want 1 with rejection\nstderr: %s", code, stderr)
	}
	afterState, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Fatal("sensitive validation failure changed state.json")
	}
	if _, err := os.Stat(statePath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("sensitive validation failure created backup: %v", err)
	}
}

func TestImportRefreshWriteFailureRollsBackState(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	pd := "--plugin-dir=" + pluginDir
	if _, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo"); code != 0 {
		t.Fatalf("import setup: exit %d\nstderr: %s", code, stderr)
	}
	statePath := filepath.Join(dir, "state.json")
	beforeState, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	beforeSerial, _ := readStateFile(t, dir)
	if err := os.Mkdir(statePath+".backup", 0o700); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, "tchoritest_thing.demo", "t-id-refreshed")
	if code != 1 || !strings.Contains(stderr, "backup path") {
		t.Fatalf("refresh write failure: exit %d, want backup write diagnostic\nstderr: %s", code, stderr)
	}
	afterState, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state artifact
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Fatal("write failure changed state.json")
	}
	afterSerial, _ := readStateFile(t, dir)
	if afterSerial != beforeSerial {
		t.Fatalf("write failure changed serial from %d to %d", beforeSerial, afterSerial)
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
		_, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.nope", "t-id-demo")
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
		_, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.demo", "no-marker-here")
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
	if _, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo"); code != 0 {
		t.Fatalf("import (setup): exit %d, want 0\nstderr: %s", code, stderr)
	}

	t.Run("address already in state", func(t *testing.T) {
		serialBefore, resBefore := readStateFile(t, dir)
		before, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.ReadFile(filepath.Join(dir, "state.json.backup")) //nolint:gosec // test-controlled state artifact
		backupExists := err == nil
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if backupExists {
			t.Fatal("default refusal test unexpectedly started with a backup")
		}

		_, stderr, code := runCLIWithArtifactKey(t, dir, "import", pd, "tchoritest_thing.demo", "t-id-demo")
		if code != 1 || !strings.Contains(stderr, "does not overwrite") || !strings.Contains(stderr, "--refresh") {
			t.Fatalf("import already-in-state: exit %d, want explicit refusal with refresh hint\nstderr: %s", code, stderr)
		}
		serialAfter, resAfter := readStateFile(t, dir)
		if serialAfter != serialBefore || len(resAfter) != len(resBefore) {
			t.Fatalf("import already-in-state mutated state: serial %d->%d, resources %d->%d",
				serialBefore, serialAfter, len(resBefore), len(resAfter))
		}
		after, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("default refusal changed state.json")
		}
	})

	t.Run("refresh provider failure", func(t *testing.T) {
		before, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runCLIWithArtifactKey(t, dir, "import", "--refresh", pd, "tchoritest_thing.demo", "no-marker-here")
		if code != 1 || !strings.Contains(stderr, "resource does not exist") {
			t.Fatalf("refresh nonexistent id: exit %d, want provider refusal\nstderr: %s", code, stderr)
		}
		after, err := os.ReadFile(filepath.Join(dir, "state.json")) //nolint:gosec // test-controlled state artifact
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("provider failure changed state.json")
		}
	})
}

func TestStateShowMasksRecordedSensitivityAndNotesOnlyUnscanned(t *testing.T) {
	dir := t.TempDir()
	const (
		sentinel    = "tchori-e2e-super-secret-value"
		mapSentinel = "tchori-e2e-private-map-name"
	)
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{` +
		`"secret.masked":{"type":"secret","provider":"test","attributes":{"client_secret":"` + sentinel + `"},"sensitive_paths":["client_secret"]},` +
		`"secret.map":{"type":"secret","provider":"test","attributes":{"groups":{"` + mapSentinel + `":{"members":[{"token":"` + sentinel + `"}]}}},"sensitive_paths":["groups"],"sensitive_scanned":true},` +
		`"thing.scanned":{"type":"thing","provider":"test","attributes":{"value":"ok"},"sensitive_scanned":true},` +
		`"thing.legacy":{"type":"thing","provider":"test","attributes":{"value":"legacy"}}}}`
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
	stdout, stderr, code := runCLI(t, dir, "state", "show", "secret.masked")
	if code != 0 || strings.Contains(stdout+stderr, sentinel) || !strings.Contains(stderr, "not checked") {
		t.Fatalf("masked show: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	stdout, stderr, code = runCLI(t, dir, "state", "show", "secret.map")
	if code != 0 || strings.Contains(stdout+stderr, mapSentinel) || strings.Contains(stdout+stderr, sentinel) ||
		!strings.Contains(stdout, `"groups": null`) {
		t.Fatalf("sensitive map show: code=%d stdout=%s stderr=%s", code, stdout, stderr)
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

func TestStateSanitizeScrubsLegacyStateAndBackup(t *testing.T) {
	const sentinel = "tchori-e2e-super-secret-value"
	dir := t.TempDir()
	cfg := `{
  "providers": {"tchoritest": {"source":"tchori-labs/tchoritest","version":"0.0.1","config":{}}},
  "resources": {"tchoritest_lossy.svc": {"config":{"name":"svc"}}}
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{"tchoritest_lossy.svc":{"type":"tchoritest_lossy","provider":"tchoritest","attributes":{"id":"lossy-svc","name":"svc","flag":null,"tags":null,"secret":"` + sentinel + `","replace_me":null,"credentials":{"user":"agent","token":"` + sentinel + `"},"endpoints":[{"host":"example.test","api_key":"` + sentinel + `"}],"probes":[]}}}}`
	statePath := filepath.Join(dir, "state.json")
	backupPath := statePath + ".backup"
	if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, []byte(`{"legacy":"`+sentinel+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCLI(t, dir, "state", "sanitize", "--plugin-dir="+pluginDir)
	if code != 0 {
		t.Fatalf("state sanitize: exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Sanitized state:") {
		t.Fatalf("state sanitize stdout = %q, want deterministic summary", stdout)
	}
	for _, path := range []string{statePath, backupPath} {
		got, err := os.ReadFile(path) //nolint:gosec // test-controlled state paths under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(sentinel)) {
			t.Fatalf("%s retains sensitive value: %s", path, got)
		}
	}
}

func TestStateSanitizeRejectsProviderSourceDriftBeforeDiscovery(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "providers": {"tchoritest": {"source":"new.example/tchoritest","version":"0.0.1","config":{}}},
  "resources": {"tchoritest_lossy.svc": {"config":{"name":"svc"}}}
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	stateDoc := `{"format_version":"1.2","serial":1,"resources":{"tchoritest_lossy.svc":{"type":"tchoritest_lossy","provider":"tchoritest","provider_source":"old.example/tchoritest","attributes":{"id":"lossy-svc","name":"svc","secret":null,"credentials":null,"endpoints":[]}}}}`
	if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath) //nolint:gosec // test-controlled state path
	if err != nil {
		t.Fatal(err)
	}
	emptyPlugins := filepath.Join(dir, "empty-plugins")
	if err := os.Mkdir(emptyPlugins, 0o700); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, dir, "state", "sanitize", "--plugin-dir="+emptyPlugins)
	if code != 1 || !strings.Contains(stderr, "provider source") {
		t.Fatalf("state sanitize source drift: code=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stderr, "not installed") || strings.Contains(stderr, "launching provider") {
		t.Fatalf("state sanitize reached provider discovery before refusing source drift: %s", stderr)
	}
	after, _ := os.ReadFile(statePath) //nolint:gosec // test-controlled state path
	if !bytes.Equal(before, after) {
		t.Fatal("source drift refusal changed state")
	}
	if _, err := os.Stat(statePath + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("source drift refusal created backup: %v", err)
	}
}

func TestStateSanitizeMigratesUnboundEncrypted11Private(t *testing.T) {
	t.Setenv("TCHORI_ARTIFACT_KEY", testArtifactKey)
	const privateValue = "early-1.1-provider-private"
	sealed, err := privateblob.Seal([]byte(privateValue), "state\x00tchoritest_lossy.svc\x00tchoritest\x00tchoritest_lossy")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := `{
  "providers": {"tchoritest": {"source":"tchori-labs/tchoritest","version":"0.0.1","config":{}}},
  "resources": {"tchoritest_lossy.svc": {"config":{"name":"svc"}}}
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	stateDoc := fmt.Sprintf(`{"format_version":"1.1","serial":1,"resources":{"tchoritest_lossy.svc":{"type":"tchoritest_lossy","provider":"tchoritest","attributes":{"id":"lossy-svc","name":"svc","flag":null,"tags":null,"secret":null,"replace_me":null,"credentials":null,"endpoints":[],"probes":[]},"private":%s}}}`, sealed)
	if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCLI(t, dir, "state", "sanitize", "--plugin-dir="+pluginDir)
	if code != 0 {
		t.Fatalf("state sanitize early 1.1: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	for _, artifact := range []string{statePath, statePath + ".backup"} {
		loaded, err := state.Load(artifact)
		if err != nil {
			t.Fatalf("Load(%s): %v", artifact, err)
		}
		rs := loaded.Resources["tchoritest_lossy.svc"]
		if rs.ProviderSource != "tchori-labs/tchoritest" {
			t.Fatalf("%s provider source = %q", artifact, rs.ProviderSource)
		}
		if !bytes.Equal(rs.Private, []byte(privateValue)) {
			t.Fatalf("%s lost early 1.1 private bytes", artifact)
		}
	}
}

func TestStateSanitizeRefusesUnresolvedLegacyEntry(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	statePath := filepath.Join(dir, "state.json")
	backupPath := statePath + ".backup"
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{"orphan.legacy":{"type":"orphan","provider":"missing","attributes":{"unknown":"value"}}}}`
	if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, []byte("existing backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(statePath)   //nolint:gosec // test-controlled state path under t.TempDir()
	beforeBackup, _ := os.ReadFile(backupPath) //nolint:gosec // test-controlled backup path under t.TempDir()

	_, stderr, code := runCLI(t, dir, "state", "sanitize", "--plugin-dir="+pluginDir)
	if code != 1 || !strings.Contains(stderr, "orphan.legacy") {
		t.Fatalf("state sanitize unresolved: code=%d stderr=%s", code, stderr)
	}
	afterState, _ := os.ReadFile(statePath)   //nolint:gosec // test-controlled state path under t.TempDir()
	afterBackup, _ := os.ReadFile(backupPath) //nolint:gosec // test-controlled backup path under t.TempDir()
	if !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeBackup, afterBackup) {
		t.Fatal("unresolved state sanitize modified state or backup")
	}
}

func TestStateSanitizeReportsHintedOrphanAsUnresolved(t *testing.T) {
	const (
		knownValue   = "known-sensitive-value"
		unknownValue = "unknown-potentially-sensitive-value"
	)
	dir := t.TempDir()
	writeConfig(t, dir, "demo")
	statePath := filepath.Join(dir, "state.json")
	backupPath := statePath + ".backup"
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{"orphan.legacy":{"type":"orphan","provider":"missing","attributes":{"known":"` + knownValue + `","unknown":"` + unknownValue + `"},"sensitive_paths":["known"]}}}`
	if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, dir, "state", "sanitize", "--plugin-dir="+pluginDir)
	if code != 1 || !strings.Contains(stderr, "orphan.legacy") {
		t.Fatalf("state sanitize hinted orphan: code=%d stderr=%s", code, stderr)
	}
	for _, path := range []string{statePath, backupPath} {
		got, err := os.ReadFile(path) //nolint:gosec // test-controlled state paths under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(knownValue)) {
			t.Fatalf("%s retains known sensitive value: %s", path, got)
		}
		if !bytes.Contains(got, []byte(unknownValue)) {
			t.Fatalf("%s unexpectedly rewrote unclassified value: %s", path, got)
		}
		var doc struct {
			Resources map[string]struct {
				SensitiveScanned bool `json:"sensitive_scanned"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(got, &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if doc.Resources["orphan.legacy"].SensitiveScanned {
			t.Fatalf("%s falsely certifies hinted orphan as sensitivity-scanned", path)
		}
	}
}

func TestStateShowDiscoverSensitiveMasksWithoutWrites(t *testing.T) {
	const (
		sentinel = "tchori-e2e-super-secret-value"
		private  = "b3BhcXVlLXByb3ZpZGVyLXByaXZhdGU="
	)
	dir := t.TempDir()
	cfg := `{
  "providers": {"tchoritest": {"source":"tchori-labs/tchoritest","version":"0.0.1","config":{}}},
  "resources": {"tchoritest_lossy.svc": {"config":{"name":"svc"}}}
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	backupPath := statePath + ".backup"
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{"tchoritest_lossy.svc":{"type":"tchoritest_lossy","provider":"tchoritest","private":"` + private + `","attributes":{"id":"lossy-svc","name":"svc","secret":"` + sentinel + `","credentials":{"user":"agent","token":"` + sentinel + `"},"endpoints":[{"host":"example.test","api_key":"` + sentinel + `"}]}}}}`
	if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, []byte("backup must remain byte-identical"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeState, _ := os.ReadFile(statePath)   //nolint:gosec // test-controlled state path under t.TempDir()
	beforeBackup, _ := os.ReadFile(backupPath) //nolint:gosec // test-controlled backup path under t.TempDir()

	stdout, stderr, code := runCLI(t, dir, "state", "show", "--discover-sensitive", "--plugin-dir="+pluginDir, "tchoritest_lossy.svc")
	if code != 0 || strings.Contains(stdout+stderr, sentinel) || strings.Contains(stdout+stderr, private) {
		t.Fatalf("discover-sensitive show: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	afterState, _ := os.ReadFile(statePath)   //nolint:gosec // test-controlled state path under t.TempDir()
	afterBackup, _ := os.ReadFile(backupPath) //nolint:gosec // test-controlled backup path under t.TempDir()
	if !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeBackup, afterBackup) {
		t.Fatal("discover-sensitive state show modified state or backup")
	}
}

func TestStateShowDiscoverSensitiveReportsResolutionProvenance(t *testing.T) {
	t.Run("live non-sensitive schema is scanned without warning", func(t *testing.T) {
		dir := t.TempDir()
		writeConfig(t, dir, "demo")
		statePath := filepath.Join(dir, "state.json")
		stateDoc := `{"format_version":"1.0","serial":1,"resources":{"tchoritest_thing.demo":{"type":"tchoritest_thing","provider":"tchoritest","attributes":{"name":"demo"}}}}`
		if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(statePath) //nolint:gosec // test-controlled state path under t.TempDir()

		stdout, stderr, code := runCLI(t, dir, "state", "show", "--discover-sensitive", "--plugin-dir="+pluginDir, "tchoritest_thing.demo")
		if code != 0 || strings.Contains(stderr, "not checked") {
			t.Fatalf("discovered non-sensitive state: code=%d stdout=%s stderr=%s", code, stdout, stderr)
		}
		var shown struct {
			SensitiveScanned bool `json:"sensitive_scanned"`
		}
		if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
			t.Fatalf("decode state show: %v", err)
		}
		if !shown.SensitiveScanned {
			t.Fatalf("discovered non-sensitive state was not marked scanned in output: %s", stdout)
		}
		after, _ := os.ReadFile(statePath) //nolint:gosec // test-controlled state path under t.TempDir()
		if !bytes.Equal(before, after) {
			t.Fatal("discover-sensitive state show modified state.json")
		}
	})

	t.Run("hinted orphan stays unscanned and warns", func(t *testing.T) {
		const (
			knownValue   = "known-sensitive-value"
			unknownValue = "unknown-potentially-sensitive-value"
		)
		dir := t.TempDir()
		writeConfig(t, dir, "demo")
		statePath := filepath.Join(dir, "state.json")
		stateDoc := `{"format_version":"1.0","serial":1,"resources":{"orphan.legacy":{"type":"orphan","provider":"missing","attributes":{"known":"` + knownValue + `","unknown":"` + unknownValue + `"},"sensitive_paths":["known"]}}}`
		if err := os.WriteFile(statePath, []byte(stateDoc), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(statePath) //nolint:gosec // test-controlled state path under t.TempDir()

		stdout, stderr, code := runCLI(t, dir, "state", "show", "--discover-sensitive", "--plugin-dir="+pluginDir, "orphan.legacy")
		if code != 0 || strings.Contains(stdout+stderr, knownValue) || !strings.Contains(stdout, unknownValue) {
			t.Fatalf("hinted orphan show: code=%d stdout=%s stderr=%s", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "not checked") {
			t.Fatalf("hinted orphan show suppressed unresolved warning: %s", stderr)
		}
		var shown struct {
			SensitiveScanned bool `json:"sensitive_scanned"`
		}
		if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
			t.Fatalf("decode state show: %v", err)
		}
		if shown.SensitiveScanned {
			t.Fatalf("hinted orphan was falsely marked scanned in output: %s", stdout)
		}
		after, _ := os.ReadFile(statePath) //nolint:gosec // test-controlled state path under t.TempDir()
		if !bytes.Equal(before, after) {
			t.Fatal("discover-sensitive state show modified hinted orphan state")
		}
	})
}

func TestImportRequiresArtifactKeyBeforeProviderMutation(t *testing.T) {
	for name, key := range map[string]string{
		"missing": "",
		"invalid": "not-a-valid-aes-256-key",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, "demo")
			providerPIDPath := filepath.Join(dir, "provider.pid")
			_, stderr, code := runCLIEnv(t, dir, map[string]string{
				"TCHORI_ARTIFACT_KEY": key,
				"TCHORITEST_PID_FILE": providerPIDPath,
			}, "import", "--plugin-dir="+pluginDir, "tchoritest_thing.demo", "t-id-demo")
			if code != 1 || !strings.Contains(stderr, "TCHORI_ARTIFACT_KEY") {
				t.Fatalf("import with %s artifact key: code=%d stderr=%s", name, code, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
				t.Fatalf("import with %s artifact key created state: %v", name, err)
			}
			if _, err := os.Stat(providerPIDPath); !os.IsNotExist(err) {
				t.Fatalf("import with %s artifact key launched provider: %v", name, err)
			}
		})
	}
}

func TestStateSensitivityRejectsMismatchedSchema(t *testing.T) {
	for _, tc := range []struct{ name, provider, attributes string }{
		{"changed provider", "former", `{"name":"demo"}`},
		{"unknown attribute", "tchoritest", `{"name":"demo","removed_secret":"legacy-secret"}`},
	} {
		for _, command := range []string{"sanitize", "show"} {
			t.Run(tc.name+"/"+command, func(t *testing.T) {
				dir := t.TempDir()
				writeConfig(t, dir, "demo")
				document := `{"format_version":"1.0","serial":1,"resources":{"tchoritest_thing.demo":{"type":"tchoritest_thing","provider":"` + tc.provider + `","attributes":` + tc.attributes + `}}}`
				path := filepath.Join(dir, "state.json")
				if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
					t.Fatal(err)
				}
				args := []string{"state", command, "--plugin-dir=" + pluginDir}
				if command == "show" {
					args = append(args, "--discover-sensitive", "tchoritest_thing.demo")
				}
				stdout, stderr, code := runCLI(t, dir, args...)
				if code != 1 || strings.Contains(stdout+stderr, "legacy-secret") {
					t.Fatalf("mismatched schema must fail without disclosure: code=%d", code)
				}
				after, err := os.ReadFile(path) //nolint:gosec // test-controlled artifact
				if err != nil || !bytes.Equal(after, []byte(document)) {
					t.Fatal("schema refusal changed state")
				}
				if _, err := os.Stat(path + ".backup"); !os.IsNotExist(err) {
					t.Fatalf("schema refusal created backup: %v", err)
				}
			})
		}
	}
}

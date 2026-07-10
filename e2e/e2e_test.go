//go:build e2e

// Package e2e exercises the built tchori binary end to end, in three parts:
//
//   - lifecycle: validate → plan → apply → plan (no-op) → destroy plan →
//     apply, against the in-repo fake tfplugin6 provider via --plugin-dir,
//     with a real cross-resource reference chain
//   - registry_install: a real download from registry.opentofu.org,
//     SHA256-verified, cache layout asserted via `providers list -json`
//     (install never launches the binary, so its protocol is irrelevant)
//   - protocol5_graceful_failure: launching the installed protocol-5-only
//     null provider fails with exit 1 and a structured diagnostic naming
//     the protocol mismatch
//
// Run with: go test -tags e2e ./e2e -v   (network required)
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// nullVersion pins opentofu/null per research-registry.md §6 (verified
// 2026-07-10). Every published version of opentofu/null serves plugin
// protocol 5 only (§2) — which is exactly what the graceful-failure subtest
// needs. Do not swap in a different provider without re-verifying.
const nullVersion = "3.3.0"

// lifecycleConfig is the fake-provider workspace: two tchoritest_thing
// resources where b's tag references a's computed id — a real dependency
// chain through "${tchoritest_thing.a.id}". The provider is resolved from
// the type prefix (tchoritest_thing -> tchoritest).
const lifecycleConfig = `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": { "prefix": "e2e-" }
    }
  },
  "resources": {
    "tchoritest_thing.a": {
      "config": { "name": "alpha" }
    },
    "tchoritest_thing.b": {
      "config": {
        "name": "beta",
        "tags": { "parent": "${tchoritest_thing.a.id}" }
      }
    }
  }
}
`

// protocol5Config declares the registry-installed null provider (protocol 5
// only). Any provider-launching command against it must exit 1 with a
// structured diagnostic. fmt.Sprintf arg: nullVersion.
const protocol5Config = `{
  "providers": {
    "null": { "source": "opentofu/null", "version": "%s", "config": {} }
  },
  "resources": {
    "null_resource.demo": {
      "config": { "triggers": { "k": "v" } }
    }
  }
}
`

// stateDoc mirrors the on-disk state.json format (internal/state.State) so
// the test reads the file as a black box, without importing engine packages.
type stateDoc struct {
	FormatVersion string `json:"format_version"`
	Serial        uint64 `json:"serial"`
	Resources     map[string]struct {
		Type       string          `json:"type"`
		Provider   string          `json:"provider"`
		Attributes json.RawMessage `json:"attributes"`
	} `json:"resources"`
}

func TestEndToEnd(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	// Build the real binary and the Task 5 fake provider once, into a temp
	// dir (same pattern as Task 13's CLI test).
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "tchori")
	pluginDir := filepath.Join(binDir, "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	builds := []struct{ target, pkg string }{
		{bin, "./cmd/tchori"},
		{filepath.Join(pluginDir, "terraform-provider-tchoritest"),
			"./internal/provider/testprovider"},
	}
	for _, b := range builds {
		cmd := exec.Command("go", "build", "-o", b.target, b.pkg)
		cmd.Dir = repoRoot
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			t.Fatalf("go build %s: %v\n%s", b.pkg, buildErr, out)
		}
	}

	// Isolated HOME: the provider cache defaults to $HOME/.tchori/providers,
	// so overriding HOME in the child process env keeps the registry
	// download out of the developer's real home and makes the test hermetic
	// and repeatable.
	home := t.TempDir()

	// run executes the built binary in dir, asserts its exit code (dumping
	// stdout/stderr on any mismatch), and returns stdout and stderr.
	run := func(t *testing.T, dir string, wantExit int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "HOME=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "HOME="+home)
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		runErr := cmd.Run()
		exit := 0
		if runErr != nil {
			var ee *exec.ExitError
			if !errors.As(runErr, &ee) {
				t.Fatalf("tchori %v: %v", args, runErr)
			}
			exit = ee.ExitCode()
		}
		if exit != wantExit {
			t.Fatalf("tchori %v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
				args, exit, wantExit, outBuf.String(), errBuf.String())
		}
		return outBuf.String(), errBuf.String()
	}

	t.Run("lifecycle", func(t *testing.T) {
		work := t.TempDir()
		if err := os.WriteFile(filepath.Join(work, "main.tchori.json"), []byte(lifecycleConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		pd := "--plugin-dir=" + pluginDir

		// 1. validate: launches the provider and calls ValidateResource per
		//    resource ⇒ exit 0 on a clean config.
		run(t, work, 0, "validate", pd)

		// 2. plan -out: two pending creates ⇒ exit 2 (plan has changes).
		run(t, work, 2, "plan", pd, "-out", "plan.json")
		var pl struct {
			Summary struct {
				Create  int `json:"create"`
				Update  int `json:"update"`
				Delete  int `json:"delete"`
				Replace int `json:"replace"`
			} `json:"summary"`
		}
		readJSON(t, filepath.Join(work, "plan.json"), &pl)
		if pl.Summary.Create != 2 || pl.Summary.Update != 0 || pl.Summary.Delete != 0 || pl.Summary.Replace != 0 {
			t.Fatalf("plan summary = %+v, want {Create:2 Update:0 Delete:0 Replace:0}", pl.Summary)
		}

		// 3. apply the saved plan.
		run(t, work, 0, "apply", pd, "plan.json")

		// The reference chain must have resolved for real: b's tag equals
		// a's provider-computed id ("e2e-" prefix from the provider config
		// proves Configure ran with the composed config).
		var st stateDoc
		readJSON(t, filepath.Join(work, "state.json"), &st)
		if st.FormatVersion != "1.0" {
			t.Fatalf("state format_version = %q, want %q", st.FormatVersion, "1.0")
		}
		if len(st.Resources) != 2 {
			t.Fatalf("state has %d resources after apply, want 2: %v", len(st.Resources), addresses(st))
		}
		aRes, ok := st.Resources["tchoritest_thing.a"]
		if !ok {
			t.Fatalf("tchoritest_thing.a not in state after apply: %v", addresses(st))
		}
		bRes, ok := st.Resources["tchoritest_thing.b"]
		if !ok {
			t.Fatalf("tchoritest_thing.b not in state after apply: %v", addresses(st))
		}
		var a struct {
			ID string `json:"id"`
		}
		mustUnmarshal(t, aRes.Attributes, &a)
		var b struct {
			Tags map[string]string `json:"tags"`
		}
		mustUnmarshal(t, bRes.Attributes, &b)
		if a.ID != "e2e-id-alpha" {
			t.Fatalf("tchoritest_thing.a id = %q, want %q", a.ID, "e2e-id-alpha")
		}
		if b.Tags["parent"] != a.ID {
			t.Fatalf("tchoritest_thing.b tags.parent = %q, want a's id %q", b.Tags["parent"], a.ID)
		}

		// 4. plan again: state matches config ⇒ no changes ⇒ exit 0.
		run(t, work, 0, "plan", pd)

		// 5. destroy -out: a destroy plan with two deletes ⇒ exit 2.
		run(t, work, 2, "destroy", pd, "-out", "destroy.json")

		// 6. apply the destroy plan.
		run(t, work, 0, "apply", pd, "destroy.json")

		// 7. state.json must still exist and hold zero resources.
		st = stateDoc{}
		readJSON(t, filepath.Join(work, "state.json"), &st)
		if len(st.Resources) != 0 {
			t.Fatalf("state has %d resources after destroy, want 0: %v", len(st.Resources), addresses(st))
		}
	})

	t.Run("registry_install", func(t *testing.T) {
		work := t.TempDir()

		// Real download from registry.opentofu.org: version lookup, zip
		// fetch, SHA256 verification against the SHA256SUMS document,
		// extraction into the cache. install never launches the binary.
		run(t, work, 0, "providers", "install", "opentofu/null", nullVersion)

		// Cache layout asserted via providers list -json.
		stdout, _ := run(t, work, 0, "-json", "providers", "list")
		var installed []struct {
			Source  string `json:"source"`
			Version string `json:"version"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal([]byte(stdout), &installed); err != nil {
			t.Fatalf("providers list -json output is not JSON: %v\n%s", err, stdout)
		}
		wantDir := filepath.Join(home, ".tchori", "providers", "opentofu", "null",
			nullVersion, runtime.GOOS+"_"+runtime.GOARCH)
		found := false
		for _, in := range installed {
			if in.Source != "opentofu/null" || in.Version != nullVersion {
				continue
			}
			found = true
			if filepath.Dir(in.Path) != wantDir {
				t.Errorf("cached path dir = %s, want %s", filepath.Dir(in.Path), wantDir)
			}
			if !strings.HasPrefix(filepath.Base(in.Path), "terraform-provider-null") {
				t.Errorf("cached binary = %s, want terraform-provider-null* (unversioned inside the zip)",
					filepath.Base(in.Path))
			}
			info, err := os.Stat(in.Path)
			if err != nil {
				t.Fatalf("stat %s: %v", in.Path, err)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Errorf("cached binary mode = %v, want executable bits set", info.Mode())
			}
		}
		if !found {
			t.Fatalf("providers list has no opentofu/null@%s entry:\n%s", nullVersion, stdout)
		}
	})

	t.Run("protocol5_graceful_failure", func(t *testing.T) {
		work := t.TempDir()
		cfg := fmt.Sprintf(protocol5Config, nullVersion)
		if err := os.WriteFile(filepath.Join(work, "main.tchori.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}

		// plan discovers the null binary in the cache (installed by the
		// registry_install subtest above), launches it, and must fail the
		// protocol-6-only negotiation with exit 1 and a structured
		// diagnostic naming the mismatch — not a hang, not a raw go-plugin
		// stack trace. stderr is a pipe here, so diagnostics are JSON lines.
		_, stderr := run(t, work, 1, "plan")
		if !strings.Contains(stderr, `"severity":"error"`) {
			t.Errorf("stderr carries no structured JSON error diagnostic: %q", stderr)
		}
		if !strings.Contains(stderr, "provider protocol unsupported") {
			t.Errorf("stderr does not name the protocol mismatch: %q", stderr)
		}
		if !strings.Contains(stderr, "tfplugin6") {
			t.Errorf("stderr does not say tchori speaks tfplugin6: %q", stderr)
		}
	})
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decoding attributes %s: %v", raw, err)
	}
}

func addresses(st stateDoc) []string {
	out := make([]string, 0, len(st.Resources))
	for addr := range st.Resources {
		out = append(out, addr)
	}
	return out
}

//go:build smoke

// Package e2e contains the non-PR live-registry smoke test. Unlike the e2e
// build-tag suite, this test intentionally reaches registry.opentofu.org.
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

const smokeNullVersion = "3.3.0"

const smokeProtocol5Config = `{
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

func TestLiveRegistrySmoke(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "tchori")
	build := exec.Command("go", "build", "-o", bin, "./cmd/tchori")
	build.Dir = repoRoot
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build ./cmd/tchori: %v\n%s", buildErr, out)
	}

	home := t.TempDir()
	run := func(t *testing.T, dir string, wantExit int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "TCHORI_REGISTRY_URL=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "HOME="+home)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		exit := 0
		if runErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				t.Fatalf("tchori %v: %v", args, runErr)
			}
			exit = exitErr.ExitCode()
		}
		if exit != wantExit {
			t.Fatalf("tchori %v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s",
				args, exit, wantExit, stdout.String(), stderr.String())
		}
		return stdout.String(), stderr.String()
	}

	work := t.TempDir()
	install := exec.Command(bin, "providers", "install", "opentofu/null", smokeNullVersion)
	install.Dir = work
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "TCHORI_REGISTRY_URL=") {
			install.Env = append(install.Env, entry)
		}
	}
	install.Env = append(install.Env, "HOME="+home)
	var installOut, installErr bytes.Buffer
	install.Stdout = &installOut
	install.Stderr = &installErr
	if err := install.Run(); err != nil {
		if isSmokeNetworkError(installErr.String()) {
			t.Skipf("live registry unavailable: %s", installErr.String())
		}
		t.Fatalf("providers install opentofu/null %s: %v\nstdout:\n%s\nstderr:\n%s",
			smokeNullVersion, err, installOut.String(), installErr.String())
	}

	stdout, _ := run(t, work, 0, "-json", "providers", "list")
	var installed []struct {
		Source  string `json:"source"`
		Version string `json:"version"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &installed); err != nil {
		t.Fatalf("providers list output is not JSON: %v\n%s", err, stdout)
	}
	wantDir := filepath.Join(home, ".tchori", "providers", "opentofu", "null",
		smokeNullVersion, runtime.GOOS+"_"+runtime.GOARCH)
	found := false
	for _, provider := range installed {
		if provider.Source == "opentofu/null" && provider.Version == smokeNullVersion {
			found = true
			if filepath.Dir(provider.Path) != wantDir {
				t.Errorf("cached path dir = %s, want %s", filepath.Dir(provider.Path), wantDir)
			}
		}
	}
	if !found {
		t.Fatalf("providers list has no opentofu/null@%s entry:\n%s", smokeNullVersion, stdout)
	}

	cfg := fmt.Sprintf(smokeProtocol5Config, smokeNullVersion)
	if err := os.WriteFile(filepath.Join(work, "main.tchori.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
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
}

func isSmokeNetworkError(message string) bool {
	message = strings.ToLower(message)
	patterns := []string{
		"no such host",
		"connection refused",
		"i/o timeout",
		"tls handshake timeout",
		"temporary failure in name resolution",
		"dial tcp",
		"network is unreachable",
	}
	for _, pattern := range patterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

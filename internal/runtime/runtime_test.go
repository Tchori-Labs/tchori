package runtime_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	engineruntime "github.com/tchori-labs/tchori/internal/runtime"
)

var runtimePluginDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tchori-runtime-test")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mkdtemp:", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "terraform-provider-tchoritest")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/provider/testprovider") //nolint:gosec // fixed command and temp output
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "building test provider: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	runtimePluginDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func writeRuntimeConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := `{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": {"prefix": {"env": "TCHORI_TEST_UNRESOLVED"}}
    }
  },
  "resources": {}
}`
	if err := os.WriteFile(filepath.Join(dir, "main.tchori.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestBuildRejectsUnresolvedReferenceBeforeConfigure(t *testing.T) {
	t.Setenv("TCHORI_TEST_UNRESOLVED", "secret-${tchoritest_thing.ghost.id}-suffix")
	workdir := t.TempDir()
	writeRuntimeConfig(t, workdir)

	rt, ds := engineruntime.Build(context.Background(), engineruntime.Options{
		Workdir:   workdir,
		PluginDir: runtimePluginDir,
		CacheDir:  t.TempDir(),
	})
	if rt != nil {
		rt.Close()
		t.Fatal("Build returned a Runtime for unresolved provider config")
	}
	if len(ds) != 1 || ds[0].Summary != "unresolved reference" || ds[0].Address != "" || !strings.Contains(ds[0].Detail, "prefix") || !strings.Contains(ds[0].Detail, "${tchoritest_thing.ghost.id}") {
		t.Fatalf("diagnostics = %+v, want only the composition unresolved-reference diagnostic", ds)
	}
	if strings.Contains(ds[0].Detail, "secret-") || strings.Contains(ds[0].Detail, "-suffix") {
		t.Fatalf("diagnostic leaked full environment value: %q", ds[0].Detail)
	}
}

func TestBuildConfiguresCleanEnvironmentValue(t *testing.T) {
	t.Setenv("TCHORI_TEST_UNRESOLVED", "clean-prefix-")
	workdir := t.TempDir()
	writeRuntimeConfig(t, workdir)

	rt, ds := engineruntime.Build(context.Background(), engineruntime.Options{
		Workdir:   workdir,
		PluginDir: runtimePluginDir,
		CacheDir:  t.TempDir(),
	})
	if ds.HasErrors() {
		t.Fatalf("Build diagnostics = %+v", ds)
	}
	if rt == nil {
		t.Fatal("Build returned nil Runtime for clean provider config")
	}
	rt.Close()
}

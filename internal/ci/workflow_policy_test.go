package ci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJobsMissingTimeoutLiveWorkflow(t *testing.T) {
	root := repositoryRoot(t)
	workflowPath := filepath.Clean(filepath.Join(root, ".github", "workflows", "ci.yml"))
	workflowYAML, err := os.ReadFile(workflowPath) //nolint:gosec // G304: test reads the fixed in-repo CI workflow.
	if err != nil {
		t.Fatalf("read live CI workflow: %v", err)
	}

	missing, err := JobsMissingTimeout(workflowYAML)
	if err != nil {
		t.Fatalf("check live CI workflow: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("CI jobs missing a positive timeout-minutes: %v", missing)
	}
}

func TestJobsMissingTimeoutDetectsOmission(t *testing.T) {
	workflowYAML := []byte(`jobs:
  bounded:
    runs-on: ubuntu-latest
    timeout-minutes: 10
  missing:
    runs-on: ubuntu-latest
`)

	missing, err := JobsMissingTimeout(workflowYAML)
	if err != nil {
		t.Fatalf("check workflow fixture: %v", err)
	}
	if len(missing) != 1 || missing[0] != "missing" {
		t.Fatalf("JobsMissingTimeout() = %v, want [missing]", missing)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect repository root candidate %q: %v", dir, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root containing go.mod")
		}
		dir = parent
	}
}

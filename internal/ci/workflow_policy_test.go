package ci

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestJobsMissingTimeoutLiveWorkflow(t *testing.T) {
	missing, err := JobsMissingTimeout(readLiveWorkflow(t))
	if err != nil {
		t.Fatalf("check live CI workflow: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("CI jobs missing a positive timeout-minutes: %v", missing)
	}
}

func TestCheckJobEnforcesRaceDetectorLiveWorkflow(t *testing.T) {
	workflowYAML := readLiveWorkflow(t)
	if err := CheckJobEnforcesRaceDetector(workflowYAML); err != nil {
		t.Fatalf("check live CI race enforcement: %v", err)
	}
}

func TestCheckJobEnforcesRaceDetectorFixtures(t *testing.T) {
	tests := []struct {
		name         string
		workflowYAML string
		wantError    string
	}{
		{
			name: "folded into required check",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=2m ./...
`,
		},
		{
			name: "independent race job is unenforced",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  race:
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=2m ./...
`,
			wantError: "standalone race job",
		},
		{
			name: "race failure cannot be ignored",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=2m ./...
        continue-on-error: true
`,
			wantError: "race detector step must not allow failures",
		},
		{
			name: "check failure cannot be ignored",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: go test -race -timeout=2m ./...
`,
			wantError: "check job must not allow failures",
		},
		{
			name: "conditional race step is not proof",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=2m ./...
        if: github.event_name == 'push'
`,
			wantError: "race detector step must not declare an if condition",
		},
		{
			name: "zero timeout is not bounded",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=0 ./...
`,
			wantError: "explicit -timeout",
		},
		{
			name: "plain needs permits skipped check",
			workflowYAML: `jobs:
  check:
    needs: race
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=2m ./...
  race:
    runs-on: ubuntu-latest
    steps:
      - run: go test -race -timeout=2m ./...
`,
			wantError: "must not declare needs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckJobEnforcesRaceDetector([]byte(tt.workflowYAML))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("CheckJobEnforcesRaceDetector() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("CheckJobEnforcesRaceDetector() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestActionRefsPinnedLiveWorkflows(t *testing.T) {
	root := repositoryRoot(t)
	workflowsDir := filepath.Join(root, ".github", "workflows")

	matches, err := filepath.Glob(filepath.Join(workflowsDir, "*.yml"))
	if err != nil {
		t.Fatalf("glob workflow files: %v", err)
	}
	yamlMatches, err := filepath.Glob(filepath.Join(workflowsDir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob workflow files: %v", err)
	}
	matches = append(matches, yamlMatches...)

	if len(matches) < 3 {
		t.Fatalf("expected at least 3 workflow files under %s, found %v", workflowsDir, matches)
	}

	sort.Strings(matches)
	for _, path := range matches {
		path := filepath.Clean(path)
		t.Run(filepath.Base(path), func(t *testing.T) {
			workflowYAML, err := os.ReadFile(path) //nolint:gosec // G304: test reads fixed in-repo workflow files under .github/workflows.
			if err != nil {
				t.Fatalf("read workflow %s: %v", path, err)
			}

			violations, err := UnpinnedActionRefs(workflowYAML)
			if err != nil {
				t.Fatalf("check unpinned action refs in %s: %v", path, err)
			}
			if len(violations) != 0 {
				t.Fatalf("%s has unpinned action refs: %v", path, violations)
			}
		})
	}
}

func TestUnpinnedActionRefsFixtures(t *testing.T) {
	tests := []struct {
		name          string
		workflowYAML  string
		wantViolation []string
	}{
		{
			name: "tag ref is flagged",
			workflowYAML: `jobs:
  secretscan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
`,
			wantViolation: []string{"secretscan/actions/checkout@v7"},
		},
		{
			name: "sha pin with version comment and run-only step pass",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7.0.0
      - run: go test ./...
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, err := UnpinnedActionRefs([]byte(tt.workflowYAML))
			if err != nil {
				t.Fatalf("UnpinnedActionRefs() error = %v", err)
			}
			if len(violations) != len(tt.wantViolation) {
				t.Fatalf("UnpinnedActionRefs() = %v, want %v", violations, tt.wantViolation)
			}
			for i, v := range tt.wantViolation {
				if violations[i] != v {
					t.Fatalf("UnpinnedActionRefs() = %v, want %v", violations, tt.wantViolation)
				}
			}
		})
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

func readLiveWorkflow(t *testing.T) []byte {
	t.Helper()

	root := repositoryRoot(t)
	workflowPath := filepath.Clean(filepath.Join(root, ".github", "workflows", "ci.yml"))
	workflowYAML, err := os.ReadFile(workflowPath) //nolint:gosec // G304: test reads the fixed in-repo CI workflow.
	if err != nil {
		t.Fatalf("read live CI workflow: %v", err)
	}
	return workflowYAML
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

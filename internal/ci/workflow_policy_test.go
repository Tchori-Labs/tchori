package ci

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestE2EProxyEnvIsStepScopedLiveWorkflow(t *testing.T) {
	if err := E2EProxyEnvIsStepScoped(readLiveWorkflow(t)); err != nil {
		t.Fatalf("check live e2e proxy scope: %v", err)
	}
}

func TestE2EProxyEnvIsStepScopedFixtures(t *testing.T) {
	tests := []struct {
		name         string
		workflowYAML string
		wantError    string
	}{
		{
			name: "step-scoped denial passes",
			workflowYAML: `jobs:
  e2e:
    steps:
      - uses: actions/checkout@0123456789012345678901234567890123456789
      - run: go mod download
      - run: go test -tags e2e ./e2e -v
        env:
          HTTPS_PROXY: 'http://127.0.0.1:1'
          HTTP_PROXY: 'http://127.0.0.1:1'
          NO_PROXY: '127.0.0.1,localhost'
`,
		},
		{
			name: "pre-fix job-scoped denial is rejected",
			workflowYAML: `jobs:
  e2e:
    env:
      HTTPS_PROXY: 'http://127.0.0.1:1'
      HTTP_PROXY: 'http://127.0.0.1:1'
      NO_PROXY: '127.0.0.1,localhost'
    steps:
      - uses: actions/checkout@0123456789012345678901234567890123456789
      - run: go test -tags e2e ./e2e -v
`,
			wantError: "at job scope",
		},
		{
			name: "deleted denial is rejected",
			workflowYAML: `jobs:
  e2e:
    steps:
      - run: go test -tags e2e ./e2e -v
`,
			wantError: "e2e test step must declare",
		},
		{
			name: "proxy on setup step is rejected",
			workflowYAML: `jobs:
  e2e:
    steps:
      - uses: actions/setup-go@0123456789012345678901234567890123456789
        env:
          HTTP_PROXY: 'http://127.0.0.1:1'
      - run: go test -tags e2e ./e2e -v
        env:
          HTTPS_PROXY: 'http://127.0.0.1:1'
          HTTP_PROXY: 'http://127.0.0.1:1'
          NO_PROXY: '127.0.0.1,localhost'
`,
			wantError: "only the e2e test step",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := E2EProxyEnvIsStepScoped([]byte(tt.workflowYAML))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("E2EProxyEnvIsStepScoped() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("E2EProxyEnvIsStepScoped() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

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

func TestCheckJobRunsWindowsVetLiveWorkflow(t *testing.T) {
	if err := CheckJobRunsWindowsVet(readLiveWorkflow(t)); err != nil {
		t.Fatalf("check live CI Windows vet enforcement: %v", err)
	}
}

func TestCheckJobRunsWindowsVetFixtures(t *testing.T) {
	tests := []struct {
		name         string
		workflowYAML string
		wantError    string
	}{
		{
			name: "Windows vet runs directly",
			workflowYAML: `jobs:
  check:
    steps:
      - run: GOOS=windows go vet ./...
`,
		},
		{
			name: "plain vet is insufficient",
			workflowYAML: `jobs:
  check:
    steps:
      - run: go vet ./...
`,
			wantError: "must directly run",
		},
		{
			name: "Windows vet failure cannot be ignored",
			workflowYAML: `jobs:
  check:
    steps:
      - run: GOOS=windows go vet ./...
        continue-on-error: true
`,
			wantError: "must not allow failures",
		},
		{
			name: "conditional Windows vet is not proof",
			workflowYAML: `jobs:
  check:
    steps:
      - run: GOOS=windows go vet ./...
        if: github.event_name == 'push'
`,
			wantError: "must not declare an if condition",
		},
		{
			name: "check job missing",
			workflowYAML: `jobs:
  test:
    steps:
      - run: GOOS=windows go vet ./...
`,
			wantError: "no check job",
		},
		{
			name: "commented command is not executed",
			workflowYAML: `jobs:
  check:
    steps:
      - run: |
          # GOOS=windows go vet ./...
`,
			wantError: "must directly run",
		},
		{
			name: "echoed command is not executed",
			workflowYAML: `jobs:
  check:
    steps:
      - run: echo GOOS=windows go vet ./...
`,
			wantError: "must directly run",
		},
		{
			name: "suppressed failure is not proof",
			workflowYAML: `jobs:
  check:
    steps:
      - run: GOOS=windows go vet ./... || true
`,
			wantError: "must directly run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckJobRunsWindowsVet([]byte(tt.workflowYAML))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("CheckJobRunsWindowsVet() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("CheckJobRunsWindowsVet() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestCheckJobRunsWorkflowLintLiveWorkflow(t *testing.T) {
	if err := CheckJobRunsWorkflowLint(readLiveWorkflow(t)); err != nil {
		t.Fatalf("check live CI workflow lint enforcement: %v", err)
	}
}

func TestCheckJobRunsWorkflowLintFixtures(t *testing.T) {
	const validInstall = "go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12"
	tests := []struct {
		name      string
		jobBody   string
		wantError string
	}{
		{name: "unconditional lint passes", jobBody: "    steps:\n      - run: " + validInstall + "\n      - run: bash scripts/actionlint-verify.sh\n"},
		{name: "lint step missing", jobBody: "    steps:\n      - run: " + validInstall + "\n", wantError: "must run scripts/actionlint-verify.sh"},
		{name: "lint step conditional", jobBody: "    steps:\n      - run: " + validInstall + "\n      - run: bash scripts/actionlint-verify.sh\n        if: github.event_name == 'push'\n", wantError: "lint step must not declare an if"},
		{name: "lint failure allowed", jobBody: "    steps:\n      - run: " + validInstall + "\n      - run: bash scripts/actionlint-verify.sh\n        continue-on-error: true\n", wantError: "lint step must not allow failures"},
		{name: "check job conditional", jobBody: "    if: github.event_name == 'push'\n    steps:\n      - run: " + validInstall + "\n      - run: bash scripts/actionlint-verify.sh\n", wantError: "job-level if condition"},
		{name: "install step missing", jobBody: "    steps:\n      - run: bash scripts/actionlint-verify.sh\n", wantError: "must include an actionlint install step"},
		{name: "latest pin rejected", jobBody: "    steps:\n      - run: go install github.com/rhysd/actionlint/cmd/actionlint@latest\n      - run: bash scripts/actionlint-verify.sh\n", wantError: "found @latest"},
		{name: "master pin rejected", jobBody: "    steps:\n      - run: go install github.com/rhysd/actionlint/cmd/actionlint@master\n      - run: bash scripts/actionlint-verify.sh\n", wantError: "found @master"},
		{name: "missing pin rejected", jobBody: "    steps:\n      - run: go install github.com/rhysd/actionlint/cmd/actionlint\n      - run: bash scripts/actionlint-verify.sh\n", wantError: "explicit semver version"},
		{name: "or true suppression rejected", jobBody: "    steps:\n      - run: " + validInstall + "\n      - run: bash scripts/actionlint-verify.sh || true\n", wantError: "suppress failures with || true"},
		{name: "set plus e suppression rejected", jobBody: "    steps:\n      - run: " + validInstall + "\n      - run: |\n          set +e\n          bash scripts/actionlint-verify.sh\n", wantError: "preceded by set +e"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckJobRunsWorkflowLint([]byte("jobs:\n  check:\n" + tt.jobBody))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("CheckJobRunsWorkflowLint() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("CheckJobRunsWorkflowLint() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestActionlintPinConsistencyLiveFiles(t *testing.T) {
	root := repositoryRoot(t)
	scriptPath := filepath.Clean(filepath.Join(root, "scripts", "actionlint-verify.sh"))
	script, err := os.ReadFile(scriptPath) //nolint:gosec // G304: test reads the fixed in-repo verification script.
	if err != nil {
		t.Fatalf("read actionlint verification script: %v", err)
	}
	readmePath := filepath.Clean(filepath.Join(root, "README.md"))
	readme, err := os.ReadFile(readmePath) //nolint:gosec // G304: test reads the fixed in-repo README.
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if err := ActionlintPinConsistency(readLiveWorkflow(t), script, readme); err != nil {
		t.Fatalf("check live actionlint pin consistency: %v", err)
	}
}

func TestActionlintPinConsistencyFixtures(t *testing.T) {
	workflow := func(version string) []byte {
		return []byte("jobs:\n  check:\n    steps:\n      - run: go install github.com/rhysd/actionlint/cmd/actionlint@" + version + "\n")
	}
	script := func(version string) []byte { return []byte("ACTIONLINT_VERSION=" + version + "\n") }
	readme := func(version string) []byte {
		return []byte("go install github.com/rhysd/actionlint/cmd/actionlint@" + version + "\n")
	}

	tests := []struct {
		name      string
		workflow  []byte
		script    []byte
		readme    []byte
		wantError string
	}{
		{name: "matching pins pass", workflow: workflow("v1.7.12"), script: script("v1.7.12"), readme: readme("v1.7.12")},
		{name: "workflow differs from script", workflow: workflow("v1.7.11"), script: script("v1.7.12"), readme: readme("v1.7.12"), wantError: "workflow pin v1.7.11 differs from script pin v1.7.12"},
		{name: "script differs from README", workflow: workflow("v1.7.12"), script: script("v1.7.11"), readme: readme("v1.7.12"), wantError: "script pin v1.7.11 differs from README pin v1.7.12"},
		{name: "README differs from workflow", workflow: workflow("v1.7.12"), script: script("v1.7.12"), readme: readme("v1.7.11"), wantError: "README pin v1.7.11 differs from workflow pin v1.7.12"},
		{name: "workflow pin absent", workflow: []byte("jobs:\n  check: {}\n"), script: script("v1.7.12"), readme: readme("v1.7.12"), wantError: "absent from workflow"},
		{name: "script pin absent", workflow: workflow("v1.7.12"), script: []byte("#!/bin/sh\n"), readme: readme("v1.7.12"), wantError: "absent from verification script"},
		{name: "README pin absent", workflow: workflow("v1.7.12"), script: script("v1.7.12"), readme: []byte("# Development\n"), wantError: "absent from README"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ActionlintPinConsistency(tt.workflow, tt.script, tt.readme)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ActionlintPinConsistency() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ActionlintPinConsistency() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestLiveWorkflowsDirectoryIsNotEmpty(t *testing.T) {
	workflowsDir := filepath.Join(repositoryRoot(t), ".github", "workflows")
	matches, err := filepath.Glob(filepath.Join(workflowsDir, "*.y*ml"))
	if err != nil {
		t.Fatalf("glob workflow files: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected workflow files under %s", workflowsDir)
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

func TestPRSourceGateGuardsMainLiveWorkflow(t *testing.T) {
	if err := PRSourceGateGuardsMain(readLiveWorkflow(t)); err != nil {
		t.Fatalf("check live pull-request source gate: %v", err)
	}
}

func TestPRSourceGateGuardsMainFixtures(t *testing.T) {
	const gateRun = `if [ "$EVENT_NAME" != "pull_request" ] || [ "$BASE_REF" != "main" ]; then
  exit 0
fi
if [ "$HEAD_REF" != "develop" ]; then
  echo "pull requests into main must originate from develop" >&2
  exit 1
fi
`

	gateJob := func(job string) string {
		return "jobs:\n  pr-source:\n" + job + `    steps:
      - name: only develop may target main
        env:
          EVENT_NAME: ${{ github.event_name }}
          BASE_REF: ${{ github.base_ref }}
          HEAD_REF: ${{ github.head_ref }}
        run: |
` + indentLines(gateRun, "          ")
	}

	tests := []struct {
		name         string
		workflowYAML string
		wantError    string
	}{
		{
			name:         "unconditional env-driven gate passes",
			workflowYAML: gateJob("    runs-on: ubuntu-latest\n    timeout-minutes: 5\n"),
		},
		{
			name: "deleted gate job is rejected",
			workflowYAML: `jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: go test -count=1 ./...
`,
			wantError: "declares no pr-source job",
		},
		{
			name:         "job-level if is rejected",
			workflowYAML: gateJob("    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    if: github.base_ref == 'main'\n"),
			wantError:    "job-level if condition",
		},
		{
			name:         "needs is rejected",
			workflowYAML: gateJob("    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    needs: check\n"),
			wantError:    "must not declare needs",
		},
		{
			name:         "continue-on-error is rejected",
			workflowYAML: gateJob("    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    continue-on-error: true\n"),
			wantError:    "continue-on-error",
		},
		{
			name: "step-level if is rejected",
			workflowYAML: `jobs:
  pr-source:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: only develop may target main
        if: github.head_ref != 'develop'
        env:
          EVENT_NAME: ${{ github.event_name }}
          BASE_REF: ${{ github.base_ref }}
          HEAD_REF: ${{ github.head_ref }}
        run: |
          test "$HEAD_REF" = develop || exit 1
`,
			wantError: "must not declare an if condition",
		},
		{
			name: "gate that never fails is rejected",
			workflowYAML: `jobs:
  pr-source:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: only develop may target main
        env:
          EVENT_NAME: ${{ github.event_name }}
          BASE_REF: ${{ github.base_ref }}
          HEAD_REF: ${{ github.head_ref }}
        run: |
          if [ "$HEAD_REF" != "develop" ]; then
            echo "warning: head is $HEAD_REF"
          fi
`,
			wantError: "non-zero exit",
		},
		{
			name: "gate allowing a head ref other than develop is rejected",
			workflowYAML: `jobs:
  pr-source:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: only develop may target main
        env:
          EVENT_NAME: ${{ github.event_name }}
          BASE_REF: ${{ github.base_ref }}
          HEAD_REF: ${{ github.head_ref }}
        run: |
          if [ "$HEAD_REF" != "release" ]; then
            exit 1
          fi
`,
			wantError: "must compare the head ref to develop",
		},
		{
			name: "gate that ignores the guarded base is rejected",
			workflowYAML: `jobs:
  pr-source:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: only develop may target main
        env:
          EVENT_NAME: ${{ github.event_name }}
          BASE_REF: ${{ github.base_ref }}
          HEAD_REF: ${{ github.head_ref }}
        run: |
          if [ "$HEAD_REF" != "develop" ]; then
            exit 1
          fi
`,
			wantError: "must scope the gate to base ref main",
		},
		{
			name: "missing env mapping is rejected",
			workflowYAML: `jobs:
  pr-source:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: only develop may target main
        env:
          HEAD_REF: ${{ github.head_ref }}
        run: |
          if [ "$EVENT_NAME" != "pull_request" ] || [ "$BASE_REF" != "main" ]; then
            exit 0
          fi
          if [ "$HEAD_REF" != "develop" ]; then
            exit 1
          fi
`,
			wantError: "must map BASE_REF",
		},
		{
			name: "ref interpolated into the run script is rejected",
			workflowYAML: `jobs:
  pr-source:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - name: only develop may target main
        env:
          EVENT_NAME: ${{ github.event_name }}
          BASE_REF: ${{ github.base_ref }}
          HEAD_REF: ${{ github.head_ref }}
        run: |
          if [ "$EVENT_NAME" != "pull_request" ] || [ "$BASE_REF" != "main" ]; then
            exit 0
          fi
          if [ "${{ github.head_ref }}" != "develop" ]; then
            exit 1
          fi
`,
			wantError: "must not interpolate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := PRSourceGateGuardsMain([]byte(tt.workflowYAML))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("PRSourceGateGuardsMain() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("PRSourceGateGuardsMain() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func indentLines(script, indent string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
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

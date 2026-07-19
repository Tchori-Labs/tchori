package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryNetworkPolicy(t *testing.T) {
	root := findRepoRoot(t)
	workflowPath := filepath.Clean(filepath.Join(root, ".github", "workflows", "registry-smoke.yml"))
	workflow, err := os.ReadFile(workflowPath) //nolint:gosec // fixed in-repo workflow selected by the test
	if err != nil {
		t.Fatalf("reading smoke workflow: %v", err)
	}
	triggers, err := ForbiddenWorkflowTriggers(workflow)
	if err != nil {
		t.Fatalf("checking smoke workflow: %v", err)
	}
	if len(triggers) != 0 {
		t.Fatalf("registry smoke workflow has forbidden PR/push triggers: %v", triggers)
	}

	matches, err := filepath.Glob(filepath.Join(root, "e2e", "*.go"))
	if err != nil {
		t.Fatalf("finding e2e sources: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("found no e2e Go sources to enforce")
	}
	sources := make(map[string][]byte, len(matches))
	for _, path := range matches {
		cleanPath := filepath.Clean(path)
		content, err := os.ReadFile(cleanPath) //nolint:gosec // glob is restricted to the fixed in-repo e2e directory
		if err != nil {
			t.Fatalf("reading %s: %v", cleanPath, err)
		}
		sources[filepath.Base(cleanPath)] = content
	}
	violations, err := PublicRegistrySourceViolations(sources)
	if err != nil {
		t.Fatalf("checking e2e sources: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("PR-gated e2e source references the public registry: %v", violations)
	}
}

func TestForbiddenWorkflowTriggersDetectsPullRequest(t *testing.T) {
	workflow := []byte(`name: unsafe smoke
on:
  schedule:
    - cron: '0 0 * * *'
  pull_request:
  workflow_dispatch:
`)
	violations, err := ForbiddenWorkflowTriggers(workflow)
	if err != nil {
		t.Fatalf("ForbiddenWorkflowTriggers: %v", err)
	}
	if len(violations) != 1 || violations[0] != "pull_request" {
		t.Fatalf("violations = %v, want [pull_request]", violations)
	}
}

func TestForbiddenWorkflowTriggersDetectsPush(t *testing.T) {
	workflow := []byte("name: unsafe smoke\non: [workflow_dispatch, push]\n")
	violations, err := ForbiddenWorkflowTriggers(workflow)
	if err != nil {
		t.Fatalf("ForbiddenWorkflowTriggers: %v", err)
	}
	if len(violations) != 1 || violations[0] != "push" {
		t.Fatalf("violations = %v, want [push]", violations)
	}
}

func TestPublicRegistrySourceViolationsDetectsPRSource(t *testing.T) {
	sources := map[string][]byte{
		"e2e_test.go":   []byte("//go:build e2e\n\npackage e2e\n\nconst registry = \"https://registry.opentofu.org\"\n"),
		"smoke_test.go": []byte("//go:build smoke\n\npackage e2e\n\nconst registry = \"https://registry.opentofu.org\"\n"),
	}
	violations, err := PublicRegistrySourceViolations(sources)
	if err != nil {
		t.Fatalf("PublicRegistrySourceViolations: %v", err)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "e2e_test.go") {
		t.Fatalf("violations = %v, want one violation naming e2e_test.go", violations)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root containing go.mod")
		}
		dir = parent
	}
}

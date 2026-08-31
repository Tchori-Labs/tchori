package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	correctEnvironmentList   = `{"total_count":1,"environments":[{"name":"release"}]}`
	correctEnvironmentDetail = `{
  "protection_rules": [{
    "type": "required_reviewers",
    "prevent_self_review": true,
    "reviewers": [{"type":"User","reviewer":{"login":"VictorCano","id":6369606}}]
  }],
  "deployment_branch_policy": {"protected_branches":false,"custom_branch_policies":true}
}`
	correctBranchPolicies = `{"total_count":2,"branch_policies":[{"id":1,"name":"main","type":"branch"},{"id":2,"name":"v*","type":"tag"}]}`
)

func TestReleaseWorkflowMatchesPolicyLiveWorkflow(t *testing.T) {
	workflow := readRepositoryFile(t, ".github", "workflows", "release.yml")
	if err := ReleaseWorkflowMatchesPolicy(workflow); err != nil {
		t.Fatalf("check live release workflow policy: %v", err)
	}
}

func TestReleaseWorkflowMatchesPolicyFixtures(t *testing.T) {
	valid := `jobs:
  dry-run:
    environment:
      name: release
    permissions:
      contents: read
  publish:
    environment: release
    permissions:
      contents: write
`
	tests := []struct {
		name      string
		workflow  string
		wantError string
	}{
		{name: "string and mapping Environment forms pass", workflow: valid},
		{name: "dry-run Environment removed", workflow: strings.Replace(valid, "    environment:\n      name: release\n", "", 1), wantError: "dry-run job must declare environment: release"},
		{name: "publish Environment removed", workflow: strings.Replace(valid, "    environment: release\n", "", 1), wantError: "publish job must declare environment: release"},
		{name: "dry-run contents write", workflow: strings.Replace(valid, "contents: read", "contents: write", 1), wantError: "want read"},
		{name: "publish contents read", workflow: strings.Replace(valid, "contents: write", "contents: read", 1), wantError: "want write"},
		{name: "another job may not write", workflow: valid + "  helper:\n    permissions:\n      contents: write\n", wantError: "only publish may write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ReleaseWorkflowMatchesPolicy([]byte(tt.workflow))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ReleaseWorkflowMatchesPolicy() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ReleaseWorkflowMatchesPolicy() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestCommittedReleaseEnvironmentPayloadMatchesPolicy(t *testing.T) {
	environment := readRepositoryFile(t, ".github", "environments", "release.json")
	policies := readRepositoryFile(t, ".github", "environments", "release-deployment-branch-policies.json")
	if violations := ValidateReleaseEnvironmentPayload(environment, policies); len(violations) != 0 {
		t.Fatalf("committed release Environment payload violations: %v", violations)
	}
}

func TestValidateReleaseEnvironmentPayloadFixtures(t *testing.T) {
	validEnvironment := `{"wait_timer":0,"prevent_self_review":true,"reviewers":[{"type":"User","id":6369606}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}`
	tests := []struct {
		name        string
		environment string
		policies    string
		want        string
	}{
		{name: "correct payload", environment: validEnvironment, policies: correctBranchPolicies},
		{name: "self review allowed", environment: strings.Replace(validEnvironment, `"prevent_self_review":true`, `"prevent_self_review":false`, 1), policies: correctBranchPolicies, want: "prevent_self_review"},
		{name: "wrong reviewer", environment: strings.Replace(validEnvironment, "6369606", "1", 1), policies: correctBranchPolicies, want: "User id 6369606"},
		{name: "protected branches mode", environment: strings.Replace(validEnvironment, `"protected_branches":false,"custom_branch_policies":true`, `"protected_branches":true,"custom_branch_policies":false`, 1), policies: correctBranchPolicies, want: "protected_branches"},
		{name: "extra policy", environment: validEnvironment, policies: `{"branch_policies":[{"name":"main","type":"branch"},{"name":"v*","type":"tag"},{"name":"*","type":"branch"}]}`, want: "exactly"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := ValidateReleaseEnvironmentPayload([]byte(tt.environment), []byte(tt.policies))
			assertViolations(t, violations, tt.want)
		})
	}
}

func TestValidateLiveReleaseEnvironmentStates(t *testing.T) {
	withoutRule := `{"protection_rules":[],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}`
	wrongReviewer := strings.Replace(correctEnvironmentDetail, "VictorCano", "OtherUser", 1)
	selfReview := strings.Replace(correctEnvironmentDetail, `"prevent_self_review": true`, `"prevent_self_review": false`, 1)
	nullPolicy := strings.Replace(correctEnvironmentDetail, `{"protected_branches":false,"custom_branch_policies":true}`, "null", 1)
	protectedMode := strings.Replace(correctEnvironmentDetail, `{"protected_branches":false,"custom_branch_policies":true}`, `{"protected_branches":true,"custom_branch_policies":false}`, 1)

	tests := []struct {
		name        string
		list        string
		environment string
		policies    string
		want        string
	}{
		{name: "a exact observed absence", list: `{"total_count":0,"environments":[]}`, environment: "", policies: "", want: "release Environment is absent"},
		{name: "a release missing despite nonzero list", list: `{"total_count":1,"environments":[{"name":"staging"}]}`, environment: correctEnvironmentDetail, policies: correctBranchPolicies, want: "release Environment is absent"},
		{name: "a detail 404 captured as empty", list: correctEnvironmentList, environment: "", policies: correctBranchPolicies, want: "detail is empty"},
		{name: "b no required reviewer rule", list: correctEnvironmentList, environment: withoutRule, policies: correctBranchPolicies, want: "required_reviewers"},
		{name: "c wrong reviewer", list: correctEnvironmentList, environment: wrongReviewer, policies: correctBranchPolicies, want: "@VictorCano"},
		{name: "d self review allowed", list: correctEnvironmentList, environment: selfReview, policies: correctBranchPolicies, want: "prevent_self_review"},
		{name: "e null deployment policy", list: correctEnvironmentList, environment: nullPolicy, policies: correctBranchPolicies, want: "must not be null"},
		{name: "f protected branches mode", list: correctEnvironmentList, environment: protectedMode, policies: correctBranchPolicies, want: "protected_branches"},
		{name: "g missing main", list: correctEnvironmentList, environment: correctEnvironmentDetail, policies: `{"total_count":1,"branch_policies":[{"name":"v*","type":"tag"}]}`, want: "exactly"},
		{name: "g missing v tag", list: correctEnvironmentList, environment: correctEnvironmentDetail, policies: `{"total_count":1,"branch_policies":[{"name":"main","type":"branch"}]}`, want: "exactly"},
		{name: "g extra permissive policy", list: correctEnvironmentList, environment: correctEnvironmentDetail, policies: `{"total_count":3,"branch_policies":[{"name":"main","type":"branch"},{"name":"v*","type":"tag"},{"name":"*","type":"branch"}]}`, want: "exactly"},
		{name: "h fully correct", list: correctEnvironmentList, environment: correctEnvironmentDetail, policies: correctBranchPolicies},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := ValidateLiveReleaseEnvironment([]byte(tt.list), []byte(tt.environment), []byte(tt.policies))
			assertViolations(t, violations, tt.want)
		})
	}
}

// TestLiveReleaseEnvironmentMatchesPolicy is opt-in so ordinary CI remains
// offline. scripts/verify-release-environment.sh supplies all three files.
func TestReleaseEnvironmentVerifierRetainsRequiredCriteria(t *testing.T) {
	script := string(readRepositoryFile(t, "scripts", "verify-release-environment.sh"))
	for _, token := range []string{
		"repos/${repo}/environments/release",
		"deployment-branch-policies",
		"prevent_self_review",
		"TestLiveReleaseEnvironmentMatchesPolicy",
	} {
		if !strings.Contains(script, token) {
			t.Errorf("release Environment verifier must reference %q", token)
		}
	}
}

func TestLiveReleaseEnvironmentMatchesPolicy(t *testing.T) {
	listPath := os.Getenv("RELEASE_ENVIRONMENTS_JSON")
	environmentPath := os.Getenv("RELEASE_ENVIRONMENT_JSON")
	policiesPath := os.Getenv("RELEASE_BRANCH_POLICIES_JSON")
	if listPath == "" && environmentPath == "" && policiesPath == "" {
		t.Skip("live release Environment response paths are not set")
	}
	if listPath == "" || environmentPath == "" || policiesPath == "" {
		t.Fatal("all live release Environment response paths must be set")
	}
	list := readLiveResponse(t, listPath)
	environment := readLiveResponse(t, environmentPath)
	policies := readLiveResponse(t, policiesPath)
	violations := ValidateLiveReleaseEnvironment(list, environment, policies)
	for _, violation := range violations {
		t.Logf("FAIL: %s", violation)
	}
	if len(violations) != 0 {
		t.Fatalf("RESULT: FAIL (%d release Environment policy violation(s))", len(violations))
	}
	t.Log("PASS: required reviewer is exactly @VictorCano")
	t.Log("PASS: prevent_self_review is true")
	t.Log("PASS: custom deployment policies are exactly branch main and tag v*")
	t.Log("RESULT: PASS (live release Environment matches committed policy)")
}

func assertViolations(t *testing.T, violations []string, want string) {
	t.Helper()
	if want == "" {
		if len(violations) != 0 {
			t.Fatalf("violations = %v, want none", violations)
		}
		return
	}
	if len(violations) == 0 || !strings.Contains(strings.Join(violations, "\n"), want) {
		t.Fatalf("violations = %v, want one containing %q", violations, want)
	}
}

func readRepositoryFile(t *testing.T, elements ...string) []byte {
	t.Helper()
	path := filepath.Clean(filepath.Join(append([]string{repositoryRoot(t)}, elements...)...))
	content, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed in-repo policy file.
	if err != nil {
		t.Fatalf("read repository file %s: %v", path, err)
	}
	return content
}

func readLiveResponse(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // G304: opt-in audit reads verifier-created temporary response files.
	if err != nil {
		t.Fatalf("read live API response %s: %v", path, err)
	}
	return content
}

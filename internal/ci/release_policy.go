package ci

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	expectedReleaseEnvironment = "release"
	expectedReleaseReviewer    = "VictorCano"
	expectedReleaseReviewerID  = 6369606
)

type environmentPayload struct {
	WaitTimer              *int                    `json:"wait_timer"`
	PreventSelfReview      *bool                   `json:"prevent_self_review"`
	Reviewers              []payloadReviewer       `json:"reviewers"`
	DeploymentBranchPolicy *deploymentBranchPolicy `json:"deployment_branch_policy"`
}

type payloadReviewer struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

type deploymentBranchPolicy struct {
	ProtectedBranches    *bool `json:"protected_branches"`
	CustomBranchPolicies *bool `json:"custom_branch_policies"`
}

type branchPolicyList struct {
	TotalCount     int            `json:"total_count"`
	BranchPolicies []branchPolicy `json:"branch_policies"`
}

type branchPolicy struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type environmentsResponse struct {
	TotalCount   int `json:"total_count"`
	Environments []struct {
		Name string `json:"name"`
	} `json:"environments"`
}

type environmentResponse struct {
	ProtectionRules        []protectionRule        `json:"protection_rules"`
	DeploymentBranchPolicy *deploymentBranchPolicy `json:"deployment_branch_policy"`
}

type protectionRule struct {
	Type              string         `json:"type"`
	PreventSelfReview *bool          `json:"prevent_self_review"`
	Reviewers         []liveReviewer `json:"reviewers"`
}

type liveReviewer struct {
	Type     string `json:"type"`
	Reviewer struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"reviewer"`
}

// ReleaseWorkflowMatchesPolicy verifies that both OIDC-capable release jobs
// use the protected release Environment and retain their intended contents
// permissions. No other job may receive contents: write.
func ReleaseWorkflowMatchesPolicy(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	for _, name := range []string{"dry-run", "publish"} {
		job, ok := doc.Jobs[name]
		if !ok {
			return fmt.Errorf("release workflow declares no %s job", name)
		}
		if strings.TrimSpace(job.Environment.Name) != expectedReleaseEnvironment {
			return fmt.Errorf("%s job must declare environment: release", name)
		}
	}
	workflowLevelPermissions, err := parseWorkflowPermissions(workflowYAML)
	if err != nil {
		return err
	}
	if got := effectiveContentsPermission(doc.Jobs["dry-run"], workflowLevelPermissions); got != "read" {
		return fmt.Errorf("dry-run job contents permission = %q, want read", got)
	}
	if got := effectiveContentsPermission(doc.Jobs["publish"], workflowLevelPermissions); got != "write" {
		return fmt.Errorf("publish job contents permission = %q, want write", got)
	}
	for name, job := range doc.Jobs {
		if name != "publish" && effectiveContentsPermission(job, workflowLevelPermissions) == "write" {
			return fmt.Errorf("%s job must not receive contents: write; only publish may write", name)
		}
	}
	return nil
}

// parseWorkflowPermissions extracts the workflow-level permissions block,
// which a job without a job-level permissions block inherits in full.
func parseWorkflowPermissions(workflowYAML []byte) (workflowPermissions, error) {
	var top struct {
		Permissions workflowPermissions `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(workflowYAML, &top); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	return top.Permissions, nil
}

// effectiveContentsPermission resolves the contents permission a job
// actually runs with: its own job-level permissions block if it declares
// one, otherwise the workflow-level block it silently inherits.
func effectiveContentsPermission(job workflowJob, workflowLevelPermissions workflowPermissions) string {
	if job.Permissions != nil {
		return job.Permissions["contents"]
	}
	return workflowLevelPermissions["contents"]
}

// ValidateReleaseEnvironmentPayload checks the committed PUT body and the
// separately declared deployment branch-policy list.
func ValidateReleaseEnvironmentPayload(environmentJSON, branchPoliciesJSON []byte) []string {
	var environment environmentPayload
	if err := json.Unmarshal(environmentJSON, &environment); err != nil {
		return []string{fmt.Sprintf("release Environment payload is invalid JSON: %v", err)}
	}
	var policies branchPolicyList
	if err := json.Unmarshal(branchPoliciesJSON, &policies); err != nil {
		return []string{fmt.Sprintf("release deployment branch-policy payload is invalid JSON: %v", err)}
	}

	var violations []string
	if environment.WaitTimer == nil || *environment.WaitTimer != 0 {
		violations = append(violations, "release Environment wait_timer must be 0")
	}
	if environment.PreventSelfReview == nil || !*environment.PreventSelfReview {
		violations = append(violations, "release Environment must set prevent_self_review to true")
	}
	if len(environment.Reviewers) != 1 || environment.Reviewers[0].Type != "User" || environment.Reviewers[0].ID != expectedReleaseReviewerID {
		violations = append(violations, fmt.Sprintf("release Environment reviewers must be exactly User id %d (@%s)", expectedReleaseReviewerID, expectedReleaseReviewer))
	}
	violations = append(violations, validateDeploymentMode(environment.DeploymentBranchPolicy)...)
	violations = append(violations, validateBranchPolicies(policies.BranchPolicies)...)
	return violations
}

// ValidateLiveReleaseEnvironment checks the distinct responses from the
// environments list, Environment detail, and deployment branch-policy APIs.
func ValidateLiveReleaseEnvironment(environmentsJSON, environmentJSON, branchPoliciesJSON []byte) []string {
	var list environmentsResponse
	if err := json.Unmarshal(environmentsJSON, &list); err != nil {
		return []string{fmt.Sprintf("release Environment list is not auditable: %v", err)}
	}
	found := false
	for _, environment := range list.Environments {
		if environment.Name == expectedReleaseEnvironment {
			found = true
			break
		}
	}
	if list.TotalCount == 0 || !found {
		return []string{"release Environment is absent from the repository environments list"}
	}
	if len(environmentJSON) == 0 {
		return []string{"release Environment detail is empty or unreadable (a 404 is not protected)"}
	}

	var environment environmentResponse
	if err := json.Unmarshal(environmentJSON, &environment); err != nil {
		return []string{fmt.Sprintf("release Environment detail is not auditable: %v", err)}
	}
	var violations []string
	required := make([]protectionRule, 0, 1)
	for _, rule := range environment.ProtectionRules {
		if rule.Type == "required_reviewers" {
			required = append(required, rule)
		}
	}
	if len(required) != 1 {
		violations = append(violations, fmt.Sprintf("release Environment must have exactly one required_reviewers protection rule; found %d", len(required)))
	} else {
		rule := required[0]
		if rule.PreventSelfReview == nil || !*rule.PreventSelfReview {
			violations = append(violations, "release Environment required_reviewers rule must set prevent_self_review to true")
		}
		if len(rule.Reviewers) != 1 || rule.Reviewers[0].Type != "User" || rule.Reviewers[0].Reviewer.Login != expectedReleaseReviewer || rule.Reviewers[0].Reviewer.ID != expectedReleaseReviewerID {
			violations = append(violations, fmt.Sprintf("release Environment required reviewer must be exactly @%s (User id %d)", expectedReleaseReviewer, expectedReleaseReviewerID))
		}
	}
	violations = append(violations, validateDeploymentMode(environment.DeploymentBranchPolicy)...)

	var policies branchPolicyList
	if err := json.Unmarshal(branchPoliciesJSON, &policies); err != nil {
		violations = append(violations, fmt.Sprintf("release deployment branch policies are not auditable: %v", err))
		return violations
	}
	if policies.TotalCount > len(policies.BranchPolicies) {
		violations = append(violations, fmt.Sprintf("release deployment branch policies response is truncated: total_count %d exceeds the %d entries returned (paginate to fetch the rest)", policies.TotalCount, len(policies.BranchPolicies)))
	}
	violations = append(violations, validateBranchPolicies(policies.BranchPolicies)...)
	return violations
}

func validateDeploymentMode(policy *deploymentBranchPolicy) []string {
	if policy == nil {
		return []string{"release Environment deployment_branch_policy must not be null"}
	}
	var violations []string
	if policy.ProtectedBranches == nil || *policy.ProtectedBranches {
		violations = append(violations, "release Environment deployment policy must set protected_branches to false")
	}
	if policy.CustomBranchPolicies == nil || !*policy.CustomBranchPolicies {
		violations = append(violations, "release Environment deployment policy must set custom_branch_policies to true")
	}
	return violations
}

func validateBranchPolicies(policies []branchPolicy) []string {
	got := make([]string, 0, len(policies))
	for _, policy := range policies {
		got = append(got, policy.Type+":"+policy.Name)
	}
	sort.Strings(got)
	want := []string{"branch:main", "tag:v*"}
	if len(got) != len(want) || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return []string{fmt.Sprintf("release deployment branch policies must be exactly %v; got %v", want, got)}
	}
	return nil
}

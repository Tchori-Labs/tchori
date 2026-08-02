// Package ci provides policy checks for the repository's CI configuration.
package ci

import (
	"bufio"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type workflowDoc struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	TimeoutMinutes  *int           `yaml:"timeout-minutes"`
	Needs           any            `yaml:"needs"`
	If              string         `yaml:"if"`
	ContinueOnError bool           `yaml:"continue-on-error"`
	Steps           []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Run             string `yaml:"run"`
	Uses            string `yaml:"uses"`
	If              string `yaml:"if"`
	ContinueOnError bool   `yaml:"continue-on-error"`
}

// JobsMissingTimeout returns the sorted names of jobs in a GitHub Actions
// workflow document that lack a finite, positive timeout-minutes bound.
func JobsMissingTimeout(workflowYAML []byte) ([]string, error) {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return nil, err
	}

	var missing []string
	for name, job := range doc.Jobs {
		if job.TimeoutMinutes == nil || *job.TimeoutMinutes <= 0 {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// unpinnedActionRefSHA matches a full 40-hex-character lowercase commit SHA,
// the only ref form considered immutable enough for a `uses:` reference.
var (
	shaRefPattern            = regexp.MustCompile(`^[0-9a-f]{40}$`)
	actionlintInstallPattern = regexp.MustCompile(`github\.com/rhysd/actionlint/cmd/actionlint@([^\s;&|]+)`)
	actionlintSemverPattern  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	actionlintScriptPin      = regexp.MustCompile(`(?m)^ACTIONLINT_VERSION=(v[0-9]+\.[0-9]+\.[0-9]+)$`)
)

// UnpinnedActionRefs returns sorted "job/action@ref" identifiers for every
// `uses:` step reference in a GitHub Actions workflow document that is not
// pinned to a full 40-hex lowercase commit SHA. Local composite/reusable
// actions referenced by relative path (e.g. "./.github/actions/foo") are
// exempt: they resolve to content already inside this repository's own
// commit, not a mutable external ref, so there is nothing to retarget.
func UnpinnedActionRefs(workflowYAML []byte) ([]string, error) {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return nil, err
	}

	var violations []string
	for jobName, job := range doc.Jobs {
		for _, step := range job.Steps {
			uses := strings.TrimSpace(step.Uses)
			if uses == "" || strings.HasPrefix(uses, "./") || strings.HasPrefix(uses, "docker://") {
				continue
			}

			_, ref, found := strings.Cut(uses, "@")
			if !found || !shaRefPattern.MatchString(ref) {
				violations = append(violations, fmt.Sprintf("%s/%s", jobName, uses))
			}
		}
	}
	sort.Strings(violations)
	return violations, nil
}

// CheckJobEnforcesRaceDetector verifies that the required check job directly
// runs the bounded race detector without dependency or job-level conditions
// that could cause GitHub to skip the required status context.
func CheckJobEnforcesRaceDetector(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	check, ok := doc.Jobs["check"]
	if !ok {
		return fmt.Errorf("workflow declares no check job")
	}
	if check.Needs != nil {
		return fmt.Errorf("check job must not declare needs; dependencies can skip the required context")
	}
	if strings.TrimSpace(check.If) != "" {
		return fmt.Errorf("check job must not declare a job-level if condition")
	}
	if check.ContinueOnError {
		return fmt.Errorf("check job must not allow failures with continue-on-error")
	}
	if _, ok := doc.Jobs["race"]; ok {
		return fmt.Errorf("standalone race job is not enforced by the required check context")
	}

	for _, step := range check.Steps {
		if runsBoundedRaceDetector(step.Run) {
			if step.ContinueOnError {
				return fmt.Errorf("race detector step must not allow failures with continue-on-error")
			}
			if strings.TrimSpace(step.If) != "" {
				return fmt.Errorf("race detector step must not declare an if condition")
			}
			return nil
		}
	}
	return fmt.Errorf("check job must directly run go test with -race, an explicit -timeout, and all packages")
}

// CheckJobRunsWorkflowLint verifies that the required check job installs an
// immutable actionlint release and unconditionally runs the repository's
// workflow-verification script without suppressing failures.
func CheckJobRunsWorkflowLint(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	check, ok := doc.Jobs["check"]
	if !ok {
		return fmt.Errorf("workflow declares no check job")
	}
	if strings.TrimSpace(check.If) != "" {
		return fmt.Errorf("check job must not declare a job-level if condition for workflow lint")
	}

	var lintStep *workflowStep
	var installRef string
	installCandidate := false
	for i := range check.Steps {
		step := &check.Steps[i]
		if strings.Contains(step.Run, "scripts/actionlint-verify.sh") {
			lintStep = step
		}
		if strings.Contains(step.Run, "github.com/rhysd/actionlint/cmd/actionlint") &&
			!strings.Contains(step.Run, "scripts/actionlint-verify.sh") {
			installCandidate = true
			if match := actionlintInstallPattern.FindStringSubmatch(step.Run); match != nil {
				installRef = match[1]
			}
		}
	}

	if lintStep == nil {
		return fmt.Errorf("check job must run scripts/actionlint-verify.sh")
	}
	if strings.TrimSpace(lintStep.If) != "" {
		return fmt.Errorf("workflow lint step must not declare an if condition")
	}
	if lintStep.ContinueOnError {
		return fmt.Errorf("workflow lint step must not allow failures with continue-on-error")
	}
	if strings.Contains(lintStep.Run, "|| true") {
		return fmt.Errorf("workflow lint command must not suppress failures with || true")
	}
	if strings.Contains(lintStep.Run, "set +e") {
		return fmt.Errorf("workflow lint command must not be preceded by set +e")
	}
	if !installCandidate {
		return fmt.Errorf("check job must include an actionlint install step")
	}
	if !actionlintSemverPattern.MatchString(installRef) {
		if installRef == "" {
			return fmt.Errorf("actionlint install step must pin an explicit semver version")
		}
		return fmt.Errorf("actionlint install step must pin an explicit semver version, found @%s", installRef)
	}
	return nil
}

// ActionlintPinConsistency checks that CI, the verification script, and the
// contributor documentation all name the same exact actionlint release.
func ActionlintPinConsistency(workflowYAML, script, readme []byte) error {
	workflowMatch := actionlintInstallPattern.FindSubmatch(workflowYAML)
	if workflowMatch == nil || !actionlintSemverPattern.Match(workflowMatch[1]) {
		return fmt.Errorf("actionlint version absent from workflow install command")
	}
	scriptMatch := actionlintScriptPin.FindSubmatch(script)
	if scriptMatch == nil {
		return fmt.Errorf("ACTIONLINT_VERSION absent from verification script")
	}
	readmeMatch := actionlintInstallPattern.FindSubmatch(readme)
	if readmeMatch == nil || !actionlintSemverPattern.Match(readmeMatch[1]) {
		return fmt.Errorf("actionlint version absent from README install command")
	}

	workflowVersion := string(workflowMatch[1])
	scriptVersion := string(scriptMatch[1])
	readmeVersion := string(readmeMatch[1])
	switch {
	case workflowVersion == scriptVersion && scriptVersion == readmeVersion:
		return nil
	case scriptVersion == readmeVersion:
		return fmt.Errorf("workflow pin %s differs from script pin %s", workflowVersion, scriptVersion)
	case workflowVersion == readmeVersion:
		return fmt.Errorf("script pin %s differs from README pin %s", scriptVersion, readmeVersion)
	case workflowVersion == scriptVersion:
		return fmt.Errorf("README pin %s differs from workflow pin %s", readmeVersion, workflowVersion)
	default:
		return fmt.Errorf("actionlint pins all differ: workflow=%s script=%s README=%s", workflowVersion, scriptVersion, readmeVersion)
	}
}

func parseWorkflow(workflowYAML []byte) (workflowDoc, error) {
	var doc workflowDoc
	if err := yaml.Unmarshal(workflowYAML, &doc); err != nil {
		return workflowDoc{}, fmt.Errorf("parse workflow yaml: %w", err)
	}
	if len(doc.Jobs) == 0 {
		return workflowDoc{}, fmt.Errorf("workflow declares no jobs")
	}
	return doc, nil
}

func runsBoundedRaceDetector(script string) bool {
	scanner := bufio.NewScanner(strings.NewReader(script))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.ContainsAny(line, ";|&") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "go" || fields[1] != "test" {
			continue
		}

		var hasRace, hasTimeout, hasAllPackages bool
		for _, field := range fields[2:] {
			switch {
			case field == "-race":
				hasRace = true
			case strings.HasPrefix(field, "-timeout="):
				duration, err := time.ParseDuration(strings.TrimPrefix(field, "-timeout="))
				hasTimeout = err == nil && duration > 0
			case field == "./...":
				hasAllPackages = true
			}
		}
		if hasRace && hasTimeout && hasAllPackages {
			return true
		}
	}
	return false
}

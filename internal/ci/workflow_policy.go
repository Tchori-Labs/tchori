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
	TimeoutMinutes  *int                `yaml:"timeout-minutes"`
	Needs           any                 `yaml:"needs"`
	If              string              `yaml:"if"`
	ContinueOnError bool                `yaml:"continue-on-error"`
	Environment     workflowEnvironment `yaml:"environment"`
	Permissions     map[string]string   `yaml:"permissions"`
	Env             map[string]string   `yaml:"env"`
	Steps           []workflowStep      `yaml:"steps"`
}

type workflowEnvironment struct {
	Name string
}

func (e *workflowEnvironment) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		e.Name = node.Value
		return nil
	case yaml.MappingNode:
		var value struct {
			Name string `yaml:"name"`
		}
		if err := node.Decode(&value); err != nil {
			return err
		}
		e.Name = value.Name
		return nil
	default:
		return fmt.Errorf("environment must be a string or mapping")
	}
}

type workflowStep struct {
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	If              string            `yaml:"if"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
}

var e2eProxyEnv = map[string]string{
	"HTTPS_PROXY": "http://127.0.0.1:1",
	"HTTP_PROXY":  "http://127.0.0.1:1",
	"NO_PROXY":    "127.0.0.1,localhost",
}

const e2eTestCommand = "go test -tags e2e ./e2e -v"

// E2EProxyEnvIsStepScoped verifies that network denial applies only to the
// e2e test command, leaving checkout, Go setup, and module download unproxied.
func E2EProxyEnvIsStepScoped(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	e2e, ok := doc.Jobs["e2e"]
	if !ok {
		return fmt.Errorf("workflow declares no e2e job")
	}
	for name := range e2eProxyEnv {
		if _, exists := e2e.Env[name]; exists {
			return fmt.Errorf("e2e job must not declare %s at job scope", name)
		}
	}

	testSteps := 0
	for _, step := range e2e.Steps {
		isE2ETest := strings.TrimSpace(step.Run) == e2eTestCommand
		if isE2ETest {
			testSteps++
		}
		for name, want := range e2eProxyEnv {
			got, exists := step.Env[name]
			switch {
			case isE2ETest && !exists:
				return fmt.Errorf("e2e test step must declare %s", name)
			case isE2ETest && got != want:
				return fmt.Errorf("e2e test step %s = %q, want %q", name, got, want)
			case !isE2ETest && exists:
				return fmt.Errorf("only the e2e test step may declare %s", name)
			}
		}
	}
	if testSteps != 1 {
		return fmt.Errorf("e2e job must contain exactly one %q step, found %d", e2eTestCommand, testSteps)
	}
	return nil
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

// CheckJobRunsWindowsVet verifies that the required check job directly and
// unconditionally cross-vets every package for Windows.
func CheckJobRunsWindowsVet(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	check, ok := doc.Jobs["check"]
	if !ok {
		return fmt.Errorf("workflow declares no check job")
	}
	for _, step := range check.Steps {
		if runsWindowsVet(step.Run) {
			if step.ContinueOnError {
				return fmt.Errorf("windows vet step must not allow failures with continue-on-error")
			}
			if strings.TrimSpace(step.If) != "" {
				return fmt.Errorf("windows vet step must not declare an if condition")
			}
			return nil
		}
	}
	return fmt.Errorf("check job must directly run the windows cross-vet command")
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

// Pull requests may only reach main from the integration branch. GitHub
// rulesets can only condition on the base ref, so the source-branch
// restriction is carried by a required status context instead.
const (
	prSourceGateJob     = "pr-source"
	prSourceAllowedHead = "develop"
	prSourceGuardedBase = "main"
)

// prSourceGateEnv maps the shell variables the gate script reads to the
// workflow expressions that must supply them. Reading refs through env keeps
// attacker-chosen branch names out of the script body.
var prSourceGateEnv = map[string]string{
	"EVENT_NAME": "${{ github.event_name }}",
	"BASE_REF":   "${{ github.base_ref }}",
	"HEAD_REF":   "${{ github.head_ref }}",
}

// nonZeroExit matches a shell exit with a failing status.
var nonZeroExit = regexp.MustCompile(`\bexit\s+[1-9][0-9]*\b`)

// PRSourceGateGuardsMain verifies that the workflow declares an
// unconditional pr-source job whose script rejects a pull request into main
// that does not originate from develop. The job declares no dependency and no
// condition, so GitHub always reports the required status context instead of
// reporting it skipped.
func PRSourceGateGuardsMain(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	gate, ok := doc.Jobs[prSourceGateJob]
	if !ok {
		return fmt.Errorf("workflow declares no %s job", prSourceGateJob)
	}
	if gate.Needs != nil {
		return fmt.Errorf("%s job must not declare needs; dependencies can skip the required context", prSourceGateJob)
	}
	if strings.TrimSpace(gate.If) != "" {
		return fmt.Errorf("%s job must not declare a job-level if condition", prSourceGateJob)
	}
	if gate.ContinueOnError {
		return fmt.Errorf("%s job must not allow failures with continue-on-error", prSourceGateJob)
	}

	for _, step := range gate.Steps {
		_, declaresHeadRef := step.Env["HEAD_REF"]
		if !declaresHeadRef && !strings.Contains(step.Run, "$HEAD_REF") {
			continue
		}
		return prSourceGateStepIsSound(step)
	}
	return fmt.Errorf("%s job must run a gate step that reads the head ref from env", prSourceGateJob)
}

func prSourceGateStepIsSound(step workflowStep) error {
	if step.ContinueOnError {
		return fmt.Errorf("%s gate step must not allow failures with continue-on-error", prSourceGateJob)
	}
	if strings.TrimSpace(step.If) != "" {
		return fmt.Errorf("%s gate step must not declare an if condition", prSourceGateJob)
	}
	if strings.Contains(step.Run, "${{") {
		return fmt.Errorf("%s gate step must not interpolate workflow expressions into the script; read refs from env", prSourceGateJob)
	}

	names := make([]string, 0, len(prSourceGateEnv))
	for name := range prSourceGateEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if step.Env[name] != prSourceGateEnv[name] {
			return fmt.Errorf("%s gate step must map %s to %s", prSourceGateJob, name, prSourceGateEnv[name])
		}
	}

	if !strings.Contains(step.Run, prSourceAllowedHead) {
		return fmt.Errorf("%s gate step must compare the head ref to %s", prSourceGateJob, prSourceAllowedHead)
	}
	if !nonZeroExit.MatchString(step.Run) {
		return fmt.Errorf("%s gate step must reject a disallowed head ref with a non-zero exit", prSourceGateJob)
	}
	if !strings.Contains(step.Run, "$BASE_REF") || !strings.Contains(step.Run, prSourceGuardedBase) {
		return fmt.Errorf("%s gate step must scope the gate to base ref %s", prSourceGateJob, prSourceGuardedBase)
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

func runsWindowsVet(script string) bool {
	scanner := bufio.NewScanner(strings.NewReader(script))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.ContainsAny(line, ";|&") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "GOOS=windows" && fields[1] == "go" && fields[2] == "vet" && fields[3] == "./..." {
			return true
		}
	}
	return false
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

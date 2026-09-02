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
	On   workflowTriggers       `yaml:"on"`
}

type workflowTriggers struct {
	PullRequest *workflowPullRequestTrigger `yaml:"pull_request"`
}

type workflowPullRequestTrigger struct {
	Types []string `yaml:"types"`
}

type workflowJob struct {
	TimeoutMinutes  *int                `yaml:"timeout-minutes"`
	RunsOn          any                 `yaml:"runs-on"`
	Needs           any                 `yaml:"needs"`
	If              string              `yaml:"if"`
	ContinueOnError bool                `yaml:"continue-on-error"`
	Environment     workflowEnvironment `yaml:"environment"`
	Permissions     workflowPermissions `yaml:"permissions"`
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

// workflowPermissions is a job's or workflow's permissions block. GitHub
// Actions accepts either an explicit per-scope mapping or the shorthand
// strings read-all/write-all. For this policy's purposes (it only inspects
// the contents scope) the shorthands expand to the equivalent contents
// permission.
type workflowPermissions map[string]string

func (p *workflowPermissions) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		switch node.Value {
		case "read-all":
			*p = workflowPermissions{"contents": "read"}
		case "write-all":
			*p = workflowPermissions{"contents": "write"}
		default:
			return fmt.Errorf("permissions string must be read-all or write-all, got %q", node.Value)
		}
		return nil
	case yaml.MappingNode:
		var value map[string]string
		if err := node.Decode(&value); err != nil {
			return err
		}
		*p = value
		return nil
	default:
		return fmt.Errorf("permissions must be a string or mapping")
	}
}

type workflowStep struct {
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	ID              string            `yaml:"id"`
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

// selfHostedRunnerLabels are the labels every CI job must request. The
// organization's runners advertise [self-hosted Linux X64 docker]; requiring
// the first three pins the jobs to those machines without demanding the
// docker label a future runner might not carry. GitHub matches runner labels
// exactly and never falls back, so a job that omits them silently runs on
// paid GitHub-hosted infrastructure instead.
var selfHostedRunnerLabels = []string{"self-hosted", "Linux", "X64"}

// JobsOffSelfHostedRunners returns the sorted names of jobs whose runs-on
// does not request every label in selfHostedRunnerLabels. A job with no
// runs-on, or one that resolves its runner through a workflow expression the
// repository cannot audit statically, counts as off the self-hosted runners.
func JobsOffSelfHostedRunners(workflowYAML []byte) ([]string, error) {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return nil, err
	}

	var off []string
	for name, job := range doc.Jobs {
		if !requestsSelfHostedRunner(job.RunsOn) {
			off = append(off, name)
		}
	}
	sort.Strings(off)
	return off, nil
}

func requestsSelfHostedRunner(runsOn any) bool {
	var labels []string
	switch value := runsOn.(type) {
	case string:
		labels = []string{value}
	case []any:
		for _, item := range value {
			label, ok := item.(string)
			if !ok {
				return false
			}
			labels = append(labels, label)
		}
	default:
		return false
	}

	for _, required := range selfHostedRunnerLabels {
		found := false
		for _, label := range labels {
			if strings.Contains(label, "${{") {
				return false
			}
			if label == required {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// codeQLCapabilityStepID is the id of the step that probes whether code
// scanning is available, and codeQLGateCondition is the exact condition every
// CodeQL step must carry so the job reports success — with an explanation —
// instead of a permanent red check on a plan where GitHub Advanced Security
// is unavailable.
const (
	codeQLCapabilityStepID = "capability"
	codeQLGateCondition    = "steps.capability.outputs.available == 'true'"
)

// codeQLGatedActions are the CodeQL actions that fail without code scanning.
var codeQLGatedActions = []string{
	"github/codeql-action/init",
	"github/codeql-action/autobuild",
	"github/codeql-action/analyze",
}

// CodeQLAnalysisIsCapabilityGated verifies that the CodeQL workflow probes
// code-scanning availability in an unconditional step and runs every CodeQL
// action only when that probe reported availability. The probe itself must
// still fail the job on any status it does not recognise, and no step may
// hide a real failure behind continue-on-error.
func CodeQLAnalysisIsCapabilityGated(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	for _, name := range sortedJobNames(doc) {
		job := doc.Jobs[name]
		if !declaresCodeQLAction(job) {
			continue
		}
		if err := codeQLJobIsGated(job); err != nil {
			return fmt.Errorf("job %s: %w", name, err)
		}
	}
	return nil
}

func sortedJobNames(doc workflowDoc) []string {
	names := make([]string, 0, len(doc.Jobs))
	for name := range doc.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func declaresCodeQLAction(job workflowJob) bool {
	for _, step := range job.Steps {
		for _, action := range codeQLGatedActions {
			if strings.HasPrefix(step.Uses, action+"@") {
				return true
			}
		}
	}
	return false
}

func codeQLJobIsGated(job workflowJob) error {
	probe, ok := findStepByID(job, codeQLCapabilityStepID)
	if !ok {
		return fmt.Errorf("must declare a step with id %s that probes code-scanning availability", codeQLCapabilityStepID)
	}
	if strings.TrimSpace(probe.If) != "" {
		return fmt.Errorf("capability probe must not declare an if condition; it decides for every event")
	}
	if probe.ContinueOnError {
		return fmt.Errorf("capability probe must not allow failures with continue-on-error; an unrecognised status has to fail the job")
	}

	for _, step := range job.Steps {
		action, gated := codeQLGatedAction(step.Uses)
		if !gated {
			continue
		}
		if step.ContinueOnError {
			return fmt.Errorf("%s must not allow failures with continue-on-error", action)
		}
		if strings.TrimSpace(step.If) != codeQLGateCondition {
			return fmt.Errorf("%s must run only when %s", action, codeQLGateCondition)
		}
	}
	return nil
}

func codeQLGatedAction(uses string) (string, bool) {
	for _, action := range codeQLGatedActions {
		if strings.HasPrefix(uses, action+"@") {
			return action, true
		}
	}
	return "", false
}

func findStepByID(job workflowJob, id string) (workflowStep, bool) {
	for _, step := range job.Steps {
		if step.ID == id {
			return step, true
		}
	}
	return workflowStep{}, false
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

// prSourceGateRequiredTypes are the pull_request activity types the gate's
// on.pull_request trigger must declare. `edited` is required because
// retargeting an open pull request's base branch to main changes base_ref
// without a new commit: without that trigger type, a check run already
// attached to the head SHA from the pull request's earlier base would keep
// satisfying the required status context after the retarget.
var prSourceGateRequiredTypes = []string{"opened", "synchronize", "reopened", "edited"}

// prSourceGateEnv maps the shell variables the gate script reads to the
// workflow expressions that must supply them. Reading refs through env keeps
// attacker-chosen branch and repository names out of the script body.
// HEAD_REPO and BASE_REPO close the fork bypass where github.head_ref is a
// bare branch name: a fork whose branch happens to be named develop must
// still be rejected, because its head repository differs from the base.
var prSourceGateEnv = map[string]string{
	"EVENT_NAME": "${{ github.event_name }}",
	"BASE_REF":   "${{ github.base_ref }}",
	"HEAD_REF":   "${{ github.head_ref }}",
	"HEAD_REPO":  "${{ github.event.pull_request.head.repo.full_name }}",
	"BASE_REPO":  "${{ github.repository }}",
}

// headRepoEqualityCheck is the exact shell comparison the gate script must
// contain to reject a pull request whose head repository differs from the
// base repository.
const headRepoEqualityCheck = `"$HEAD_REPO" != "$BASE_REPO"`

// nonZeroExit matches a shell exit with a failing status.
var nonZeroExit = regexp.MustCompile(`\bexit\s+[1-9][0-9]*\b`)

// PRSourceGateGuardsMain verifies that the workflow declares an
// unconditional pr-source job whose script rejects a pull request into main
// that does not originate from develop in the base repository itself. The
// job declares no dependency and no condition, so GitHub always reports the
// required status context instead of reporting it skipped, and its trigger
// re-runs the gate when a pull request is retargeted.
func PRSourceGateGuardsMain(workflowYAML []byte) error {
	doc, err := parseWorkflow(workflowYAML)
	if err != nil {
		return err
	}

	gate, ok := doc.Jobs[prSourceGateJob]
	if !ok {
		return fmt.Errorf("workflow declares no %s job", prSourceGateJob)
	}
	if err := prSourceGateTriggerIsSound(doc.On); err != nil {
		return err
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

// prSourceGateTriggerIsSound verifies that the workflow's on.pull_request
// trigger declares every activity type the gate depends on to re-run after a
// pull request is retargeted at main without a new commit.
func prSourceGateTriggerIsSound(on workflowTriggers) error {
	if on.PullRequest == nil {
		return fmt.Errorf("workflow must declare an on.pull_request trigger so the %s job re-runs on retarget", prSourceGateJob)
	}
	if len(on.PullRequest.Types) == 0 {
		return fmt.Errorf("%s workflow's pull_request trigger must declare explicit types; the default types omit edited", prSourceGateJob)
	}
	declared := make(map[string]bool, len(on.PullRequest.Types))
	for _, t := range on.PullRequest.Types {
		declared[t] = true
	}
	for _, want := range prSourceGateRequiredTypes {
		if !declared[want] {
			return fmt.Errorf("%s workflow's pull_request trigger must include type %q", prSourceGateJob, want)
		}
	}
	return nil
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
	if !strings.Contains(step.Run, headRepoEqualityCheck) {
		return fmt.Errorf("%s gate step must reject pull requests whose head repository differs from the base repository (expected %s)", prSourceGateJob, headRepoEqualityCheck)
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

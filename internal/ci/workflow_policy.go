// Package ci provides policy checks for the repository's CI configuration.
package ci

import (
	"bufio"
	"fmt"
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

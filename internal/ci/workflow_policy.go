// Package ci provides policy checks for the repository's CI configuration.
package ci

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

type workflowDoc struct {
	Jobs map[string]struct {
		TimeoutMinutes *int `yaml:"timeout-minutes"`
	} `yaml:"jobs"`
}

// JobsMissingTimeout returns the sorted names of jobs in a GitHub Actions
// workflow document that lack a finite, positive timeout-minutes bound.
func JobsMissingTimeout(workflowYAML []byte) ([]string, error) {
	var doc workflowDoc
	if err := yaml.Unmarshal(workflowYAML, &doc); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	if len(doc.Jobs) == 0 {
		return nil, fmt.Errorf("workflow declares no jobs")
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

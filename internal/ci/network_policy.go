// Package ci implements repository CI policy checks enforced by Go tests.
package ci

import (
	"fmt"
	"go/build/constraint"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var publicRegistryReferences = []string{
	"registry.opentofu.org",
	"https://registry.opentofu.org",
}

// ForbiddenWorkflowTriggers returns any pull_request or push events declared
// by workflow. The live-registry smoke must remain schedule/manual-only.
func ForbiddenWorkflowTriggers(workflow []byte) ([]string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(workflow, &document); err != nil {
		return nil, fmt.Errorf("parse workflow YAML: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("workflow YAML must contain a mapping document")
	}

	root := document.Content[0]
	var on *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "on" {
			on = root.Content[i+1]
			break
		}
	}
	if on == nil {
		return nil, fmt.Errorf("workflow YAML has no on trigger")
	}

	var triggers []string
	switch on.Kind {
	case yaml.ScalarNode:
		triggers = append(triggers, on.Value)
	case yaml.SequenceNode:
		for _, item := range on.Content {
			triggers = append(triggers, item.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			triggers = append(triggers, on.Content[i].Value)
		}
	default:
		return nil, fmt.Errorf("workflow on trigger has unsupported YAML kind %d", on.Kind)
	}

	var violations []string
	for _, forbidden := range []string{"pull_request", "push"} {
		if slices.Contains(triggers, forbidden) {
			violations = append(violations, forbidden)
		}
	}
	return violations, nil
}

// PublicRegistrySourceViolations returns non-smoke source filenames that
// reference the public OpenTofu registry. A source is smoke-only when its
// its build expression requires the smoke tag; all other sources are treated
// as potentially PR-gated and scanned.
func PublicRegistrySourceViolations(sources map[string][]byte) ([]string, error) {
	var violations []string
	for name, source := range sources {
		smokeOnly, err := requiresSmokeTag(source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if smokeOnly {
			continue
		}
		lower := strings.ToLower(string(source))
		for _, reference := range publicRegistryReferences {
			if strings.Contains(lower, reference) {
				violations = append(violations, name+": references "+reference)
				break
			}
		}
	}
	slices.Sort(violations)
	return violations, nil
}

func requiresSmokeTag(source []byte) (bool, error) {
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			break
		}
		if strings.HasPrefix(trimmed, "//go:build ") {
			expr, err := constraint.Parse(trimmed)
			if err != nil {
				return false, fmt.Errorf("parse build constraint: %w", err)
			}
			return expressionRequiresSmoke(expr), nil
		}
	}
	return false, nil
}

// expressionRequiresSmoke reports whether every path through the common Go
// build-expression forms requires the smoke tag. Returning false for unusual
// negations is conservative: the source is scanned rather than exempted.
func expressionRequiresSmoke(expr constraint.Expr) bool {
	switch typed := expr.(type) {
	case *constraint.TagExpr:
		return typed.Tag == "smoke"
	case *constraint.AndExpr:
		return expressionRequiresSmoke(typed.X) || expressionRequiresSmoke(typed.Y)
	case *constraint.OrExpr:
		return expressionRequiresSmoke(typed.X) && expressionRequiresSmoke(typed.Y)
	default:
		return false
	}
}

// Package security implements repository security-policy document checks enforced by Go tests.
package security

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	pendingMarker  = "<!-- security-channel: pending -->"
	pvrMarker      = "<!-- security-channel: github-pvr -->"
	fallbackPrefix = "<!-- security-channel: fallback-contact"
	regionStart    = "<!-- security-contact:published:start -->"
	regionEnd      = "<!-- security-contact:published:end -->"
	maintainerText = "Maintainer action required"
	runbookLink    = "docs/security-disclosure-channel.md"
	decisionPrefix = "Tchori-Labs/main:"
)

var (
	addressRE            = regexp.MustCompile(`(?i)mailto:\S|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	labelRE              = regexp.MustCompile(`(?m)^[\t ]*[*_>#\-\t ]*Private security contact:[\t ]*\S`)
	pgpRE                = regexp.MustCompile(`BEGIN PGP`)
	bareFingerprintRE    = regexp.MustCompile(`[0-9A-Fa-f]{40}`)
	groupedFingerprintRE = regexp.MustCompile(`(?:[0-9A-Fa-f]{4}[ \t-]){9}[0-9A-Fa-f]{4}`)
	longKeyIDRE          = regexp.MustCompile(`0x[0-9A-Fa-f]{16,40}`)
	fallbackMarkerRE     = regexp.MustCompile(`<!-- security-channel: fallback-contact decision=([^\s]+) -->`)
	decisionRefRE        = regexp.MustCompile(`^Tchori-Labs/main:decisions/(?:[A-Za-z0-9][A-Za-z0-9._-]*/)*[A-Za-z0-9][A-Za-z0-9._-]*\.md$`)
)

// Markers exposes occurrence counts so duplicate declarations cannot collapse to booleans.
type Markers struct {
	Pending           int
	GitHubPVR         int
	Fallback          int
	DecisionReference string
}

// Region describes the bounded publication region without interpreting its value.
type Region struct {
	StartCount           int
	EndCount             int
	StartLine            int
	EndLine              int
	BodyLines            []string
	AffordanceOutside    bool
	ContactShapedOutside bool
}

// MaintainerActionNoticePresent reports whether the outstanding-action notice remains.
func MaintainerActionNoticePresent(securityMD []byte) bool {
	return strings.Contains(string(securityMD), maintainerText)
}

// ContactAffordancePresent implements only reporter-actionable C1/payload/region forms.
// It intentionally has no placeholder exemption; that exemption belongs only to the
// documentation contract-example grep and must not be ported here.
func ContactAffordancePresent(securityMD []byte) bool {
	return addressRE.Match(securityMD) || labelRE.Match(securityMD)
}

// ContactShapedValuePresent is the wider pending/outside-region prohibition.
// It intentionally has no placeholder exemption; see ContactAffordancePresent.
func ContactShapedValuePresent(securityMD []byte) bool {
	return ContactAffordancePresent(securityMD) || pgpRE.Match(securityMD) ||
		bareFingerprintRE.Match(securityMD) || groupedFingerprintRE.Match(securityMD) ||
		longKeyIDRE.Match(securityMD)
}

// ChannelMarkers parses marker occurrence counts and the sole exact fallback reference.
func ChannelMarkers(securityMD []byte) (Markers, error) {
	s := string(securityMD)
	m := Markers{
		Pending:   strings.Count(s, pendingMarker),
		GitHubPVR: strings.Count(s, pvrMarker),
		Fallback:  strings.Count(s, fallbackPrefix),
	}
	matches := fallbackMarkerRE.FindAllSubmatch(securityMD, -1)
	if m.Fallback == 1 && len(matches) == 1 {
		m.DecisionReference = string(matches[0][1])
	}
	if m.Fallback == 1 && len(matches) != 1 {
		return m, errors.New("fallback marker does not contain exactly one decision reference")
	}
	return m, nil
}

// PublishedRegion resolves sentinels, body bounds, and values outside the region.
func PublishedRegion(securityMD []byte) (Region, error) {
	s := string(securityMD)
	r := Region{StartCount: strings.Count(s, regionStart), EndCount: strings.Count(s, regionEnd)}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.Contains(line, regionStart) && r.StartLine == 0 {
			r.StartLine = i + 1
		}
		if strings.Contains(line, regionEnd) && r.EndLine == 0 {
			r.EndLine = i + 1
		}
	}
	if r.StartCount == 1 && r.EndCount == 1 && r.EndLine > r.StartLine {
		r.BodyLines = append([]string(nil), lines[r.StartLine:r.EndLine-1]...)
		outside := append([]string(nil), lines[:r.StartLine-1]...)
		outside = append(outside, lines[r.EndLine:]...)
		outsideBytes := []byte(strings.Join(outside, "\n"))
		r.AffordanceOutside = ContactAffordancePresent(outsideBytes)
		r.ContactShapedOutside = ContactShapedValuePresent(outsideBytes)
	}
	return r, nil
}

// ValidateDecisionReference enforces syntax and lexical containment.
func ValidateDecisionReference(ref string) error {
	if !decisionRefRE.MatchString(ref) {
		return fmt.Errorf("decision reference violates anchored grammar")
	}
	if strings.ContainsAny(ref, "@\\% \t\r\n") || strings.Count(ref, ":") != 1 || strings.Contains(strings.ToLower(ref), "mailto") {
		return fmt.Errorf("decision reference contains a prohibited character or scheme")
	}
	p := strings.TrimPrefix(ref, decisionPrefix)
	if p == ref || path.Clean(p) != p || !strings.HasPrefix(p, "decisions/") {
		return fmt.Errorf("decision reference escapes decisions namespace")
	}
	if strings.HasPrefix(path.Base(p), "EXAMPLE-") {
		return fmt.Errorf("decision reference uses reserved EXAMPLE prefix")
	}
	return nil
}

// CheckDisclosureChannelConsistency validates markers, notice, descriptions, and region.
func CheckDisclosureChannelConsistency(securityMD []byte) error {
	markers, markerErr := ChannelMarkers(securityMD)
	if markerErr != nil {
		return markerErr
	}
	region, _ := PublishedRegion(securityMD)
	pending := markers.Pending == 1 && markers.GitHubPVR == 0 && markers.Fallback == 0
	operative := markers.Pending == 0 && markers.GitHubPVR <= 1 && markers.Fallback <= 1 && markers.GitHubPVR+markers.Fallback >= 1

	switch {
	case markers.Pending > 1:
		return errors.New("duplicate pending marker")
	case markers.GitHubPVR > 1:
		return errors.New("duplicate github-pvr marker")
	case markers.Fallback > 1:
		return errors.New("duplicate fallback-contact marker")
	case markers.Pending > 0 && markers.GitHubPVR+markers.Fallback > 0:
		return errors.New("pending and operative markers coexist")
	case !pending && !operative:
		return errors.New("document declares no valid channel state")
	}

	if pending {
		if !MaintainerActionNoticePresent(securityMD) {
			return errors.New("pending state requires maintainer-action notice")
		}
		if !strings.Contains(string(securityMD), runbookLink) {
			return errors.New("pending state requires disclosure-channel runbook link")
		}
		if ContactShapedValuePresent(securityMD) {
			return errors.New("pending state contains a contact-shaped value")
		}
		if region.StartCount != 0 || region.EndCount != 0 {
			return errors.New("pending state contains published-region sentinel")
		}
		return nil
	}

	if MaintainerActionNoticePresent(securityMD) {
		return errors.New("operative state retains maintainer-action notice")
	}
	lower := strings.ToLower(string(securityMD))
	if markers.GitHubPVR == 1 && !strings.Contains(lower, "private vulnerability reporting is enabled") && !strings.Contains(lower, "enabled private vulnerability reporting") {
		return errors.New("github-pvr marker is not described as enabled")
	}
	if markers.Fallback == 0 {
		if region.StartCount != 0 || region.EndCount != 0 {
			return errors.New("published region exists without fallback marker")
		}
		return nil
	}
	if err := ValidateDecisionReference(markers.DecisionReference); err != nil {
		return err
	}
	if !strings.Contains(lower, "board-approved") || !strings.Contains(string(securityMD), markers.DecisionReference) {
		return errors.New("fallback marker is not described with board approval and decision reference")
	}
	if region.StartCount != 1 || region.EndCount != 1 {
		return errors.New("fallback requires exactly one published-region sentinel pair")
	}
	if region.EndLine <= region.StartLine {
		return errors.New("published region is unbalanced or misordered")
	}
	if len(region.BodyLines) < 1 || len(region.BodyLines) > 3 {
		return errors.New("published region body must contain 1-3 lines")
	}
	for _, line := range region.BodyLines {
		if strings.TrimSpace(line) == "" {
			return errors.New("published region contains a blank line")
		}
		if !ContactAffordancePresent([]byte(line)) {
			return errors.New("published region line is not a contact affordance")
		}
	}
	if region.ContactShapedOutside {
		return errors.New("contact-shaped value appears outside published region")
	}
	return nil
}

// CheckNoSyntheticContact is a live-document-only guard. Fixture consistency and
// the shell audit omit placeholder-domain rejection so synthetic harness values work;
// this does not create an angle-bracket-placeholder exemption in either detector.
func CheckNoSyntheticContact(securityMD []byte) error {
	lower := strings.ToLower(string(securityMD))
	if strings.Contains(lower, "not-a-real-key") {
		return errors.New("live policy contains synthetic key token")
	}
	matches := regexp.MustCompile(`(?i)[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+)`).FindAllStringSubmatch(string(securityMD), -1)
	for _, match := range matches {
		domain := strings.ToLower(match[1])
		if domain == "localhost" || strings.HasSuffix(domain, ".invalid") || strings.HasSuffix(domain, ".example") ||
			domain == "example.com" || domain == "example.org" || domain == "example.net" {
			return errors.New("live policy contains a reserved placeholder contact domain")
		}
	}
	return nil
}

// CheckPolicyLinked requires the README to point at both policy and runbook.
func CheckPolicyLinked(readmeMD []byte) error {
	s := string(readmeMD)
	if !strings.Contains(s, "SECURITY.md") {
		return errors.New("README does not link SECURITY.md")
	}
	if !strings.Contains(s, "docs/security-disclosure-channel.md") {
		return errors.New("README does not link disclosure-channel runbook")
	}
	return nil
}

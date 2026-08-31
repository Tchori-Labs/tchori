package provider

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
)

// UnresolvedReferenceFinding identifies a reference-shaped substring that
// survived composition. Match is only the matched ${...} fragment, never the
// complete attribute value, because provider and state-derived values may be
// secrets.
type UnresolvedReferenceFinding struct {
	Path  string
	Match string
}

// FindUnresolvedReferences walks a composed cty value and returns findings in
// deterministic path/match order. Unknown and null values are not inspected or
// descended into. Provider-returned values are intentionally outside this
// scanner's boundary; callers use it only on values tchori is about to send.
func FindUnresolvedReferences(value cty.Value) []UnresolvedReferenceFinding {
	var findings []UnresolvedReferenceFinding
	_ = cty.Walk(value, func(path cty.Path, current cty.Value) (bool, error) {
		if !current.IsKnown() || current.IsNull() {
			return false, nil
		}
		if current.Type() == cty.String {
			if match, found := config.FindUnresolvedReference(current.AsString()); found {
				findings = append(findings, UnresolvedReferenceFinding{
					Path:  unresolvedPath(value, path),
					Match: match,
				})
			}
			return false, nil
		}
		return true, nil
	})
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Match < findings[j].Match
	})
	return findings
}

// UnresolvedReferenceDiagnostic constructs the shared TC-048 diagnostic.
// Compose passes an empty address because composition has only an attribute
// path in scope; apply passes the resource address. Detail intentionally omits
// the complete attribute value.
func UnresolvedReferenceDiagnostic(address string, finding UnresolvedReferenceFinding) diag.Diagnostic {
	return diag.Errorf(address, "unresolved reference", fmt.Sprintf(
		"attribute %s contains unresolved reference %s", finding.Path, finding.Match))
}

// unresolvedPath renders object attributes, maps, and sequences without
// exposing values. A set's IndexStep key is the element value itself; rendering
// it like an ordinary string index would leak the full (possibly secret) set
// element. Detect set containers from their type and mask the step instead.
func unresolvedPath(root cty.Value, path cty.Path) string {
	var out strings.Builder
	container := root
	for i, step := range path {
		switch typed := step.(type) {
		case cty.GetAttrStep:
			if i > 0 {
				out.WriteByte('.')
			}
			out.WriteString(typed.Name)
		case cty.IndexStep:
			if container.IsKnown() && !container.IsNull() && container.Type().IsSetType() {
				out.WriteString("[<set element>]")
			} else {
				key := typed.Key
				switch {
				case key.Type() == cty.Number && key.IsKnown() && !key.IsNull():
					_, _ = fmt.Fprintf(&out, "[%s]", key.AsBigFloat().Text('g', -1))
				case key.Type() == cty.String && key.IsKnown() && !key.IsNull():
					_, _ = fmt.Fprintf(&out, "[%s]", strconv.Quote(key.AsString()))
				default:
					out.WriteString("[...]")
				}
			}
		}
		if container.IsKnown() && !container.IsNull() {
			next, err := step.Apply(container)
			if err == nil {
				container = next
			}
		}
	}
	return out.String()
}

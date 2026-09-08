package apply

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/sensitive"
)

type attemptedChange struct {
	path          cty.Path
	before, after cty.Value
}

// attemptedChanges walks the prior and planned values without traversing a
// null or unknown container. Objects and maps have stable named
// correspondence; ordered collections and sets are compared wholesale.
func attemptedChanges(path cty.Path, prior, planned cty.Value, block *provider.SchemaBlock, spec *sensitive.Spec, out *[]attemptedChange) {
	if prior.RawEquals(planned) {
		return
	}
	if !prior.IsKnown() || !planned.IsKnown() || prior.IsNull() != planned.IsNull() {
		*out = append(*out, attemptedChange{path: copyPath(path), before: prior, after: planned})
		return
	}
	// RawEquals handled two null values above, and exactly one null returned
	// above. Classify before any traversal or element access.
	if prior.IsNull() || planned.IsNull() {
		return
	}

	ty := prior.Type()
	if sensitiveDiagnosticCollection(block, spec, path, ty) {
		*out = append(*out, attemptedChange{path: copyPath(path), before: prior, after: planned})
		return
	}
	switch {
	case ty.IsObjectType() && planned.Type().Equals(ty):
		names := make([]string, 0, len(ty.AttributeTypes()))
		for name := range ty.AttributeTypes() {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			attemptedChanges(appendPath(path, cty.GetAttrStep{Name: name}), prior.GetAttr(name), planned.GetAttr(name), block, spec, out)
		}
	case ty.IsMapType() && planned.Type().Equals(ty):
		beforeElems := prior.AsValueMap()
		afterElems := planned.AsValueMap()
		keys := make(map[string]struct{}, len(beforeElems)+len(afterElems))
		for key := range beforeElems {
			keys[key] = struct{}{}
		}
		for key := range afterElems {
			keys[key] = struct{}{}
		}
		sortedKeys := make([]string, 0, len(keys))
		for key := range keys {
			sortedKeys = append(sortedKeys, key)
		}
		sort.Strings(sortedKeys)
		for _, key := range sortedKeys {
			before, ok := beforeElems[key]
			if !ok {
				before = cty.NullVal(ty.ElementType())
			}
			after, ok := afterElems[key]
			if !ok {
				after = cty.NullVal(ty.ElementType())
			}
			attemptedChanges(appendPath(path, cty.IndexStep{Key: cty.StringVal(key)}), before, after, block, spec, out)
		}
	default:
		*out = append(*out, attemptedChange{path: copyPath(path), before: prior, after: planned})
	}
}

// attemptedChangeSummary describes the exact before/after values sent to an
// ApplyResource update. Creates have no prior object and therefore no useful
// before/after attribute list.
func attemptedChangeSummary(addr, action string, block *provider.SchemaBlock, spec *sensitive.Spec, prior, planned cty.Value) diag.Diagnostics {
	if !prior.IsKnown() || prior.IsNull() || !prior.Type().IsObjectType() {
		return nil
	}

	var changes []attemptedChange
	attemptedChanges(nil, prior, planned, block, spec, &changes)
	if len(changes) == 0 {
		return nil
	}
	sort.Slice(changes, func(i, j int) bool {
		return renderedPath(changes[i].path) < renderedPath(changes[j].path)
	})

	var detail strings.Builder
	_, _ = fmt.Fprintf(&detail, "the failing %s for %s attempted these changes:\n", action, addr)
	for _, change := range changes {
		redact := redactConsistencyValue(block, spec, change.path)
		_, _ = fmt.Fprintf(&detail, "  %s: %s -> %s\n", renderedPath(change.path),
			renderConsistencySide(consistencySide{value: change.before}, redact),
			renderConsistencySide(consistencySide{value: change.after}, redact))
	}
	detail.WriteString("these are the values tchori sent to the provider; the provider or its API rejected the request")
	return diag.Diagnostics{diag.Warnf(addr, "attempted change", detail.String())}
}

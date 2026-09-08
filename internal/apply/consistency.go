package apply

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/sensitive"
)

type consistencySide struct {
	value  cty.Value
	absent bool
}

type consistencyDivergence struct {
	path             cty.Path
	planned, applied consistencySide
}

// checkResultConsistency verifies the concrete promises made by configured
// attributes in a non-destroy plan. Traversal deliberately uses IsKnown
// (shallow knownness), just like resolvePlannedUnknowns: a resource object
// normally contains unknown computed leaves and must still be traversed.
// IsWhollyKnown is reserved for values compared or rendered as a whole.
func checkResultConsistency(addr string, block *provider.SchemaBlock, spec *sensitive.Spec, planned, cfgVal, newState cty.Value) diag.Diagnostics {
	if newState.IsNull() {
		return diag.Diagnostics{diag.Errorf(addr, "provider returned no state after apply",
			"a create or update must return the resource object; nothing was persisted for this address")}
	}
	if !newState.IsKnown() {
		return diag.Diagnostics{diag.Errorf(addr, "provider returned unknown state after apply",
			"post-apply resource state must be resolved; nothing was persisted for this address")}
	}

	var divergences []consistencyDivergence
	walkConsistency(nil, planned, cfgVal, newState, block, spec, true, &divergences)
	if len(divergences) == 0 {
		return nil
	}
	sort.Slice(divergences, func(i, j int) bool {
		return renderedPath(divergences[i].path) < renderedPath(divergences[j].path)
	})

	var detail strings.Builder
	detail.WriteString("the provider accepted the apply without honouring these configured attributes:\n")
	for _, d := range divergences {
		redact := redactConsistencyValue(block, spec, d.path)
		_, _ = fmt.Fprintf(&detail, "  %s: planned %s, applied %s\n",
			renderedPath(d.path), renderConsistencySide(d.planned, redact), renderConsistencySide(d.applied, redact))
	}
	detail.WriteString("state records what the provider actually returned, so the next plan will propose the same change again")
	return diag.Diagnostics{diag.Errorf(addr, "provider produced inconsistent result after apply", detail.String())}
}

func walkConsistency(path cty.Path, planned, cfgVal, applied cty.Value, block *provider.SchemaBlock, spec *sensitive.Spec, aggregateSensitive bool, out *[]consistencyDivergence) {
	// A null or shallow-unknown config node authors no promise. Classify all
	// three values before any traversal: cty panics when null/unknown object
	// and map values are indexed or iterated.
	if !cfgVal.IsKnown() || cfgVal.IsNull() {
		return
	}
	if !planned.IsKnown() {
		return
	}
	if planned.IsNull() {
		compareConsistencyWhole(path, planned, cfgVal, applied, out)
		return
	}

	ty := planned.Type()
	if aggregateSensitive && sensitiveDiagnosticCollection(block, spec, path, ty) {
		var nested []consistencyDivergence
		walkConsistency(path, planned, cfgVal, applied, block, spec, false, &nested)
		if len(nested) != 0 {
			*out = append(*out, consistencyDivergence{
				path: copyPath(path), planned: consistencySide{value: planned}, applied: consistencySide{value: applied},
			})
		}
		return
	}
	switch {
	case ty.IsObjectType() || ty.IsMapType():
		if !applied.IsKnown() || applied.IsNull() {
			*out = append(*out, consistencyDivergence{path: copyPath(path), planned: consistencySide{value: planned}, applied: consistencySide{value: applied}})
			return
		}
		if ty.IsObjectType() {
			names := make([]string, 0, len(ty.AttributeTypes()))
			for name := range ty.AttributeTypes() {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				walkConsistency(appendPath(path, cty.GetAttrStep{Name: name}), planned.GetAttr(name), cfgVal.GetAttr(name), applied.GetAttr(name), block, spec, aggregateSensitive, out)
			}
			return
		}

		// Unlike resolvePlannedUnknowns, consistency walks the sorted union of
		// planned and applied map keys and has no empty-map short-circuit. The
		// map container authors structure (its complete key set), while each
		// concrete config element separately authors its value.
		plannedElems := planned.AsValueMap()
		appliedElems := applied.AsValueMap()
		cfgElems := cfgVal.AsValueMap()
		keys := make(map[string]struct{}, len(plannedElems)+len(appliedElems))
		for key := range plannedElems {
			keys[key] = struct{}{}
		}
		for key := range appliedElems {
			keys[key] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for key := range keys {
			sorted = append(sorted, key)
		}
		sort.Strings(sorted)
		for _, key := range sorted {
			p, pok := plannedElems[key]
			a, aok := appliedElems[key]
			c, cok := cfgElems[key]
			keyPath := appendPath(path, cty.IndexStep{Key: cty.StringVal(key)})
			switch {
			case pok && !aok:
				if cok { // plan-only provider inventions are outside this check.
					*out = append(*out, consistencyDivergence{path: keyPath, planned: consistencySide{value: p}, applied: consistencySide{absent: true}})
				}
			case !pok && aok:
				*out = append(*out, consistencyDivergence{path: keyPath, planned: consistencySide{absent: true}, applied: consistencySide{value: a}})
			case pok && aok && cok:
				walkConsistency(keyPath, p, c, a, block, spec, aggregateSensitive, out)
			}
		}
		return

	case ty.IsListType() || ty.IsTupleType():
		if !applied.IsKnown() || applied.IsNull() {
			*out = append(*out, consistencyDivergence{path: copyPath(path), planned: consistencySide{value: planned}, applied: consistencySide{value: applied}})
			return
		}

		// Keep traversal aligned with resolvePlannedUnknowns: ordered
		// collections have sound positional correspondence only when config,
		// plan, and result lengths agree. Recurse then so an unknown element
		// does not hide divergence in a concrete sibling. A length mismatch is
		// compared wholesale because indexing could misalign elements.
		plannedElems := planned.AsValueSlice()
		if cfgVal.LengthInt() != len(plannedElems) || applied.LengthInt() != len(plannedElems) {
			compareConsistencyWhole(path, planned, cfgVal, applied, out)
			return
		}
		cfgElems := cfgVal.AsValueSlice()
		appliedElems := applied.AsValueSlice()
		for i, p := range plannedElems {
			walkConsistency(appendPath(path, cty.IndexStep{Key: cty.NumberIntVal(int64(i))}), p, cfgElems[i], appliedElems[i], block, spec, aggregateSensitive, out)
		}
		return

	default:
		// Sets have no stable element correspondence, so they and primitive
		// values are compared wholesale.
		compareConsistencyWhole(path, planned, cfgVal, applied, out)
	}
}

func compareConsistencyWhole(path cty.Path, planned, cfgVal, applied cty.Value, out *[]consistencyDivergence) {
	if !cfgVal.IsWhollyKnown() || cfgVal.IsNull() || !planned.IsWhollyKnown() {
		return
	}
	if !applied.IsWhollyKnown() || applied.IsNull() {
		if planned.IsNull() && applied.IsNull() {
			return
		}
		*out = append(*out, consistencyDivergence{path: copyPath(path), planned: consistencySide{value: planned}, applied: consistencySide{value: applied}})
		return
	}
	eq := planned.Equals(applied)
	if !eq.IsKnown() || !eq.True() {
		*out = append(*out, consistencyDivergence{path: copyPath(path), planned: consistencySide{value: planned}, applied: consistencySide{value: applied}})
	}
}

func renderConsistencySide(side consistencySide, sensitive bool) string {
	if sensitive {
		return "(sensitive value)"
	}
	if side.absent {
		return "absent"
	}
	if !side.value.IsWhollyKnown() {
		return "(unknown value)"
	}
	if side.value.IsNull() {
		return "null"
	}
	b, err := ctyjson.Marshal(side.value, side.value.Type())
	if err != nil {
		return "(unrenderable value)"
	}
	return string(b)
}

func renderedPath(path cty.Path) string {
	if out := plan.PathString(path); out != "" {
		return out
	}
	return "<root>"
}

func copyPath(path cty.Path) cty.Path {
	return append(cty.Path(nil), path...)
}

func appendPath(path cty.Path, step cty.PathStep) cty.Path {
	out := make(cty.Path, len(path), len(path)+1)
	copy(out, path)
	return append(out, step)
}

func sensitiveDiagnosticCollection(block *provider.SchemaBlock, spec *sensitive.Spec, path cty.Path, ty cty.Type) bool {
	if len(path) == 0 || (!ty.IsMapType() && !ty.IsListType() && !ty.IsSetType() && !ty.IsTupleType()) {
		return false
	}
	return redactConsistencyValue(block, spec, path)
}

// redactConsistencyValue applies both the effective resource sensitivity
// contract and provider schema sensitivity. Effective paths redact their
// complete subtree and any aggregate containing a sensitive descendant.
// Schema paths that cannot be resolved fail closed.
func redactConsistencyValue(block *provider.SchemaBlock, spec *sensitive.Spec, path cty.Path) bool {
	if spec != nil && spec.RedactsDiagnosticPath(path) {
		return true
	}
	return redactSchemaValue(block, path)
}

func redactSchemaValue(block *provider.SchemaBlock, path cty.Path) bool {
	cur := block
	for i := 0; i < len(path); i++ {
		step, ok := path[i].(cty.GetAttrStep)
		if !ok || cur == nil {
			return true
		}
		if attr, ok := cur.Attributes[step.Name]; ok {
			if attr == nil || attr.Sensitive {
				return true
			}
			if len(attr.NestedType) == 0 {
				return false
			}
			nested := &provider.SchemaBlock{Attributes: attr.NestedType}
			if attr.Type.IsListType() || attr.Type.IsSetType() || attr.Type.IsMapType() {
				i++ // skip collection index or key
			}
			if i >= len(path)-1 {
				return blockHasSensitiveDescendant(nested)
			}
			return redactSchemaValue(nested, path[i+1:])
		}
		nb, ok := cur.Blocks[step.Name]
		if !ok || nb == nil || nb.Block == nil {
			return true
		}
		if i == len(path)-1 {
			return blockHasSensitiveDescendant(nb.Block)
		}
		if nb.Nesting != "single" {
			i++ // skip list/set index or map key
			if i >= len(path) {
				return blockHasSensitiveDescendant(nb.Block)
			}
		}
		cur = nb.Block
	}
	return true
}

func blockHasSensitiveDescendant(block *provider.SchemaBlock) bool {
	if block == nil {
		return true
	}
	for _, attr := range block.Attributes {
		if attr == nil || attr.Sensitive {
			return true
		}
		if len(attr.NestedType) != 0 && blockHasSensitiveDescendant(&provider.SchemaBlock{Attributes: attr.NestedType}) {
			return true
		}
	}
	for _, nb := range block.Blocks {
		if nb == nil || blockHasSensitiveDescendant(nb.Block) {
			return true
		}
	}
	return false
}

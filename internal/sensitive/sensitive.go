package sensitive

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider"
)

// Spec is the effective sensitive-path contract for one resource. Paths are
// index-insensitive; exemptions identify individual raw-config instances.
type Spec struct {
	paths       []string
	exempt      []string
	setPrefixes []string
}

// Resolve is the single constructor used by persistence and planning paths.
func Resolve(block *provider.SchemaBlock, declared []string, rawCfg map[string]any) (*Spec, diag.Diagnostics) {
	ds := ValidateDeclared(block, declared)
	if ds.HasErrors() {
		return nil, ds
	}
	set := map[string]bool{}
	var setPrefixes []string
	walkSchema(block, "", set, &setPrefixes)
	for _, path := range declared {
		set[path] = true
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	sort.Strings(setPrefixes)
	s := &Spec{paths: paths, setPrefixes: setPrefixes}
	return s.Effective(rawCfg), ds
}

func walkSchema(block *provider.SchemaBlock, prefix string, paths map[string]bool, setPrefixes *[]string) {
	if block == nil {
		return
	}
	for name, attr := range block.Attributes {
		path := join(prefix, name)
		if attr != nil && attr.Sensitive {
			paths[path] = true
		}
	}
	for name, nested := range block.Blocks {
		if nested == nil {
			continue
		}
		path := join(prefix, name)
		if nested.Nesting == "set" {
			*setPrefixes = append(*setPrefixes, path)
		}
		walkSchema(nested.Block, path, paths, setPrefixes)
	}
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// ValidateDeclared rejects misspelled paths. Repeated declarations are valid.
func ValidateDeclared(block *provider.SchemaBlock, declared []string) diag.Diagnostics {
	var ds diag.Diagnostics
	seen := map[string]bool{}
	for _, path := range declared {
		if seen[path] {
			continue
		}
		seen[path] = true
		if path == "" || !schemaPathExists(block, strings.Split(path, ".")) {
			ds = append(ds, diag.Errorf("", "unknown sensitive attribute", fmt.Sprintf("sensitive_attributes path %q does not exist in the resource schema", path)))
		}
	}
	return ds
}

func schemaPathExists(block *provider.SchemaBlock, parts []string) bool {
	if block == nil || len(parts) == 0 {
		return false
	}
	if len(parts) == 1 {
		_, attr := block.Attributes[parts[0]]
		_, nested := block.Blocks[parts[0]]
		return attr || nested
	}
	if nested, ok := block.Blocks[parts[0]]; ok && nested != nil {
		return schemaPathExists(nested.Block, parts[1:])
	}
	attr, ok := block.Attributes[parts[0]]
	if !ok || attr == nil {
		return false
	}
	return typePathExists(attr.Type, parts[1:])
}

func typePathExists(ty cty.Type, parts []string) bool {
	for ty.IsListType() || ty.IsSetType() || ty.IsMapType() {
		ty = ty.ElementType()
	}
	if !ty.IsObjectType() || len(parts) == 0 || !ty.HasAttribute(parts[0]) {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	return typePathExists(ty.AttributeType(parts[0]), parts[1:])
}

// Paths returns the sorted effective path set. Literal exemptions never narrow it.
func (s *Spec) Paths() []string { return append([]string(nil), s.paths...) }

// ExemptInstances returns sorted, index-qualified raw-literal instances.
func (s *Spec) ExemptInstances() []string { return append([]string(nil), s.exempt...) }

// Effective records per-instance exemptions from raw config syntax. The
// exemption exists only because the literal is already present in committed
// configuration. It must never be inferred from composed values: doing so
// would exempt referenced secrets (and future env wrappers, TC-054/#46).
func (s *Spec) Effective(rawCfg map[string]any) *Spec {
	out := &Spec{paths: s.Paths(), setPrefixes: append([]string(nil), s.setPrefixes...)}
	if rawCfg == nil {
		return out
	}
	exempt := map[string]bool{}
	walkRaw(rawCfg, "", "", out.paths, out.setPrefixes, exempt)
	for path := range exempt {
		out.exempt = append(out.exempt, path)
	}
	sort.Strings(out.exempt)
	return out
}

func walkRaw(v any, logical, instance string, paths, setPrefixes []string, exempt map[string]bool) {
	if scalarLiteral(v) && contains(paths, logical) && !underPrefix(logical, setPrefixes) {
		exempt[instance] = true
		return
	}
	switch x := v.(type) {
	case map[string]any:
		if isEnvWrapper(x) {
			return
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			candidate := join(logical, key)
			if pathPrefix(candidate, paths) {
				walkRaw(x[key], candidate, join(instance, key), paths, setPrefixes, exempt)
			} else {
				walkRaw(x[key], logical, instance+"["+strconv.Quote(key)+"]", paths, setPrefixes, exempt)
			}
		}
	case []any:
		for i, item := range x {
			walkRaw(item, logical, fmt.Sprintf("%s[%d]", instance, i), paths, setPrefixes, exempt)
		}
	}
}

func scalarLiteral(v any) bool {
	switch x := v.(type) {
	case string:
		_, ref := config.ParseRef(x)
		return !ref
	case jsonNumber:
		return true
	case float64, float32, int, int64, bool:
		return true
	default:
		return false
	}
}

// jsonNumber is an interface alias avoided here; encoding/json decodes config numbers as float64.
type jsonNumber interface{ String() string }

func isEnvWrapper(v map[string]any) bool {
	if len(v) != 1 {
		return false
	}
	s, ok := v["env"].(string)
	return ok && s != ""
}
func contains(paths []string, wanted string) bool {
	i := sort.SearchStrings(paths, wanted)
	return i < len(paths) && paths[i] == wanted
}
func pathPrefix(prefix string, paths []string) bool {
	for _, p := range paths {
		if p == prefix || strings.HasPrefix(p, prefix+".") {
			return true
		}
	}
	return false
}
func underPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

// Redact replaces concrete sensitive non-exempt values with typed nulls.
func (s *Spec) Redact(v cty.Value) (cty.Value, []string, error) {
	return s.transform(v, transformRedact)
}

// Unknown replaces sensitive non-exempt values with typed unknowns for plans.
func (s *Spec) Unknown(v cty.Value) (cty.Value, error) {
	out, _, err := s.transform(v, transformUnknown)
	return out, err
}

// Mask nulls sensitive non-exempt values unconditionally for comparison.
func (s *Spec) Mask(v cty.Value) (cty.Value, error) {
	out, _, err := s.transform(v, transformMask)
	return out, err
}

type transformMode int

const (
	transformRedact transformMode = iota
	transformUnknown
	transformMask
)

func (s *Spec) transform(v cty.Value, mode transformMode) (cty.Value, []string, error) {
	var changed []string
	out, err := cty.Transform(v, func(path cty.Path, val cty.Value) (cty.Value, error) {
		full := PathString(path)
		if !contains(s.paths, stripIndices(full)) || contains(s.exempt, full) {
			return val, nil
		}
		switch mode {
		case transformUnknown:
			if val.IsNull() {
				return val, nil
			}
			return cty.UnknownVal(val.Type()), nil
		case transformMask:
			return cty.NullVal(val.Type()), nil
		default:
			if !val.IsKnown() || val.IsNull() {
				return val, nil
			}
			changed = append(changed, full)
			return cty.NullVal(val.Type()), nil
		}
	})
	sort.Strings(changed)
	return out, changed, err
}

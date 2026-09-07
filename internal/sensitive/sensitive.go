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
// index-insensitive; exemptions bind an individual raw-config instance to the
// exact authored scalar value.
type Spec struct {
	paths               []string
	exempt              map[string]any
	setPrefixes         []string
	allSetPrefixes      []string
	mapRecoveryPrefixes []string
	allMapPrefixes      []string
}

// Resolve is the single constructor used by persistence and planning paths.
func Resolve(block *provider.SchemaBlock, declared []string, rawCfg map[string]any) (*Spec, diag.Diagnostics) {
	ds := ValidateDeclared(block, declared)
	if ds.HasErrors() {
		return nil, ds
	}
	set := map[string]bool{}
	var setPrefixes, mapPrefixes []string
	walkSchema(block, "", set, &setPrefixes)
	walkSchemaMaps(block, "", &mapPrefixes)
	for _, path := range declared {
		set[path] = true
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	allSetPrefixes := sortedUnique(setPrefixes)
	allMapPrefixes := sortedUnique(mapPrefixes)
	setPrefixes = affectedSets(allSetPrefixes, paths)
	s := &Spec{
		paths:               paths,
		setPrefixes:         setPrefixes,
		allSetPrefixes:      allSetPrefixes,
		mapRecoveryPrefixes: affectedSensitiveMaps(allMapPrefixes, setPrefixes, paths),
		allMapPrefixes:      allMapPrefixes,
	}
	return s.Effective(rawCfg), ds
}

// ResolveWithPersisted applies current schema/config sensitivity plus every
// previously recorded path that still exists in the live schema. Planning,
// applying, and state-only deletion use this shared rule so sensitivity memory
// cannot disappear between phases.
func ResolveWithPersisted(block *provider.SchemaBlock, declared, persisted []string, rawCfg map[string]any) (*Spec, diag.Diagnostics) {
	recalled := make([]string, 0, len(persisted))
	for _, path := range persisted {
		if path != "" && schemaPathExists(block, strings.Split(path, ".")) {
			recalled = append(recalled, path)
		}
	}
	return Resolve(block, append(append([]string(nil), declared...), recalled...), rawCfg)
}

func affectedSets(allSetPrefixes, paths []string) []string {
	affected := make([]string, 0, len(allSetPrefixes))
	for _, prefix := range allSetPrefixes {
		for _, path := range paths {
			if path == prefix || strings.HasPrefix(path, prefix+".") || strings.HasPrefix(prefix, path+".") {
				affected = append(affected, prefix)
				break
			}
		}
	}
	return affected
}

func walkSchema(block *provider.SchemaBlock, prefix string, paths map[string]bool, setPrefixes *[]string) {
	if block == nil {
		return
	}
	walkAttributes(block.Attributes, prefix, paths, setPrefixes)
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

func walkAttributes(attrs map[string]*provider.Attr, prefix string, paths map[string]bool, setPrefixes *[]string) {
	for name, attr := range attrs {
		if attr == nil {
			continue
		}
		path := join(prefix, name)
		if attr.Sensitive {
			paths[path] = true
		}
		walkTypeSets(attr.Type, path, setPrefixes)
		if len(attr.NestedType) != 0 {
			walkAttributes(attr.NestedType, path, paths, setPrefixes)
		}
	}
}

// walkTypeSets discovers sets from the cty type independently of provider
// NestedType metadata. Protocol 5 has no NestedType representation, and flat
// protocol 6 attributes may also carry arbitrarily nested collection types.
func walkTypeSets(ty cty.Type, path string, setPrefixes *[]string) {
	switch {
	case ty.IsSetType():
		*setPrefixes = append(*setPrefixes, path)
		walkTypeSets(ty.ElementType(), path, setPrefixes)
	case ty.IsListType(), ty.IsMapType():
		walkTypeSets(ty.ElementType(), path, setPrefixes)
	case ty.IsTupleType():
		for _, elementType := range ty.TupleElementTypes() {
			walkTypeSets(elementType, path, setPrefixes)
		}
	case ty.IsObjectType():
		for name, attributeType := range ty.AttributeTypes() {
			walkTypeSets(attributeType, join(path, name), setPrefixes)
		}
	}
}

func walkSchemaMaps(block *provider.SchemaBlock, prefix string, mapPrefixes *[]string) {
	if block == nil {
		return
	}
	for name, attr := range block.Attributes {
		if attr != nil {
			walkTypeMaps(attr.Type, join(prefix, name), mapPrefixes)
		}
	}
	for name, nested := range block.Blocks {
		if nested == nil {
			continue
		}
		path := join(prefix, name)
		if nested.Nesting == "map" {
			*mapPrefixes = append(*mapPrefixes, path)
		}
		walkSchemaMaps(nested.Block, path, mapPrefixes)
	}
}

func walkTypeMaps(ty cty.Type, path string, mapPrefixes *[]string) {
	switch {
	case ty.IsMapType():
		*mapPrefixes = append(*mapPrefixes, path)
		walkTypeMaps(ty.ElementType(), path, mapPrefixes)
	case ty.IsListType(), ty.IsSetType():
		walkTypeMaps(ty.ElementType(), path, mapPrefixes)
	case ty.IsTupleType():
		for _, elementType := range ty.TupleElementTypes() {
			walkTypeMaps(elementType, path, mapPrefixes)
		}
	case ty.IsObjectType():
		for name, attributeType := range ty.AttributeTypes() {
			walkTypeMaps(attributeType, join(path, name), mapPrefixes)
		}
	}
}

func affectedSensitiveMaps(allMaps, affectedSetPrefixes, paths []string) []string {
	var candidates []string
	for _, mapPrefix := range allMaps {
		insideCapturedSet := false
		for _, setPrefix := range affectedSetPrefixes {
			if strings.HasPrefix(mapPrefix, setPrefix+".") {
				insideCapturedSet = true
				break
			}
		}
		if insideCapturedSet {
			continue
		}
		containsAffectedSet := false
		for _, setPrefix := range affectedSetPrefixes {
			if strings.HasPrefix(setPrefix, mapPrefix+".") {
				containsAffectedSet = true
				break
			}
		}
		if !containsAffectedSet {
			continue
		}
		for _, path := range paths {
			if mapPrefix == path || strings.HasPrefix(mapPrefix, path+".") {
				candidates = append(candidates, mapPrefix)
				break
			}
		}
	}
	candidates = sortedUnique(candidates)
	var outermost []string
	for _, candidate := range candidates {
		covered := false
		for _, existing := range outermost {
			if strings.HasPrefix(candidate, existing+".") {
				covered = true
				break
			}
		}
		if !covered {
			outermost = append(outermost, candidate)
		}
	}
	return outermost
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
func (s *Spec) ExemptInstances() []string {
	instances := make([]string, 0, len(s.exempt))
	for instance := range s.exempt {
		instances = append(instances, instance)
	}
	sort.Strings(instances)
	return instances
}

// Effective records per-instance exemptions from raw config syntax. The
// exemption exists only because the literal is already present in committed
// configuration. It must never be inferred from composed values: doing so
// would exempt referenced secrets (and future env wrappers, TC-054/#46).
func (s *Spec) Effective(rawCfg map[string]any) *Spec {
	out := &Spec{
		paths:               s.Paths(),
		exempt:              map[string]any{},
		setPrefixes:         append([]string(nil), s.setPrefixes...),
		allSetPrefixes:      append([]string(nil), s.allSetPrefixes...),
		mapRecoveryPrefixes: append([]string(nil), s.mapRecoveryPrefixes...),
		allMapPrefixes:      append([]string(nil), s.allMapPrefixes...),
	}
	if rawCfg == nil {
		return out
	}
	walkRaw(rawCfg, "", "", out.paths, out.setPrefixes, out.exempt)
	return out
}

func walkRaw(v any, logical, instance string, paths, setPrefixes []string, exempt map[string]any) {
	if scalarLiteral(v) && contains(paths, logical) && !underPrefix(logical, setPrefixes) {
		exempt[instance] = v
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

// Mask nulls sensitive non-exempt values outside identity-sensitive sets.
// Leaves inside such sets remain concrete so membership changes cannot vanish
// when distinct elements share the same public projection.
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
		logical := logicalPath(path)
		if mode == transformMask && (underPrefix(logical, s.setPrefixes) || prefixAtOrBelow(logical, s.setPrefixes)) {
			return val, nil
		}
		if !coveredBySensitivePath(logical, s.paths) {
			return val, nil
		}
		if literal, ok := s.exempt[full]; ok && literalMatches(val, literal) {
			return val, nil
		}
		ty := val.Type()
		composite := ty.IsObjectType() || ty.IsMapType() || ty.IsListType() || ty.IsTupleType() || ty.IsSetType()
		if ty.IsMapType() && mode != transformMask {
			switch mode {
			case transformUnknown:
				if val.IsNull() {
					return val, nil
				}
				return cty.UnknownVal(ty), nil
			default:
				if !val.IsKnown() || val.IsNull() {
					return val, nil
				}
				changed = append(changed, full)
				return cty.NullVal(ty), nil
			}
		}
		if composite && (underPrefix(logical, s.setPrefixes) || prefixAtOrBelow(logical, s.setPrefixes)) {
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

func coveredBySensitivePath(path string, paths []string) bool {
	for _, sensitivePath := range paths {
		if path == sensitivePath || strings.HasPrefix(path, sensitivePath+".") {
			return true
		}
	}
	return false
}

func prefixAtOrBelow(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == "" || path == prefix || strings.HasPrefix(prefix, path+".") {
			return true
		}
	}
	return false
}

func literalMatches(value cty.Value, literal any) bool {
	if !value.IsKnown() || value.IsNull() {
		return false
	}
	switch raw := literal.(type) {
	case string:
		return value.Type().Equals(cty.String) && value.AsString() == raw
	case bool:
		return value.Type().Equals(cty.Bool) && value.True() == raw
	case jsonNumber:
		expected, err := cty.ParseNumberVal(raw.String())
		return err == nil && value.Type().Equals(cty.Number) && value.RawEquals(expected)
	case float64:
		return value.Type().Equals(cty.Number) && value.RawEquals(cty.NumberFloatVal(raw))
	case float32:
		return value.Type().Equals(cty.Number) && value.RawEquals(cty.NumberFloatVal(float64(raw)))
	case int:
		return value.Type().Equals(cty.Number) && value.RawEquals(cty.NumberIntVal(int64(raw)))
	case int64:
		return value.Type().Equals(cty.Number) && value.RawEquals(cty.NumberIntVal(raw))
	default:
		return false
	}
}

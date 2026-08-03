package provider

// This file implements typeconv: EncodeDynamic/DecodeDynamic bridge cty
// values to the tfplugin6 DynamicValue wire encoding, and Compose converts
// raw JSON resource/provider config into schema-conforming cty values.

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// EncodeDynamic encodes v as a protocol DynamicValue. tchori always sends
// msgpack: it is the only encoding in the protocol that can carry unknown
// values (cty/msgpack encodes them as a msgpack extension).
func EncodeDynamic(v cty.Value, ty cty.Type) (*tfplugin6.DynamicValue, error) {
	b, err := msgpack.Marshal(v, ty.WithoutOptionalAttributesDeep())
	if err != nil {
		return nil, fmt.Errorf("msgpack encode: %w", err)
	}
	return &tfplugin6.DynamicValue{Msgpack: b}, nil
}

// DecodeMsgpack decodes msgpack at a deeply marker-free value type. The cty
// decoder can panic while aggregating malformed heterogeneous collections;
// issue #50 requires that provider data instead become an ordinary error.
func DecodeMsgpack(b []byte, ty cty.Type) (v cty.Value, err error) {
	ty = ty.WithoutOptionalAttributesDeep()
	defer func() {
		if recovered := recover(); recovered != nil {
			v = cty.NilVal
			err = fmt.Errorf("cty panic decoding %s: %v", ty.FriendlyName(), recovered)
		}
	}()
	return msgpack.Unmarshal(b, ty)
}

// DecodeJSON decodes cty JSON at a deeply marker-free value type. As with
// DecodeMsgpack, residual cty panics from malformed data are recovered and
// returned as errors rather than crashing plan or apply (issue #50).
func DecodeJSON(b []byte, ty cty.Type) (v cty.Value, err error) {
	ty = ty.WithoutOptionalAttributesDeep()
	defer func() {
		if recovered := recover(); recovered != nil {
			v = cty.NilVal
			err = fmt.Errorf("cty panic decoding %s: %v", ty.FriendlyName(), recovered)
		}
	}()
	return ctyjson.Unmarshal(b, ty)
}

// DecodeDynamic decodes a protocol DynamicValue as ty. Msgpack is preferred;
// Json is accepted as a fallback for providers that answer in JSON. A nil or
// empty DynamicValue decodes to a null value of ty.
func DecodeDynamic(dv *tfplugin6.DynamicValue, ty cty.Type) (cty.Value, error) {
	ty = ty.WithoutOptionalAttributesDeep()
	switch {
	case dv == nil:
		return cty.NullVal(ty), nil
	case len(dv.Msgpack) > 0:
		v, err := DecodeMsgpack(dv.Msgpack, ty)
		if err != nil {
			return cty.NilVal, fmt.Errorf("msgpack decode: %w", err)
		}
		return v, nil
	case len(dv.Json) > 0:
		v, err := DecodeJSON(dv.Json, ty)
		if err != nil {
			return cty.NilVal, fmt.Errorf("json decode: %w", err)
		}
		return v, nil
	default:
		return cty.NullVal(ty), nil
	}
}

// RefResolver resolves a whole-string ${type.name.attr} reference to a
// cty.Value. During planning the resolver may return unknown values for
// attributes that are not known until apply.
type RefResolver func(ref config.Ref) (cty.Value, diag.Diagnostics)

// EnvPolicy controls how Compose handles an unset variable referenced by an
// {"env":"VAR"} wrapper. EnvResolve, the zero value, requires the variable
// to be set and emits an error otherwise. EnvUnknownIfUnset resolves a set
// variable normally but composes an unset variable as an unknown string.
type EnvPolicy uint8

const (
	EnvResolve EnvPolicy = iota
	EnvUnknownIfUnset
)

// Compose converts raw JSON config into a cty.Value conforming to ty:
// missing optional/computed attrs -> null; whole-string refs (per
// config.ParseRef) -> resolve(); {"env":"VAR"} or {"env":["VAR_A","VAR_B"]}
// wrappers at string-typed attributes -> values governed by env; unexpected
// attributes -> error diagnostics naming the attribute. Reference-shaped
// strings that survive either raw conversion or post-composition resolution
// are hard errors under TC-048. Those diagnostics identify the attribute path
// only because Compose has no resource address in scope. Values returned by
// providers are not composed or scanned here.
func Compose(raw map[string]any, ty cty.Type, env EnvPolicy, resolve RefResolver) (cty.Value, diag.Diagnostics) {
	ty = ty.WithoutOptionalAttributesDeep()
	if !ty.IsObjectType() {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "internal error: Compose requires an object type", "got "+ty.FriendlyName())}
	}
	if raw == nil {
		raw = map[string]any{}
	}
	composed, ds := rawToCty("", raw, ty, env, resolve)
	if ds.HasErrors() {
		return composed, ds
	}
	for _, finding := range FindUnresolvedReferences(composed) {
		ds = append(ds, UnresolvedReferenceDiagnostic("", finding))
	}
	if ds.HasErrors() {
		return cty.NilVal, ds
	}
	return composed, ds
}

// rawToCty converts one raw JSON value into a cty.Value of exactly type ty,
// recursing generically driven by ty. path is the dotted attribute path used
// in diagnostics ("" at the root).
func rawToCty(path string, raw any, ty cty.Type, env EnvPolicy, resolve RefResolver) (cty.Value, diag.Diagnostics) {
	// JSON null is a valid value at any type.
	if raw == nil {
		return cty.NullVal(ty), nil
	}

	// A whole-string ${type.name.attr} reference is valid at any type: the
	// resolved value is converted to ty. config.ParseRef is the engine's
	// single reference grammar (the same regex ExtractRefs applies), so a
	// string is a reference here iff it also creates a graph edge.
	if s, ok := raw.(string); ok {
		if ref, isRef := config.ParseRef(s); isRef {
			return resolveRefValue(path, s, ref, ty, resolve)
		}
		if match, found := config.FindUnresolvedReference(s); found {
			finding := UnresolvedReferenceFinding{Path: path, Match: match}
			return cty.NilVal, diag.Diagnostics{UnresolvedReferenceDiagnostic("", finding)}
		}
	}

	// An {"env": "VAR"} or {"env": ["VAR_A", "VAR_B"]} wrapper is only
	// meaningful where a primitive is expected; at object/map/list types a
	// single-key "env" object is plain data. resolveEnvValue rejects primitive
	// types other than string.
	if m, ok := raw.(map[string]any); ok && ty.IsPrimitiveType() && isEnvWrapper(m) {
		return resolveEnvValue(path, m, ty, env)
	}

	switch {
	case ty.IsObjectType():
		m, ok := raw.(map[string]any)
		if !ok {
			return cty.NilVal, typeMismatch(path, raw, ty)
		}
		atys := ty.AttributeTypes()
		var diags diag.Diagnostics
		for _, k := range sortedKeys(m) {
			if _, known := atys[k]; !known {
				diags = append(diags, diag.Errorf("", fmt.Sprintf("unexpected attribute %q", joinPath(path, k)), "this attribute is not declared in the schema"))
			}
		}
		if diags.HasErrors() {
			return cty.NilVal, diags
		}
		names := make([]string, 0, len(atys))
		for name := range atys {
			names = append(names, name)
		}
		sort.Strings(names)
		attrs := make(map[string]cty.Value, len(atys))
		for _, name := range names {
			rv, has := m[name]
			if !has {
				attrs[name] = cty.NullVal(atys[name])
				continue
			}
			av, adiags := rawToCty(joinPath(path, name), rv, atys[name], env, resolve)
			diags = append(diags, adiags...)
			if adiags.HasErrors() {
				return cty.NilVal, diags
			}
			attrs[name] = av
		}
		return cty.ObjectVal(attrs), diags

	case ty.IsMapType():
		m, ok := raw.(map[string]any)
		if !ok {
			return cty.NilVal, typeMismatch(path, raw, ty)
		}
		ety := ty.ElementType()
		if len(m) == 0 {
			return cty.MapValEmpty(ety), nil
		}
		var diags diag.Diagnostics
		elems := make(map[string]cty.Value, len(m))
		for _, k := range sortedKeys(m) {
			ev, ediags := rawToCty(joinPath(path, k), m[k], ety, env, resolve)
			diags = append(diags, ediags...)
			if ediags.HasErrors() {
				return cty.NilVal, diags
			}
			elems[k] = ev
		}
		return cty.MapVal(elems), diags

	case ty.IsListType(), ty.IsSetType():
		l, ok := raw.([]any)
		if !ok {
			return cty.NilVal, typeMismatch(path, raw, ty)
		}
		ety := ty.ElementType()
		if len(l) == 0 {
			if ty.IsSetType() {
				return cty.SetValEmpty(ety), nil
			}
			return cty.ListValEmpty(ety), nil
		}
		var diags diag.Diagnostics
		elems := make([]cty.Value, 0, len(l))
		for i, rv := range l {
			ev, ediags := rawToCty(fmt.Sprintf("%s[%d]", path, i), rv, ety, env, resolve)
			diags = append(diags, ediags...)
			if ediags.HasErrors() {
				return cty.NilVal, diags
			}
			elems = append(elems, ev)
		}
		if ty.IsSetType() {
			return cty.SetVal(elems), diags
		}
		return cty.ListVal(elems), diags

	case ty == cty.String:
		s, ok := raw.(string)
		if !ok {
			return cty.NilVal, typeMismatch(path, raw, ty)
		}
		return cty.StringVal(s), nil

	case ty == cty.Bool:
		b, ok := raw.(bool)
		if !ok {
			return cty.NilVal, typeMismatch(path, raw, ty)
		}
		return cty.BoolVal(b), nil

	case ty == cty.Number:
		return numberToCty(path, raw)

	default:
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "unsupported attribute type",
			fmt.Sprintf("attribute %q has type %s, which tchori cannot compose (out of MVP scope)", path, ty.FriendlyName()))}
	}
}

// resolveRefValue calls the resolver for a parsed reference and converts the
// result to the expected attribute type.
func resolveRefValue(path, refStr string, ref config.Ref, ty cty.Type, resolve RefResolver) (cty.Value, diag.Diagnostics) {
	if resolve == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "reference not supported here",
			fmt.Sprintf("attribute %q: %s cannot be resolved in this context", path, refStr))}
	}
	v, diags := resolve(ref)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	cv, err := convert.Convert(v, ty)
	if err != nil {
		diags = append(diags, diag.Errorf("", "incompatible reference value",
			fmt.Sprintf("attribute %q: cannot use %s as %s: %s", path, refStr, ty.FriendlyName(), err)))
		return cty.NilVal, diags
	}
	return cv, diags
}

// resolveEnvValue reads an {"env": "VAR"} or
// {"env": ["VAR_A", "VAR_B"]} wrapper. Wrappers are only valid where a
// string is expected. Candidates are consulted in order and the first one
// that is set wins; an empty string counts as set. Policy controls the result
// when every candidate is unset.
func resolveEnvValue(path string, m map[string]any, ty cty.Type, env EnvPolicy) (cty.Value, diag.Diagnostics) {
	names, valid := envWrapperNames(m)
	if !valid {
		if values, ok := m["env"].([]any); ok {
			if len(values) == 0 {
				return cty.NilVal, diag.Diagnostics{diag.Errorf("", "invalid env wrapper",
					fmt.Sprintf("attribute %q: {\"env\": ...} requires at least one environment variable name", path))}
			}
			for i, value := range values {
				if _, ok := value.(string); !ok {
					return cty.NilVal, diag.Diagnostics{diag.Errorf("", "invalid env wrapper",
						fmt.Sprintf("attribute %q: {\"env\": ...} candidate at index %d must be a string", path, i))}
				}
			}
		}
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "invalid env wrapper",
			fmt.Sprintf("attribute %q has a malformed {\"env\": ...} wrapper", path))}
	}
	if ty != cty.String {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "invalid env wrapper",
			fmt.Sprintf("attribute %q: {\"env\": %s} is only valid where a string is expected, not %s",
				path, quotedNames(names), ty.FriendlyName()))}
	}
	for _, name := range names {
		if val, set := os.LookupEnv(name); set {
			return cty.StringVal(val), nil
		}
	}
	if env == EnvUnknownIfUnset {
		return cty.UnknownVal(cty.String), nil
	}
	detail := fmt.Sprintf("attribute %q: none of the candidate environment variables %s are set.\n", path, quotedNames(names)) +
		"These names come from the {\"env\": ...} wrapper in *.tchori.json; tchori defines no built-in or provider-specific environment variable names.\n" +
		"Export one of these variables, or add the name your environment already uses to the wrapper list."
	return cty.NilVal, diag.Diagnostics{diag.Errorf("", "environment variable not set", detail)}
}

// numberToCty converts the Go values encoding/json (and config loaders using
// json.Number) can produce for a JSON number.
func numberToCty(path string, raw any) (cty.Value, diag.Diagnostics) {
	switch n := raw.(type) {
	case float64:
		return cty.NumberFloatVal(n), nil
	case int:
		return cty.NumberIntVal(int64(n)), nil
	case int64:
		return cty.NumberIntVal(n), nil
	case json.Number:
		f, _, err := big.ParseFloat(n.String(), 10, 512, big.ToNearestEven)
		if err != nil {
			return cty.NilVal, typeMismatch(path, raw, cty.Number)
		}
		return cty.NumberVal(f), nil
	default:
		return cty.NilVal, typeMismatch(path, raw, cty.Number)
	}
}

// isEnvWrapper reports whether m has the single-key env-wrapper shape. The
// payload may be a string or a candidate list; resolveEnvValue diagnoses a
// malformed candidate list.
func isEnvWrapper(m map[string]any) bool {
	if len(m) != 1 {
		return false
	}
	switch m["env"].(type) {
	case string, []any:
		return true
	default:
		return false
	}
}

// envWrapperNames returns a well-formed wrapper's candidate names in config
// order. Its caller is responsible for diagnosing malformed candidate lists.
func envWrapperNames(m map[string]any) ([]string, bool) {
	switch value := m["env"].(type) {
	case string:
		return []string{value}, true
	case []any:
		if len(value) == 0 {
			return nil, false
		}
		names := make([]string, len(value))
		for i, candidate := range value {
			name, ok := candidate.(string)
			if !ok {
				return nil, false
			}
			names[i] = name
		}
		return names, true
	default:
		return nil, false
	}
}

func quotedNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}

func typeMismatch(path string, raw any, ty cty.Type) diag.Diagnostics {
	return diag.Diagnostics{diag.Errorf("", "type mismatch",
		fmt.Sprintf("attribute %q: cannot use JSON value of Go type %T where %s is expected", path, raw, ty.FriendlyName()))}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
	b, err := msgpack.Marshal(v, ty)
	if err != nil {
		return nil, fmt.Errorf("msgpack encode: %w", err)
	}
	return &tfplugin6.DynamicValue{Msgpack: b}, nil
}

// DecodeDynamic decodes a protocol DynamicValue as ty. Msgpack is preferred;
// Json is accepted as a fallback for providers that answer in JSON. A nil or
// empty DynamicValue decodes to a null value of ty.
func DecodeDynamic(dv *tfplugin6.DynamicValue, ty cty.Type) (cty.Value, error) {
	switch {
	case dv == nil:
		return cty.NullVal(ty), nil
	case len(dv.Msgpack) > 0:
		v, err := msgpack.Unmarshal(dv.Msgpack, ty)
		if err != nil {
			return cty.NilVal, fmt.Errorf("msgpack decode: %w", err)
		}
		return v, nil
	case len(dv.Json) > 0:
		v, err := ctyjson.Unmarshal(dv.Json, ty)
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

// Compose converts raw JSON config into a cty.Value conforming to ty:
// missing optional/computed attrs -> null; whole-string refs (per
// config.ParseRef) -> resolve(); {"env":"VAR"} wrappers -> string from
// environment, permitted only when allowEnv is true (error diag if the
// variable is unset; error diag when allowEnv is false — the spec allows env
// wrappers only in provider config, so callers pass true for provider config
// and false for resource configs); unexpected attributes -> error diag
// naming the attribute.
func Compose(raw map[string]any, ty cty.Type, allowEnv bool, resolve RefResolver) (cty.Value, diag.Diagnostics) {
	if !ty.IsObjectType() {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "internal error: Compose requires an object type", "got "+ty.FriendlyName())}
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return rawToCty("", raw, ty, allowEnv, resolve)
}

// rawToCty converts one raw JSON value into a cty.Value of exactly type ty,
// recursing generically driven by ty. path is the dotted attribute path used
// in diagnostics ("" at the root).
func rawToCty(path string, raw any, ty cty.Type, allowEnv bool, resolve RefResolver) (cty.Value, diag.Diagnostics) {
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
	}

	// An {"env": "VAR"} wrapper is only meaningful where a primitive is
	// expected; at object/map types a single-key "env" object is plain data.
	// The spec allows wrappers only in provider config, so resource-config
	// composition (allowEnv=false) rejects them outright.
	if m, ok := raw.(map[string]any); ok && ty.IsPrimitiveType() && isEnvWrapper(m) {
		if !allowEnv {
			return cty.NilVal, diag.Diagnostics{diag.Errorf("", "env wrappers are only allowed in provider config",
				fmt.Sprintf("attribute %q uses an {\"env\": ...} wrapper, which is valid only inside a provider's config block", path))}
		}
		return resolveEnvValue(path, m, ty)
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
			av, adiags := rawToCty(joinPath(path, name), rv, atys[name], allowEnv, resolve)
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
			ev, ediags := rawToCty(joinPath(path, k), m[k], ety, allowEnv, resolve)
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
			ev, ediags := rawToCty(fmt.Sprintf("%s[%d]", path, i), rv, ety, allowEnv, resolve)
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

// resolveEnvValue reads an {"env": "VAR"} wrapper. Wrappers are only valid
// where a string is expected; the variable must be set (empty string is set).
func resolveEnvValue(path string, m map[string]any, ty cty.Type) (cty.Value, diag.Diagnostics) {
	name, _ := m["env"].(string)
	if ty != cty.String {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "invalid env wrapper",
			fmt.Sprintf("attribute %q: {\"env\": %q} is only valid where a string is expected, not %s", path, name, ty.FriendlyName()))}
	}
	val, set := os.LookupEnv(name)
	if !set {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "environment variable not set",
			fmt.Sprintf("attribute %q reads environment variable %q, which is not set", path, name))}
	}
	return cty.StringVal(val), nil
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

// isEnvWrapper reports whether m has the exact {"env": "<string>"} shape.
func isEnvWrapper(m map[string]any) bool {
	if len(m) != 1 {
		return false
	}
	_, ok := m["env"].(string)
	return ok
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

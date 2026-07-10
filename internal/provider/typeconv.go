package provider

// This file implements typeconv: EncodeDynamic/DecodeDynamic bridge cty
// values to the tfplugin6 DynamicValue wire encoding, and Compose (added
// below in this task) converts raw JSON config into schema-conforming cty
// values.

import (
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"

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

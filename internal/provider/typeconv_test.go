package provider

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

func TestEncodeDynamicMsgpackRoundTripUnknown(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{
		"name": cty.String,
		"id":   cty.String,
	})
	v := cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("web"),
		"id":   cty.UnknownVal(cty.String), // computed, unknown until apply
	})

	dv, err := EncodeDynamic(v, ty)
	if err != nil {
		t.Fatalf("EncodeDynamic: %v", err)
	}
	if len(dv.Msgpack) == 0 {
		t.Fatal("EncodeDynamic: Msgpack field is empty; msgpack must always be produced")
	}
	if len(dv.Json) != 0 {
		t.Fatalf("EncodeDynamic: Json field must stay empty, got %q", dv.Json)
	}

	got, err := DecodeDynamic(dv, ty)
	if err != nil {
		t.Fatalf("DecodeDynamic: %v", err)
	}
	if !got.RawEquals(v) {
		t.Fatalf("round-trip mismatch:\ngot  %#v\nwant %#v", got, v)
	}
	if got.GetAttr("id").IsKnown() {
		t.Fatal("id came back known; want the unknown to survive the round-trip")
	}
}

func TestDecodeDynamicJSONFallback(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"name": cty.String})
	dv := &tfplugin6.DynamicValue{Json: []byte(`{"name":"web"}`)}

	got, err := DecodeDynamic(dv, ty)
	if err != nil {
		t.Fatalf("DecodeDynamic: %v", err)
	}
	want := cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("web")})
	if !got.RawEquals(want) {
		t.Fatalf("DecodeDynamic(json) = %#v, want %#v", got, want)
	}
}

func TestDecodeDynamicNilAndEmpty(t *testing.T) {
	for name, dv := range map[string]*tfplugin6.DynamicValue{
		"nil":   nil,
		"empty": {},
	} {
		got, err := DecodeDynamic(dv, cty.String)
		if err != nil {
			t.Fatalf("%s: DecodeDynamic: %v", name, err)
		}
		if !got.RawEquals(cty.NullVal(cty.String)) {
			t.Fatalf("%s: DecodeDynamic = %#v, want null string", name, got)
		}
	}
}

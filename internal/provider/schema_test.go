package provider

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// TestBlockFromProtoNestedType guards issue #7: attributes carrying
// tfplugin6.Schema_Attribute.NestedType (the wire nested_type mechanism)
// must convert to the equivalent cty type instead of being rejected, for
// every nesting mode, including a nested_type attribute nested inside
// another nested_type attribute, and honoring optional-attribute semantics
// so a config that omits an optional nested attribute (or the whole nested
// value) still composes cleanly against the resulting cty.Type.
func TestBlockFromProtoNestedType(t *testing.T) {
	tests := []struct {
		name string
		attr *tfplugin6.Schema_Attribute
		want cty.Type
	}{
		{
			name: "single nesting mode converts to an object, required/optional preserved",
			attr: &tfplugin6.Schema_Attribute{
				Name:     "settings",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_SINGLE,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "flag", Type: []byte(`"bool"`), Optional: true},
						{Name: "label", Type: []byte(`"string"`), Required: true},
					},
				},
			},
			want: cty.ObjectWithOptionalAttrs(map[string]cty.Type{
				"flag":  cty.Bool,
				"label": cty.String,
			}, []string{"flag"}),
		},
		{
			name: "list nesting mode converts to list(object)",
			attr: &tfplugin6.Schema_Attribute{
				Name:     "records",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_LIST,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "value", Type: []byte(`"string"`), Optional: true},
					},
				},
			},
			want: cty.List(cty.ObjectWithOptionalAttrs(map[string]cty.Type{
				"value": cty.String,
			}, []string{"value"})),
		},
		{
			name: "set nesting mode converts to set(object)",
			attr: &tfplugin6.Schema_Attribute{
				Name:     "records",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_SET,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "value", Type: []byte(`"string"`), Required: true},
					},
				},
			},
			want: cty.Set(cty.Object(map[string]cty.Type{
				"value": cty.String,
			})),
		},
		{
			name: "map nesting mode converts to map(object)",
			attr: &tfplugin6.Schema_Attribute{
				Name:     "records",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_MAP,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "value", Type: []byte(`"string"`), Optional: true},
					},
				},
			},
			want: cty.Map(cty.ObjectWithOptionalAttrs(map[string]cty.Type{
				"value": cty.String,
			}, []string{"value"})),
		},
		{
			name: "nested_type nested inside nested_type converts arbitrarily deep",
			attr: &tfplugin6.Schema_Attribute{
				Name:     "data",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_SINGLE,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "kind", Type: []byte(`"string"`), Required: true},
						{
							Name:     "detail",
							Optional: true,
							NestedType: &tfplugin6.Schema_Object{
								Nesting: tfplugin6.Schema_Object_LIST,
								Attributes: []*tfplugin6.Schema_Attribute{
									{Name: "note", Type: []byte(`"string"`), Optional: true},
								},
							},
						},
					},
				},
			},
			want: cty.ObjectWithOptionalAttrs(map[string]cty.Type{
				"kind": cty.String,
				"detail": cty.List(cty.ObjectWithOptionalAttrs(map[string]cty.Type{
					"note": cty.String,
				}, []string{"note"})),
			}, []string{"detail"}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			block, err := blockFromProto(&tfplugin6.Schema_Block{
				Attributes: []*tfplugin6.Schema_Attribute{tc.attr},
			})
			if err != nil {
				t.Fatalf("blockFromProto: %v", err)
			}
			a := block.Attributes[tc.attr.Name]
			if a == nil {
				t.Fatalf("attribute %q missing from converted block", tc.attr.Name)
			}
			if !a.Type.Equals(tc.want) {
				t.Errorf("attribute %q type =\n  %#v\nwant\n  %#v", tc.attr.Name, a.Type, tc.want)
			}
			if a.Required != tc.attr.Required || a.Optional != tc.attr.Optional || a.Computed != tc.attr.Computed {
				t.Errorf("attribute %q required/optional/computed = %v/%v/%v, want %v/%v/%v",
					tc.attr.Name, a.Required, a.Optional, a.Computed,
					tc.attr.Required, tc.attr.Optional, tc.attr.Computed)
			}
		})
	}
}

// TestBlockFromProtoNestedTypeOmittedComposesToNull confirms the concrete
// acceptance shape from issue #7: a config that leaves a nested_type
// attribute entirely unset must compose to a null value of that attribute's
// converted type, and Compose must accept it without error (the whole point
// of marking non-required nested attributes optional).
func TestBlockFromProtoNestedTypeOmittedComposesToNull(t *testing.T) {
	block, err := blockFromProto(&tfplugin6.Schema_Block{
		Attributes: []*tfplugin6.Schema_Attribute{
			{Name: "name", Type: []byte(`"string"`), Required: true},
			{
				Name:     "settings",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_SINGLE,
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "flag", Type: []byte(`"bool"`), Optional: true},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("blockFromProto: %v", err)
	}
	ty := block.ImpliedType()

	v, ds := Compose(map[string]any{"name": "demo"}, ty, false, nil)
	if ds.HasErrors() {
		t.Fatalf("Compose: %+v", ds)
	}
	settings := v.GetAttr("settings")
	if !settings.IsNull() {
		t.Errorf("settings = %#v, want null (omitted from config)", settings)
	}
	wantSettingsType := cty.ObjectWithOptionalAttrs(map[string]cty.Type{"flag": cty.Bool}, []string{"flag"})
	if !settings.Type().Equals(wantSettingsType) {
		t.Errorf("settings null value type = %#v, want %#v", settings.Type(), wantSettingsType)
	}
}

// TestBlockFromProtoUnknownNestingMode guards the boundary the tolerate-
// until-used machinery (issue #5) must keep covering: nested_type support
// itself does not mean every nested shape converts — a nesting mode this
// engine does not recognize (not SINGLE/LIST/SET/MAP) is still a genuinely
// unconvertible schema and must return an error (not a guess), naming both
// the attribute and "nested_type" so it is recognizable as this class of
// failure to the tolerate-until-used caller in Schemas().
func TestBlockFromProtoUnknownNestingMode(t *testing.T) {
	_, err := blockFromProto(&tfplugin6.Schema_Block{
		Attributes: []*tfplugin6.Schema_Attribute{
			{
				Name:     "settings",
				Optional: true,
				NestedType: &tfplugin6.Schema_Object{
					Nesting: tfplugin6.Schema_Object_NestingMode(99),
					Attributes: []*tfplugin6.Schema_Attribute{
						{Name: "flag", Type: []byte(`"bool"`), Optional: true},
					},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("blockFromProto succeeded for an unrecognized nesting mode, want error")
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("error = %q, want it to name the attribute %q", err, "settings")
	}
	if !strings.Contains(err.Error(), "nested_type") {
		t.Errorf("error = %q, want it to mention nested_type", err)
	}
}

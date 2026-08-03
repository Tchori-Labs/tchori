package provider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestFindUnresolvedReferences(t *testing.T) {
	tests := []struct {
		name  string
		value cty.Value
		want  []UnresolvedReferenceFinding
	}{
		{
			name: "clean object",
			value: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("clean"),
			}),
		},
		{
			name: "nested object map list and tuple",
			value: cty.ObjectVal(map[string]cty.Value{
				"nested": cty.ObjectVal(map[string]cty.Value{
					"name": cty.StringVal("prefix-${a.b.c}"),
				}),
				"tags": cty.MapVal(map[string]cty.Value{
					"content": cty.StringVal("${d.e.f}.suffix"),
				}),
				"items": cty.ListVal([]cty.Value{cty.StringVal("${g.h.i}-list")}),
				"tuple": cty.TupleVal([]cty.Value{cty.StringVal("${j.k.l}-tuple")}),
			}),
			want: []UnresolvedReferenceFinding{
				{Path: "items[0]", Match: "${g.h.i}"},
				{Path: "nested.name", Match: "${a.b.c}"},
				{Path: `tags["content"]`, Match: "${d.e.f}"},
				{Path: "tuple[0]", Match: "${j.k.l}"},
			},
		},
		{
			name: "unknown and null strings and collections",
			value: cty.ObjectVal(map[string]cty.Value{
				"unknown":         cty.UnknownVal(cty.String),
				"null":            cty.NullVal(cty.String),
				"unknown_map":     cty.UnknownVal(cty.Map(cty.String)),
				"null_collection": cty.NullVal(cty.List(cty.String)),
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindUnresolvedReferences(tt.value)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FindUnresolvedReferences() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestFindUnresolvedReferencesMasksSetElement(t *testing.T) {
	const fullValue = "secret-prefix-${a.b.c}-secret-suffix"
	value := cty.ObjectVal(map[string]cty.Value{
		"values": cty.SetVal([]cty.Value{cty.StringVal(fullValue)}),
	})

	got := FindUnresolvedReferences(value)
	want := []UnresolvedReferenceFinding{{Path: "values[<set element>]", Match: "${a.b.c}"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindUnresolvedReferences() = %#v, want %#v", got, want)
	}
	d := UnresolvedReferenceDiagnostic("thing.example", got[0])
	printed := fmt.Sprintf("%+v", got) + fmt.Sprintf("%+v", d)
	if strings.Contains(printed, fullValue) {
		t.Fatalf("finding or diagnostic leaked full set element %q: %s", fullValue, printed)
	}
}

func TestFindUnresolvedReferencesSorted(t *testing.T) {
	value := cty.ObjectVal(map[string]cty.Value{
		"z": cty.StringVal("${z.y.x}"),
		"a": cty.StringVal("${a.b.c}"),
	})
	got := FindUnresolvedReferences(value)
	want := []UnresolvedReferenceFinding{
		{Path: "a", Match: "${a.b.c}"},
		{Path: "z", Match: "${z.y.x}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindUnresolvedReferences() = %#v, want %#v", got, want)
	}
}

func TestUnresolvedReferenceDiagnosticAddress(t *testing.T) {
	finding := UnresolvedReferenceFinding{Path: `tags["content"]`, Match: "${a.b.c}"}
	withAddress := UnresolvedReferenceDiagnostic("thing.example", finding)
	withoutAddress := UnresolvedReferenceDiagnostic("", finding)
	if withAddress.Address != "thing.example" || withoutAddress.Address != "" {
		t.Fatalf("addresses = (%q, %q), want (%q, empty)", withAddress.Address, withoutAddress.Address, "thing.example")
	}
	for _, d := range []struct {
		name    string
		summary string
		detail  string
	}{
		{name: "addressed", summary: withAddress.Summary, detail: withAddress.Detail},
		{name: "unaddressed", summary: withoutAddress.Summary, detail: withoutAddress.Detail},
	} {
		if d.summary != "unresolved reference" || !strings.Contains(d.detail, finding.Path) || !strings.Contains(d.detail, finding.Match) {
			t.Errorf("%s diagnostic = summary %q detail %q", d.name, d.summary, d.detail)
		}
	}
}

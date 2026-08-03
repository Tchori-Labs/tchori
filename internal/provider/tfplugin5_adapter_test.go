package provider

import (
	"testing"

	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin5"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

func TestDynamicValue5to6(t *testing.T) {
	if got := dynamicValue5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin5.DynamicValue{Msgpack: []byte{0x01, 0x02}, Json: []byte(`{"a":1}`)}
	got := dynamicValue5to6(in)
	if got == nil {
		t.Fatal("got nil, want non-nil")
	}
	if string(got.Msgpack) != string(in.Msgpack) || string(got.Json) != string(in.Json) {
		t.Fatalf("bytes not copied verbatim: got %+v, want %+v", got, in)
	}
}

func TestDynamicValue6to5(t *testing.T) {
	if got := dynamicValue6to5(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin6.DynamicValue{Msgpack: []byte{0xAA}, Json: nil}
	got := dynamicValue6to5(in)
	if got == nil {
		t.Fatal("got nil, want non-nil")
	}
	if string(got.Msgpack) != string(in.Msgpack) || got.Json != nil {
		t.Fatalf("bytes not copied verbatim: got %+v, want %+v", got, in)
	}
}

func TestDiagnosticSeverity5to6(t *testing.T) {
	cases := []struct {
		name string
		in   tfplugin5.Diagnostic_Severity
		want tfplugin6.Diagnostic_Severity
	}{
		{"warning", tfplugin5.Diagnostic_WARNING, tfplugin6.Diagnostic_WARNING},
		{"error", tfplugin5.Diagnostic_ERROR, tfplugin6.Diagnostic_ERROR},
		{"invalid fails closed to error", tfplugin5.Diagnostic_INVALID, tfplugin6.Diagnostic_ERROR},
		{"unknown severity fails closed to error", tfplugin5.Diagnostic_Severity(99), tfplugin6.Diagnostic_ERROR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diagnosticSeverity5to6(tc.in); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiagnostic5to6(t *testing.T) {
	if got := diagnostic5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin5.Diagnostic{
		Severity: tfplugin5.Diagnostic_WARNING,
		Summary:  "summary",
		Detail:   "detail",
		Attribute: &tfplugin5.AttributePath{
			Steps: []*tfplugin5.AttributePath_Step{
				{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "name"}},
			},
		},
	}
	got := diagnostic5to6(in)
	if got.Severity != tfplugin6.Diagnostic_WARNING || got.Summary != "summary" || got.Detail != "detail" {
		t.Fatalf("fields not converted: %+v", got)
	}
	if got.Attribute == nil || len(got.Attribute.Steps) != 1 {
		t.Fatalf("attribute path not converted: %+v", got.Attribute)
	}
}

func TestDiagnostics5to6(t *testing.T) {
	if got := diagnostics5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := []*tfplugin5.Diagnostic{
		nil, // dropped
		{Severity: tfplugin5.Diagnostic_ERROR, Summary: "one"},
		{Severity: tfplugin5.Diagnostic_WARNING, Summary: "two"},
	}
	got := diagnostics5to6(in)
	if len(got) != 2 {
		t.Fatalf("got %d diagnostics, want 2 (nil dropped): %+v", len(got), got)
	}
	if got[0].Summary != "one" || got[1].Summary != "two" {
		t.Fatalf("order/content not preserved: %+v", got)
	}
}

func TestAttributePathStep5to6(t *testing.T) {
	if got := attributePathStep5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	cases := []struct {
		name string
		in   *tfplugin5.AttributePath_Step
	}{
		{"attribute name", &tfplugin5.AttributePath_Step{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "attr"}}},
		{"element key string", &tfplugin5.AttributePath_Step{Selector: &tfplugin5.AttributePath_Step_ElementKeyString{ElementKeyString: "key"}}},
		{"element key int", &tfplugin5.AttributePath_Step{Selector: &tfplugin5.AttributePath_Step_ElementKeyInt{ElementKeyInt: 7}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attributePathStep5to6(tc.in)
			switch sel := tc.in.Selector.(type) {
			case *tfplugin5.AttributePath_Step_AttributeName:
				gs, ok := got.Selector.(*tfplugin6.AttributePath_Step_AttributeName)
				if !ok || gs.AttributeName != sel.AttributeName {
					t.Fatalf("got %+v, want AttributeName=%q", got, sel.AttributeName)
				}
			case *tfplugin5.AttributePath_Step_ElementKeyString:
				gs, ok := got.Selector.(*tfplugin6.AttributePath_Step_ElementKeyString)
				if !ok || gs.ElementKeyString != sel.ElementKeyString {
					t.Fatalf("got %+v, want ElementKeyString=%q", got, sel.ElementKeyString)
				}
			case *tfplugin5.AttributePath_Step_ElementKeyInt:
				gs, ok := got.Selector.(*tfplugin6.AttributePath_Step_ElementKeyInt)
				if !ok || gs.ElementKeyInt != sel.ElementKeyInt {
					t.Fatalf("got %+v, want ElementKeyInt=%d", got, sel.ElementKeyInt)
				}
			}
		})
	}
}

func TestAttributePath5to6(t *testing.T) {
	if got := attributePath5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin5.AttributePath{Steps: []*tfplugin5.AttributePath_Step{
		{Selector: &tfplugin5.AttributePath_Step_AttributeName{AttributeName: "tags"}},
		{Selector: &tfplugin5.AttributePath_Step_ElementKeyString{ElementKeyString: "env"}},
	}}
	got := attributePath5to6(in)
	if len(got.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(got.Steps))
	}
	if dottedPath(got) != "tags.env" {
		t.Fatalf("dottedPath(got) = %q, want %q", dottedPath(got), "tags.env")
	}
}

func TestSchemaNestingMode5to6(t *testing.T) {
	cases := []struct {
		in   tfplugin5.Schema_NestedBlock_NestingMode
		want tfplugin6.Schema_NestedBlock_NestingMode
	}{
		{tfplugin5.Schema_NestedBlock_SINGLE, tfplugin6.Schema_NestedBlock_SINGLE},
		{tfplugin5.Schema_NestedBlock_LIST, tfplugin6.Schema_NestedBlock_LIST},
		{tfplugin5.Schema_NestedBlock_SET, tfplugin6.Schema_NestedBlock_SET},
		{tfplugin5.Schema_NestedBlock_MAP, tfplugin6.Schema_NestedBlock_MAP},
		{tfplugin5.Schema_NestedBlock_GROUP, tfplugin6.Schema_NestedBlock_GROUP},
		{tfplugin5.Schema_NestedBlock_INVALID, tfplugin6.Schema_NestedBlock_INVALID},
		{tfplugin5.Schema_NestedBlock_NestingMode(99), tfplugin6.Schema_NestedBlock_INVALID},
	}
	for _, tc := range cases {
		if got := schemaNestingMode5to6(tc.in); got != tc.want {
			t.Fatalf("nesting mode %v: got %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSchemaAttribute5to6(t *testing.T) {
	if got := schemaAttribute5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin5.Schema_Attribute{
		Name:               "name",
		Type:               []byte(`"string"`),
		Description:        "desc",
		Required:           true,
		Optional:           false,
		Computed:           true,
		Sensitive:          true,
		DescriptionKind:    tfplugin5.StringKind_MARKDOWN,
		Deprecated:         true,
		WriteOnly:          true,
		DeprecationMessage: "deprecated msg",
	}
	got := schemaAttribute5to6(in)
	if got.Name != in.Name || string(got.Type) != string(in.Type) || got.Description != in.Description ||
		got.Required != in.Required || got.Optional != in.Optional || got.Computed != in.Computed ||
		got.Sensitive != in.Sensitive || got.DescriptionKind != tfplugin6.StringKind_MARKDOWN ||
		got.Deprecated != in.Deprecated || got.WriteOnly != in.WriteOnly ||
		got.DeprecationMessage != in.DeprecationMessage {
		t.Fatalf("fields not converted verbatim: got %+v, want %+v", got, in)
	}
	if got.NestedType != nil {
		t.Fatalf("NestedType must stay nil for protocol-5 attributes: got %+v", got.NestedType)
	}
}

func TestSchemaBlock5to6_AllNestingModes(t *testing.T) {
	if got := schemaBlock5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin5.Schema_Block{
		Version: 3,
		Attributes: []*tfplugin5.Schema_Attribute{
			{Name: "id", Type: []byte(`"string"`), Computed: true},
		},
		BlockTypes: []*tfplugin5.Schema_NestedBlock{
			{TypeName: "single_blk", Nesting: tfplugin5.Schema_NestedBlock_SINGLE, Block: &tfplugin5.Schema_Block{
				Attributes: []*tfplugin5.Schema_Attribute{{Name: "a", Type: []byte(`"string"`)}},
			}},
			{TypeName: "list_blk", Nesting: tfplugin5.Schema_NestedBlock_LIST, Block: &tfplugin5.Schema_Block{}},
			{TypeName: "set_blk", Nesting: tfplugin5.Schema_NestedBlock_SET, Block: &tfplugin5.Schema_Block{}},
			{TypeName: "map_blk", Nesting: tfplugin5.Schema_NestedBlock_MAP, Block: &tfplugin5.Schema_Block{}},
			{TypeName: "group_blk", Nesting: tfplugin5.Schema_NestedBlock_GROUP, Block: &tfplugin5.Schema_Block{}},
		},
		Description:        "block desc",
		DescriptionKind:    tfplugin5.StringKind_PLAIN,
		Deprecated:         false,
		DeprecationMessage: "",
	}
	got := schemaBlock5to6(in)
	if got.Version != 3 || len(got.Attributes) != 1 || got.Attributes[0].Name != "id" {
		t.Fatalf("attributes not converted: %+v", got)
	}
	if len(got.BlockTypes) != 5 {
		t.Fatalf("got %d block types, want 5", len(got.BlockTypes))
	}
	wantNesting := map[string]tfplugin6.Schema_NestedBlock_NestingMode{
		"single_blk": tfplugin6.Schema_NestedBlock_SINGLE,
		"list_blk":   tfplugin6.Schema_NestedBlock_LIST,
		"set_blk":    tfplugin6.Schema_NestedBlock_SET,
		"map_blk":    tfplugin6.Schema_NestedBlock_MAP,
		"group_blk":  tfplugin6.Schema_NestedBlock_GROUP,
	}
	for _, nb := range got.BlockTypes {
		want, ok := wantNesting[nb.TypeName]
		if !ok {
			t.Fatalf("unexpected block type %q", nb.TypeName)
		}
		if nb.Nesting != want {
			t.Fatalf("block %q: got nesting %v, want %v", nb.TypeName, nb.Nesting, want)
		}
	}
	// The single_blk's inner attribute proves recursion.
	for _, nb := range got.BlockTypes {
		if nb.TypeName == "single_blk" {
			if len(nb.Block.Attributes) != 1 || nb.Block.Attributes[0].Name != "a" {
				t.Fatalf("nested block attributes not converted: %+v", nb.Block)
			}
		}
	}
}

func TestSchema5to6(t *testing.T) {
	if got := schema5to6(nil); got != nil {
		t.Fatalf("nil input: got %v, want nil", got)
	}
	in := &tfplugin5.Schema{Version: 2, Block: &tfplugin5.Schema_Block{Version: 2}}
	got := schema5to6(in)
	if got.Version != 2 || got.Block == nil || got.Block.Version != 2 {
		t.Fatalf("schema not converted: %+v", got)
	}
}

func TestProtocol5SensitiveAttributePropagation(t *testing.T) {
	converted := schemaAttribute5to6(&tfplugin5.Schema_Attribute{Name: "secret", Type: []byte(`"string"`), Computed: true, Sensitive: true})
	if converted == nil || !converted.Sensitive {
		t.Fatal("protocol-5 adapter dropped Sensitive flag")
	}
	block, err := blockFromProto(&tfplugin6.Schema_Block{Attributes: []*tfplugin6.Schema_Attribute{converted}})
	if err != nil {
		t.Fatal(err)
	}
	if !block.Attributes["secret"].Sensitive {
		t.Fatal("adapted sensitivity did not reach provider Attr")
	}
}

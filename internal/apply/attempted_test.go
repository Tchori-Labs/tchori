package apply

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/provider"
)

func attemptedBlock() *provider.SchemaBlock {
	return &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"name":    {Type: cty.String, Optional: true},
		"secret":  {Type: cty.String, Optional: true, Sensitive: true},
		"tags":    {Type: cty.Map(cty.String), Optional: true},
		"items":   {Type: cty.List(cty.String), Optional: true},
		"members": {Type: cty.Set(cty.String), Optional: true},
	}, Blocks: map[string]*provider.NestedBlock{}}
}

func attemptedRoot(name, secret, tags, items, members cty.Value) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"name": name, "secret": secret, "tags": tags, "items": items, "members": members,
	})
}

func attemptedDefaults() (cty.Value, cty.Value, cty.Value) {
	return cty.NullVal(cty.String), cty.MapValEmpty(cty.String), cty.ListValEmpty(cty.String)
}

func TestAttemptedChangeSummaryNoDiffAndNullPrior(t *testing.T) {
	nullString, emptyMap, emptyList := attemptedDefaults()
	value := attemptedRoot(cty.StringVal("same"), nullString, emptyMap, emptyList, cty.SetValEmpty(cty.String))
	if ds := attemptedChangeSummary("test.x", "update", attemptedBlock(), value, value); ds != nil {
		t.Fatalf("no-diff diagnostics = %#v", ds)
	}
	if ds := attemptedChangeSummary("test.x", "create", attemptedBlock(), cty.NullVal(value.Type()), value); ds != nil {
		t.Fatalf("create diagnostics = %#v", ds)
	}
	if ds := attemptedChangeSummary("test.x", "update", attemptedBlock(), cty.UnknownVal(value.Type()), value); ds != nil {
		t.Fatalf("unknown-prior diagnostics = %#v", ds)
	}
}

func TestAttemptedChangeSummaryScalarNullAndUnknown(t *testing.T) {
	nullString, emptyMap, emptyList := attemptedDefaults()
	base := attemptedRoot(cty.StringVal("before"), nullString, emptyMap, emptyList, cty.SetValEmpty(cty.String))
	for _, tc := range []struct {
		name  string
		after cty.Value
		want  string
	}{
		{"scalar", cty.StringVal("after"), `name: "before" -> "after"`},
		{"value to null", nullString, `name: "before" -> null`},
		{"unknown", cty.UnknownVal(cty.String), `name: "before" -> (unknown value)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			planned := attemptedRoot(tc.after, nullString, emptyMap, emptyList, cty.SetValEmpty(cty.String))
			ds := attemptedChangeSummary("test.x", "update", attemptedBlock(), base, planned)
			if len(ds) != 1 || ds[0].Summary != "attempted change" || ds[0].Address != "test.x" || !strings.Contains(ds[0].Detail, tc.want) {
				t.Fatalf("diagnostics = %#v", ds)
			}
		})
	}

	beforeNull := attemptedRoot(nullString, nullString, emptyMap, emptyList, cty.SetValEmpty(cty.String))
	afterValue := attemptedRoot(cty.StringVal("set"), nullString, emptyMap, emptyList, cty.SetValEmpty(cty.String))
	ds := attemptedChangeSummary("test.x", "update", attemptedBlock(), beforeNull, afterValue)
	if len(ds) != 1 || !strings.Contains(ds[0].Detail, `name: null -> "set"`) {
		t.Fatalf("null-to-value diagnostics = %#v", ds)
	}
}

func TestAttemptedChangesMapKeyUnionAndEmptyMaps(t *testing.T) {
	before := cty.MapVal(map[string]cty.Value{
		"removed": cty.StringVal("old"),
		"changed": cty.StringVal("before"),
	})
	after := cty.MapVal(map[string]cty.Value{
		"added":   cty.StringVal("new"),
		"changed": cty.StringVal("after"),
	})
	var changes []attemptedChange
	attemptedChanges(cty.Path{cty.GetAttrStep{Name: "tags"}}, before, after, &changes)
	if len(changes) != 3 {
		t.Fatalf("changes = %#v", changes)
	}

	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{"tags": {Type: cty.Map(cty.String), Optional: true}}, Blocks: map[string]*provider.NestedBlock{}}
	root := func(v cty.Value) cty.Value { return cty.ObjectVal(map[string]cty.Value{"tags": v}) }
	ds := attemptedChangeSummary("test.x", "update", block, root(before), root(after))
	for _, want := range []string{
		`tags["added"]: null -> "new"`,
		`tags["changed"]: "before" -> "after"`,
		`tags["removed"]: "old" -> null`,
	} {
		if len(ds) != 1 || !strings.Contains(ds[0].Detail, want) {
			t.Fatalf("detail = %#v; want %q", ds, want)
		}
	}

	// Exercise an empty map on either side without constructing cty.MapVal
	// from an empty element map or calling LengthInt on an invalid value.
	empty := cty.MapValEmpty(cty.String)
	for _, pair := range [][2]cty.Value{{empty, after}, {before, empty}} {
		changes = nil
		attemptedChanges(nil, pair[0], pair[1], &changes)
		if len(changes) == 0 {
			t.Fatalf("empty-map pair produced no changes: %#v", pair)
		}
	}
}

func TestAttemptedChangesCollectionsComparedWholesale(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after cty.Value
	}{
		{"list", cty.ListVal([]cty.Value{cty.StringVal("a")}), cty.ListVal([]cty.Value{cty.StringVal("b")})},
		{"set", cty.SetVal([]cty.Value{cty.StringVal("a")}), cty.SetVal([]cty.Value{cty.StringVal("b")})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := cty.Path{cty.GetAttrStep{Name: "collection"}}
			var changes []attemptedChange
			attemptedChanges(path, tc.before, tc.after, &changes)
			if len(changes) != 1 || renderedPath(changes[0].path) != "collection" {
				t.Fatalf("changes = %#v", changes)
			}
		})
	}
}

func TestAttemptedChangeSummaryRedactsBothSidesAndFailsClosed(t *testing.T) {
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"secret": {Type: cty.String, Optional: true, Sensitive: true},
	}, Blocks: map[string]*provider.NestedBlock{}}
	root := func(secret, extra cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"secret": secret, "extra": extra})
	}
	prior := root(cty.StringVal("before-secret"), cty.StringVal("before-extra"))
	planned := root(cty.StringVal("after-secret"), cty.StringVal("after-extra"))
	ds := attemptedChangeSummary("test.x", "update", block, prior, planned)
	if len(ds) != 1 || strings.Contains(ds[0].Detail, "before-secret") || strings.Contains(ds[0].Detail, "after-secret") ||
		strings.Contains(ds[0].Detail, "before-extra") || strings.Contains(ds[0].Detail, "after-extra") ||
		strings.Count(ds[0].Detail, "(sensitive value) -> (sensitive value)") != 2 {
		t.Fatalf("redacted diagnostics = %#v", ds)
	}
}

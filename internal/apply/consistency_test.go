package apply

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/provider"
)

func scalarBlock(sensitive bool) *provider.SchemaBlock {
	return &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"id":   {Type: cty.String, Computed: true},
		"flag": {Type: cty.Bool, Optional: true, Sensitive: sensitive},
	}, Blocks: map[string]*provider.NestedBlock{}}
}

func scalarRoot(id, flag cty.Value) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{"id": id, "flag": flag})
}

func TestCheckResultConsistencyTraversesShallowKnownRoot(t *testing.T) {
	planned := scalarRoot(cty.UnknownVal(cty.String), cty.True)
	cfg := scalarRoot(cty.NullVal(cty.String), cty.True)
	applied := scalarRoot(cty.StringVal("id"), cty.False)
	if planned.IsWhollyKnown() {
		t.Fatal("fixture planned root unexpectedly wholly known")
	}
	ds := checkResultConsistency("test.x", scalarBlock(false), planned, cfg, applied)
	if len(ds) != 1 || ds[0].Summary != "provider produced inconsistent result after apply" {
		t.Fatalf("diagnostics = %#v", ds)
	}
	if !strings.Contains(ds[0].Detail, "flag: planned true, applied false") || strings.Contains(ds[0].Detail, "<root>") {
		t.Fatalf("detail = %q", ds[0].Detail)
	}
}

func TestCheckResultConsistencyRootResultsAreNamed(t *testing.T) {
	ty := scalarBlock(false).ImpliedType()
	cfg := scalarRoot(cty.NullVal(cty.String), cty.True)
	planned := scalarRoot(cty.UnknownVal(cty.String), cty.True)
	for _, tc := range []struct {
		name, summary string
		state         cty.Value
	}{
		{"null", "provider returned no state after apply", cty.NullVal(ty)},
		{"unknown", "provider returned unknown state after apply", cty.UnknownVal(ty)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := checkResultConsistency("test.x", scalarBlock(false), planned, cfg, tc.state)
			if len(ds) != 1 || ds[0].Summary != tc.summary {
				t.Fatalf("diagnostics = %#v", ds)
			}
			if regexp.MustCompile(`(?m)^\s*:`).MatchString(ds[0].Detail) || strings.Contains(ds[0].Detail, "  : planned") {
				t.Fatalf("detail has empty path: %q", ds[0].Detail)
			}
		})
	}
	if got := renderedPath(nil); got != "<root>" {
		t.Fatalf("renderedPath(nil) = %q", got)
	}
}

func mapBlock(sensitive bool) *provider.SchemaBlock {
	return &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"tags":   {Type: cty.Map(cty.String), Optional: true, Sensitive: sensitive},
		"secret": {Type: cty.String, Optional: true, Sensitive: true},
	}, Blocks: map[string]*provider.NestedBlock{}}
}

func mapRoot(tags, secret cty.Value) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{"tags": tags, "secret": secret})
}

func TestCheckResultConsistencyMapKeySet(t *testing.T) {
	empty := cty.MapValEmpty(cty.String)
	cases := []struct {
		name           string
		cfg, plan, got cty.Value
		want           []string
	}{
		{"equal empty", empty, empty, empty, nil},
		{"empty populated", empty, empty, cty.MapVal(map[string]cty.Value{"injected": cty.StringVal("by-provider")}), []string{`tags["injected"]: planned absent, applied "by-provider"`}},
		{"dropped", cty.MapVal(map[string]cty.Value{"dropped": cty.StringVal("x")}), cty.MapVal(map[string]cty.Value{"dropped": cty.StringVal("x")}), empty, []string{`tags["dropped"]: planned "x", applied absent`}},
		{"mixed sorted", cty.MapVal(map[string]cty.Value{"a": cty.StringVal("1"), "b": cty.StringVal("2")}), cty.MapVal(map[string]cty.Value{"a": cty.StringVal("1"), "b": cty.StringVal("2")}), cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x"), "c": cty.StringVal("3")}), []string{`tags["a"]`, `tags["b"]`, `tags["c"]`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nullSecret := cty.NullVal(cty.String)
			ds := checkResultConsistency("test.x", mapBlock(false), mapRoot(tc.plan, nullSecret), mapRoot(tc.cfg, nullSecret), mapRoot(tc.got, nullSecret))
			if len(tc.want) == 0 {
				if len(ds) != 0 {
					t.Fatalf("diagnostics = %#v", ds)
				}
				return
			}
			if len(ds) != 1 {
				t.Fatalf("diagnostics = %#v", ds)
			}
			last := -1
			for _, want := range tc.want {
				idx := strings.Index(ds[0].Detail, want)
				if idx < 0 || idx < last {
					t.Fatalf("detail = %q; missing/out of order %q", ds[0].Detail, want)
				}
				last = idx
			}
		})
	}
}

func TestCheckResultConsistencyMapAuthorshipAndRedaction(t *testing.T) {
	plan := cty.MapVal(map[string]cty.Value{"planned": cty.StringVal("p")})
	got := cty.MapVal(map[string]cty.Value{"extra": cty.UnknownVal(cty.String)})
	nullSecret := cty.NullVal(cty.String)

	// A null map container is wholly unauthored, including its key set.
	if ds := checkResultConsistency("test.x", mapBlock(false), mapRoot(plan, nullSecret), mapRoot(cty.NullVal(cty.Map(cty.String)), nullSecret), mapRoot(got, nullSecret)); len(ds) != 0 {
		t.Fatalf("unauthored map diagnostics = %#v", ds)
	}

	ds := checkResultConsistency("test.x", mapBlock(true), mapRoot(plan, nullSecret), mapRoot(plan, nullSecret), mapRoot(got, nullSecret))
	if len(ds) != 1 || strings.Contains(ds[0].Detail, `"p"`) || !strings.Contains(ds[0].Detail, "(sensitive value)") {
		t.Fatalf("sensitive detail = %#v", ds)
	}
}

func TestCheckResultConsistencyContainerAndWholesaleUnknowns(t *testing.T) {
	block := mapBlock(false)
	tags := cty.MapVal(map[string]cty.Value{"a": cty.StringVal("visible")})
	nullSecret := cty.NullVal(cty.String)
	ds := checkResultConsistency("test.x", block, mapRoot(tags, nullSecret), mapRoot(tags, nullSecret), mapRoot(cty.NullVal(cty.Map(cty.String)), nullSecret))
	if len(ds) != 1 || !strings.Contains(ds[0].Detail, `tags: planned {"a":"visible"}, applied null`) {
		t.Fatalf("null container detail = %#v", ds)
	}

	listBlock := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{"items": {Type: cty.List(cty.String), Optional: true}}, Blocks: map[string]*provider.NestedBlock{}}
	root := func(v cty.Value) cty.Value { return cty.ObjectVal(map[string]cty.Value{"items": v}) }
	planned := cty.ListVal([]cty.Value{cty.StringVal("a")})
	partial := cty.ListVal([]cty.Value{cty.UnknownVal(cty.String)})
	if ds := checkResultConsistency("test.x", listBlock, root(partial), root(planned), root(planned)); len(ds) != 0 {
		t.Fatalf("partially unknown config should be skipped: %#v", ds)
	}
	ds = checkResultConsistency("test.x", listBlock, root(planned), root(planned), root(partial))
	if len(ds) != 1 || !strings.Contains(ds[0].Detail, "applied (unknown value)") || strings.Contains(ds[0].Detail, "marshal") {
		t.Fatalf("partially unknown applied detail = %#v", ds)
	}
}

func TestCheckResultConsistencyOrderedCollectionsTraverseConcreteSiblings(t *testing.T) {
	for _, tc := range []struct {
		name string
		ty   cty.Type
		make func(cty.Value, cty.Value) cty.Value
	}{
		{
			name: "list",
			ty:   cty.List(cty.String),
			make: func(first, second cty.Value) cty.Value {
				return cty.ListVal([]cty.Value{first, second})
			},
		},
		{
			name: "tuple",
			ty:   cty.Tuple([]cty.Type{cty.String, cty.String}),
			make: func(first, second cty.Value) cty.Value {
				return cty.TupleVal([]cty.Value{first, second})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"items": {Type: tc.ty, Optional: true},
			}, Blocks: map[string]*provider.NestedBlock{}}
			root := func(v cty.Value) cty.Value {
				return cty.ObjectVal(map[string]cty.Value{"items": v})
			}
			unknown := cty.UnknownVal(cty.String)
			planned := tc.make(unknown, cty.StringVal("planned"))
			cfg := tc.make(unknown, cty.StringVal("planned"))
			applied := tc.make(cty.StringVal("provider-computed"), cty.StringVal("applied"))

			ds := checkResultConsistency("test.x", block, root(planned), root(cfg), root(applied))
			if len(ds) != 1 || !strings.Contains(ds[0].Detail, `items[1]: planned "planned", applied "applied"`) {
				t.Fatalf("diagnostics = %#v", ds)
			}
			if strings.Contains(ds[0].Detail, "items[0]") || strings.Contains(ds[0].Detail, "items: planned") {
				t.Fatalf("unknown element hid or collapsed concrete sibling divergence: %q", ds[0].Detail)
			}
		})
	}
}

func TestCheckResultConsistencyNestedListTraversesAndRedactsPerLeaf(t *testing.T) {
	inner := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"user":  {Type: cty.String, Optional: true},
		"token": {Type: cty.String, Optional: true, Sensitive: true},
	}, Blocks: map[string]*provider.NestedBlock{}}
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{}, Blocks: map[string]*provider.NestedBlock{
		"credentials": {Nesting: "list", Block: inner},
	}}
	entry := func(user, token cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"user": user, "token": token})
	}
	root := func(entries ...cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"credentials": cty.ListVal(entries)})
	}
	unknown := cty.UnknownVal(cty.String)
	planned := root(
		entry(cty.StringVal("alice"), unknown),
		entry(cty.StringVal("carol"), cty.StringVal("never-print")),
	)
	cfg := planned
	applied := root(
		entry(cty.StringVal("bob"), cty.StringVal("provider-computed")),
		entry(cty.StringVal("carol"), cty.StringVal("also-never-print")),
	)

	ds := checkResultConsistency("test.x", block, planned, cfg, applied)
	if len(ds) != 1 || !strings.Contains(ds[0].Detail, `credentials[0].user: planned "alice", applied "bob"`) {
		t.Fatalf("diagnostics = %#v", ds)
	}
	if strings.Contains(ds[0].Detail, "credentials[0].token") ||
		!strings.Contains(ds[0].Detail, "credentials[1].token: planned (sensitive value), applied (sensitive value)") ||
		strings.Contains(ds[0].Detail, "never-print") || strings.Contains(ds[0].Detail, "also-never-print") {
		t.Fatalf("nested list redaction detail = %q", ds[0].Detail)
	}
}

func TestCheckResultConsistencyNestedBlockRedaction(t *testing.T) {
	innerSensitive := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{"token": {Type: cty.String, Optional: true, Sensitive: true}}, Blocks: map[string]*provider.NestedBlock{}}
	innerPlain := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{"path": {Type: cty.String, Optional: true}}, Blocks: map[string]*provider.NestedBlock{}}
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{}, Blocks: map[string]*provider.NestedBlock{
		"endpoints": {Nesting: "list", Block: innerSensitive},
		"probes":    {Nesting: "set", Block: innerPlain},
	}}
	endpoint := cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("never-print")})
	probe := cty.ObjectVal(map[string]cty.Value{"path": cty.StringVal("/ok")})
	planned := cty.ObjectVal(map[string]cty.Value{"endpoints": cty.ListVal([]cty.Value{endpoint}), "probes": cty.SetVal([]cty.Value{probe})})
	cfg := planned
	applied := cty.ObjectVal(map[string]cty.Value{"endpoints": cty.ListValEmpty(endpoint.Type()), "probes": cty.SetValEmpty(probe.Type())})
	ds := checkResultConsistency("test.x", block, planned, cfg, applied)
	if len(ds) != 1 || strings.Contains(ds[0].Detail, "never-print") || !strings.Contains(ds[0].Detail, "endpoints: planned (sensitive value), applied (sensitive value)") || !strings.Contains(ds[0].Detail, `"/ok"`) {
		t.Fatalf("nested detail = %#v", ds)
	}
}

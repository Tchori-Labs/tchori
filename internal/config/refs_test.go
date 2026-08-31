package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tchori-labs/tchori/internal/diag"
)

func TestParseRef(t *testing.T) {
	r, ok := ParseRef("${null_resource.a.triggers.foo}")
	if !ok {
		t.Fatal("ParseRef rejected a valid reference")
	}
	if r.Address != "null_resource.a" || r.Attr != "triggers.foo" {
		t.Errorf("ParseRef = %+v, want Address null_resource.a, Attr triggers.foo", r)
	}
	for _, s := range []string{
		"prefix ${null_resource.a.id}", // not the whole value
		"${Null_resource.a.id}",        // uppercase type: outside the grammar
		"${null_resource.a}",           // no attribute
		"plain string",
	} {
		if _, ok := ParseRef(s); ok {
			t.Errorf("ParseRef(%q) accepted, want rejected", s)
		}
	}
}

func TestFindUnresolvedReference(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "reported suffix", input: "${cloudflare_tunnel.waha.id}.cfargotunnel.com", want: "${cloudflare_tunnel.waha.id}"},
		{name: "prefix", input: "prefix-${null_resource.a.id}", want: "${null_resource.a.id}"},
		{name: "prefix and suffix", input: "prefix-${null_resource.a.id}-suffix", want: "${null_resource.a.id}"},
		{name: "multiple returns first", input: "${null_resource.a.id}-${null_resource.b.id}", want: "${null_resource.a.id}"},
		// The detector reports valid whole-string references too. Compose must
		// call ParseRef first so this still resolves rather than errors.
		{name: "whole string reference", input: "${null_resource.a.id}", want: "${null_resource.a.id}"},
		{name: "uppercase type near miss", input: "${Null_resource.a.id}", want: "${Null_resource.a.id}"},
		{name: "two segments no attribute", input: "${null_resource.a}", want: "${null_resource.a}"},
		{name: "shell variable", input: "${HOME}"},
		{name: "shell default", input: "${VAR:-x}"},
		{name: "empty braces", input: "${}"},
		{name: "money", input: "$100"},
		{name: "no dollar", input: "{a.b.c}"},
		{name: "plain text", input: "plain text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := FindUnresolvedReference(tt.input)
			if got != tt.want || found != (tt.want != "") {
				t.Errorf("FindUnresolvedReference(%q) = (%q, %t), want (%q, %t)", tt.input, got, found, tt.want, tt.want != "")
			}
		})
	}
}

func TestExtractRefs(t *testing.T) {
	cfg := map[string]any{
		"b_ref": "${null_resource.b.id}",
		"a_ref": "${null_resource.a.id}",
		"dup":   "${null_resource.a.id}",
		"nested": map[string]any{
			"deep": []any{
				"${null_resource.a.triggers.foo}",
				map[string]any{"x": "${tchoritest_thing.my-name.echo}"},
			},
		},
		"interpolation": "prefix ${null_resource.a.id}",
		"bad_case":      "${Null_resource.a.id}",
		"env_wrapper":   map[string]any{"env": "SOME_VAR"},
		"number":        float64(42),
		"boolean":       true,
		"null":          nil,
	}
	got := ExtractRefs(cfg)
	want := []Ref{
		{Address: "null_resource.a", Attr: "id"},
		{Address: "null_resource.a", Attr: "triggers.foo"},
		{Address: "null_resource.b", Attr: "id"},
		{Address: "tchoritest_thing.my-name", Attr: "echo"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractRefsEmpty(t *testing.T) {
	got := ExtractRefs(map[string]any{"a": "plain string", "b": float64(1)})
	if len(got) != 0 {
		t.Errorf("ExtractRefs() = %#v, want empty", got)
	}
}

// testConfig builds a *Config from address -> raw resource config.
// Order() only inspects Resources, so Providers stays empty.
func testConfig(resources map[string]map[string]any) *Config {
	c := &Config{
		Providers: map[string]*ProviderConfig{},
		Resources: map[string]*Resource{},
	}
	for addr, raw := range resources {
		dot := strings.Index(addr, ".")
		typeName := addr[:dot]
		c.Resources[addr] = &Resource{
			Address:  addr,
			Type:     typeName,
			Name:     addr[dot+1:],
			Provider: typeName[:strings.Index(typeName, "_")],
			Config:   raw,
		}
	}
	return c
}

func TestDependencies(t *testing.T) {
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {},
		"null_resource.b": {
			"first":  "${null_resource.a.id}",
			"second": "${null_resource.a.other}",
		},
		"null_resource.c": {
			"z": "${null_resource.b.id}",
			"a": "${null_resource.a.id}",
		},
	})
	got, diags := c.Dependencies()
	if diags.HasErrors() {
		t.Fatalf("Dependencies diagnostics: %+v", diags)
	}
	want := map[string][]string{
		"null_resource.a": {},
		"null_resource.b": {"null_resource.a"},
		"null_resource.c": {"null_resource.a", "null_resource.b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies() = %#v, want %#v", got, want)
	}
}

func TestDependenciesTransitiveChainReturnsDirectEdges(t *testing.T) {
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {},
		"null_resource.b": {"dep": "${null_resource.a.id}"},
		"null_resource.c": {"dep": "${null_resource.b.id}"},
	})
	got, diags := c.Dependencies()
	if diags.HasErrors() {
		t.Fatalf("Dependencies diagnostics: %+v", diags)
	}
	if !reflect.DeepEqual(got["null_resource.c"], []string{"null_resource.b"}) {
		t.Fatalf("c dependencies = %v, want direct dependency b only", got["null_resource.c"])
	}
}

func TestDependenciesUndeclaredReference(t *testing.T) {
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {"dep": "${null_resource.ghost.id}"},
	})
	got, diags := c.Dependencies()
	if got != nil || !diags.HasErrors() {
		t.Fatalf("Dependencies() = (%v, %+v), want nil and undeclared-reference error", got, diags)
	}
	if diags[0].Summary != "reference to undeclared resource" || diags[0].Address != "null_resource.a" {
		t.Fatalf("diagnostic = %+v", diags[0])
	}
}

func TestDependenciesEmptyConfig(t *testing.T) {
	got, diags := testConfig(map[string]map[string]any{}).Dependencies()
	if diags.HasErrors() || got == nil || len(got) != 0 {
		t.Fatalf("Dependencies() = (%#v, %+v), want non-nil empty map and no errors", got, diags)
	}
}

func TestOrderChain(t *testing.T) {
	// b references a, c references b: expected order a, b, c.
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {},
		"null_resource.b": {"triggers": map[string]any{"dep": "${null_resource.a.id}"}},
		"null_resource.c": {"triggers": map[string]any{"dep": "${null_resource.b.id}"}},
	})
	order, diags := c.Order()
	if diags.HasErrors() {
		t.Fatalf("unexpected error diagnostics: %+v", diags)
	}
	want := []string{"null_resource.a", "null_resource.b", "null_resource.c"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("Order() = %v, want %v", order, want)
	}
}

func TestOrderChainBeatsLexicalOrder(t *testing.T) {
	// a references b, b references c: dependency order must invert the
	// lexical order of the addresses, proving edges (not sorting) decide.
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {"triggers": map[string]any{"dep": "${null_resource.b.id}"}},
		"null_resource.b": {"triggers": map[string]any{"dep": "${null_resource.c.id}"}},
		"null_resource.c": {},
	})
	order, diags := c.Order()
	if diags.HasErrors() {
		t.Fatalf("unexpected error diagnostics: %+v", diags)
	}
	want := []string{"null_resource.c", "null_resource.b", "null_resource.a"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("Order() = %v, want %v", order, want)
	}
}

func TestOrderDeterministicTieBreak(t *testing.T) {
	// Independent resources: lexical order, stable across repeated runs
	// (Go map iteration order is random, so 20 runs catch regressions).
	c := testConfig(map[string]map[string]any{
		"null_resource.zeta":  {},
		"null_resource.alpha": {},
		"null_resource.mid":   {},
	})
	want := []string{"null_resource.alpha", "null_resource.mid", "null_resource.zeta"}
	for i := 0; i < 20; i++ {
		order, diags := c.Order()
		if diags.HasErrors() {
			t.Fatalf("run %d: unexpected error diagnostics: %+v", i, diags)
		}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("run %d: Order() = %v, want %v", i, order, want)
		}
	}
}

func TestOrderUnknownRef(t *testing.T) {
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {"triggers": map[string]any{"dep": "${null_resource.ghost.id}"}},
	})
	order, diags := c.Order()
	if order != nil {
		t.Errorf("Order() = %v, want nil order on error", order)
	}
	if !diags.HasErrors() {
		t.Fatal("Order() returned no error diagnostics, want reference error")
	}
	found := false
	for _, d := range diags {
		if d.Severity != diag.Error {
			continue
		}
		found = true
		if d.Summary != "reference to undeclared resource" {
			t.Errorf("Summary = %q, want %q", d.Summary, "reference to undeclared resource")
		}
		if d.Address != "null_resource.a" {
			t.Errorf("Address = %q, want %q (the referring resource)", d.Address, "null_resource.a")
		}
		if !strings.Contains(d.Detail, "null_resource.ghost") {
			t.Errorf("Detail = %q, want it to name null_resource.ghost", d.Detail)
		}
	}
	if !found {
		t.Error("no error diagnostic found")
	}
}

func TestOrderCycle(t *testing.T) {
	c := testConfig(map[string]map[string]any{
		"null_resource.a": {"triggers": map[string]any{"dep": "${null_resource.b.id}"}},
		"null_resource.b": {"triggers": map[string]any{"dep": "${null_resource.a.id}"}},
	})
	order, diags := c.Order()
	if order != nil {
		t.Errorf("Order() = %v, want nil order on cycle", order)
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics %+v, want exactly 1", len(diags), diags)
	}
	d := diags[0]
	if d.Severity != diag.Error {
		t.Errorf("Severity = %q, want %q", d.Severity, diag.Error)
	}
	if d.Summary != "reference cycle" {
		t.Errorf("Summary = %q, want %q", d.Summary, "reference cycle")
	}
	wantDetail := "null_resource.a -> null_resource.b -> null_resource.a"
	if d.Detail != wantDetail {
		t.Errorf("Detail = %q, want %q", d.Detail, wantDetail)
	}
	if d.Address != "null_resource.a" {
		t.Errorf("Address = %q, want %q", d.Address, "null_resource.a")
	}
}

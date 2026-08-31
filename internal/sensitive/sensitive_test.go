package sensitive

import (
	"fmt"
	"testing"

	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/zclconf/go-cty/cty"
)

func testBlock(nesting string) *provider.SchemaBlock {
	inner := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{"token": {Type: cty.String, Optional: true, Sensitive: true}}, Blocks: map[string]*provider.NestedBlock{}}
	return &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"client_secret": {Type: cty.String, Computed: true, Sensitive: true},
		"note":          {Type: cty.String, Optional: true},
	}, Blocks: map[string]*provider.NestedBlock{"rules": {Nesting: nesting, Block: inner}}}
}

func TestResolveAndEffectiveMixedInstances(t *testing.T) {
	s, ds := Resolve(testBlock("list"), []string{"note", "note"}, map[string]any{
		"rules": []any{map[string]any{"token": "literal"}, map[string]any{"token": "${thing.a.id}"}}, //nolint:gosec // schema attribute name in synthetic redaction input
	})
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	wantPaths := "[client_secret note rules.token]"
	if got := fmtSlice(s.Paths()); got != wantPaths {
		t.Fatalf("Paths = %s, want %s", got, wantPaths)
	}
	if got := fmtSlice(s.ExemptInstances()); got != "[rules[0].token]" {
		t.Fatalf("ExemptInstances = %s", got)
	}
	ty := cty.Object(map[string]cty.Type{"client_secret": cty.String, "note": cty.String, "rules": cty.List(cty.Object(map[string]cty.Type{"token": cty.String}))})
	v := cty.ObjectVal(map[string]cty.Value{"client_secret": cty.StringVal("secret"), "note": cty.StringVal("provider"), "rules": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("literal")}), cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("secret")})})})
	out, _, err := s.Redact(v)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Type().Equals(ty) {
		t.Fatalf("type changed: %s", out.Type().FriendlyName())
	}
	if out.GetAttr("rules").Index(cty.NumberIntVal(0)).GetAttr("token").AsString() != "literal" {
		t.Fatal("literal instance redacted")
	}
	if !out.GetAttr("rules").Index(cty.NumberIntVal(1)).GetAttr("token").IsNull() {
		t.Fatal("reference instance leaked")
	}
}

func TestEffectiveLiteralRules(t *testing.T) {
	cases := []struct {
		name   string
		raw    any
		exempt bool
	}{
		{"string", "x", true}, {"number", float64(1), true}, {"bool", true, true},
		{"reference", "${thing.a.id}", false}, {"env", map[string]any{"env": "TOKEN"}, false}, {"null", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ds := Resolve(testBlock("list"), nil, map[string]any{"client_secret": tc.raw})
			if ds.HasErrors() {
				t.Fatal(ds)
			}
			if got := len(s.ExemptInstances()) == 1; got != tc.exempt {
				t.Fatalf("exempt=%v paths=%v", got, s.ExemptInstances())
			}
		})
	}
}

func TestSetNestedBlockNeverExempt(t *testing.T) {
	s, ds := Resolve(testBlock("set"), nil, map[string]any{"rules": []any{map[string]any{"token": "literal"}}})
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	if len(s.ExemptInstances()) != 0 {
		t.Fatalf("set exemptions = %v", s.ExemptInstances())
	}
}

func TestUnknownDeclaredPath(t *testing.T) {
	_, ds := Resolve(testBlock("list"), []string{"typo"}, nil)
	if !ds.HasErrors() {
		t.Fatal("expected diagnostic")
	}
}

func fmtSlice(v []string) string { return fmt.Sprint(v) }

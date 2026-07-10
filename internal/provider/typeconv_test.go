package provider

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
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

func TestComposeEnvWrapper(t *testing.T) {
	t.Setenv("TCHORI_TEST_TOKEN", "s3cr3t")
	ty := cty.Object(map[string]cty.Type{
		"token": cty.String,
		"note":  cty.String,
	})
	raw := map[string]any{
		"token": map[string]any{"env": "TCHORI_TEST_TOKEN"},
	}

	// allowEnv=true: this is how provider config is composed.
	got, diags := Compose(raw, ty, true, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if g := got.GetAttr("token"); !g.RawEquals(cty.StringVal("s3cr3t")) {
		t.Errorf("token = %#v, want \"s3cr3t\"", g)
	}
	if g := got.GetAttr("note"); !g.RawEquals(cty.NullVal(cty.String)) {
		t.Errorf("missing attribute note = %#v, want null string", g)
	}
}

func TestComposeEnvUnset(t *testing.T) {
	const name = "TCHORI_TEST_UNSET_TOKEN"
	t.Setenv(name, "placeholder")             // registers cleanup restoring the original state
	if err := os.Unsetenv(name); err != nil { // now guaranteed unset for this test
		t.Fatalf("Unsetenv: %v", err)
	}

	ty := cty.Object(map[string]cty.Type{"token": cty.String})
	raw := map[string]any{"token": map[string]any{"env": name}}

	_, diags := Compose(raw, ty, true, nil)
	if !diags.HasErrors() {
		t.Fatal("Compose succeeded; want error diagnostic for unset environment variable")
	}
	found := false
	for _, d := range diags {
		if d.Summary == "environment variable not set" && strings.Contains(d.Detail, name) {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics %+v do not name the unset variable %q", diags, name)
	}
}

func TestComposeEnvWrapperNonString(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"count": cty.Number})
	raw := map[string]any{"count": map[string]any{"env": "SOME_VAR"}}

	_, diags := Compose(raw, ty, true, nil)
	if !diags.HasErrors() {
		t.Fatal("Compose succeeded; want error: env wrapper only valid for string attributes")
	}
	if diags[0].Summary != "invalid env wrapper" {
		t.Errorf("summary = %q, want %q", diags[0].Summary, "invalid env wrapper")
	}
}

func TestComposeEnvWrapperDisallowed(t *testing.T) {
	// allowEnv=false is how RESOURCE configs are composed: the spec permits
	// {"env": ...} wrappers only inside provider config.
	t.Setenv("TCHORI_TEST_TOKEN", "s3cr3t")
	ty := cty.Object(map[string]cty.Type{"token": cty.String})
	raw := map[string]any{"token": map[string]any{"env": "TCHORI_TEST_TOKEN"}}

	_, diags := Compose(raw, ty, false, nil)
	if !diags.HasErrors() {
		t.Fatal("Compose succeeded; want error: env wrappers are only allowed in provider config")
	}
	if want := "env wrappers are only allowed in provider config"; diags[0].Summary != want {
		t.Errorf("summary = %q, want %q", diags[0].Summary, want)
	}

	// A single-key "env" object where a MAP is expected stays plain data even
	// with allowEnv=false — the wrapper check applies only at primitives.
	mapTy := cty.Object(map[string]cty.Type{"tags": cty.Map(cty.String)})
	got, mdiags := Compose(map[string]any{"tags": map[string]any{"env": "prod"}}, mapTy, false, nil)
	if mdiags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", mdiags)
	}
	if g := got.GetAttr("tags"); !g.RawEquals(cty.MapVal(map[string]cty.Value{"env": cty.StringVal("prod")})) {
		t.Errorf("tags = %#v, want the map to carry the env key as plain data", g)
	}
}

func TestComposeRefs(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{
		"name":   cty.String,
		"id":     cty.String,
		"budget": cty.Number,
	})
	var gotRefs []config.Ref
	resolve := func(ref config.Ref) (cty.Value, diag.Diagnostics) {
		gotRefs = append(gotRefs, ref)
		switch ref.Address + "." + ref.Attr {
		case "tchoritest_thing.base.id":
			return cty.UnknownVal(cty.String), nil // computed, not yet applied
		case "tchoritest_thing.base.tags.budget":
			return cty.StringVal("42"), nil // resolver returns string; attr wants number
		default:
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "unknown reference", "")}
		}
	}
	raw := map[string]any{
		"name":   "web",
		"id":     "${tchoritest_thing.base.id}",
		"budget": "${tchoritest_thing.base.tags.budget}",
	}

	// allowEnv=false: references appear in resource configs.
	got, diags := Compose(raw, ty, false, resolve)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if g := got.GetAttr("id"); g.IsKnown() {
		t.Errorf("id = %#v, want unknown (propagated from resolver)", g)
	}
	if g := got.GetAttr("budget"); !g.RawEquals(cty.NumberIntVal(42)) {
		t.Errorf("budget = %#v, want 42 (converted from resolver's string)", g)
	}
	// Attributes compose in sorted name order, so resolver call order is
	// deterministic: "budget" before "id". Attr keeps its dotted sub-path.
	wantRefs := []config.Ref{
		{Address: "tchoritest_thing.base", Attr: "tags.budget"},
		{Address: "tchoritest_thing.base", Attr: "id"},
	}
	if !reflect.DeepEqual(gotRefs, wantRefs) {
		t.Errorf("resolver saw refs %+v, want %+v", gotRefs, wantRefs)
	}
}

func TestComposeUnexpectedAttribute(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"name": cty.String})
	raw := map[string]any{"name": "web", "nope": true}

	_, diags := Compose(raw, ty, false, nil)
	if !diags.HasErrors() {
		t.Fatal("Compose succeeded; want error for unexpected attribute")
	}
	if want := `unexpected attribute "nope"`; diags[0].Summary != want {
		t.Errorf("summary = %q, want %q", diags[0].Summary, want)
	}
}

func TestComposeNested(t *testing.T) {
	t.Setenv("TCHORI_TEST_TEAM", "growth")
	ty := cty.Object(map[string]cty.Type{
		"tags": cty.Map(cty.String),
		"rules": cty.List(cty.Object(map[string]cty.Type{
			"port":  cty.Number,
			"allow": cty.Bool,
		})),
		"owner": cty.Object(map[string]cty.Type{
			"team":    cty.String,
			"contact": cty.String,
		}),
	})
	raw := map[string]any{
		// A single-key "env" object where a MAP is expected is plain data,
		// not an env wrapper (wrappers apply only where a string is expected).
		"tags": map[string]any{"env": "prod"},
		"rules": []any{
			map[string]any{"port": 443, "allow": true},
			map[string]any{"port": float64(80)}, // "allow" missing -> null
		},
		// Wrappers work at any depth where a string is expected (allowEnv=true:
		// provider-config mode).
		"owner": map[string]any{"team": map[string]any{"env": "TCHORI_TEST_TEAM"}},
	}

	got, diags := Compose(raw, ty, true, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	want := cty.ObjectVal(map[string]cty.Value{
		"tags": cty.MapVal(map[string]cty.Value{"env": cty.StringVal("prod")}),
		"rules": cty.ListVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{"port": cty.NumberIntVal(443), "allow": cty.True}),
			cty.ObjectVal(map[string]cty.Value{"port": cty.NumberIntVal(80), "allow": cty.NullVal(cty.Bool)}),
		}),
		"owner": cty.ObjectVal(map[string]cty.Value{
			"team":    cty.StringVal("growth"),
			"contact": cty.NullVal(cty.String),
		}),
	})
	if !got.RawEquals(want) {
		t.Fatalf("Compose =\n%#v\nwant\n%#v", got, want)
	}
}

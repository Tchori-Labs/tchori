package provider

import (
	"fmt"
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
	got, diags := Compose(raw, ty, EnvResolve, nil)
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

	_, diags := Compose(raw, ty, EnvResolve, nil)
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

func TestComposeEnvWrapperNestingPolicies(t *testing.T) {
	const envName = "TCHORI_TEST_NESTED_VALUE"
	for _, tc := range []struct {
		name string
		ty   cty.Type
		raw  map[string]any
		want cty.Value
	}{
		{
			name: "top-level string",
			ty:   cty.Object(map[string]cty.Type{"value": cty.String}),
			raw:  map[string]any{"value": map[string]any{"env": envName}},
			want: cty.ObjectVal(map[string]cty.Value{"value": cty.StringVal("secret")}),
		},
		{
			name: "map element",
			ty:   cty.Object(map[string]cty.Type{"value": cty.Map(cty.String)}),
			raw:  map[string]any{"value": map[string]any{"key": map[string]any{"env": envName}}},
			want: cty.ObjectVal(map[string]cty.Value{"value": cty.MapVal(map[string]cty.Value{"key": cty.StringVal("secret")})}),
		},
		{
			name: "list element",
			ty:   cty.Object(map[string]cty.Type{"value": cty.List(cty.String)}),
			raw:  map[string]any{"value": []any{map[string]any{"env": envName}}},
			want: cty.ObjectVal(map[string]cty.Value{"value": cty.ListVal([]cty.Value{cty.StringVal("secret")})}),
		},
		{
			name: "set element",
			ty:   cty.Object(map[string]cty.Type{"value": cty.Set(cty.String)}),
			raw:  map[string]any{"value": []any{map[string]any{"env": envName}}},
			want: cty.ObjectVal(map[string]cty.Value{"value": cty.SetVal([]cty.Value{cty.StringVal("secret")})}),
		},
		{
			name: "nested object leaf and two wrappers",
			ty: cty.Object(map[string]cty.Type{
				"value": cty.Object(map[string]cty.Type{"first": cty.String, "second": cty.String}),
			}),
			raw: map[string]any{"value": map[string]any{
				"first":  map[string]any{"env": envName},
				"second": map[string]any{"env": envName},
			}},
			want: cty.ObjectVal(map[string]cty.Value{"value": cty.ObjectVal(map[string]cty.Value{
				"first": cty.StringVal("secret"), "second": cty.StringVal("secret"),
			})}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envName, "secret")
			for _, policy := range []EnvPolicy{EnvResolve, EnvUnknownIfUnset} {
				got, diags := Compose(tc.raw, tc.ty, policy, nil)
				if diags.HasErrors() {
					t.Fatalf("policy %d: unexpected diagnostics: %+v", policy, diags)
				}
				if !got.RawEquals(tc.want) {
					t.Errorf("policy %d: Compose = %#v, want %#v", policy, got, tc.want)
				}
			}
		})
	}
}

func TestComposeEnvWrapperEmptyStringPolicies(t *testing.T) {
	const envName = "TCHORI_TEST_EMPTY_VALUE"
	t.Setenv(envName, "")
	ty := cty.Object(map[string]cty.Type{"value": cty.String})
	raw := map[string]any{"value": map[string]any{"env": envName}}
	for _, policy := range []EnvPolicy{EnvResolve, EnvUnknownIfUnset} {
		got, diags := Compose(raw, ty, policy, nil)
		if diags.HasErrors() {
			t.Fatalf("policy %d: unexpected diagnostics: %+v", policy, diags)
		}
		if g := got.GetAttr("value"); !g.RawEquals(cty.StringVal("")) {
			t.Errorf("policy %d: value = %#v, want known empty string", policy, g)
		}
	}
}

func TestComposeEnvUnknownIfUnset(t *testing.T) {
	const envName = "TCHORI_TEST_UNKNOWN_IF_UNSET"
	t.Setenv(envName, "placeholder")
	if err := os.Unsetenv(envName); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	ty := cty.Object(map[string]cty.Type{"value": cty.String})
	raw := map[string]any{"value": map[string]any{"env": envName}}

	got, diags := Compose(raw, ty, EnvUnknownIfUnset, nil)
	if diags.HasErrors() || len(diags) != 0 {
		t.Fatalf("diagnostics = %+v, want none", diags)
	}
	value := got.GetAttr("value")
	if value.IsKnown() || value.Type() != cty.String {
		t.Errorf("value = %#v, want unknown with known string type", value)
	}
}

func TestComposeEnvWrapperNonString(t *testing.T) {
	for name, primitive := range map[string]cty.Type{"number": cty.Number, "bool": cty.Bool} {
		t.Run(name, func(t *testing.T) {
			for _, policy := range []EnvPolicy{EnvResolve, EnvUnknownIfUnset} {
				ty := cty.Object(map[string]cty.Type{"value": primitive})
				raw := map[string]any{"value": map[string]any{"env": "SOME_VAR"}}
				_, diags := Compose(raw, ty, policy, nil)
				if !diags.HasErrors() {
					t.Fatalf("policy %d: Compose succeeded; want invalid env wrapper", policy)
				}
				if diags[0].Summary != "invalid env wrapper" {
					t.Errorf("policy %d: summary = %q, want %q", policy, diags[0].Summary, "invalid env wrapper")
				}
			}
		})
	}
}

func TestComposeEnvWrapperPlainDataAtCollectionTypes(t *testing.T) {
	for _, policy := range []EnvPolicy{EnvResolve, EnvUnknownIfUnset} {
		t.Run(fmt.Sprintf("policy-%d", policy), func(t *testing.T) {
			mapTy := cty.Object(map[string]cty.Type{"value": cty.Map(cty.String)})
			got, diags := Compose(map[string]any{"value": map[string]any{"env": "prod"}}, mapTy, policy, nil)
			if diags.HasErrors() {
				t.Fatalf("map: unexpected diagnostics: %+v", diags)
			}
			if g := got.GetAttr("value"); !g.RawEquals(cty.MapVal(map[string]cty.Value{"env": cty.StringVal("prod")})) {
				t.Errorf("map value = %#v, want env key as plain data", g)
			}

			objectTy := cty.Object(map[string]cty.Type{"value": cty.Object(map[string]cty.Type{"env": cty.String})})
			got, diags = Compose(map[string]any{"value": map[string]any{"env": "prod"}}, objectTy, policy, nil)
			if diags.HasErrors() {
				t.Fatalf("object: unexpected diagnostics: %+v", diags)
			}
			if g := got.GetAttr("value").GetAttr("env"); !g.RawEquals(cty.StringVal("prod")) {
				t.Errorf("object env = %#v, want plain string data", g)
			}

			listTy := cty.Object(map[string]cty.Type{"value": cty.List(cty.String)})
			_, diags = Compose(map[string]any{"value": map[string]any{"env": "prod"}}, listTy, policy, nil)
			if !diags.HasErrors() || diags[0].Summary != "type mismatch" {
				t.Errorf("list diagnostics = %+v, want type mismatch rather than env-wrapper handling", diags)
			}
		})
	}
}

func TestComposeEnvCandidateResolution(t *testing.T) {
	const (
		first  = "TCHORI_TEST_ENV_CANDIDATE_FIRST"
		second = "TCHORI_TEST_ENV_CANDIDATE_SECOND"
	)

	tests := []struct {
		name       string
		candidates []any
		firstValue *string
		lastValue  *string
		want       string
	}{
		{
			name:       "first candidate wins when both are set",
			candidates: []any{first, second},
			firstValue: stringPointer("selected-first"),
			lastValue:  stringPointer("ignored-second"),
			want:       "selected-first",
		},
		{
			name:       "empty first candidate wins",
			candidates: []any{first, second},
			firstValue: stringPointer(""),
			lastValue:  stringPointer("ignored-second"),
			want:       "",
		},
		{
			name:       "last candidate wins when first is unset",
			candidates: []any{first, second},
			lastValue:  stringPointer("selected-second"),
			want:       "selected-second",
		},
		{
			name:       "duplicate candidate names resolve normally",
			candidates: []any{first, first},
			firstValue: stringPointer("selected-duplicate"),
			want:       "selected-duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetEnv(t, first)
			unsetEnv(t, second)
			if tt.firstValue != nil {
				t.Setenv(first, *tt.firstValue)
			}
			if tt.lastValue != nil {
				t.Setenv(second, *tt.lastValue)
			}

			ty := cty.Object(map[string]cty.Type{"token": cty.String})
			raw := map[string]any{"token": map[string]any{"env": tt.candidates}}
			got, diags := Compose(raw, ty, EnvResolve, nil)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %+v", diags)
			}
			if value := got.GetAttr("token"); !value.RawEquals(cty.StringVal(tt.want)) {
				t.Errorf("resolved token = %#v, want selected candidate", value)
			}
		})
	}
}

func TestComposeEnvCandidatesUnset(t *testing.T) {
	const (
		first  = "TCHORI_TEST_ENV_ALL_UNSET_FIRST"
		second = "TCHORI_TEST_ENV_ALL_UNSET_SECOND"
	)
	unsetEnv(t, first)
	unsetEnv(t, second)

	ty := cty.Object(map[string]cty.Type{"token": cty.String})
	raw := map[string]any{"token": map[string]any{"env": []any{first, second, first}}}
	_, diags := Compose(raw, ty, EnvResolve, nil)
	if !diags.HasErrors() {
		t.Fatal("Compose succeeded; want error diagnostic when all candidates are unset")
	}
	if diags[0].Summary != "environment variable not set" {
		t.Fatalf("summary = %q, want environment variable not set", diags[0].Summary)
	}
	detail := diags[0].Detail
	firstAt := strings.Index(detail, `"`+first+`"`)
	secondAt := strings.Index(detail, `"`+second+`"`)
	duplicateAt := strings.LastIndex(detail, `"`+first+`"`)
	if firstAt < 0 || secondAt < firstAt || duplicateAt < secondAt {
		t.Errorf("detail does not preserve candidate order and duplicates: %q", detail)
	}
	for _, want := range []string{`{"env": ...}`, "*.tchori.json", "no built-in or provider-specific", "add the name"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not contain guidance %q", detail, want)
		}
	}
}

func TestComposeMalformedEnvCandidateLists(t *testing.T) {
	tests := []struct {
		name    string
		payload []any
		want    string
	}{
		{name: "empty", payload: []any{}, want: "at least one environment variable name"},
		{name: "first element is not a string", payload: []any{1}, want: "index 0"},
		{name: "later element is not a string", payload: []any{"A", 2}, want: "index 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ty := cty.Object(map[string]cty.Type{"token": cty.String})
			raw := map[string]any{"token": map[string]any{"env": tt.payload}}
			_, diags := Compose(raw, ty, EnvResolve, nil)
			if !diags.HasErrors() {
				t.Fatal("Compose succeeded; want invalid env wrapper diagnostic")
			}
			if diags[0].Summary != "invalid env wrapper" || !strings.Contains(diags[0].Detail, tt.want) {
				t.Errorf("diagnostic = %+v, want invalid env wrapper containing %q", diags[0], tt.want)
			}
		})
	}
}

func TestComposeEnvNonWrapperPayloads(t *testing.T) {
	for _, payload := range []any{nil, map[string]any{"a": "b"}} {
		ty := cty.Object(map[string]cty.Type{"token": cty.String})
		raw := map[string]any{"token": map[string]any{"env": payload}}
		_, diags := Compose(raw, ty, EnvResolve, nil)
		if !diags.HasErrors() || diags[0].Summary != "type mismatch" {
			t.Errorf("payload %#v diagnostics = %+v, want type mismatch", payload, diags)
		}
	}
}

func TestComposeEnvCandidateListTypeSurfaces(t *testing.T) {
	for _, primitive := range []cty.Type{cty.Bool, cty.Number} {
		ty := cty.Object(map[string]cty.Type{"value": primitive})
		raw := map[string]any{"value": map[string]any{"env": []any{"A", "B"}}}
		_, diags := Compose(raw, ty, EnvResolve, nil)
		if !diags.HasErrors() || diags[0].Summary != "invalid env wrapper" {
			t.Errorf("type %s diagnostics = %+v, want invalid env wrapper", primitive.FriendlyName(), diags)
		}
	}

	// At a map-of-lists type, the same shape remains ordinary data.
	mapTy := cty.Object(map[string]cty.Type{"labels": cty.Map(cty.List(cty.String))})
	got, diags := Compose(map[string]any{
		"labels": map[string]any{"env": []any{"a", "b"}},
	}, mapTy, EnvResolve, nil)
	if diags.HasErrors() {
		t.Fatalf("plain map data produced diagnostics: %+v", diags)
	}
	wantMap := cty.MapVal(map[string]cty.Value{
		"env": cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
	})
	if value := got.GetAttr("labels"); !value.RawEquals(wantMap) {
		t.Errorf("labels = %#v, want env key preserved as plain list data", value)
	}

	// An object with another key is not the exact wrapper shape.
	stringTy := cty.Object(map[string]cty.Type{"token": cty.String})
	_, diags = Compose(map[string]any{
		"token": map[string]any{"env": "A", "other": 1},
	}, stringTy, EnvResolve, nil)
	if !diags.HasErrors() || diags[0].Summary != "type mismatch" {
		t.Errorf("multi-key object diagnostics = %+v, want type mismatch", diags)
	}
}

func stringPointer(value string) *string {
	return &value
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "restore-for-cleanup")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%q): %v", name, err)
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
	got, diags := Compose(raw, ty, EnvResolve, resolve)
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

func TestComposeRejectsRawUnresolvedReferences(t *testing.T) {
	tests := []struct {
		name string
		ty   cty.Type
		raw  map[string]any
		path string
	}{
		{
			name: "root attribute",
			ty:   cty.Object(map[string]cty.Type{"name": cty.String}),
			raw:  map[string]any{"name": "${tchoritest_thing.base.id}.suffix"},
			path: "name",
		},
		{
			name: "nested object",
			ty: cty.Object(map[string]cty.Type{"nested": cty.Object(map[string]cty.Type{
				"name": cty.String,
			})}),
			raw:  map[string]any{"nested": map[string]any{"name": "prefix-${a.b.c}"}},
			path: "nested.name",
		},
		{
			name: "map",
			ty:   cty.Object(map[string]cty.Type{"tags": cty.Map(cty.String)}),
			raw:  map[string]any{"tags": map[string]any{"content": "${a.b.c}.suffix"}},
			path: "tags.content",
		},
		{
			name: "list",
			ty:   cty.Object(map[string]cty.Type{"items": cty.List(cty.String)}),
			raw:  map[string]any{"items": []any{"${a.b.c}.suffix"}},
			path: "items[0]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ds := Compose(tt.raw, tt.ty, EnvResolve, nil)
			if !ds.HasErrors() || len(ds) != 1 {
				t.Fatalf("Compose diagnostics = %+v, want one error", ds)
			}
			d := ds[0]
			if d.Summary != "unresolved reference" || d.Address != "" || !strings.Contains(d.Detail, tt.path) || !strings.Contains(d.Detail, "${") {
				t.Errorf("diagnostic = %+v, want unresolved reference at %q with empty address", d, tt.path)
			}
		})
	}
}

func TestComposeAllowsShellStyleLiteral(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"name": cty.String})
	got, ds := Compose(map[string]any{"name": "${HOME}"}, ty, EnvResolve, nil)
	if ds.HasErrors() {
		t.Fatalf("Compose diagnostics = %+v", ds)
	}
	if !got.GetAttr("name").RawEquals(cty.StringVal("${HOME}")) {
		t.Fatalf("name = %#v, want literal ${HOME}", got.GetAttr("name"))
	}
}

func TestComposeRejectsUnresolvedReferenceFromResolver(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"name": cty.String})
	resolve := func(config.Ref) (cty.Value, diag.Diagnostics) {
		return cty.StringVal("secret-prefix-${tchoritest_thing.ghost.id}-secret-suffix"), nil
	}
	_, ds := Compose(map[string]any{"name": "${tchoritest_thing.base.id}"}, ty, EnvResolve, resolve)
	if !ds.HasErrors() || ds[0].Summary != "unresolved reference" || ds[0].Address != "" || !strings.Contains(ds[0].Detail, "name") || !strings.Contains(ds[0].Detail, "${tchoritest_thing.ghost.id}") {
		t.Fatalf("Compose diagnostics = %+v, want state-derived unresolved reference", ds)
	}
	if strings.Contains(ds[0].Detail, "secret-prefix") || strings.Contains(ds[0].Detail, "secret-suffix") {
		t.Fatalf("diagnostic leaked full resolved value: %q", ds[0].Detail)
	}
}

func TestComposeRejectsUnresolvedReferenceFromEnvironment(t *testing.T) {
	t.Setenv("TCHORI_TEST_UNRESOLVED", "secret-${a.b.c}-suffix")
	ty := cty.Object(map[string]cty.Type{"prefix": cty.String})
	_, ds := Compose(map[string]any{"prefix": map[string]any{"env": "TCHORI_TEST_UNRESOLVED"}}, ty, EnvResolve, nil)
	if !ds.HasErrors() || ds[0].Summary != "unresolved reference" || ds[0].Address != "" || !strings.Contains(ds[0].Detail, "prefix") || !strings.Contains(ds[0].Detail, "${a.b.c}") {
		t.Fatalf("Compose diagnostics = %+v, want env-derived unresolved reference", ds)
	}
	if strings.Contains(ds[0].Detail, "secret-") || strings.Contains(ds[0].Detail, "-suffix") {
		t.Fatalf("diagnostic leaked full environment value: %q", ds[0].Detail)
	}
}

func TestComposeUnexpectedAttribute(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{"name": cty.String})
	raw := map[string]any{"name": "web", "nope": true}

	_, diags := Compose(raw, ty, EnvResolve, nil)
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

	got, diags := Compose(raw, ty, EnvResolve, nil)
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

func TestComposeEnvWrapperInResourceConfig(t *testing.T) {
	t.Setenv("TCHORI_TEST_TOKEN", "s3cr3t")
	ty := cty.Object(map[string]cty.Type{"token": cty.String})
	raw := map[string]any{"token": map[string]any{"env": "TCHORI_TEST_TOKEN"}}

	got, diags := Compose(raw, ty, EnvResolve, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	if g := got.GetAttr("token"); !g.RawEquals(cty.StringVal("s3cr3t")) {
		t.Errorf("token = %#v, want resource environment value", g)
	}

	// A single-key "env" object where a MAP is expected stays plain data —
	// the wrapper check applies only at primitives.
	mapTy := cty.Object(map[string]cty.Type{"tags": cty.Map(cty.String)})
	got, mdiags := Compose(map[string]any{"tags": map[string]any{"env": "prod"}}, mapTy, EnvResolve, nil)
	if mdiags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", mdiags)
	}
	if g := got.GetAttr("tags"); !g.RawEquals(cty.MapVal(map[string]cty.Value{"env": cty.StringVal("prod")})) {
		t.Errorf("tags = %#v, want the map to carry the env key as plain data", g)
	}
}

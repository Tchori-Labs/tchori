package sensitive

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
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

func TestNestedTypeLeafRedaction(t *testing.T) {
	for _, nesting := range []string{"single", "list", "set", "map"} {
		t.Run(nesting, func(t *testing.T) {
			element := func(token string) cty.Value {
				return cty.ObjectVal(map[string]cty.Value{"user": cty.StringVal("alice"), "token": cty.StringVal(token)})
			}
			value := element("synthetic-private-value")
			switch nesting {
			case "list":
				value = cty.ListVal([]cty.Value{value})
			case "set":
				value = cty.SetVal([]cty.Value{value, element("second-synthetic-private-value")})
			case "map":
				value = cty.MapVal(map[string]cty.Value{"primary": value})
			}
			block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"credentials": {Type: value.Type(), NestedType: map[string]*provider.Attr{
					"user": {Type: cty.String}, "token": {Type: cty.String, Sensitive: true},
				}},
			}}
			spec, ds := Resolve(block, nil, nil)
			if ds.HasErrors() {
				t.Fatal(ds)
			}
			root := cty.ObjectVal(map[string]cty.Value{"credentials": value})
			if nesting == "set" {
				public, _, recovery, err := spec.Project(root)
				if err != nil {
					t.Fatal(err)
				}
				var decoded map[string][]map[string]any
				if err := json.Unmarshal(public, &decoded); err != nil {
					t.Fatal(err)
				}
				if got := len(decoded["credentials"]); got != 2 {
					t.Fatalf("redaction collapsed set cardinality to %d", got)
				}
				for _, credentials := range decoded["credentials"] {
					if credentials["token"] != nil || credentials["user"] != "alice" {
						t.Fatal("nested schema sensitivity must redact only the secret leaf")
					}
				}
				restored, err := spec.Restore(public, recovery, root.Type())
				if err != nil {
					t.Fatal(err)
				}
				if !restored.RawEquals(root) {
					t.Fatal("restored set lost provider-visible identity")
				}
				return
			}
			out, _, err := spec.Redact(root)
			if err != nil {
				t.Fatal(err)
			}
			credentials := out.GetAttr("credentials")
			if nesting != "single" {
				it := credentials.ElementIterator()
				if !it.Next() {
					t.Fatal("redaction removed the credentials container")
				}
				_, credentials = it.Element()
			}
			if !credentials.GetAttr("token").IsNull() || credentials.GetAttr("user").AsString() != "alice" {
				t.Fatal("nested schema sensitivity must redact only the secret leaf")
			}
		})
	}
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

func TestLiteralExemptionRequiresAuthoredValue(t *testing.T) {
	s, ds := Resolve(testBlock("list"), nil, map[string]any{
		"rules": []any{map[string]any{"token": "authored-public-value"}}, //nolint:gosec // schema attribute name in synthetic redaction input
	})
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	value := cty.ObjectVal(map[string]cty.Value{
		"rules": cty.ListVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("provider-substituted-secret")}),
		}),
	})
	out, _, err := s.Redact(value)
	if err != nil {
		t.Fatal(err)
	}
	if !out.GetAttr("rules").Index(cty.NumberIntVal(0)).GetAttr("token").IsNull() {
		t.Fatal("provider-substituted value inherited a path-only literal exemption")
	}
}

func TestMapKeyPunctuationDoesNotChangeSensitiveLogicalPath(t *testing.T) {
	elementType := cty.Object(map[string]cty.Type{"token": cty.String, "user": cty.String})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"credentials": {
			Type: cty.Map(elementType),
			NestedType: map[string]*provider.Attr{
				"token": {Type: cty.String, Sensitive: true},
				"user":  {Type: cty.String},
			},
		},
	}}
	spec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	for _, key := range []string{`right]bracket`, `left[bracket`, `quote"key`, `back\slash`, `unicodé雪`} {
		t.Run(key, func(t *testing.T) {
			credentials := cty.MapVal(map[string]cty.Value{
				key: cty.ObjectVal(map[string]cty.Value{
					"token": cty.StringVal("synthetic-private-value"),
					"user":  cty.StringVal("alice"),
				}),
			})
			out, _, err := spec.Redact(cty.ObjectVal(map[string]cty.Value{"credentials": credentials}))
			if err != nil {
				t.Fatal(err)
			}
			element := out.GetAttr("credentials").Index(cty.StringVal(key))
			if !element.GetAttr("token").IsNull() || element.GetAttr("user").AsString() != "alice" {
				t.Fatal("map key punctuation changed the logical schema path")
			}
		})
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

func TestSensitiveNestedBlockSetPreservesDistinctElements(t *testing.T) {
	spec, ds := Resolve(testBlock("set"), nil, nil)
	if ds.HasErrors() {
		t.Fatalf("Resolve rejected a supported sensitive set: %+v", ds)
	}
	element := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal(token)})
	}
	value := cty.ObjectVal(map[string]cty.Value{
		"rules": cty.SetVal([]cty.Value{element("first"), element("second")}),
	})
	public, _, recovery, err := spec.Project(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string][]map[string]any
	if err := json.Unmarshal(public, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := len(decoded["rules"]); got != 2 {
		t.Fatalf("redaction collapsed two provider-visible set elements to %d", got)
	}
	for _, rule := range decoded["rules"] {
		if rule["token"] != nil {
			t.Fatal("public projection exposed a set element secret")
		}
	}
	restored, err := spec.Restore(public, recovery, value.Type())
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RawEquals(value) {
		t.Fatal("restored nested block set lost provider-visible identity")
	}
}

func TestWholeSetSensitivityPreservesAuthoritativeMembership(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"label": cty.String, "token": cty.String})
	member := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"label": cty.StringVal("same"), "token": cty.StringVal(token)})
	}
	originalSet := cty.SetVal([]cty.Value{member("first-private"), member("second-private")})
	for _, tc := range []struct {
		name     string
		block    *provider.SchemaBlock
		declared []string
		value    cty.Value
	}{
		{
			name: "provider sensitive set",
			block: &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"members": {Type: cty.Set(memberType), Sensitive: true, NestedType: map[string]*provider.Attr{
					"label": {Type: cty.String}, "token": {Type: cty.String},
				}},
			}},
			value: cty.ObjectVal(map[string]cty.Value{"members": originalSet}),
		},
		{
			name: "declared sensitive set",
			block: &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"members": {Type: cty.Set(memberType), NestedType: map[string]*provider.Attr{
					"label": {Type: cty.String}, "token": {Type: cty.String},
				}},
			}},
			declared: []string{"members"},
			value:    cty.ObjectVal(map[string]cty.Value{"members": originalSet}),
		},
		{
			name: "sensitive ancestor",
			block: &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"container": {
					Type:      cty.Object(map[string]cty.Type{"members": cty.Set(memberType)}),
					Sensitive: true,
					NestedType: map[string]*provider.Attr{
						"members": {Type: cty.Set(memberType), NestedType: map[string]*provider.Attr{
							"label": {Type: cty.String}, "token": {Type: cty.String},
						}},
					},
				},
			}},
			value: cty.ObjectVal(map[string]cty.Value{
				"container": cty.ObjectVal(map[string]cty.Value{"members": originalSet}),
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, ds := Resolve(tc.block, tc.declared, nil)
			if ds.HasErrors() {
				t.Fatal(ds)
			}
			public, _, recovery, err := spec.Project(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if len(recovery) == 0 {
				t.Fatal("whole-set sensitivity omitted authoritative recovery")
			}
			var decoded any
			if err := json.Unmarshal(public, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := countJSONArrays(decoded); got < 2 {
				t.Fatalf("public projection lost set cardinality: %s", public)
			}
			restored, err := spec.Restore(public, recovery, tc.value.Type())
			if err != nil {
				t.Fatal(err)
			}
			if !restored.RawEquals(tc.value) {
				t.Fatal("whole-set sensitivity lost provider-visible membership")
			}
		})
	}
}

func TestFlatTypedSetSensitivityPreservesBoundaries(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"label": cty.String, "token": cty.String})
	member := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"label": cty.StringVal("same"),
			"token": cty.StringVal(token),
		})
	}
	members := cty.SetVal([]cty.Value{member("first-private"), member("second-private")})

	tests := []struct {
		name     string
		block    *provider.SchemaBlock
		declared []string
		value    cty.Value
	}{
		{
			name: "protocol5 flat primitive set direct sensitivity",
			block: &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"members": {Type: cty.Set(cty.String), Sensitive: true},
			}},
			value: cty.ObjectVal(map[string]cty.Value{
				"members": cty.SetVal([]cty.Value{cty.StringVal("first"), cty.StringVal("second")}),
			}),
		},
		{
			name: "protocol6 flat object set declared sensitivity",
			block: &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"members": {Type: cty.Set(memberType)},
			}},
			declared: []string{"members"},
			value:    cty.ObjectVal(map[string]cty.Value{"members": members}),
		},
		{
			name: "flat object contains inherited sensitive set",
			block: &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
				"container": {
					Type:      cty.Object(map[string]cty.Type{"members": cty.Set(memberType)}),
					Sensitive: true,
				},
			}},
			value: cty.ObjectVal(map[string]cty.Value{
				"container": cty.ObjectVal(map[string]cty.Value{"members": members}),
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, ds := Resolve(tc.block, tc.declared, nil)
			if ds.HasErrors() {
				t.Fatal(ds)
			}
			public, _, recovery, err := spec.Project(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if len(recovery) == 0 {
				t.Fatalf("flat typed set omitted recovery: %s", public)
			}
			restored, err := spec.Restore(public, recovery, tc.value.Type())
			if err != nil {
				t.Fatal(err)
			}
			if !restored.RawEquals(tc.value) {
				t.Fatal("flat typed set lost authoritative membership")
			}
		})
	}

	flat := tests[0].block
	for _, tc := range []struct {
		name         string
		value        cty.Value
		wantRecovery bool
	}{
		{
			name: "null",
			value: cty.ObjectVal(map[string]cty.Value{
				"members": cty.NullVal(cty.Set(cty.String)),
			}),
		},
		{
			name: "empty",
			value: cty.ObjectVal(map[string]cty.Value{
				"members": cty.SetValEmpty(cty.String),
			}),
			wantRecovery: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, ds := Resolve(flat, nil, nil)
			if ds.HasErrors() {
				t.Fatal(ds)
			}
			public, _, recovery, err := spec.Project(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if (len(recovery) != 0) != tc.wantRecovery {
				t.Fatalf("recovery presence = %v, want %v", len(recovery) != 0, tc.wantRecovery)
			}
			restored, err := spec.Restore(public, recovery, tc.value.Type())
			if err != nil {
				t.Fatal(err)
			}
			if !restored.RawEquals(tc.value) {
				t.Fatalf("%s set did not round-trip", tc.name)
			}
		})
	}
}

func TestSensitiveMapInsideCapturedSetWithholdsKeys(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{
		"label": cty.String,
		"meta":  cty.Map(cty.String),
	})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"label": {Type: cty.String},
				"meta":  {Type: cty.Map(cty.String), Sensitive: true},
			},
		},
	}}
	member := func(key string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"label": cty.StringVal("same"),
			"meta":  cty.MapVal(map[string]cty.Value{key: cty.StringVal("private-value")}),
		})
	}
	original := cty.ObjectVal(map[string]cty.Value{
		"members": cty.SetVal([]cty.Value{member("PRIVATE_KEY_ONE"), member("PRIVATE_KEY_TWO")}),
	})
	spec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	public, _, recovery, err := spec.Project(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"PRIVATE_KEY_ONE", "PRIVATE_KEY_TWO"} {
		if strings.Contains(string(public), sentinel) {
			t.Fatalf("public projection exposed sensitive map key %q: %s", sentinel, public)
		}
	}
	restored, err := spec.Restore(public, recovery, original.Type())
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RawEquals(original) {
		t.Fatal("captured set did not restore sensitive map keys")
	}
	unknown, err := spec.Unknown(original)
	if err != nil {
		t.Fatal(err)
	}
	planned, _, _, err := spec.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planned), "PRIVATE_KEY") {
		t.Fatalf("planned projection exposed sensitive map keys: %s", planned)
	}
}

func TestSensitiveMapAboveSetWithholdsKeysAndRestores(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"token": cty.String})
	groupType := cty.Object(map[string]cty.Type{"members": cty.Set(memberType)})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"groups": {
			Type:      cty.Map(groupType),
			Sensitive: true,
			NestedType: map[string]*provider.Attr{
				"members": {
					Type: cty.Set(memberType),
					NestedType: map[string]*provider.Attr{
						"token": {Type: cty.String},
					},
				},
			},
		},
	}}
	original := cty.ObjectVal(map[string]cty.Value{
		"groups": cty.MapVal(map[string]cty.Value{
			"PRIVATE_GROUP_KEY": cty.ObjectVal(map[string]cty.Value{
				"members": cty.SetVal([]cty.Value{
					cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("private-token")}),
				}),
			}),
		}),
	})
	spec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	public, _, recovery, err := spec.Project(original)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "PRIVATE_GROUP_KEY") {
		t.Fatalf("public projection exposed sensitive ancestor map key: %s", public)
	}
	if len(recovery) == 0 {
		t.Fatal("sensitive ancestor map omitted authenticated recovery")
	}
	restored, err := spec.Restore(public, recovery, original.Type())
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RawEquals(original) {
		t.Fatal("sensitive ancestor map did not restore exact keys and membership")
	}
	for name, groups := range map[string]cty.Value{
		"null":  cty.NullVal(cty.Map(groupType)),
		"empty": cty.MapValEmpty(groupType),
	} {
		t.Run(name, func(t *testing.T) {
			value := cty.ObjectVal(map[string]cty.Value{"groups": groups})
			projected, _, edgeRecovery, err := spec.Project(value)
			if err != nil {
				t.Fatal(err)
			}
			if len(edgeRecovery) == 0 {
				t.Fatalf("%s sensitive map omitted authenticated recovery", name)
			}
			roundTrip, err := spec.Restore(projected, edgeRecovery, value.Type())
			if err != nil {
				t.Fatal(err)
			}
			if !roundTrip.RawEquals(value) {
				t.Fatalf("%s sensitive map did not round-trip exactly", name)
			}
			if _, err := spec.Restore(projected, nil, value.Type()); err == nil {
				t.Fatalf("%s sensitive map accepted missing recovery", name)
			}
		})
	}
	unknown, err := spec.Unknown(original)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.GetAttr("groups").IsKnown() {
		t.Fatal("plan retained the sensitive map shape instead of withholding it")
	}
}

func countJSONArrays(value any) int {
	switch value := value.(type) {
	case []any:
		return len(value)
	case map[string]any:
		maximum := 0
		for _, child := range value {
			if count := countJSONArrays(child); count > maximum {
				maximum = count
			}
		}
		return maximum
	default:
		return 0
	}
}

func TestSensitiveSetMaskPreservesMembershipChanges(t *testing.T) {
	spec, ds := Resolve(testBlock("set"), nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	element := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal(token)})
	}
	value := func(tokens ...string) cty.Value {
		elements := make([]cty.Value, len(tokens))
		for i, token := range tokens {
			elements[i] = element(token)
		}
		return cty.ObjectVal(map[string]cty.Value{"rules": cty.SetVal(elements)})
	}
	prior, err := spec.Mask(value("first", "second"))
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]cty.Value{
		"member changed": value("first", "third"),
		"member added":   value("first", "second", "third"),
		"member removed": value("first"),
	} {
		t.Run(name, func(t *testing.T) {
			masked, err := spec.Mask(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if masked.RawEquals(prior) {
				t.Fatal("comparison mask hid a sensitive set membership transition")
			}
		})
	}
}

func TestSensitiveSetRecoveryNestedMapPunctuationAndTamper(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"label": cty.String, "token": cty.String})
	groupType := cty.Object(map[string]cty.Type{"members": cty.Set(memberType)})
	resourceType := cty.Object(map[string]cty.Type{"groups": cty.Map(groupType)})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"groups": {
			Type: cty.Map(groupType),
			NestedType: map[string]*provider.Attr{
				"members": {
					Type: cty.Set(memberType),
					NestedType: map[string]*provider.Attr{
						"label": {Type: cty.String},
						"token": {Type: cty.String, Sensitive: true},
					},
				},
			},
		},
	}}
	spec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	member := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"label": cty.StringVal("same"), "token": cty.StringVal(token)})
	}
	const key = `quote\"]left[right\\雪`
	original := cty.ObjectVal(map[string]cty.Value{
		"groups": cty.MapVal(map[string]cty.Value{
			key: cty.ObjectVal(map[string]cty.Value{
				"members": cty.SetVal([]cty.Value{member("first-private"), member("second-private")}),
			}),
		}),
	})
	public, _, recovery, err := spec.Project(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(public) == "" || len(recovery) == 0 {
		t.Fatal("projection omitted public or recovery data")
	}
	restored, err := spec.Restore(public, recovery, resourceType)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RawEquals(original) {
		t.Fatal("punctuated map key changed recovery identity")
	}
	if _, err := spec.Restore(public, nil, resourceType); err == nil {
		t.Fatal("Restore accepted ambiguous legacy set state without recovery")
	}

	var changedPublic map[string]any
	if err := json.Unmarshal(public, &changedPublic); err != nil {
		t.Fatal(err)
	}
	groups := changedPublic["groups"].(map[string]any)
	members := groups[key].(map[string]any)["members"].([]any)
	members[0].(map[string]any)["label"] = "tampered"
	tamperedPublic, err := json.Marshal(changedPublic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Restore(tamperedPublic, recovery, resourceType); err == nil {
		t.Fatal("Restore accepted recovery bound to a different public projection")
	}

	var changedRecovery recoveryPayload
	if err := json.Unmarshal(recovery, &changedRecovery); err != nil {
		t.Fatal(err)
	}
	changedRecovery.Values[0].Path[1].Name = "other"
	tamperedRecovery, err := json.Marshal(changedRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Restore(public, tamperedRecovery, resourceType); err == nil {
		t.Fatal("Restore accepted a recovery path for another map element")
	}
}

func TestSensitiveSetUnknownProjectionPreservesCardinality(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"label": cty.String, "token": cty.String})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"label": {Type: cty.String},
				"token": {Type: cty.String, Sensitive: true},
			},
		},
	}}
	spec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	member := func(label string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"label": cty.StringVal(label), "token": cty.UnknownVal(cty.String)})
	}
	value := cty.ObjectVal(map[string]cty.Value{
		"members": cty.SetVal([]cty.Value{member("first"), member("second")}),
	})
	public, _, unknown, err := spec.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string][]any
	if err := json.Unmarshal(public, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded["members"]) != 2 || len(unknown) != 2 {
		t.Fatalf("unknown projection cardinality=%d paths=%v", len(decoded["members"]), unknown)
	}
	if _, _, _, err := spec.Project(value); err == nil {
		t.Fatal("Project serialized unknown state")
	}
}

func TestSensitiveSetRecoverySurvivesRemovedDeclarationAndEmptySet(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"token": cty.String})
	resourceType := cty.Object(map[string]cty.Type{"members": cty.Set(memberType)})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"token": {Type: cty.String},
			},
		},
	}}
	declared, ds := Resolve(block, []string{"members.token"}, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	empty := cty.ObjectVal(map[string]cty.Value{"members": cty.SetValEmpty(memberType)})
	public, _, recovery, err := declared.Project(empty)
	if err != nil {
		t.Fatal(err)
	}
	withoutDeclaration, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	nextPublic, _, nextRecovery, err := withoutDeclaration.SanitizeJSON(public, recovery, resourceType, declared.Paths(), declared.Paths())
	if err != nil {
		t.Fatal(err)
	}
	if len(nextRecovery) == 0 {
		t.Fatal("removed declaration dropped sensitive set recovery")
	}
	restored, err := declared.Restore(nextPublic, nextRecovery, resourceType)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RawEquals(empty) {
		t.Fatal("empty sensitive set did not survive sanitization")
	}
}

func TestSensitiveSetRecoveryRotatesAcrossPolicyExpansion(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"label": cty.String, "token": cty.String})
	resourceType := cty.Object(map[string]cty.Type{
		"members": cty.Set(memberType),
		"note":    cty.String,
	})
	oldBlock := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"label": {Type: cty.String},
				"token": {Type: cty.String, Sensitive: true},
			},
		},
		"note": {Type: cty.String},
	}}
	newBlock := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"label": {Type: cty.String, Sensitive: true},
				"token": {Type: cty.String, Sensitive: true},
			},
		},
		"note": {Type: cty.String, Sensitive: true},
	}}
	oldSpec, ds := Resolve(oldBlock, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	newSpec, ds := Resolve(newBlock, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	member := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"label": cty.StringVal("newly-sensitive"), "token": cty.StringVal(token)})
	}
	original := cty.ObjectVal(map[string]cty.Value{
		"members": cty.SetVal([]cty.Value{member("first-private"), member("second-private")}),
		"note":    cty.StringVal("newly-sensitive-note"),
	})
	public, _, recovery, err := oldSpec.Project(original)
	if err != nil {
		t.Fatal(err)
	}
	var legacy recoveryPayload
	if err := json.Unmarshal(recovery, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Version = 1
	legacy.ProjectionPaths = nil
	legacy.Sets = legacy.Values
	legacy.Values = nil
	recovery, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	restoredLegacy, err := newSpec.RestoreProjected(public, recovery, resourceType, oldSpec.Paths())
	if err != nil {
		t.Fatalf("version 1 recovery could not use recorded generation paths: %v", err)
	}
	if !restoredLegacy.GetAttr("members").RawEquals(original.GetAttr("members")) {
		t.Fatal("version 1 recovery lost authoritative membership")
	}
	rotatedPublic, _, rotatedRecovery, err := newSpec.SanitizeJSON(public, recovery, resourceType, oldSpec.Paths(), newSpec.Paths())
	if err != nil {
		t.Fatalf("policy expansion could not rotate recovery: %v", err)
	}
	if bytes := string(rotatedPublic); strings.Contains(bytes, "newly-sensitive") {
		t.Fatalf("expanded policy left newly sensitive values public: %s", rotatedPublic)
	}
	restored, err := newSpec.Restore(rotatedPublic, rotatedRecovery, resourceType)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.GetAttr("members").RawEquals(original.GetAttr("members")) {
		t.Fatal("policy expansion lost authoritative membership")
	}
	if !restored.GetAttr("note").IsNull() {
		t.Fatal("expanded non-set sensitivity was not withheld")
	}
}

func TestNewlyAffectedSensitiveSetRotatesFromPublicState(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"label": cty.String, "token": cty.String})
	resourceType := cty.Object(map[string]cty.Type{"members": cty.Set(memberType)})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"label": {Type: cty.String},
				"token": {Type: cty.String},
			},
		},
	}}
	oldSpec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	newSpec, ds := Resolve(block, []string{"members.token"}, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	member := func(token string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"label": cty.StringVal("same"), "token": cty.StringVal(token)})
	}
	original := cty.ObjectVal(map[string]cty.Value{
		"members": cty.SetVal([]cty.Value{member("first-private"), member("second-private")}),
	})
	public, _, recovery, err := oldSpec.Project(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery) != 0 {
		t.Fatal("non-sensitive generation unexpectedly emitted recovery")
	}
	rotatedPublic, _, rotatedRecovery, err := newSpec.SanitizeJSON(public, recovery, resourceType, oldSpec.Paths(), newSpec.Paths())
	if err != nil {
		t.Fatal(err)
	}
	if len(rotatedRecovery) == 0 || strings.Contains(string(rotatedPublic), "private") {
		t.Fatalf("newly affected set was not rotated safely: %s", rotatedPublic)
	}
	restored, err := newSpec.Restore(rotatedPublic, rotatedRecovery, resourceType)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.RawEquals(original) {
		t.Fatal("newly affected set lost authoritative membership")
	}
}

func TestSensitiveSetRecoveryReturnsTupleArityCorruption(t *testing.T) {
	memberType := cty.Object(map[string]cty.Type{"token": cty.String})
	resourceType := cty.Object(map[string]cty.Type{
		"members": cty.Set(memberType),
		"pair":    cty.Tuple([]cty.Type{cty.String, cty.String}),
	})
	block := &provider.SchemaBlock{Attributes: map[string]*provider.Attr{
		"members": {
			Type: cty.Set(memberType),
			NestedType: map[string]*provider.Attr{
				"token": {Type: cty.String, Sensitive: true},
			},
		},
		"pair": {Type: cty.Tuple([]cty.Type{cty.String, cty.String})},
	}}
	spec, ds := Resolve(block, nil, nil)
	if ds.HasErrors() {
		t.Fatal(ds)
	}
	original := cty.ObjectVal(map[string]cty.Value{
		"members": cty.SetVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{"token": cty.StringVal("private")}),
		}),
		"pair": cty.TupleVal([]cty.Value{cty.StringVal("left"), cty.StringVal("right")}),
	})
	public, _, recovery, err := spec.Project(original)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(public, &root); err != nil {
		t.Fatal(err)
	}
	root["pair"] = []any{"left"}
	tamperedPublic, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var payload recoveryPayload
	if err := json.Unmarshal(recovery, &payload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(tamperedPublic)
	payload.ProjectionSHA256 = sum[:]
	tamperedRecovery, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("tuple corruption panicked: %v", recovered)
		}
	}()
	if _, err := spec.Restore(tamperedPublic, tamperedRecovery, resourceType); err == nil ||
		!strings.Contains(err.Error(), "tuple") {
		t.Fatalf("Restore error = %v, want tuple arity corruption", err)
	}
}

func TestUnknownDeclaredPath(t *testing.T) {
	_, ds := Resolve(testBlock("list"), []string{"typo"}, nil)
	if !ds.HasErrors() {
		t.Fatal("expected diagnostic")
	}
}

func fmtSlice(v []string) string { return fmt.Sprint(v) }

package plan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderHumanCreate(t *testing.T) {
	pl := renderPlan(&Change{Address: "thing.demo", Action: "create", Before: raw(`null`), After: raw(`{"enabled":true,"name":"demo","optional":null,"token":null}`), UnknownAfter: []string{"token"}})
	pl.Summary.Create = 1
	want := "+ thing.demo\n" +
		"    + enabled = true\n" +
		"    + name = \"demo\"\n" +
		"    + optional = null\n" +
		"    + token = (known after apply)\n" +
		"Plan: 1 to create, 0 to update, 0 to delete, 0 to replace.\n"
	assertRendered(t, pl, nil, want)
}

func TestRenderHumanUpdateUnknownAndPathNormalization(t *testing.T) {
	pl := renderPlan(&Change{
		Address: "thing.demo", Action: "update",
		Before:       raw(`{"echo":"degraded:unhealthy","rules":[{"name":"old"}],"tags":{"parent":"old"}}`),
		After:        raw(`{"echo":null,"rules":[{"name":"new"}],"tags":{"parent":"new"}}`),
		UnknownAfter: []string{"echo"}, RequiresReplace: []string{`tags["parent"]`, "rules.0.name"},
	})
	pl.Summary.Update = 1
	want := "~ thing.demo\n" +
		"    ~ echo = \"degraded:unhealthy\" -> (known after apply)\n" +
		"    ~ rules[0].name = \"old\" -> \"new\" # forces replacement\n" +
		"    ~ tags.parent = \"old\" -> \"new\" # forces replacement\n" +
		"Plan: 0 to create, 1 to update, 0 to delete, 0 to replace.\n"
	assertRendered(t, pl, nil, want)
}

func TestRenderHumanReplaceAndDelete(t *testing.T) {
	replace := &Change{Address: "thing.a", Action: "replace", Before: raw(`{"name":"old"}`), After: raw(`{"name":"new"}`), RequiresReplace: []string{"name"}}
	deleted := &Change{Address: "thing.b", Action: "delete", Before: raw(`{"secret":"must-not-print"}`), After: raw(`null`)}
	pl := renderPlan(replace, deleted)
	pl.Summary.Replace, pl.Summary.Delete = 1, 1
	want := "-/+ thing.a\n" +
		"    ~ name = \"old\" -> \"new\" # forces replacement\n" +
		"- thing.b\n" +
		"Plan: 0 to create, 0 to update, 1 to delete, 1 to replace.\n"
	assertRendered(t, pl, nil, want)
}

func TestRenderHumanNoOpOnly(t *testing.T) {
	pl := renderPlan(&Change{Address: "thing.demo", Action: "no-op", Before: raw(`{"name":"same"}`), After: raw(`{"name":"same"}`)})
	assertRendered(t, pl, nil, "No changes. Configuration matches state.\n")
}

func TestRenderHumanDriftWithAndWithoutChanges(t *testing.T) {
	drift := &Drift{Address: "thing.demo", Before: raw(`{"echo":"healthy"}`), After: raw(`{"echo":"degraded:unhealthy"}`), Paths: []string{"echo"}}
	without := renderPlan()
	without.Drift = []*Drift{drift}
	wantNote := "Note: objects have changed outside tchori since the last apply.\n" +
		"  ~ thing.demo\n" +
		"    ~ echo = \"healthy\" -> \"degraded:unhealthy\"\n\n"
	assertRendered(t, without, nil, wantNote+"No changes. Configuration matches state.\n")

	with := renderPlan(&Change{Address: "thing.demo", Action: "update", Before: raw(`{"name":"a"}`), After: raw(`{"name":"b"}`)})
	with.Drift, with.Summary.Update = []*Drift{drift}, 1
	assertRendered(t, with, nil, wantNote+"~ thing.demo\n    ~ name = \"a\" -> \"b\"\nPlan: 0 to create, 1 to update, 0 to delete, 0 to replace.\n")
}

func TestRenderHumanVanishedDrift(t *testing.T) {
	pl := renderPlan()
	pl.Drift = []*Drift{{Address: "thing.gone", Before: raw(`{"name":"gone"}`), After: raw(`null`)}}
	want := "Note: objects have changed outside tchori since the last apply.\n" +
		"  ~ thing.gone (object no longer exists)\n\n" +
		"No changes. Configuration matches state.\n"
	assertRendered(t, pl, nil, want)
}

func TestRenderHumanSensitiveAndLongValues(t *testing.T) {
	long := strings.Repeat("x", 121)
	pl := renderPlan(&Change{Address: "thing.demo", Action: "update", Before: raw(`{"secret":"old","text":"short"}`), After: mustRaw(map[string]any{"secret": "new", "text": long})})
	pl.Summary.Update = 1
	want := "~ thing.demo\n" +
		"    ~ secret = (sensitive value) -> (sensitive value)\n" +
		"    ~ text = \"short\" -> \"" + strings.Repeat("x", 120) + "\"… (121 runes)\n" +
		"Plan: 0 to create, 1 to update, 0 to delete, 0 to replace.\n"
	assertRendered(t, pl, func(_ string, path string) bool { return path == "secret" }, want)
}

func renderPlan(changes ...*Change) *Plan {
	return &Plan{FormatVersion: FormatVersion, Changes: changes}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func assertRendered(t *testing.T, pl *Plan, sensitive func(string, string) bool, want string) {
	t.Helper()
	var out bytes.Buffer
	if err := RenderHuman(&out, pl, sensitive); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if got := out.String(); got != want {
		t.Fatalf("RenderHuman output:\n%s\nwant:\n%s", got, want)
	}
}

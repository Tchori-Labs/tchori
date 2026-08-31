package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestChangedPathsNestedCollectionsAndNulls(t *testing.T) {
	before := map[string]any{
		"tags":  map[string]any{"parent": "old", "same": true},
		"rules": []any{map[string]any{"name": "old", "enabled": true}},
		"maybe": nil,
		"empty": "",
	}
	after := map[string]any{
		"tags":  map[string]any{"parent": "new", "same": true},
		"rules": []any{map[string]any{"name": "new", "enabled": true}},
		"maybe": "set",
		"empty": nil,
	}
	want := []string{"empty", "maybe", "rules[0].name", "tags.parent"}
	if got := changedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("changedPaths() = %#v, want %#v", got, want)
	}
}

func TestChangedPathsPresenceAndEquality(t *testing.T) {
	before := map[string]any{"same": json.Number("2"), "removed": false}
	after := map[string]any{"same": json.Number("2"), "added": true}
	want := []string{"added", "removed"}
	if got := changedPaths(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("changedPaths() = %#v, want %#v", got, want)
	}
	if got := changedPaths(before, before); len(got) != 0 {
		t.Fatalf("equal values changed at %#v", got)
	}
}

func TestNormalizePath(t *testing.T) {
	pairs := [][2]string{
		{`tags["parent"]`, "tags.parent"},
		{"rules[0].name", "rules.0.name"},
		{`matrix["parent"][0].name`, "matrix.parent.0.name"},
	}
	for _, pair := range pairs {
		if got, want := normalizePath(pair[0]), normalizePath(pair[1]); got != want {
			t.Errorf("normalizePath(%q) = %q, normalizePath(%q) = %q", pair[0], got, pair[1], want)
		}
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{"", `""`},
		{nil, "null"},
		{true, "true"},
		{json.Number("12.50"), "12.50"},
	}
	for _, test := range tests {
		if got := formatValue(test.value); got != test.want {
			t.Errorf("formatValue(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestNewDriftVanishedAndFinalizeSorting(t *testing.T) {
	entry := &Drift{Address: "thing.gone", Before: json.RawMessage(`{"name":"gone"}`), After: json.RawMessage("null")}
	pl := &Plan{Drift: []*Drift{{Address: "thing.z"}, entry, {Address: "thing.a"}}}
	finalize(pl)
	if pl.HasChanges() {
		t.Fatal("drift must not affect HasChanges")
	}
	want := []string{"thing.a", "thing.gone", "thing.z"}
	for i, address := range want {
		if pl.Drift[i].Address != address {
			t.Fatalf("drift[%d] = %q, want %q", i, pl.Drift[i].Address, address)
		}
	}
	if len(entry.Paths) != 0 || string(entry.After) != "null" {
		t.Fatalf("vanished drift = %#v", entry)
	}
}

func TestFormatValueTruncationBoundary(t *testing.T) {
	exact := strings.Repeat("é", maxRenderedValueRunes)
	if got, want := formatValue(exact), `"`+exact+`"`; got != want {
		t.Fatalf("boundary value was truncated: %q", got)
	}
	long := exact + "界"
	got := formatValue(long)
	if !strings.Contains(got, "… (121 runes)") || strings.Contains(got, "界") {
		t.Fatalf("long value not visibly rune-truncated: %q", got)
	}
}

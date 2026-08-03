package provider

import (
	"strings"
	"testing"

	"github.com/tchori-labs/tchori/internal/diag"
)

func TestContextNonJSONHint(t *testing.T) {
	tests := []struct {
		name     string
		ds       diag.Diagnostics
		wantHint bool
	}{
		{
			name:     "marker in summary",
			ds:       diag.Diagnostics{diag.Errorf("", "decode: "+nonJSONResponseMarker, "provider detail")},
			wantHint: true,
		},
		{
			name:     "marker in detail",
			ds:       diag.Diagnostics{diag.Errorf("", "Error reading project", "decoding response: "+nonJSONResponseMarker+" looking for beginning of value")},
			wantHint: true,
		},
		{
			name:     "marker absent",
			ds:       diag.Diagnostics{diag.Errorf("", "Error reading project", "connection refused")},
			wantHint: false,
		},
		{
			name:     "warning marker does not explain a failure",
			ds:       diag.Diagnostics{diag.Warnf("", "decode warning", nonJSONResponseMarker)},
			wantHint: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeHasErrors := test.ds.HasErrors()
			got := Context("tchoritest_thing.web", test.ds)
			wantLen := len(test.ds)
			if test.wantHint {
				wantLen++
			}
			if len(got) != wantLen {
				t.Fatalf("len(Context) = %d, want %d: %#v", len(got), wantLen, got)
			}
			if got.HasErrors() != beforeHasErrors {
				t.Fatalf("HasErrors changed from %v to %v", beforeHasErrors, got.HasErrors())
			}
			if got[0].Address != "tchoritest_thing.web" {
				t.Errorf("provider diagnostic Address = %q", got[0].Address)
			}
			if test.wantHint {
				hint := got[len(got)-1]
				if hint.Severity != diag.Warning || hint.Summary != "provider received a non-JSON response (HTML)" {
					t.Errorf("unexpected hint: %#v", hint)
				}
				if hint.Address != "tchoritest_thing.web" {
					t.Errorf("hint Address = %q", hint.Address)
				}
				for _, phrase := range []string{"identity-aware proxy", "Cloudflare Access", "web UI", "API base URL", "expired", "rejected API token", "direct credentialed request", "returns JSON"} {
					if !strings.Contains(hint.Detail, phrase) {
						t.Errorf("hint Detail does not contain %q: %q", phrase, hint.Detail)
					}
				}
			}
		})
	}
}

func TestContextAppendsOneHintForMultipleErrors(t *testing.T) {
	ds := diag.Diagnostics{
		diag.Errorf("", "first "+nonJSONResponseMarker, "first detail"),
		diag.Errorf("name", "second error", "second "+nonJSONResponseMarker),
	}
	got := Context("tchoritest_thing.web", ds)
	if len(got) != 3 {
		t.Fatalf("len(Context) = %d, want 3: %#v", len(got), got)
	}
	if got[0].Address != "tchoritest_thing.web" || got[1].Address != "tchoritest_thing.web.name" {
		t.Fatalf("addresses = %q, %q", got[0].Address, got[1].Address)
	}
	if got[2].Severity != diag.Warning {
		t.Fatalf("last diagnostic = %#v, want warning hint", got[2])
	}
}

func TestContextEmptyBatch(t *testing.T) {
	var nilBatch diag.Diagnostics
	if got := Context("tchoritest_thing.web", nilBatch); got != nil {
		t.Fatalf("nil batch became %#v", got)
	}
	if got := Context("tchoritest_thing.web", diag.Diagnostics{}); len(got) != 0 {
		t.Fatalf("empty batch became %#v", got)
	}
}

func TestContextEmptyAddressDoesNotMutateInput(t *testing.T) {
	backing := make(diag.Diagnostics, 1, 3)
	backing[0] = diag.Errorf("name", "Error reading project", "decoding: "+nonJSONResponseMarker)
	got := Context("", backing)
	if len(got) != 2 || got[0].Address != "name" || got[1].Address != "" {
		t.Fatalf("Context with empty address = %#v", got)
	}
	got[0].Summary = "changed"
	if backing[0].Summary != "Error reading project" {
		t.Fatal("Context mutated or aliased the input slice")
	}
	if len(backing) != 1 {
		t.Fatalf("input length = %d, want 1", len(backing))
	}
}

func TestContextDoesNotMutateInput(t *testing.T) {
	original := diag.Diagnostics{diag.Errorf("name", "Error reading project", "decoding: "+nonJSONResponseMarker)}
	got := Context("tchoritest_thing.web", original)
	if original[0].Address != "name" || original[0].Summary != "Error reading project" || len(original) != 1 {
		t.Fatalf("input mutated: %#v", original)
	}
	got[0].Summary = "changed"
	if original[0].Summary != "Error reading project" {
		t.Fatal("output aliases input")
	}
}

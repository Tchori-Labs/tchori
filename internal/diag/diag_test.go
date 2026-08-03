package diag

import (
	"bytes"
	"testing"
)

func testDiagnostics() Diagnostics {
	return Diagnostics{
		Errorf("null_resource.a", "resource failed", "underlying cause"),
		Warnf("", "deprecated attribute", ""),
		Errorf("metaads_boost.a", "reference cycle", "metaads_boost.a → metaads_boost.b\nmetaads_boost.b → metaads_boost.a"),
	}
}

func TestDiagnosticsInContext(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{name: "empty address", addr: "", want: "tchoritest_thing.web"},
		{name: "attribute path", addr: "name", want: "tchoritest_thing.web.name"},
		{name: "nested attribute path", addr: "tags.env", want: "tchoritest_thing.web.tags.env"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := Diagnostics{Errorf(test.addr, "provider summary", "provider detail")}
			got := original.InContext("tchoritest_thing.web")
			if got[0].Address != test.want {
				t.Fatalf("Address = %q, want %q", got[0].Address, test.want)
			}
			if got[0].Severity != Error || got[0].Summary != "provider summary" || got[0].Detail != "provider detail" {
				t.Fatalf("non-address fields changed: %#v", got[0])
			}
			if original[0].Address != test.addr {
				t.Fatalf("input was mutated: Address = %q, want %q", original[0].Address, test.addr)
			}
			got[0].Summary = "mutated output"
			if original[0].Summary != "provider summary" {
				t.Fatal("output aliases the input slice")
			}
		})
	}
}

func TestDiagnosticsInContextEmptyContextUnchanged(t *testing.T) {
	original := Diagnostics{Errorf("name", "provider summary", "provider detail")}
	got := original.InContext("")
	if got[0] != original[0] {
		t.Fatalf("InContext(\"\") = %#v, want %#v", got, original)
	}
}

func TestDiagnosticsInContextNil(t *testing.T) {
	var original Diagnostics
	if got := original.InContext("tchoritest_thing.web"); got != nil {
		t.Fatalf("InContext on nil = %#v, want nil", got)
	}
}

func TestHasErrors(t *testing.T) {
	cases := []struct {
		name string
		ds   Diagnostics
		want bool
	}{
		{"empty", Diagnostics{}, false},
		{"only warning", Diagnostics{Warnf("", "deprecated attribute", "")}, false},
		{"error present", Diagnostics{
			Warnf("", "deprecated attribute", ""),
			Errorf("null_resource.a", "resource failed", ""),
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ds.HasErrors(); got != c.want {
				t.Errorf("HasErrors() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestEmitCompact(t *testing.T) {
	var buf bytes.Buffer
	Emit(&buf, testDiagnostics(), false)

	want := `{"severity":"error","summary":"resource failed","detail":"underlying cause","address":"null_resource.a"}
{"severity":"warning","summary":"deprecated attribute"}
{"severity":"error","summary":"reference cycle","detail":"metaads_boost.a → metaads_boost.b\nmetaads_boost.b → metaads_boost.a","address":"metaads_boost.a"}
`
	if got := buf.String(); got != want {
		t.Errorf("Emit(pretty=false) =\n%q\nwant\n%q", got, want)
	}
}

func TestEmitPretty(t *testing.T) {
	var buf bytes.Buffer
	Emit(&buf, testDiagnostics(), true)

	want := `Error: resource failed (null_resource.a)
  underlying cause
Warning: deprecated attribute
Error: reference cycle (metaads_boost.a)
  metaads_boost.a → metaads_boost.b
  metaads_boost.b → metaads_boost.a
`
	if got := buf.String(); got != want {
		t.Errorf("Emit(pretty=true) =\n%q\nwant\n%q", got, want)
	}
}

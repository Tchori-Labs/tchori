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

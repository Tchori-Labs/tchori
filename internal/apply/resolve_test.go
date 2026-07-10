package apply

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// TestResolvePlannedUnknowns is a table-driven unit test for
// resolvePlannedUnknowns's documented invariants. Unlike apply_test.go's
// suite, it needs no provider subprocess: it calls the unexported function
// directly (this file lives in package apply, not apply_test) with
// hand-built cty.Values, so it stays fast and exercises the function in
// isolation from ApplyResource, state and config plumbing.
func TestResolvePlannedUnknowns(t *testing.T) {
	cases := []struct {
		name    string
		planned cty.Value
		cfgVal  cty.Value
		want    cty.Value
	}{
		{
			name:    "known planned leaf never overridden by differing cfg value",
			planned: cty.StringVal("planned-value"),
			cfgVal:  cty.StringVal("cfg-value-different"),
			want:    cty.StringVal("planned-value"),
		},
		{
			name:    "null planned leaf stays null",
			planned: cty.NullVal(cty.String),
			cfgVal:  cty.StringVal("cfg-value"),
			want:    cty.NullVal(cty.String),
		},
		{
			name:    "unknown leaf with concrete cfg gets cfg value",
			planned: cty.UnknownVal(cty.String),
			cfgVal:  cty.StringVal("concrete"),
			want:    cty.StringVal("concrete"),
		},
		{
			name:    "unknown computed leaf with null cfg stays unknown",
			planned: cty.UnknownVal(cty.String),
			cfgVal:  cty.NullVal(cty.String),
			want:    cty.UnknownVal(cty.String),
		},
		{
			// Regression guard: cty.MapVal panics on an empty map (it can't
			// infer an element type from zero entries), so
			// resolvePlannedUnknowns must short-circuit on an empty planned
			// map before ever assembling a result map to hand to
			// cty.MapVal. cfgVal is deliberately null here too, so the case
			// also proves the empty-map return happens before any
			// cfgVal.AsValueMap() access (which would itself panic on a
			// null value).
			name:    "empty-map guard (no panic)",
			planned: cty.MapValEmpty(cty.String),
			cfgVal:  cty.NullVal(cty.Map(cty.String)),
			want:    cty.MapValEmpty(cty.String),
		},
		{
			// Lists are returned unchanged (not recursed into), so an
			// unknown element inside a list passes through untouched even
			// though cfgVal holds a concrete list of the same length.
			name: "unknown inside a list passes through unchanged",
			planned: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
			cfgVal: cty.ListVal([]cty.Value{
				cty.StringVal("cfg1"),
				cty.StringVal("cfg2"),
			}),
			want: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolvePlannedUnknowns(c.planned, c.cfgVal)
			if !got.RawEquals(c.want) {
				t.Errorf("resolvePlannedUnknowns(%#v, %#v) = %#v, want %#v", c.planned, c.cfgVal, got, c.want)
			}
		})
	}
}

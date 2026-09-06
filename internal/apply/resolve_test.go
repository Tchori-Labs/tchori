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
			// TC-033 / Tchori-Labs/tchori-internal#11 regression: lists now recurse
			// per-index, so an unknown element resolves to the corresponding
			// concrete cfg element while a known planned element is left
			// untouched even though cfgVal differs there.
			name: "unknown inside a list resolves per-index",
			planned: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
			cfgVal: cty.ListVal([]cty.Value{
				cty.StringVal("cfg1"),
				cty.StringVal("cfg2-different"),
			}),
			want: cty.ListVal([]cty.Value{
				cty.StringVal("cfg1"),
				cty.StringVal("known"),
			}),
		},
		{
			name: "list length mismatch returns planned unchanged",
			planned: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
			}),
			cfgVal: cty.ListVal([]cty.Value{
				cty.StringVal("cfg1"),
				cty.StringVal("cfg2"),
			}),
			want: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
			}),
		},
		{
			// Mirrors the empty-map guard: cty.ListVal panics on an empty
			// slice, so the empty-list branch must return before ever
			// touching cfgVal (deliberately null here too).
			name:    "empty-list guard (no panic)",
			planned: cty.ListValEmpty(cty.String),
			cfgVal:  cty.NullVal(cty.List(cty.String)),
			want:    cty.ListValEmpty(cty.String),
		},
		{
			name: "unknown list element with null cfg element stays unknown",
			planned: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
			cfgVal: cty.ListVal([]cty.Value{
				cty.NullVal(cty.String),
				cty.StringVal("cfg2"),
			}),
			want: cty.ListVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
		},
		{
			// The issue's exact nesting shape: a list of objects, each
			// holding another list of objects with a string leaf, two
			// levels deep. An unknown leaf resolves to the concrete cfg
			// value while a sibling null/computed attr (e.g. a
			// provider-assigned "id") stays untouched.
			name: "nested list-of-object-of-list-of-object resolves deep unknown",
			planned: cty.ListVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{
					"id": cty.UnknownVal(cty.String),
					"include": cty.ListVal([]cty.Value{
						cty.ObjectVal(map[string]cty.Value{
							"token_id": cty.UnknownVal(cty.String),
						}),
					}),
				}),
			}),
			cfgVal: cty.ListVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{
					"id": cty.NullVal(cty.String),
					"include": cty.ListVal([]cty.Value{
						cty.ObjectVal(map[string]cty.Value{
							"token_id": cty.StringVal("concrete-token-id"),
						}),
					}),
				}),
			}),
			want: cty.ListVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{
					"id": cty.UnknownVal(cty.String),
					"include": cty.ListVal([]cty.Value{
						cty.ObjectVal(map[string]cty.Value{
							"token_id": cty.StringVal("concrete-token-id"),
						}),
					}),
				}),
			}),
		},
		{
			name: "set with unknown element remains provider planned",
			planned: cty.SetVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
			cfgVal: cty.SetVal([]cty.Value{
				cty.StringVal("cfg1"),
				cty.StringVal("cfg2"),
			}),
			want: cty.SetVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
		},
		{
			name: "set with null cfg returns planned unchanged",
			planned: cty.SetVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
			cfgVal: cty.NullVal(cty.Set(cty.String)),
			want: cty.SetVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.StringVal("known"),
			}),
		},
		{
			name: "tuple per-index merge",
			planned: cty.TupleVal([]cty.Value{
				cty.UnknownVal(cty.String),
				cty.NumberIntVal(1),
			}),
			cfgVal: cty.TupleVal([]cty.Value{
				cty.StringVal("concrete"),
				cty.NumberIntVal(2),
			}),
			want: cty.TupleVal([]cty.Value{
				cty.StringVal("concrete"),
				cty.NumberIntVal(1),
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

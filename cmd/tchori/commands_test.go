package main

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/state"
)

// TestPlanSensitivePredicateUnionsStateAndConfigSensitivePaths guards against
// Tchori-Labs/tchori's high-severity sensitive-path bug: planSensitivePredicate
// must union config-declared sensitive_attributes with state-recorded
// SensitivePaths, not let a resource still present in config overwrite the
// state's recollection. A resource whose sensitive_attributes declaration was
// removed from config, but whose state still recalls a path as sensitive from
// a prior run, must keep printing that path as withheld on refresh.
func TestPlanSensitivePredicateUnionsStateAndConfigSensitivePaths(t *testing.T) {
	const addr = "tchoritest_thing.demo"

	cfg := &config.Config{
		Resources: map[string]*config.Resource{
			addr: {
				Address:  addr,
				Type:     "tchoritest_thing",
				Provider: "tchoritest",
				// No SensitiveAttributes: config no longer declares "note" sensitive.
			},
		},
	}
	st := &state.State{
		Resources: map[string]*state.ResourceState{
			addr: {
				Type:           "tchoritest_thing",
				Provider:       "tchoritest",
				SensitivePaths: []string{"note"}, // recorded sensitive by a prior run
			},
		},
	}
	schemas := map[string]*provider.ProviderSchemas{
		"tchoritest": {
			ResourceTypes: map[string]*provider.Schema{
				"tchoritest_thing": {
					Block: &provider.SchemaBlock{
						Attributes: map[string]*provider.Attr{
							"note": {Type: cty.String, Optional: true}, // not schema-sensitive
						},
					},
				},
			},
		},
	}

	predicate := planSensitivePredicate(cfg, st, schemas)
	if !predicate(addr, "note") {
		t.Fatal("predicate(addr, \"note\") = false, want true: config no longer declares \"note\" sensitive but state still recalls it, and the still-secret remote value must stay withheld")
	}
}

package apply_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/tchori-labs/tchori/internal/apply"
	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
)

func requireUnresolvedAt(t *testing.T, ds diag.Diagnostics, addr string) {
	t.Helper()
	for _, d := range ds {
		if d.Summary == "unresolved reference" && d.Address == addr && strings.Contains(d.Detail, "${") {
			return
		}
	}
	t.Fatalf("diagnostics = %+v, want unresolved reference addressed to %s", ds, addr)
}

func TestApplyGuardRunsBeforeReplaceDestroy(t *testing.T) {
	const addr = "tchoritest_thing.foo"
	h := newHarness(t, map[string]*config.Resource{addr: thing("foo", "foo")})
	ctx := context.Background()

	initial := loadState(t, h.statePath)
	if _, ds := apply.Apply(ctx, h.plan(t, initial, false), h.cfg, h.providers, h.schemas, initial, h.statePath); ds.HasErrors() {
		t.Fatalf("initial Apply: %+v", ds)
	}
	before := loadState(t, h.statePath)
	beforeSerial := before.Serial

	h.cfg.Resources[addr].Config["replace_me"] = "replacement"
	replacePlan := h.plan(t, before, false)
	if len(replacePlan.Changes) != 1 || replacePlan.Changes[0].Action != "replace" {
		t.Fatalf("changes = %+v, want one replace", replacePlan.Changes)
	}
	// Inject after planning: the planner itself now rejects this value.
	h.cfg.Resources[addr].Config["tags"] = map[string]any{"content": "${tchoritest_thing.base.id}.suffix"}

	_, ds := apply.Apply(ctx, replacePlan, h.cfg, h.providers, h.schemas, before, h.statePath)
	requireUnresolvedAt(t, ds, addr)
	saved := loadState(t, h.statePath)
	if saved.Resources[addr] == nil {
		t.Fatal("replace guard destroyed the existing resource before rejecting config")
	}
	if saved.Serial != beforeSerial+2 {
		t.Fatalf("state serial = %d, want %d (pre-flight marker + failure finalizer; destroy leg must not save)", saved.Serial, beforeSerial+2)
	}
	if saved.Incomplete == nil || saved.Incomplete.FailedAddress != addr {
		t.Fatalf("incomplete_apply = %+v, want failed address %s", saved.Incomplete, addr)
	}
}

func TestApplyGuardRejectsUnresolvedPlannedValue(t *testing.T) {
	const addr = "tchoritest_thing.foo"
	h := newHarness(t, map[string]*config.Resource{addr: thing("foo", "foo")})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "create" {
		t.Fatalf("changes = %+v, want one create", pl.Changes)
	}

	schema, _, ok := h.schemas["tchoritest"].LookupResourceType("tchoritest_thing")
	if !ok || schema == nil {
		t.Fatal("tchoritest_thing schema missing")
	}
	ty := schema.Block.ImpliedType()
	planned, err := ctymsgpack.Unmarshal(pl.Changes[0].PlannedRaw, ty)
	if err != nil {
		t.Fatalf("unmarshal planned: %v", err)
	}
	attrs := planned.AsValueMap()
	attrs["tags"] = cty.MapVal(map[string]cty.Value{
		"content": cty.StringVal("${tchoritest_thing.base.id}.suffix"),
	})
	pl.Changes[0].PlannedRaw, err = ctymsgpack.Marshal(cty.ObjectVal(attrs), ty)
	if err != nil {
		t.Fatalf("marshal planned: %v", err)
	}

	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	requireUnresolvedAt(t, ds, addr)
	if saved := loadState(t, h.statePath); saved.Resources[addr] != nil {
		t.Fatal("resource was persisted despite unresolved planned value")
	}
}

func TestApplyGuardAllowsCleanCreate(t *testing.T) {
	const addr = "tchoritest_thing.foo"
	h := newHarness(t, map[string]*config.Resource{addr: thing("foo", "foo")})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	_, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}
	if saved := loadState(t, h.statePath); saved.Resources[addr] == nil {
		t.Fatal("clean create missing from state")
	}
}

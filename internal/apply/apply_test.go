package apply_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/tchori-labs/tchori/internal/apply"
	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/state"
)

// buildTestProvider compiles the Task 5 fake provider into a temp dir and
// returns the binary path (same pattern as the internal/provider tests).
func buildTestProvider(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "terraform-provider-tchoritest")
	cmd := exec.Command("go", "build", "-o", bin, //nolint:gosec // fixed command; bin is a t.TempDir artifact
		"github.com/tchori-labs/tchori/internal/provider/testprovider")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building test provider: %v\n%s", err, out)
	}
	return bin
}

// harness bundles everything Apply needs: a running, configured fake
// provider, its schemas, a config, and a state path in a temp dir.
type harness struct {
	cfg       *config.Config
	providers map[string]*provider.Client
	schemas   map[string]*provider.ProviderSchemas
	statePath string
}

func newHarness(t *testing.T, resources map[string]*config.Resource) *harness {
	t.Helper()
	ctx := context.Background()

	c, err := provider.Launch(ctx, buildTestProvider(t))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})

	ps, ds := c.Schemas(ctx)
	if ds.HasErrors() {
		t.Fatalf("Schemas: %+v", ds)
	}
	// Provider config { prefix: null } => apply produces ids like "id-<name>".
	if ds := c.Configure(ctx, cty.ObjectVal(map[string]cty.Value{
		"prefix": cty.NullVal(cty.String),
	})); ds.HasErrors() {
		t.Fatalf("Configure: %+v", ds)
	}

	return &harness{
		cfg: &config.Config{
			Providers: map[string]*config.ProviderConfig{
				"tchoritest": {Name: "tchoritest", Source: "tchori-labs/tchoritest", Version: "0.0.1"},
			},
			Resources: resources,
		},
		providers: map[string]*provider.Client{"tchoritest": c},
		schemas:   map[string]*provider.ProviderSchemas{"tchoritest": ps},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
}

// plan runs the Task 10 planner over the harness config and the given state.
func (h *harness) plan(t *testing.T, st *state.State, destroy bool) *plan.Plan {
	t.Helper()
	p := &plan.Planner{
		Config:        h.cfg,
		State:         st,
		Providers:     h.providers,
		Schemas:       h.schemas,
		EngineVersion: "0.1.0-dev",
		Refresh:       true,
		Destroy:       destroy,
	}
	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan: %+v", ds)
	}
	return pl
}

// thing returns a tchoritest_thing resource. addrName is the resource name
// in the address; cfgName is the value of the "name" attribute.
func thing(addrName, cfgName string) *config.Resource {
	return &config.Resource{
		Address:  "tchoritest_thing." + addrName,
		Type:     "tchoritest_thing",
		Name:     addrName,
		Provider: "tchoritest",
		Config:   map[string]any{"name": cfgName},
	}
}

// brokenThing returns a tchoritest_broken_thing resource: a resource type
// whose schema tchori cannot convert (nested_type attribute — see
// testprovider's brokenThingSchema).
func brokenThing(addrName, cfgName string) *config.Resource {
	return &config.Resource{
		Address:  "tchoritest_broken_thing." + addrName,
		Type:     "tchoritest_broken_thing",
		Name:     addrName,
		Provider: "tchoritest",
		Config:   map[string]any{"name": cfgName},
	}
}

func loadState(t *testing.T, path string) *state.State {
	t.Helper()
	st, err := state.Load(path)
	if err != nil {
		t.Fatalf("state.Load(%s): %v", path, err)
	}
	return st
}

// stateAttrs re-loads the saved state file and decodes one resource's
// ctyjson attributes into a plain map.
func stateAttrs(t *testing.T, path, addr string) map[string]any {
	t.Helper()
	st := loadState(t, path)
	rs := st.Resources[addr]
	if rs == nil {
		t.Fatalf("resource %s not in saved state (have %d resources)", addr, len(st.Resources))
	}
	var attrs map[string]any
	if err := json.Unmarshal(rs.Attributes, &attrs); err != nil {
		t.Fatalf("decoding %s attributes: %v", addr, err)
	}
	return attrs
}

func TestApplyCreate(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.foo": thing("foo", "foo"),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath) // empty: serial 0
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "create" {
		t.Fatalf("plan = %+v, want exactly one create change", pl.Changes)
	}

	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}

	attrs := stateAttrs(t, h.statePath, "tchoritest_thing.foo")
	if got := attrs["id"]; got != "id-foo" {
		t.Errorf(`saved id = %v, want "id-foo"`, got)
	}
	if got := attrs["echo"]; got != "foo" {
		t.Errorf(`saved echo = %v, want "foo"`, got)
	}

	saved := loadState(t, h.statePath)
	if saved.Serial != 1 {
		t.Errorf("state serial = %d, want 1 (exactly one save for one change)", saved.Serial)
	}
}

func TestApplyStalePlan(t *testing.T) {
	// No provider is launched at all: a stale plan must be refused before
	// Apply touches providers or the state file.
	statePath := filepath.Join(t.TempDir(), "state.json")
	st := loadState(t, statePath) // empty: serial 0

	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   5, // plan captured at serial 5; current state is serial 0
		Changes:       []*plan.Change{{Address: "tchoritest_thing.foo", Action: "create"}},
		Summary:       plan.Summary{Create: 1},
	}

	ds := apply.Apply(context.Background(), pl, &config.Config{}, nil, nil, st, statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply accepted a stale plan")
	}
	found := false
	for _, d := range ds {
		if d.Summary == "stale plan" {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics do not include the stale-plan error: %+v", ds)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file was written during a refused apply (stat err = %v)", err)
	}
}

// TestApplyConfigDriftRefused guards the fix for the bug where Apply built
// its create/update/replace execution list solely by walking cfg.Order()
// (i.e. cfg.Resources' addresses) and looking up a matching plan change: a
// change whose address had been removed from the config since the plan was
// taken never appeared in that walk and so silently vanished from the
// execution list — "plan, then edit config to remove the resource, then
// apply" applied nothing and reported zero diagnostics. Like
// TestApplyStalePlan, no provider is launched: the refusal must happen
// before Apply ever touches a provider or the state file.
func TestApplyConfigDriftRefused(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	st := loadState(t, statePath) // empty: serial 0

	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   0,
		Changes:       []*plan.Change{{Address: "tchoritest_thing.foo", Action: "create"}},
		Summary:       plan.Summary{Create: 1},
	}

	// cfg no longer declares tchoritest_thing.foo: the config changed after
	// the plan was created.
	cfg := &config.Config{Resources: map[string]*config.Resource{}}

	ds := apply.Apply(context.Background(), pl, cfg, nil, nil, st, statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply accepted a plan whose resource is no longer in configuration")
	}
	found := false
	for _, d := range ds {
		if d.Summary == "plan does not match configuration" && d.Address == "tchoritest_thing.foo" {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics do not include the config-drift error for tchoritest_thing.foo: %+v", ds)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file was written during a refused apply (stat err = %v)", err)
	}
}

func TestApplyPartialFailure(t *testing.T) {
	// Changes apply in plan (address) order: ...alpha succeeds first, then
	// ...boom (name "explode") errors inside the provider's apply. The first
	// resource must survive in the saved state; the failed one must not.
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.alpha": thing("alpha", "alpha"),
		"tchoritest_thing.boom":  thing("boom", "explode"),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 2 {
		t.Fatalf("plan has %d changes, want 2: %+v", len(pl.Changes), pl.Changes)
	}

	ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply reported success despite a provider apply error")
	}
	found := false
	for _, d := range ds {
		if d.Summary == "apply exploded" {
			found = true
		}
	}
	if !found {
		t.Errorf("provider error diagnostic not propagated: %+v", ds)
	}

	saved := loadState(t, h.statePath)
	if saved.Resources["tchoritest_thing.boom"] != nil {
		t.Error("failed resource must not be recorded in state")
	}
	attrs := stateAttrs(t, h.statePath, "tchoritest_thing.alpha")
	if got := attrs["id"]; got != "id-alpha" {
		t.Errorf(`first resource id = %v, want "id-alpha" (must stay saved after mid-sequence failure)`, got)
	}
	if saved.Serial != 1 {
		t.Errorf("state serial = %d, want 1 (one save before the failure)", saved.Serial)
	}
}

// TestApplyRefOrderBeatsAddressOrder guards the fix for the bug where Apply
// executed changes in pl.Changes' document order (alphabetical by address,
// per plan.finalize) instead of dependency order. a_first sorts before
// z_second, but a_first's tags reference z_second's id, so a_first depends
// on z_second. If Apply still executed in address order, a_first would run
// first and resolveRef would fail with "reference to missing resource"
// because z_second has no state yet.
func TestApplyRefOrderBeatsAddressOrder(t *testing.T) {
	aFirst := thing("a_first", "a_first")
	aFirst.Config["tags"] = map[string]any{"ref": "${tchoritest_thing.z_second.id}"}
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.a_first":  aFirst,
		"tchoritest_thing.z_second": thing("z_second", "z_second"),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 2 {
		t.Fatalf("plan has %d changes, want 2: %+v", len(pl.Changes), pl.Changes)
	}
	// The plan document itself is still address-sorted (a_first, z_second):
	// it carries no dependency information. Apply must derive execution
	// order from cfg.Order() instead of trusting this order.
	if pl.Changes[0].Address != "tchoritest_thing.a_first" || pl.Changes[1].Address != "tchoritest_thing.z_second" {
		t.Fatalf("plan changes not in address order: %s, %s", pl.Changes[0].Address, pl.Changes[1].Address)
	}

	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}

	zAttrs := stateAttrs(t, h.statePath, "tchoritest_thing.z_second")
	if got := zAttrs["id"]; got != "id-z_second" {
		t.Fatalf(`z_second id = %v, want "id-z_second"`, got)
	}

	aAttrs := stateAttrs(t, h.statePath, "tchoritest_thing.a_first")
	tags, ok := aAttrs["tags"].(map[string]any)
	if !ok {
		t.Fatalf("a_first tags = %#v, want a map", aAttrs["tags"])
	}
	if got := tags["ref"]; got != "id-z_second" {
		t.Errorf(`a_first tags["ref"] = %v, want "id-z_second" (z_second's applied id, proving z_second applied first)`, got)
	}

	saved := loadState(t, h.statePath)
	if len(saved.Resources) != 2 {
		t.Errorf("state has %d resources, want 2", len(saved.Resources))
	}
}

// TestApplyReplace closes a coverage gap: no existing test drove the
// destroy-then-create "replace" branch of applyChange. It seeds state via a
// create apply, changes replace_me (which the fake provider's
// PlanResourceChange marks as forcing replacement), plans (expecting a
// "replace" action), applies, and asserts the resource was destroyed and
// recreated: a fresh id (still deterministically "id-<name>"), the state
// serial advanced by two saves (destroy leg + create leg), and exactly one
// resource left in state.
func TestApplyReplace(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.foo": thing("foo", "foo"),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath) // empty: serial 0
	if ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create Apply: %+v", ds)
	}

	// Reload (as the CLI would) so the replace plan captures serial 1, then
	// change replace_me to force a replace.
	st2 := loadState(t, h.statePath)
	h.cfg.Resources["tchoritest_thing.foo"].Config["replace_me"] = "new-value"
	pl := h.plan(t, st2, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "replace" {
		t.Fatalf("plan = %+v, want exactly one replace change", pl.Changes)
	}

	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st2, h.statePath); ds.HasErrors() {
		t.Fatalf("replace Apply: %+v", ds)
	}

	saved := loadState(t, h.statePath)
	if len(saved.Resources) != 1 {
		t.Fatalf("state has %d resources after replace, want 1", len(saved.Resources))
	}
	attrs := stateAttrs(t, h.statePath, "tchoritest_thing.foo")
	if got := attrs["id"]; got != "id-foo" {
		t.Errorf(`id after replace = %v, want "id-foo" (destroy-then-create still yields the deterministic fake id)`, got)
	}
	if saved.Serial != 3 {
		t.Errorf("state serial = %d, want 3 (create save + destroy save + create save)", saved.Serial)
	}
}

func TestApplyDestroy(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.foo": thing("foo", "foo"),
	})
	ctx := context.Background()

	// Create first.
	st := loadState(t, h.statePath)
	if ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create Apply: %+v", ds)
	}

	// Reload (as the CLI would) so the destroy plan captures serial 1.
	st2 := loadState(t, h.statePath)
	dpl := h.plan(t, st2, true)
	if len(dpl.Changes) != 1 || dpl.Changes[0].Action != "delete" {
		t.Fatalf("destroy plan = %+v, want exactly one delete change", dpl.Changes)
	}

	if ds := apply.Apply(ctx, dpl, h.cfg, h.providers, h.schemas, st2, h.statePath); ds.HasErrors() {
		t.Fatalf("destroy Apply: %+v", ds)
	}

	saved := loadState(t, h.statePath)
	if len(saved.Resources) != 0 {
		t.Errorf("state still holds %d resources after destroy", len(saved.Resources))
	}
	if saved.Serial != 2 {
		t.Errorf("state serial = %d, want 2 (create save + destroy save)", saved.Serial)
	}
}

// TestApplyCreateIgnoresStalePriorState guards the fix for the bug where
// createOrUpdate always decoded "prior" from whatever state.json happened to
// hold for the address, even when ch.Action == "create". A plan's "create"
// action is only produced when the planner considered there to be no live
// prior object (see plan.Planner.Plan / classify) — but refresh mutates only
// the planner's in-memory state.State, and `plan` never re-saves it, so a
// resource that vanished out of band and was detected during a --refresh
// plan run leaves a stale, non-null entry sitting in state.json for a
// separate `apply` invocation to load. Before the fix, that stale entry
// still got decoded and handed to the provider as "prior" (and, if its
// shape no longer matches the current schema, decoding it can itself fail)
// even though the plan document says "create". This test manufactures
// exactly that situation by hand (the fake provider's ReadResource always
// echoes state back, so it can never itself surface an out-of-band
// deletion): state.json is seeded with an entry for the address whose
// "tags" attribute is a JSON string rather than the map the schema
// declares — a stand-in for "state we can no longer make sense of" — paired
// with a genuine create-shaped plan.Change (built via a real, prior-null
// PlanResourceChange call, so PlannedRaw carries unknown id/echo exactly as
// a real create would). Pre-fix, applyChange's unconditional decode of that
// stale entry errors out ("corrupt state attributes") before the create
// ever reaches the provider — permanent non-convergence, as described in
// the finding. Post-fix, ch.Action == "create" skips the decode entirely
// (cty.NullVal(ty) instead), so the create proceeds normally: the provider
// mints a fresh id, observable in the saved state.
func TestApplyCreateIgnoresStalePriorState(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.foo": thing("foo", "foo"),
	})
	ctx := context.Background()
	addr := "tchoritest_thing.foo"
	ty := h.schemas["tchoritest"].ResourceTypes["tchoritest_thing"].Block.ImpliedType()

	// A create-shaped planned value: the same RPC call the planner itself
	// would make for a resource whose prior is null.
	proposed, cds := provider.Compose(h.cfg.Resources[addr].Config, ty, false, nil)
	if cds.HasErrors() {
		t.Fatalf("Compose: %+v", cds)
	}
	pc, pds := h.providers["tchoritest"].PlanResource(ctx, "tchoritest_thing", cty.NullVal(ty), proposed, proposed, nil)
	if pds.HasErrors() {
		t.Fatalf("PlanResource: %+v", pds)
	}
	raw, err := ctymsgpack.Marshal(pc.State, ty)
	if err != nil {
		t.Fatalf("marshal planned: %v", err)
	}

	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   0,
		Changes: []*plan.Change{{
			Address:    addr,
			Action:     "create",
			Before:     json.RawMessage("null"),
			PlannedRaw: raw,
			Private:    pc.Private,
		}},
		Summary: plan.Summary{Create: 1},
	}

	// A stale state entry for the same address, whose "tags" attribute no
	// longer matches the schema's map type — stands in for state the
	// engine can no longer make sense of, e.g. after out-of-band deletion
	// and drift. A conforming-but-stale entry (matching id/name/echo, just
	// out of date) would decode without error and — since this fake
	// provider's ApplyResourceChange ignores PriorState entirely and
	// derives new state solely from planned's already-unknown id/echo —
	// would not actually distinguish the buggy and fixed code paths; this
	// shape does, by making the pre-fix unconditional decode itself fail.
	st := &state.State{
		FormatVersion: "1.0",
		Serial:        0,
		Resources: map[string]*state.ResourceState{
			addr: {
				Type:       "tchoritest_thing",
				Provider:   "tchoritest",
				Attributes: json.RawMessage(`{"echo":"stale","id":"stale-id","name":"stale","replace_me":null,"tags":"not-a-map"}`),
			},
		},
	}

	ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}

	attrs := stateAttrs(t, h.statePath, addr)
	if got := attrs["id"]; got != "id-foo" {
		t.Errorf(`saved id = %v, want "id-foo" (provider-generated create id, not anything derived from the stale prior)`, got)
	}
	if got := attrs["echo"]; got != "foo" {
		t.Errorf(`saved echo = %v, want "foo"`, got)
	}
}

// TestApplyUnsupportedResourceType guards issue #5's fix in apply.go's own
// schema lookups (applyChange): a plan change addressing
// tchoritest_broken_thing (a resource type whose schema tchori cannot
// convert — nested_type attribute, see testprovider's brokenThingSchema)
// must fail with a diagnostic naming the stored conversion detail, not the
// generic "missing resource schema" message used for a type the provider
// never defined at all. The plan document is hand-built (bypassing the
// planner, which would already refuse this address) so this test isolates
// apply's own defense-in-depth lookup.
func TestApplyUnsupportedResourceType(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_broken_thing.boom": brokenThing("boom", "boom"),
	})
	st := loadState(t, h.statePath) // empty: serial 0

	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   0,
		Changes:       []*plan.Change{{Address: "tchoritest_broken_thing.boom", Action: "create"}},
		Summary:       plan.Summary{Create: 1},
	}

	ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply accepted a resource type with an unsupported (nested_type) schema, want error")
	}
	found := false
	for _, d := range ds {
		if strings.Contains(d.Summary, "unsupported schema") && strings.Contains(d.Detail, "nested_type") {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %+v, want one with summary containing %q and detail containing %q",
			ds, "unsupported schema", "nested_type")
	}
}

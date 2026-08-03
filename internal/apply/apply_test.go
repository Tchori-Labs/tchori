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
	"github.com/tchori-labs/tchori/internal/diag"
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

func secretful(addrName string, cfg map[string]any) *config.Resource {
	values := map[string]any{"name": addrName}
	for k, v := range cfg {
		values[k] = v
	}
	return &config.Resource{Address: "tchoritest_secretful." + addrName, Type: "tchoritest_secretful", Name: addrName, Provider: "tchoritest", Config: values}
}

// nestedThing returns a tchoritest_nested_thing resource (issue #7's
// acceptance fixture — see testprovider's nestedThingSchema): "settings" is
// a nested_type (SINGLE) attribute with two optional leaf attributes. A nil
// settings omits the "settings" key from Config entirely, matching the real
// acceptance shape (a config that leaves the nested attribute unset).
func lossyThing(addrName string, values map[string]any) *config.Resource {
	cfg := map[string]any{"name": addrName}
	for key, value := range values {
		cfg[key] = value
	}
	return &config.Resource{
		Address:  "tchoritest_lossy." + addrName,
		Type:     "tchoritest_lossy",
		Name:     addrName,
		Provider: "tchoritest",
		Config:   cfg,
	}
}

func nestedThing(addrName, cfgName string, settings map[string]any) *config.Resource {
	cfg := map[string]any{"name": cfgName}
	if settings != nil {
		cfg["settings"] = settings
	}
	return &config.Resource{
		Address:  "tchoritest_nested_thing." + addrName,
		Type:     "tchoritest_nested_thing",
		Name:     addrName,
		Provider: "tchoritest",
		Config:   cfg,
	}
}

// brokenThing returns a tchoritest_broken_thing resource: a resource type
// whose schema tchori cannot convert (nested_type attribute using a nesting
// mode blockFromProto does not recognize — see testprovider's
// brokenThingSchema).
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

func TestApplyLossyCreateUpdateReplaceAndDestroy(t *testing.T) {
	const addr = "tchoritest_lossy.svc"
	resource := lossyThing("svc", map[string]any{"flag": true, "replace_me": "a"})
	h := newHarness(t, map[string]*config.Resource{addr: resource})
	ctx := context.Background()
	st := loadState(t, h.statePath)

	createPlan := h.plan(t, st, false)
	if len(createPlan.Changes) != 1 || createPlan.Changes[0].Action != "create" {
		t.Fatalf("create plan = %#v", createPlan.Changes)
	}
	ty := h.schemas["tchoritest"].ResourceTypes["tchoritest_lossy"].Block.ImpliedType()
	planned, err := ctymsgpack.Unmarshal(createPlan.Changes[0].PlannedRaw, ty)
	if err != nil {
		t.Fatal(err)
	}
	if planned.IsWhollyKnown() || planned.GetAttr("id").IsKnown() {
		t.Fatal("lossy planned create must retain unknown computed id")
	}
	createDs := apply.Apply(ctx, createPlan, h.cfg, h.providers, h.schemas, st, h.statePath)
	if len(createDs) != 1 || createDs[0].Summary != "provider produced inconsistent result after apply" || !strings.Contains(createDs[0].Detail, "flag: planned true, applied false") {
		t.Fatalf("create diagnostics = %#v", createDs)
	}
	if got := stateAttrs(t, h.statePath, addr)["flag"]; got != false {
		t.Fatalf("saved create flag = %#v, want provider's false", got)
	}

	// The fake provider's update path honours the flag, matching the reported
	// provider's PATCH workaround, so an identical second plan converges.
	st = loadState(t, h.statePath)
	updatePlan := h.plan(t, st, false)
	if updatePlan.Changes[0].Action != "update" {
		t.Fatalf("update action = %q", updatePlan.Changes[0].Action)
	}
	if ds := apply.Apply(ctx, updatePlan, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("update diagnostics = %#v", ds)
	}
	if got := stateAttrs(t, h.statePath, addr)["flag"]; got != true {
		t.Fatalf("saved update flag = %#v", got)
	}

	resource.Config["replace_me"] = "b"
	st = loadState(t, h.statePath)
	replacePlan := h.plan(t, st, false)
	if replacePlan.Changes[0].Action != "replace" || len(replacePlan.Changes[0].RequiresReplace) != 1 || replacePlan.Changes[0].RequiresReplace[0] != "replace_me" {
		t.Fatalf("replace plan = %#v", replacePlan.Changes[0])
	}
	replaceDs := apply.Apply(ctx, replacePlan, h.cfg, h.providers, h.schemas, st, h.statePath)
	if len(replaceDs) != 1 || !strings.Contains(replaceDs[0].Detail, "flag: planned true, applied false") {
		t.Fatalf("replace diagnostics = %#v", replaceDs)
	}

	st = loadState(t, h.statePath)
	destroyPlan := h.plan(t, st, true)
	if ds := apply.Apply(ctx, destroyPlan, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("destroy diagnostics = %#v", ds)
	}
}

func TestApplyLossyMapKeySet(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags map[string]any
		want string
	}{
		{"injected", map[string]any{"inject": "x"}, `tags["injected"]: planned absent, applied "by-provider"`},
		{"authored empty", map[string]any{}, `tags["injected"]: planned absent, applied "by-provider"`},
		{"dropped", map[string]any{"dropped": "x"}, `tags["dropped"]: planned "x", applied absent`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const addr = "tchoritest_lossy.svc"
			h := newHarness(t, map[string]*config.Resource{addr: lossyThing("svc", map[string]any{"tags": tc.tags})})
			st := loadState(t, h.statePath)
			ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
			if len(ds) != 1 || !strings.Contains(ds[0].Detail, tc.want) {
				t.Fatalf("diagnostics = %#v", ds)
			}
		})
	}
}

func TestApplyLossyNestedRedactionAndNullContainers(t *testing.T) {
	const addr = "tchoritest_lossy.svc"
	t.Run("nested redaction", func(t *testing.T) {
		h := newHarness(t, map[string]*config.Resource{addr: lossyThing("svc", map[string]any{
			"credentials": map[string]any{"user": "alice", "token": "token-secret"},
			"endpoints":   []any{map[string]any{"host": "api", "api_key": "api-secret"}},
			"probes":      []any{map[string]any{"path": "/health"}},
		})})
		st := loadState(t, h.statePath)
		ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
		if len(ds) != 1 {
			t.Fatalf("diagnostics = %#v", ds)
		}
		detail := ds[0].Detail
		for _, secret := range []string{"token-secret", "TOKEN-SECRET", "api-secret"} {
			if strings.Contains(detail, secret) {
				t.Fatalf("detail leaked %q: %s", secret, detail)
			}
		}
		if !strings.Contains(detail, "credentials.token: planned (sensitive value), applied (sensitive value)") ||
			!strings.Contains(detail, `credentials.user: planned "alice", applied ""`) ||
			!strings.Contains(detail, "endpoints: planned (sensitive value), applied (sensitive value)") ||
			!strings.Contains(detail, `probes: planned [{"path":"/health"}], applied []`) {
			t.Fatalf("detail = %s", detail)
		}
	})

	t.Run("returned null aggregates", func(t *testing.T) {
		h := newHarness(t, map[string]*config.Resource{addr: lossyThing("svc", map[string]any{
			"tags":        map[string]any{"nullify": "visible"},
			"credentials": map[string]any{"user": "nullify", "token": "hidden"},
		})})
		st := loadState(t, h.statePath)
		ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
		if len(ds) != 1 {
			t.Fatalf("diagnostics = %#v", ds)
		}
		if !strings.Contains(ds[0].Detail, `tags: planned {"nullify":"visible"}, applied null`) || strings.Contains(ds[0].Detail, "tags.nullify") ||
			!strings.Contains(ds[0].Detail, "credentials: planned (sensitive value), applied (sensitive value)") || strings.Contains(ds[0].Detail, "hidden") {
			t.Fatalf("detail = %s", ds[0].Detail)
		}
		attrs := stateAttrs(t, h.statePath, addr)
		if attrs["tags"] != nil || attrs["credentials"] != nil {
			t.Fatalf("state did not retain returned nulls: %#v", attrs)
		}
	})
}

func TestApplyLossyResolvedReferencesRemainChecked(t *testing.T) {
	const lossyAddr = "tchoritest_lossy.svc"
	source := thing("source", "source")
	lossy := lossyThing("svc", map[string]any{ //nolint:gosec // schema attribute name, not a credential literal
		"secret": "${tchoritest_thing.source.echo}",
		"tags":   map[string]any{"dropped": "${tchoritest_thing.source.id}"},
	})
	h := newHarness(t, map[string]*config.Resource{source.Address: source, lossyAddr: lossy})
	st := loadState(t, h.statePath)
	ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if len(ds) != 2 || ds[0].Severity != diag.Warning || ds[1].Address != lossyAddr || !strings.Contains(ds[1].Detail, `tags["dropped"]: planned "id-source", applied absent`) || !strings.Contains(ds[1].Detail, "secret: planned (sensitive value), applied (sensitive value)") || strings.Contains(ds[1].Detail, "SOURCE") {
		t.Fatalf("diagnostics = %#v", ds)
	}
}

func TestApplyWithholdsSensitiveComputedAndReferencedValues(t *testing.T) {
	a := secretful("a", nil)
	b := secretful("b", map[string]any{"token": "${tchoritest_secretful.a.client_secret}"})                                                                              //nolint:gosec // schema attribute name in redaction regression
	c := secretful("c", map[string]any{"rules": []any{map[string]any{"token": "literal-token-ok"}, map[string]any{"token": "${tchoritest_secretful.a.client_secret}"}}}) //nolint:gosec // fake values exercise per-instance redaction
	h := newHarness(t, map[string]*config.Resource{a.Address: a, b.Address: b, c.Address: c})
	st := loadState(t, h.statePath)
	ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if ds.HasErrors() {
		t.Fatalf("Apply: %#v", ds)
	}
	data, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tchori-e2e-super-secret-value") {
		t.Fatal("sensitive sentinel persisted")
	}
	if !strings.Contains(string(data), "literal-token-ok") {
		t.Fatal("literal exemption was not preserved")
	}
	attrs := stateAttrs(t, h.statePath, a.Address)
	if attrs["client_secret"] != nil {
		t.Fatalf("client_secret = %#v, want null", attrs["client_secret"])
	}
	reloaded := loadState(t, h.statePath)
	pl := h.plan(t, reloaded, false)
	for _, ch := range pl.Changes {
		if ch.Action != "no-op" {
			t.Fatalf("post-apply action for %s = %s", ch.Address, ch.Action)
		}
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
		if d.Summary == "apply exploded" && d.Address == "tchoritest_thing.boom" {
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
func TestApplyDestroyProviderDiagnosticHasResourceAddress(t *testing.T) {
	const addr = "tchoritest_thing.web"
	h := newHarness(t, map[string]*config.Resource{addr: thing("web", "explode_destroy")})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	if ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create fixture: %#v", ds)
	}

	st = loadState(t, h.statePath)
	ds := apply.Apply(ctx, h.plan(t, st, true), h.cfg, h.providers, h.schemas, st, h.statePath)
	if len(ds) != 1 || ds[0].Summary != "destroy exploded" || ds[0].Address != addr {
		t.Fatalf("destroy diagnostics = %#v", ds)
	}
	if loadState(t, h.statePath).Resources[addr] == nil {
		t.Fatal("failed destroy removed the resource from state")
	}
}

func TestApplyReplaceDoesNotDoublePrefixProviderDiagnostic(t *testing.T) {
	const addr = "tchoritest_thing.web"
	resource := thing("web", "before")
	h := newHarness(t, map[string]*config.Resource{addr: resource})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	if ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create fixture: %#v", ds)
	}

	resource.Config["name"] = "explode"
	resource.Config["replace_me"] = "replacement"
	st = loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "replace" {
		t.Fatalf("replace plan = %#v", pl.Changes)
	}
	ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if len(ds) != 1 || ds[0].Summary != "apply exploded" || ds[0].Address != addr {
		t.Fatalf("replace diagnostics = %#v", ds)
	}
	if strings.Contains(ds[0].Address, addr+"."+addr) {
		t.Fatalf("replace diagnostic was double-prefixed: %#v", ds[0])
	}
	if loadState(t, h.statePath).Resources[addr] != nil {
		t.Fatal("replace destroy leg did not remain committed before create failure")
	}
}

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

func TestApplyRejectsIssue58EmbeddedReference(t *testing.T) {
	const (
		tunnelAddr = "tchoritest_thing.tunnel"
		whAddr     = "tchoritest_thing.wh"
	)
	wh := thing("wh", "wh")
	wh.Config["tags"] = map[string]any{"content": "safe.example"}
	h := newHarness(t, map[string]*config.Resource{
		tunnelAddr: thing("tunnel", "tunnel"),
		whAddr:     wh,
	})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)

	// Apply-only fixture: planning the bad config is now forbidden, while
	// Apply's drift check deliberately compares addresses rather than values.
	h.cfg.Resources[whAddr].Config["tags"] = map[string]any{
		"content": "${tchoritest_thing.tunnel.id}.cfargotunnel.com",
	}
	ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	requireUnresolvedAt(t, ds, whAddr)

	saved := loadState(t, h.statePath)
	if saved.Resources[whAddr] != nil {
		t.Fatal("dependent resource with embedded reference was persisted")
	}
	if saved.Resources[tunnelAddr] == nil {
		t.Fatal("safe resource preceding the rejected value was not persisted")
	}
}

func TestApplyRejectsPoisonedStateReferencePropagation(t *testing.T) {
	const (
		aAddr = "tchoritest_thing.a"
		bAddr = "tchoritest_thing.b"
	)
	a := thing("a", "a")
	a.Config["tags"] = map[string]any{"parent": "safe-parent"}
	b := thing("b", "b")
	b.Config["tags"] = map[string]any{"parent": "safe-parent"}
	h := newHarness(t, map[string]*config.Resource{aAddr: a, bAddr: b})
	ctx := context.Background()

	st := &state.State{
		FormatVersion: "1.0",
		Serial:        7,
		Resources: map[string]*state.ResourceState{
			aAddr: {
				Type:       "tchoritest_thing",
				Provider:   "tchoritest",
				Attributes: json.RawMessage(`{"echo":"a","id":"id-a","name":"a","replace_me":null,"tags":{"parent":"safe-parent"}}`),
			},
		},
	}
	pl := h.plan(t, st, false)

	// Apply-only fixture: after obtaining a safe plan, make b read the map
	// value through a valid whole-string reference. (The planner's MVP dotted
	// path resolver cannot traverse maps, while apply's resolver can.)
	h.cfg.Resources[bAddr].Config["tags"] = map[string]any{
		"parent": "${tchoritest_thing.a.tags.parent}",
	}

	// Reproduce state left by an older engine after planning. Write directly
	// to preserve the plan serial; the poisoned entry is intentionally not
	// cleaned by TC-048, only refused when another outgoing value reads it.
	st.Resources[aAddr].Attributes = json.RawMessage(`{"echo":"a","id":"id-a","name":"a","replace_me":null,"tags":{"parent":"${tchoritest_thing.ghost.id}"}}`)
	stateBytes, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal seeded state: %v", err)
	}
	if err := os.WriteFile(h.statePath, stateBytes, 0o600); err != nil {
		t.Fatalf("write seeded state: %v", err)
	}

	ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	requireUnresolvedAt(t, ds, bAddr)
	saved := loadState(t, h.statePath)
	if saved.Resources[bAddr] != nil {
		t.Fatal("resource receiving poisoned state reference was persisted")
	}
	if !strings.Contains(string(saved.Resources[aAddr].Attributes), "${tchoritest_thing.ghost.id}") {
		t.Fatal("pre-existing poisoned state was unexpectedly rewritten or cleaned")
	}
}

func TestApplyDependencyFailureSkipsDependent(t *testing.T) {
	const (
		tunnelAddr = "tchoritest_thing.tunnel"
		whAddr     = "tchoritest_thing.wh"
	)
	wh := thing("wh", "wh")
	wh.Config["tags"] = map[string]any{"tunnel": "${tchoritest_thing.tunnel.id}"}
	h := newHarness(t, map[string]*config.Resource{
		tunnelAddr: thing("tunnel", "explode"),
		whAddr:     wh,
	})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)

	ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply succeeded despite dependency provider failure")
	}
	foundFailure := false
	for _, d := range ds {
		if d.Summary == "apply exploded" {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("diagnostics = %+v, want provider apply failure", ds)
	}
	saved := loadState(t, h.statePath)
	if saved.Resources[whAddr] != nil || saved.Resources[tunnelAddr] != nil {
		t.Fatalf("state resources = %+v, want neither failed dependency nor skipped dependent", saved.Resources)
	}
	if saved.Serial != 0 {
		t.Fatalf("state serial = %d, want 0 because no change was saved", saved.Serial)
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
				Attributes: json.RawMessage(`{"echo":"stale","id":"stale-id","name":"stale","replace_me":null,"rules":null,"tags":"not-a-map"}`),
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

// TestApplyCreateNestedTypeOmitted guards issue #7's acceptance shape: a
// config for a nested_type resource (tchoritest_nested_thing) that leaves
// the whole nested attribute unset must validate, plan, and apply cleanly,
// with "settings" round-tripping as null all the way through Compose ->
// msgpack -> the fake provider -> saved state. This is exactly the real
// infra shape the issue names (cloudflare_dns_record's TXT-record config
// never sets the "data" nested attribute).
func TestApplyCreateNestedTypeOmitted(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_nested_thing.omitted": nestedThing("omitted", "omitted", nil),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath) // empty: serial 0
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "create" {
		t.Fatalf("plan = %+v, want exactly one create change", pl.Changes)
	}
	if !strings.Contains(string(pl.Changes[0].After), `"settings":null`) {
		t.Errorf("planned after = %s, want settings:null", pl.Changes[0].After)
	}

	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}

	attrs := stateAttrs(t, h.statePath, "tchoritest_nested_thing.omitted")
	if got := attrs["id"]; got != "id-omitted" {
		t.Errorf(`saved id = %v, want "id-omitted"`, got)
	}
	if got, ok := attrs["settings"]; !ok || got != nil {
		t.Errorf(`saved settings = %#v, want nil (the nested attribute was never set in config)`, got)
	}
}

// TestApplyCreateNestedTypePopulated guards issue #7's other required
// shape: a nested_type SINGLE attribute that IS populated in config must
// flow its values through Compose -> msgpack -> the fake provider -> saved
// state unchanged (the fake provider's plan/apply for
// tchoritest_nested_thing never touches "settings" — see
// testprovider's planNestedThing/applyNestedThing).
func TestApplyCreateNestedTypePopulated(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_nested_thing.filled": nestedThing("filled", "filled", map[string]any{
			"flag":  true,
			"label": "hello",
		}),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath) // empty: serial 0
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "create" {
		t.Fatalf("plan = %+v, want exactly one create change", pl.Changes)
	}
	if !strings.Contains(string(pl.Changes[0].After), `"flag":true`) ||
		!strings.Contains(string(pl.Changes[0].After), `"label":"hello"`) {
		t.Errorf("planned after = %s, want it to carry the populated settings", pl.Changes[0].After)
	}

	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}

	attrs := stateAttrs(t, h.statePath, "tchoritest_nested_thing.filled")
	if got := attrs["id"]; got != "id-filled" {
		t.Errorf(`saved id = %v, want "id-filled"`, got)
	}
	settings, ok := attrs["settings"].(map[string]any)
	if !ok {
		t.Fatalf("saved settings = %#v, want a populated object", attrs["settings"])
	}
	if got := settings["flag"]; got != true {
		t.Errorf(`saved settings["flag"] = %v, want true`, got)
	}
	if got := settings["label"]; got != "hello" {
		t.Errorf(`saved settings["label"] = %v, want "hello"`, got)
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

// rulesRef builds a tchoritest_thing "rules" config value: a one-element
// list of a map whose "token_id" is the whole-string ${...} reference to
// refAddr's "id" attribute — the list-nested shape from issue #11
// (cloudflare's policies[].include[].service_token.token_id) reproduced at
// the fake-provider level via the "rules" list-of-object attribute added in
// TC-033 (see testprovider's thingRuleType).
func rulesRef(refAddr string) []any {
	return []any{
		map[string]any{"token_id": "${" + refAddr + ".id}"},
	}
}

// stateRules re-decodes a saved resource's "rules" attribute (via
// stateAttrs) into the one-element []any{map[string]any{"token_id": ...}}
// shape rulesRef produces, and returns the single element's token_id.
func stateRulesTokenID(t *testing.T, path, addr string) string {
	t.Helper()
	attrs := stateAttrs(t, path, addr)
	rules, ok := attrs["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("%s rules = %#v, want a one-element list", addr, attrs["rules"])
	}
	elem, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("%s rules[0] = %#v, want a map", addr, rules[0])
	}
	tokenID, _ := elem["token_id"].(string)
	return tokenID
}

// TestApplySingleApplyResolvesListNestedRefCreateUpdate is the exact
// create+update reproduction of Tchori-Labs/tchori#11 (TC-033): resource
// "b" already exists in state (applied in a prior run with no rules), then
// a single plan+apply both creates resource "a" and updates "b" so that
// b's "rules" list holds a reference to a's (not-yet-applied-at-plan-time)
// computed "id". Before the TC-033 fix, resolvePlannedUnknowns left the
// list-nested reference unknown at apply, so it reached the fake provider
// as null (and, for the real cloudflare provider, a 400); after the fix,
// one apply suffices — b's saved rules[0].token_id equals a's saved id.
func TestApplySingleApplyResolvesListNestedRefCreateUpdate(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.b": thing("b", "b"),
	})
	ctx := context.Background()

	// Seed state: "b" alone, no rules.
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "create" {
		t.Fatalf("seed plan = %+v, want exactly one create change", pl.Changes)
	}
	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("seed Apply: %+v", ds)
	}
	if saved := loadState(t, h.statePath); saved.Serial != 1 {
		t.Fatalf("seed state serial = %d, want 1", saved.Serial)
	}

	// Now add "a" (create) and give "b" a rules list referencing a's id
	// (making b an update).
	h.cfg.Resources["tchoritest_thing.a"] = thing("a", "a")
	h.cfg.Resources["tchoritest_thing.b"].Config["rules"] = rulesRef("tchoritest_thing.a")

	st = loadState(t, h.statePath)
	pl = h.plan(t, st, false)
	if len(pl.Changes) != 2 {
		t.Fatalf("plan has %d changes, want 2: %+v", len(pl.Changes), pl.Changes)
	}
	actions := map[string]string{}
	for _, ch := range pl.Changes {
		actions[ch.Address] = ch.Action
	}
	if actions["tchoritest_thing.a"] != "create" {
		t.Errorf("a action = %q, want %q", actions["tchoritest_thing.a"], "create")
	}
	if actions["tchoritest_thing.b"] != "update" {
		t.Errorf("b action = %q, want %q", actions["tchoritest_thing.b"], "update")
	}

	// One apply must suffice: zero error diagnostics.
	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("single Apply (create a + update b with list-nested ref): %+v", ds)
	}

	aAttrs := stateAttrs(t, h.statePath, "tchoritest_thing.a")
	aID, _ := aAttrs["id"].(string)
	if aID == "" {
		t.Fatalf("a id = %#v, want a non-empty string", aAttrs["id"])
	}

	if got := stateRulesTokenID(t, h.statePath, "tchoritest_thing.b"); got != aID {
		t.Errorf("b rules[0].token_id = %q, want %q (a's applied id, resolved within the single apply)", got, aID)
	}

	// Exactly one save per change across both apply runs: seed apply saved
	// once (serial 1), this apply saves a and b (2 more changes) -> 3.
	if saved := loadState(t, h.statePath); saved.Serial != 3 {
		t.Errorf("state serial = %d, want 3 (one save per change across both applies)", saved.Serial)
	}
}

// TestApplySingleApplyResolvesListNestedRefCreateCreate covers the
// create+create shape (both "a" and "b" new in the same plan), with the
// dependent ("a_ref") address-sorting BEFORE its dependency
// ("z_target") — mirroring TestApplyRefOrderBeatsAddressOrder — so the
// test also proves execution follows cfg.Order(), not plan-document order,
// for a list-nested reference.
func TestApplySingleApplyResolvesListNestedRefCreateCreate(t *testing.T) {
	aRef := thing("a_ref", "a_ref")
	aRef.Config["rules"] = rulesRef("tchoritest_thing.z_target")
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.a_ref":    aRef,
		"tchoritest_thing.z_target": thing("z_target", "z_target"),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 2 {
		t.Fatalf("plan has %d changes, want 2: %+v", len(pl.Changes), pl.Changes)
	}
	for _, ch := range pl.Changes {
		if ch.Action != "create" {
			t.Fatalf("change %s action = %q, want %q", ch.Address, ch.Action, "create")
		}
	}

	if ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("single Apply (create a_ref + create z_target with list-nested ref): %+v", ds)
	}

	zAttrs := stateAttrs(t, h.statePath, "tchoritest_thing.z_target")
	zID, _ := zAttrs["id"].(string)
	if zID == "" {
		t.Fatalf("z_target id = %#v, want a non-empty string", zAttrs["id"])
	}

	if got := stateRulesTokenID(t, h.statePath, "tchoritest_thing.a_ref"); got != zID {
		t.Errorf("a_ref rules[0].token_id = %q, want %q (z_target's applied id, resolved within the single apply)", got, zID)
	}

	saved := loadState(t, h.statePath)
	if saved.Serial != 2 {
		t.Errorf("state serial = %d, want 2 (one save per change)", saved.Serial)
	}
	if len(saved.Resources) != 2 {
		t.Errorf("state has %d resources, want 2", len(saved.Resources))
	}
}

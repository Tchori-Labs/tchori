package apply_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zclconf/go-cty/cty"

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

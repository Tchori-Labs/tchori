package apply_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestMain(m *testing.M) {
	if err := os.Setenv("TCHORI_ARTIFACT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32))); err != nil {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

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

func TestApplyHoldsStateLockDuringProviderMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("TCHORITEST_VERIFY_STATE_LOCK", path)
	const addr = "tchoritest_thing.foo"
	h := newHarness(t, map[string]*config.Resource{addr: thing("foo", "foo")})
	h.statePath = path
	st := loadState(t, path)
	ctx := context.Background()
	for _, destroy := range []bool{false, true} {
		_, ds := apply.Apply(ctx, h.plan(t, st, destroy), h.cfg, h.providers, h.schemas, st, path)
		if ds.HasErrors() {
			t.Fatalf("provider mutation (destroy=%v) did not hold exclusive state lock: %+v", destroy, ds)
		}
	}
}

func TestApplyPersistsPartialProviderErrorState(t *testing.T) {
	const addr = "tchoritest_thing.partial"
	const dependentAddr = "tchoritest_thing.dependent"
	dependent := thing("dependent", "dependent")
	dependent.Config["tags"] = map[string]any{"parent": "${tchoritest_thing.partial.id}"}
	h := newHarness(t, map[string]*config.Resource{
		addr: thing("partial", "partial_failure"), dependentAddr: dependent,
	})
	st := loadState(t, h.statePath)
	result, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() || len(result.NotExecuted) != 1 || result.NotExecuted[0].Address != dependentAddr {
		t.Fatalf("partial failure must fail and block dependents: %+v %+v", result, ds)
	}
	saved := loadState(t, h.statePath)
	rs := saved.Resources[addr]
	if rs == nil || !bytes.Contains(rs.Attributes, []byte("id-partial_failure")) || string(rs.Private) != "partial-recovery" {
		t.Fatal("recoverable provider state was discarded after partial failure")
	}
	if saved.Incomplete == nil || saved.Incomplete.FailedAddress != addr {
		t.Fatal("partial provider result must not mark apply converged")
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

func ingressThing(addrName, cfgName string, ingress []any) *config.Resource {
	return &config.Resource{
		Address:  "tchoritest_ingress_thing." + addrName,
		Type:     "tchoritest_ingress_thing",
		Name:     addrName,
		Provider: "tchoritest",
		Config: map[string]any{
			"name":    cfgName,
			"ingress": ingress,
		},
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

func wantIncomplete(t *testing.T, st *state.State, failed string, applied, remaining []string) {
	t.Helper()
	if st.Incomplete == nil {
		t.Fatal("state has no incomplete_apply marker")
	}
	if st.Incomplete.FailedAddress != failed || !slices.Equal(st.Incomplete.Applied, applied) || !slices.Equal(st.Incomplete.Remaining, remaining) {
		t.Fatalf("incomplete_apply = %+v, want failed=%q applied=%v remaining=%v", st.Incomplete, failed, applied, remaining)
	}
}

// stateAttrs re-loads the saved state file and decodes one resource's
// ctyjson attributes into a plain map.
func diagnosticWithSummary(ds diag.Diagnostics, summary string) *diag.Diagnostic {
	for i := range ds {
		if ds[i].Summary == summary {
			return &ds[i]
		}
	}
	return nil
}

func diagnosticCount(ds diag.Diagnostics, summary string) int {
	count := 0
	for _, d := range ds {
		if d.Summary == summary {
			count++
		}
	}
	return count
}

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

	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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
	if saved.Serial != 3 {
		t.Errorf("state serial = %d, want 3 (1 pre-flight + 1 change + 1 terminal save)", saved.Serial)
	}
	if saved.Incomplete != nil {
		t.Fatalf("successful apply left marker: %+v", saved.Incomplete)
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
	_, createDs := apply.Apply(ctx, createPlan, h.cfg, h.providers, h.schemas, st, h.statePath)
	createConsistency := diagnosticWithSummary(createDs, "provider produced inconsistent result after apply")
	if createConsistency == nil || !strings.Contains(createConsistency.Detail, "flag: planned true, applied false") {
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
	if _, ds := apply.Apply(ctx, updatePlan, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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
	_, replaceDs := apply.Apply(ctx, replacePlan, h.cfg, h.providers, h.schemas, st, h.statePath)
	replaceConsistency := diagnosticWithSummary(replaceDs, "provider produced inconsistent result after apply")
	if replaceConsistency == nil || !strings.Contains(replaceConsistency.Detail, "flag: planned true, applied false") {
		t.Fatalf("replace diagnostics = %#v", replaceDs)
	}

	st = loadState(t, h.statePath)
	destroyPlan := h.plan(t, st, true)
	if _, ds := apply.Apply(ctx, destroyPlan, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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
			_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
			consistency := diagnosticWithSummary(ds, "provider produced inconsistent result after apply")
			if consistency == nil || !strings.Contains(consistency.Detail, tc.want) {
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
		_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
		consistency := diagnosticWithSummary(ds, "provider produced inconsistent result after apply")
		if consistency == nil {
			t.Fatalf("diagnostics = %#v", ds)
		}
		detail := consistency.Detail
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
		_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
		consistency := diagnosticWithSummary(ds, "provider produced inconsistent result after apply")
		if consistency == nil {
			t.Fatalf("diagnostics = %#v", ds)
		}
		if !strings.Contains(consistency.Detail, `tags: planned {"nullify":"visible"}, applied null`) || strings.Contains(consistency.Detail, "tags.nullify") ||
			!strings.Contains(consistency.Detail, "credentials: planned (sensitive value), applied (sensitive value)") || strings.Contains(consistency.Detail, "hidden") {
			t.Fatalf("detail = %s", consistency.Detail)
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
	_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	consistency := diagnosticWithSummary(ds, "provider produced inconsistent result after apply")
	if len(ds) < 3 || ds[0].Severity != diag.Warning || consistency == nil || consistency.Address != lossyAddr || !strings.Contains(consistency.Detail, `tags["dropped"]: planned "id-source", applied absent`) || !strings.Contains(consistency.Detail, "secret: planned (sensitive value), applied (sensitive value)") || strings.Contains(consistency.Detail, "SOURCE") {
		t.Fatalf("diagnostics = %#v", ds)
	}
}

func TestApplyWithholdsSensitiveComputedAndReferencedValues(t *testing.T) {
	a := secretful("a", nil)
	b := secretful("b", map[string]any{"token": "${tchoritest_secretful.a.client_secret}"})                                                                              //nolint:gosec // schema attribute name in redaction regression
	c := secretful("c", map[string]any{"rules": []any{map[string]any{"token": "literal-token-ok"}, map[string]any{"token": "${tchoritest_secretful.a.client_secret}"}}}) //nolint:gosec // fake values exercise per-instance redaction
	h := newHarness(t, map[string]*config.Resource{a.Address: a, b.Address: b, c.Address: c})
	st := loadState(t, h.statePath)
	_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
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

func TestApplyResourceEnvWrapperRoundTrip(t *testing.T) {
	const envName = "TCHORI_TEST_APPLY_NAME"
	t.Setenv(envName, "alpha")
	resource := thing("demo", "unused")
	resource.Config["name"] = map[string]any{"env": envName}
	h := newHarness(t, map[string]*config.Resource{resource.Address: resource})
	ctx := context.Background()

	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "create" {
		t.Fatalf("plan = %+v, want exactly one create", pl.Changes)
	}
	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}
	if got := stateAttrs(t, h.statePath, resource.Address)["name"]; got != "alpha" {
		t.Errorf("saved name = %#v, want environment value alpha", got)
	}

	followUp := h.plan(t, loadState(t, h.statePath), false)
	if len(followUp.Changes) != 1 || followUp.Changes[0].Action != "no-op" || followUp.HasChanges() {
		t.Errorf("follow-up plan = %+v, want one no-op and no changes", followUp)
	}
}

func TestApplyResourceEnvWrapperUnsetBeforeProviderRPC(t *testing.T) {
	const envName = "TCHORI_TEST_APPLY_UNSET"
	t.Setenv(envName, "alpha")
	resource := thing("demo", "unused")
	resource.Config["name"] = map[string]any{"env": envName}
	h := newHarness(t, map[string]*config.Resource{resource.Address: resource})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if err := os.Unsetenv(envName); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply succeeded; want unset environment diagnostic")
	}
	if ds[0].Summary != "environment variable not set" || !strings.Contains(ds[0].Detail, envName) {
		t.Errorf("diagnostics = %+v, want unset variable %q", ds, envName)
	}
	if got := len(loadState(t, h.statePath).Resources); got != 0 {
		t.Errorf("state contains %d resources, want none after pre-RPC composition failure", got)
	}
}

func TestApplyStalePlan(t *testing.T) {
	// No provider is launched at all: a stale plan must be refused before
	// Apply touches providers or the state file.
	statePath := filepath.Join(t.TempDir(), "state.json")
	st := loadState(t, statePath)
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}

	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   5, // plan captured at serial 5; current state is serial 1
		Changes:       []*plan.Change{{Address: "tchoritest_thing.foo", Action: "create"}},
		Summary:       plan.Summary{Create: 1},
	}

	_, ds := apply.Apply(context.Background(), pl, &config.Config{}, nil, nil, st, statePath)
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
	after, err := os.ReadFile(statePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || st.Serial != 1 || st.Incomplete != nil {
		t.Errorf("state changed during stale-plan refusal: serial=%d marker=%+v\nbefore=%s\nafter=%s", st.Serial, st.Incomplete, before, after)
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
	st := loadState(t, statePath)
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}

	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   1,
		Changes:       []*plan.Change{{Address: "tchoritest_thing.foo", Action: "create"}},
		Summary:       plan.Summary{Create: 1},
	}

	// cfg no longer declares tchoritest_thing.foo: the config changed after
	// the plan was created.
	cfg := &config.Config{Resources: map[string]*config.Resource{}}

	_, ds := apply.Apply(context.Background(), pl, cfg, nil, nil, st, statePath)
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
	after, err := os.ReadFile(statePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || st.Serial != 1 || st.Incomplete != nil {
		t.Errorf("state changed during config-drift refusal: serial=%d marker=%+v\nbefore=%s\nafter=%s", st.Serial, st.Incomplete, before, after)
	}
}

func TestApplyOrderFailureLeavesStateUntouched(t *testing.T) {
	const addr = "tchoritest_thing.self"
	statePath := filepath.Join(t.TempDir(), "state.json")
	st := loadState(t, statePath)
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	self := thing("self", "self")
	self.Config["tags"] = map[string]any{"self": "${tchoritest_thing.self.id}"}
	cfg := &config.Config{Resources: map[string]*config.Resource{addr: self}}
	pl := &plan.Plan{FormatVersion: "1.0", StateSerial: st.Serial, Changes: []*plan.Change{{Address: addr, Action: "create"}}}
	if _, ds := apply.Apply(context.Background(), pl, cfg, nil, nil, st, statePath); !ds.HasErrors() {
		t.Fatal("Apply accepted cyclic configuration")
	}
	after, err := os.ReadFile(statePath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || st.Serial != 1 || st.Incomplete != nil {
		t.Fatalf("state changed during order refusal: serial=%d marker=%+v", st.Serial, st.Incomplete)
	}
}

func TestApplySerialAdvancesByChangesPlusTwo(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.alpha": thing("alpha", "alpha"),
		"tchoritest_thing.beta":  thing("beta", "beta"),
	})
	st := loadState(t, h.statePath)
	if _, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}
	if got := loadState(t, h.statePath).Serial; got != 4 {
		t.Fatalf("serial = %d, want 4 (1 pre-flight + 2 change saves + 1 terminal)", got)
	}
}

func TestApplyUnrelatedFailureDoesNotStarveStateOnlyDelete(t *testing.T) {
	const drop = "tchoritest_thing.drop"
	h := newHarness(t, map[string]*config.Resource{drop: thing("drop", "drop")})
	ctx := context.Background()

	st := loadState(t, h.statePath)
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("seed Apply: %+v", ds)
	}

	h.cfg.Resources = map[string]*config.Resource{
		"tchoritest_thing.boom": thing("boom", "explode"),
	}
	st = loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if pl.Summary.Create != 1 || pl.Summary.Delete != 1 {
		t.Fatalf("plan summary = %+v, want one create and one delete", pl.Summary)
	}
	result, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply succeeded despite provider failure")
	}
	if result.Deleted != 1 || result.Created != 0 || len(result.NotExecuted) != 0 {
		t.Fatalf("result = %+v, want one executed delete and no skipped changes", result)
	}
	if saved := loadState(t, h.statePath); saved.Resources[drop] != nil {
		t.Fatalf("%s remains in state after independent create failure", drop)
	}
}

func TestApplyReportsEachNotExecutedChange(t *testing.T) {
	const failed = "tchoritest_thing.boom"
	const blocked = "tchoritest_thing.z_after"
	dependent := thing("z_after", "z_after")
	dependent.Config["tags"] = map[string]any{"dependency": "${tchoritest_thing.boom.id}"}
	const transitive = "tchoritest_thing.zz_transitive"
	transitiveResource := thing("zz_transitive", "zz_transitive")
	transitiveResource.Config["tags"] = map[string]any{"dependency": "${tchoritest_thing.z_after.id}"}
	h := newHarness(t, map[string]*config.Resource{
		failed:     thing("boom", "explode"),
		blocked:    dependent,
		transitive: transitiveResource,
	})
	st := loadState(t, h.statePath)
	result, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply succeeded despite provider failure")
	}
	if len(result.NotExecuted) != 2 || result.NotExecuted[0].Address != blocked || result.NotExecuted[0].Action != "create" || !strings.Contains(result.NotExecuted[0].Reason, failed) || result.NotExecuted[1].Address != transitive || !strings.Contains(result.NotExecuted[1].Reason, blocked) {
		t.Fatalf("NotExecuted = %+v, want direct and transitive blocked creates", result.NotExecuted)
	}
	reported := map[string]bool{}
	for _, d := range ds {
		if d.Severity == diag.Error && d.Summary == "planned change not executed" && strings.Contains(d.Detail, `action "create"`) {
			reported[d.Address] = true
		}
	}
	if !reported[blocked] || !reported[transitive] {
		t.Fatalf("diagnostics do not report each unexecuted create: %+v", ds)
	}
}

func TestApplyPartialFailure(t *testing.T) {
	// Independent changes continue in config order: alpha succeeds, boom
	// fails, and z_after still succeeds. Successful resources must survive in
	// saved state; the failed one must not.
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.alpha":   thing("alpha", "alpha"),
		"tchoritest_thing.boom":    thing("boom", "explode"),
		"tchoritest_thing.z_after": thing("z_after", "z_after"),
	})
	ctx := context.Background()

	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 3 {
		t.Fatalf("plan has %d changes, want 3: %+v", len(pl.Changes), pl.Changes)
	}

	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
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
	if got := stateAttrs(t, h.statePath, "tchoritest_thing.z_after")["id"]; got != "id-z_after" {
		t.Errorf(`later independent resource id = %v, want "id-z_after"`, got)
	}
	if saved.Serial != 4 {
		t.Errorf("state serial = %d, want 4 (pre-flight + 2 successful changes + failure finalizer)", saved.Serial)
	}
	wantIncomplete(t, saved, "tchoritest_thing.boom", []string{"tchoritest_thing.alpha", "tchoritest_thing.z_after"}, []string{"tchoritest_thing.boom"})
	raw, err := os.ReadFile(h.statePath) //nolint:gosec // test-controlled path
	if err != nil || !strings.Contains(string(raw), `"incomplete_apply"`) {
		t.Fatalf("partial state lacks incomplete_apply marker: err=%v bytes=%s", err, raw)
	}
}

func TestApplyZeroAppliedFailureWritesMarker(t *testing.T) {
	const addr = "tchoritest_thing.boom"
	h := newHarness(t, map[string]*config.Resource{addr: thing("boom", "explode")})
	st := loadState(t, h.statePath)
	_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("Apply succeeded despite provider failure")
	}
	saved := loadState(t, h.statePath)
	wantIncomplete(t, saved, addr, []string{}, []string{addr})
	if saved.Serial != 2 {
		t.Fatalf("serial = %d, want 2 (pre-flight + failure finalizer)", saved.Serial)
	}
}

func TestApplyMarkerSaveFailureRefusesProviderCall(t *testing.T) {
	const addr = "tchoritest_thing.boom"
	h := newHarness(t, map[string]*config.Resource{addr: thing("boom", "explode")})
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	h.statePath = filepath.Join(t.TempDir(), "missing", "state.json")
	_, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatal("apply must refuse mutation when it cannot prepare durable state")
	}
	if diagnosticsContain(ds, "apply exploded") {
		t.Fatalf("provider was called after marker save failed: %+v", ds)
	}
	if _, err := os.Stat(h.statePath); !os.IsNotExist(err) {
		t.Fatalf("state file exists after refused apply: %v", err)
	}
}

func TestApplyTerminalSaveFailureKeepsPreflightMarker(t *testing.T) {
	const addr = "tchoritest_thing.boom"
	h := newHarness(t, map[string]*config.Resource{addr: thing("boom", "explode")})
	if err := os.Mkdir(h.statePath+".backup", 0o700); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, h.statePath)
	_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if !diagnosticsContain(ds, "apply exploded") || !diagnosticsContain(ds, "saving incomplete state") {
		t.Fatalf("diagnostics = %+v, want original provider and final-save errors", ds)
	}
	saved := loadState(t, h.statePath)
	if saved.Incomplete == nil || len(saved.Incomplete.Remaining) != 1 || saved.Incomplete.Remaining[0] != addr {
		t.Fatalf("on-disk pre-flight marker = %+v, want remaining %s", saved.Incomplete, addr)
	}
}

func diagnosticsContain(ds diag.Diagnostics, summary string) bool {
	for _, d := range ds {
		if d.Summary == summary {
			return true
		}
	}
	return false
}

func TestApplyReplaceCreateFailureLeavesMarker(t *testing.T) {
	const addr = "tchoritest_thing.foo"
	h := newHarness(t, map[string]*config.Resource{addr: thing("foo", "foo")})
	st := loadState(t, h.statePath)
	if _, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("seed apply: %+v", ds)
	}
	before := loadState(t, h.statePath)
	h.cfg.Resources[addr].Config["name"] = "explode"
	h.cfg.Resources[addr].Config["replace_me"] = "replacement"
	pl := h.plan(t, before, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "replace" {
		t.Fatalf("changes = %+v, want replace", pl.Changes)
	}
	_, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, before, h.statePath)
	if !diagnosticsContain(ds, "apply exploded") {
		t.Fatalf("diagnostics = %+v, want create-leg failure", ds)
	}
	saved := loadState(t, h.statePath)
	if saved.Resources[addr] != nil {
		t.Fatal("resource remains after successful replace destroy leg")
	}
	wantIncomplete(t, saved, addr, []string{}, []string{addr})
}

func TestApplyStateOnlyDeleteFailureAfterSuccess(t *testing.T) {
	const (
		alpha  = "tchoritest_thing.alpha"
		orphan = "tchoritest_thing.orphan"
	)
	h := newHarness(t, map[string]*config.Resource{alpha: thing("alpha", "alpha")})
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	pl.Changes = append(pl.Changes, &plan.Change{Address: orphan, Action: "delete"})
	pl.Summary.Delete++
	st.Resources[orphan] = &state.ResourceState{Type: "tchoritest_thing", Provider: "missing", Attributes: json.RawMessage(`{}`)}

	_, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !diagnosticsContain(ds, "provider not running") {
		t.Fatalf("diagnostics = %+v, want provider-not-running delete failure", ds)
	}
	saved := loadState(t, h.statePath)
	wantIncomplete(t, saved, orphan, []string{alpha}, []string{orphan})
	if saved.Resources[alpha] == nil || saved.Resources[orphan] == nil {
		t.Fatalf("resources = %+v, want successful alpha and untouched orphan", saved.Resources)
	}
}

func TestApplyZeroChangeMarkerClearing(t *testing.T) {
	t.Run("stale marker cleared with one save", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		st := &state.State{Resources: map[string]*state.ResourceState{}, Incomplete: &state.IncompleteApply{Applied: []string{}, Remaining: []string{"thing.old"}}}
		if err := st.Save(path); err != nil {
			t.Fatal(err)
		}
		pl := &plan.Plan{FormatVersion: "1.0", StateSerial: st.Serial}
		if _, ds := apply.Apply(context.Background(), pl, &config.Config{}, nil, nil, st, path); ds.HasErrors() {
			t.Fatalf("Apply: %+v", ds)
		}
		got := loadState(t, path)
		if got.Incomplete != nil || got.Serial != 2 {
			t.Fatalf("state = %+v, want marker cleared at serial 2", got)
		}
	})
	t.Run("converged empty apply writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		st := loadState(t, path)
		pl := &plan.Plan{FormatVersion: "1.0", StateSerial: 0}
		if _, ds := apply.Apply(context.Background(), pl, &config.Config{}, nil, nil, st, path); ds.HasErrors() {
			t.Fatalf("Apply: %+v", ds)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("zero-change converged apply wrote state: %v", err)
		}
	})
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
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create fixture: %#v", ds)
	}

	st = loadState(t, h.statePath)
	_, ds := apply.Apply(ctx, h.plan(t, st, true), h.cfg, h.providers, h.schemas, st, h.statePath)
	providerError := diagnosticWithSummary(ds, "destroy exploded")
	if providerError == nil || providerError.Address != addr {
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
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create fixture: %#v", ds)
	}

	resource.Config["name"] = "explode"
	resource.Config["replace_me"] = "replacement"
	st = loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "replace" {
		t.Fatalf("replace plan = %#v", pl.Changes)
	}
	result, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	providerError := diagnosticWithSummary(ds, "apply exploded")
	if providerError == nil || providerError.Address != addr {
		t.Fatalf("replace diagnostics = %#v", ds)
	}
	if strings.Contains(providerError.Address, addr+"."+addr) {
		t.Fatalf("replace diagnostic was double-prefixed: %#v", providerError)
	}
	if result.Replaced != 0 || len(result.NotExecuted) != 0 {
		t.Fatalf("replace result = %+v, want attempted but incomplete replacement", result)
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

	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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
	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
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
	// Write and reload the safe fixture so its compare-and-swap base serial
	// matches the file that the incomplete-apply preflight must save over.
	stateBytes, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal seeded state: %v", err)
	}
	if err := os.WriteFile(h.statePath, stateBytes, 0o600); err != nil {
		t.Fatalf("write seeded state: %v", err)
	}
	st = loadState(t, h.statePath)
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
	stateBytes, err = json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal poisoned state: %v", err)
	}
	if err := os.WriteFile(h.statePath, stateBytes, 0o600); err != nil {
		t.Fatalf("write poisoned state: %v", err)
	}

	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
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

	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
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
	if saved.Serial != 2 {
		t.Fatalf("state serial = %d, want 2 (marker save + failure-finalizer save; no resource recorded)", saved.Serial)
	}
	wantIncomplete(t, saved, tunnelAddr, []string{}, []string{tunnelAddr, whAddr})
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
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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

	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st2, h.statePath); ds.HasErrors() {
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
	if saved.Serial != 7 {
		t.Errorf("state serial = %d, want 7 (initial 3 saves + replace pre-flight + 2 legs + terminal)", saved.Serial)
	}
}

func TestApplyDestroyFailureBlocksDependencyDelete(t *testing.T) {
	const base = "tchoritest_thing.base"
	const dependentAddr = "tchoritest_thing.dependent"
	dependent := thing("dependent", "explode_destroy")
	dependent.Config["tags"] = map[string]any{"base": "${tchoritest_thing.base.id}"}
	h := newHarness(t, map[string]*config.Resource{
		base:          thing("base", "base"),
		dependentAddr: dependent,
	})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("seed Apply: %+v", ds)
	}

	st = loadState(t, h.statePath)
	result, ds := apply.Apply(ctx, h.plan(t, st, true), h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() || diagnosticWithSummary(ds, "destroy exploded") == nil {
		t.Fatalf("destroy diagnostics = %+v", ds)
	}
	if result.Deleted != 0 || len(result.NotExecuted) != 1 || result.NotExecuted[0].Address != base || result.NotExecuted[0].Action != "delete" || !strings.Contains(result.NotExecuted[0].Reason, dependentAddr) {
		t.Fatalf("destroy result = %+v, want base blocked by dependent", result)
	}
	blocked := diagnosticWithSummary(ds, "planned change not executed")
	if blocked == nil || blocked.Address != base || blocked.Severity != diag.Error {
		t.Fatalf("blocked diagnostic = %+v", blocked)
	}
	saved := loadState(t, h.statePath)
	if saved.Resources[base] == nil || saved.Resources[dependentAddr] == nil {
		t.Fatalf("destroy removed a blocked or failed resource: %+v", saved.Resources)
	}
}

func TestApplyMultipleStateOnlyDeletesIncludeNullAndPrivateState(t *testing.T) {
	const alpha = "tchoritest_thing.alpha"
	const zeta = "tchoritest_thing.zeta"
	h := newHarness(t, map[string]*config.Resource{})
	st := &state.State{FormatVersion: "1.0", Resources: map[string]*state.ResourceState{
		alpha: {
			Type:       "tchoritest_thing",
			Provider:   "tchoritest",
			Attributes: json.RawMessage("null"),
			Private:    []byte("alpha-private"),
		},
		zeta: {
			Type:       "tchoritest_thing",
			Provider:   "tchoritest",
			Attributes: json.RawMessage(`{"echo":"zeta","id":"id-zeta","name":"zeta","replace_me":null,"tags":null}`),
			Private:    []byte("zeta-private"),
		},
	}}
	if err := st.Save(h.statePath); err != nil {
		t.Fatal(err)
	}
	pl := &plan.Plan{StateSerial: st.Serial, Changes: []*plan.Change{
		{Address: alpha, Action: "delete"},
		{Address: zeta, Action: "delete"},
	}}
	result, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}
	if result.Deleted != 2 || len(result.NotExecuted) != 0 {
		t.Fatalf("result = %+v, want two executed deletes", result)
	}
	if saved := loadState(t, h.statePath); len(saved.Resources) != 0 {
		t.Fatalf("state-only deletes remain: %+v", saved.Resources)
	}
}

func TestApplyStateOnlyDeletesKeepReverseLexicalOrderAfterFailures(t *testing.T) {
	const alpha = "tchoritest_thing.alpha"
	const middle = "tchoritest_thing.middle"
	const zeta = "tchoritest_thing.zeta"
	exploding := func(private string) *state.ResourceState {
		return &state.ResourceState{
			Type:       "tchoritest_thing",
			Provider:   "tchoritest",
			Attributes: json.RawMessage(`{"echo":"explode_destroy","id":"id-explode_destroy","name":"explode_destroy","replace_me":null,"tags":null}`),
			Private:    []byte(private),
		}
	}
	h := newHarness(t, map[string]*config.Resource{})
	st := &state.State{FormatVersion: "1.0", Resources: map[string]*state.ResourceState{
		alpha:  {Type: "tchoritest_thing", Provider: "tchoritest", Attributes: json.RawMessage("null")},
		middle: exploding("middle-private"),
		zeta:   exploding("zeta-private"),
	}}
	if err := st.Save(h.statePath); err != nil {
		t.Fatal(err)
	}
	pl := &plan.Plan{StateSerial: st.Serial, Changes: []*plan.Change{
		{Address: alpha, Action: "delete"},
		{Address: middle, Action: "delete"},
		{Address: zeta, Action: "delete"},
	}}
	result, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() || result.Deleted != 1 || len(result.NotExecuted) != 0 {
		t.Fatalf("Apply = (%+v, %+v), want one successful delete and two attempted failures", result, ds)
	}
	var failed []string
	for _, d := range ds {
		if d.Summary == "destroy exploded" {
			failed = append(failed, d.Address)
		}
	}
	if !slices.Equal(failed, []string{zeta, middle}) {
		t.Fatalf("destroy failure order = %v, want reverse lexical %v", failed, []string{zeta, middle})
	}
	saved := loadState(t, h.statePath)
	if saved.Resources[alpha] != nil || saved.Resources[middle] == nil || saved.Resources[zeta] == nil {
		t.Fatalf("state resources = %+v", saved.Resources)
	}
	if !bytes.Equal(saved.Resources[zeta].Private, []byte("zeta-private")) {
		t.Fatalf("failed delete lost private bytes: %q", saved.Resources[zeta].Private)
	}
}

func TestApplyDestroy(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_thing.foo": thing("foo", "foo"),
	})
	ctx := context.Background()

	// Create first.
	st := loadState(t, h.statePath)
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("create Apply: %+v", ds)
	}

	// Reload (as the CLI would) so the destroy plan captures serial 1.
	st2 := loadState(t, h.statePath)
	dpl := h.plan(t, st2, true)
	if len(dpl.Changes) != 1 || dpl.Changes[0].Action != "delete" {
		t.Fatalf("destroy plan = %+v, want exactly one delete change", dpl.Changes)
	}

	if _, ds := apply.Apply(ctx, dpl, h.cfg, h.providers, h.schemas, st2, h.statePath); ds.HasErrors() {
		t.Fatalf("destroy Apply: %+v", ds)
	}

	saved := loadState(t, h.statePath)
	if len(saved.Resources) != 0 {
		t.Errorf("state still holds %d resources after destroy", len(saved.Resources))
	}
	if saved.Serial != 6 {
		t.Errorf("state serial = %d, want 6 (initial 3 saves + destroy pre-flight + leg + terminal)", saved.Serial)
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
	proposed, cds := provider.Compose(h.cfg.Resources[addr].Config, ty, provider.EnvResolve, nil)
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
		FormatVersion: plan.FormatVersion,
		EngineVersion: "0.1.0-dev",
		StateSerial:   0,
		Changes: []*plan.Change{{
			Address:    addr,
			Type:       "tchoritest_thing",
			Provider:   "tchoritest",
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

	_, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
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

	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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

	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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

func TestApplyMixedOptionalNestedObjects(t *testing.T) {
	ingress := []any{
		map[string]any{"service": "http://one"},
		map[string]any{"service": "http://two", "origin_request": map[string]any{}},
	}
	h := newHarness(t, map[string]*config.Resource{
		"tchoritest_ingress_thing.demo": ingressThing("demo", "renamed", ingress),
	})
	const attrs = `{"id":"id-demo","name":"demo","ingress":[{"service":"http://one","origin_request":null},{"service":"http://two","origin_request":{"connect_timeout":null,"no_tls_verify":null}}]}`
	st := &state.State{FormatVersion: "1.0", Serial: 1, Resources: map[string]*state.ResourceState{
		"tchoritest_ingress_thing.demo": {
			Type:       "tchoritest_ingress_thing",
			Provider:   "tchoritest",
			Attributes: json.RawMessage(attrs),
		},
	}}
	pl := h.plan(t, st, false)
	if len(pl.Changes) != 1 || pl.Changes[0].Action != "update" || len(pl.Changes[0].PlannedRaw) == 0 {
		t.Fatalf("plan = %+v, want one update with PlannedRaw", pl.Changes)
	}
	if _, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("Apply: %+v", ds)
	}
	got := stateAttrs(t, h.statePath, "tchoritest_ingress_thing.demo")
	if got["name"] != "renamed" {
		t.Fatalf("saved name = %v, want renamed", got["name"])
	}
	items, ok := got["ingress"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("saved ingress = %#v, want two elements", got["ingress"])
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

	_, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
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
// create+update reproduction of Tchori-Labs/tchori-internal#11 (TC-033): resource
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
	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("seed Apply: %+v", ds)
	}
	if saved := loadState(t, h.statePath); saved.Serial != 3 {
		t.Fatalf("seed state serial = %d, want 3", saved.Serial)
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
	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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

	// Each non-empty apply adds a pre-flight marker and terminal clear in
	// addition to its per-change saves: 3 for the seed plus 4 here.
	if saved := loadState(t, h.statePath); saved.Serial != 7 {
		t.Errorf("state serial = %d, want 7 (bracketing saves across both applies)", saved.Serial)
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

	if _, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
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
	if saved.Serial != 4 {
		t.Errorf("state serial = %d, want 4 (pre-flight + two changes + terminal)", saved.Serial)
	}
	if len(saved.Resources) != 2 {
		t.Errorf("state has %d resources, want 2", len(saved.Resources))
	}
}

func TestApplyReportsPartialProgressAndAttemptedUpdateAfterProvider400(t *testing.T) {
	const first = "tchoritest_thing.a_first"
	const failed = "tchoritest_thing.z_second"
	h := newHarness(t, map[string]*config.Resource{
		first:  thing("a_first", "alpha"),
		failed: thing("z_second", "beta"),
	})
	ctx := context.Background()
	st := loadState(t, h.statePath)
	if _, ds := apply.Apply(ctx, h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
		t.Fatalf("seed apply: %#v", ds)
	}

	h.cfg.Resources[first].Config["name"] = "alpha2"
	h.cfg.Resources[failed].Config["name"] = "api_400"
	st = loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	if pl.Summary != (plan.Summary{Update: 2}) {
		t.Fatalf("summary = %+v, want two updates", pl.Summary)
	}
	result, ds := apply.Apply(ctx, pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() {
		t.Fatalf("Apply diagnostics = %#v, want provider error", ds)
	}
	providerError := diagnosticWithSummary(ds, "Error updating service")
	if providerError == nil || providerError.Detail != "api error (status 400): Invalid request" || providerError.Address != failed {
		t.Fatalf("provider diagnostic changed = %#v", ds)
	}
	if result.Updated != 1 || len(result.NotExecuted) != 0 {
		t.Fatalf("result = %+v, want one completed independent update", result)
	}
	if diagnosticCount(ds, "attempted change") != 1 {
		t.Fatalf("attempted-change count = %d; diagnostics = %#v", diagnosticCount(ds, "attempted change"), ds)
	}
	attempted := diagnosticWithSummary(ds, "attempted change")
	if attempted == nil || attempted.Address != failed || !strings.Contains(attempted.Detail, `name: "beta" -> "api_400"`) {
		t.Fatalf("attempted diagnostic = %#v", attempted)
	}
	if got := stateAttrs(t, h.statePath, first)["name"]; got != "alpha2" {
		t.Fatalf("first update in state = %#v, want alpha2", got)
	}
}

func TestApplyIndependentChangesContinueAfterFirstAndMiddleFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		updates     []string
		failed      string
		wantUpdated int
	}{
		{name: "first", updates: []string{"api_400", "second"}, failed: "tchoritest_thing.a_first", wantUpdated: 1},
		{name: "middle", updates: []string{"first", "api_400", "third"}, failed: "tchoritest_thing.m_middle", wantUpdated: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resources := map[string]*config.Resource{
				"tchoritest_thing.a_first":  thing("a_first", "seed-a"),
				"tchoritest_thing.z_second": thing("z_second", "seed-z"),
			}
			if len(tc.updates) == 3 {
				delete(resources, "tchoritest_thing.z_second")
				resources["tchoritest_thing.m_middle"] = thing("m_middle", "seed-m")
				resources["tchoritest_thing.z_last"] = thing("z_last", "seed-z")
			}
			h := newHarness(t, resources)
			st := loadState(t, h.statePath)
			if _, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath); ds.HasErrors() {
				t.Fatalf("seed apply: %#v", ds)
			}
			addresses := []string{"tchoritest_thing.a_first", "tchoritest_thing.z_second"}
			if len(tc.updates) == 3 {
				addresses = []string{"tchoritest_thing.a_first", "tchoritest_thing.m_middle", "tchoritest_thing.z_last"}
			}
			for i, addr := range addresses {
				h.cfg.Resources[addr].Config["name"] = tc.updates[i]
			}
			st = loadState(t, h.statePath)
			result, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
			if !ds.HasErrors() || diagnosticWithSummary(ds, "Error updating service") == nil {
				t.Fatalf("diagnostics = %#v", ds)
			}
			if result.Updated != tc.wantUpdated || len(result.NotExecuted) != 0 {
				t.Fatalf("result = %+v, want %d independent updates", result, tc.wantUpdated)
			}
			for i, addr := range addresses {
				if addr == tc.failed {
					continue
				}
				if got := stateAttrs(t, h.statePath, addr)["name"]; got != tc.updates[i] {
					t.Fatalf("%s name = %v, want %s", addr, got, tc.updates[i])
				}
			}
		})
	}
}

func TestApplySuccessfulRunEmitsNoAbortDiagnostics(t *testing.T) {
	h := newHarness(t, map[string]*config.Resource{"tchoritest_thing.ok": thing("ok", "ok")})
	st := loadState(t, h.statePath)
	_, ds := apply.Apply(context.Background(), h.plan(t, st, false), h.cfg, h.providers, h.schemas, st, h.statePath)
	if len(ds) != 0 {
		t.Fatalf("successful apply diagnostics = %#v", ds)
	}
}

func TestApplyPreLoopRefusalsDoNotReportAbort(t *testing.T) {
	t.Run("stale plan", func(t *testing.T) {
		st := &state.State{FormatVersion: "1.0", Serial: 2, Resources: map[string]*state.ResourceState{}}
		_, ds := apply.Apply(context.Background(), &plan.Plan{StateSerial: 1}, &config.Config{}, nil, nil, st, filepath.Join(t.TempDir(), "state.json"))
		if diagnosticWithSummary(ds, "apply aborted") != nil {
			t.Fatalf("diagnostics = %#v", ds)
		}
	})
	t.Run("configuration drift", func(t *testing.T) {
		st := &state.State{FormatVersion: "1.0", Resources: map[string]*state.ResourceState{}}
		pl := &plan.Plan{Changes: []*plan.Change{{Address: "tchoritest_thing.gone", Action: "create"}}}
		_, ds := apply.Apply(context.Background(), pl, &config.Config{Resources: map[string]*config.Resource{}}, nil, nil, st, filepath.Join(t.TempDir(), "state.json"))
		if diagnosticWithSummary(ds, "apply aborted") != nil {
			t.Fatalf("diagnostics = %#v", ds)
		}
	})
	t.Run("order failure", func(t *testing.T) {
		a := thing("a", "${tchoritest_thing.b.name}")
		b := thing("b", "${tchoritest_thing.a.name}")
		cfg := &config.Config{Resources: map[string]*config.Resource{a.Address: a, b.Address: b}}
		st := &state.State{FormatVersion: "1.0", Resources: map[string]*state.ResourceState{}}
		pl := &plan.Plan{Changes: []*plan.Change{{Address: a.Address, Action: "create"}, {Address: b.Address, Action: "create"}}}
		_, ds := apply.Apply(context.Background(), pl, cfg, nil, nil, st, filepath.Join(t.TempDir(), "state.json"))
		if !ds.HasErrors() || diagnosticWithSummary(ds, "apply aborted") != nil {
			t.Fatalf("diagnostics = %#v", ds)
		}
	})
}

func TestApplyRequiresArtifactKeyBeforeMutation(t *testing.T) {
	const addr = "tchoritest_thing.example"
	h := newHarness(t, map[string]*config.Resource{addr: thing("example", "example")})
	st := loadState(t, h.statePath)
	pl := h.plan(t, st, false)
	t.Setenv("TCHORI_ARTIFACT_KEY", "")
	result, ds := apply.Apply(context.Background(), pl, h.cfg, h.providers, h.schemas, st, h.statePath)
	if !ds.HasErrors() || diagnosticWithSummary(ds, "invalid artifact key") == nil {
		t.Fatalf("Apply diagnostics = %+v, want invalid artifact key", ds)
	}
	if result.Created != 0 || result.Updated != 0 || result.Deleted != 0 ||
		result.Replaced != 0 || len(result.NotExecuted) != 0 {
		t.Fatalf("Apply result = %+v, want zero result", result)
	}
	if st.Serial != 0 || st.Incomplete != nil || len(st.Resources) != 0 {
		t.Fatal("Apply mutated in-memory state without a key")
	}
	if _, err := os.Stat(h.statePath); !os.IsNotExist(err) {
		t.Fatalf("Apply created state without a key: %v", err)
	}
}

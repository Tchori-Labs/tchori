package plan_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/state"
)

func TestHasChanges(t *testing.T) {
	pl := &plan.Plan{FormatVersion: "1.0"}
	if pl.HasChanges() {
		t.Fatal("empty plan: HasChanges() = true, want false")
	}
	pl.Summary.Create = 1
	if !pl.HasChanges() {
		t.Fatal("summary create=1: HasChanges() = false, want true")
	}
	pl.Summary = plan.Summary{Delete: 2}
	if !pl.HasChanges() {
		t.Fatal("summary delete=2: HasChanges() = false, want true")
	}
}

func TestPlanWriteReadDeterminism(t *testing.T) {
	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   4,
		Changes: []*plan.Change{{
			Address:      "tchoritest_thing.demo",
			Action:       "create",
			Before:       json.RawMessage("null"),
			After:        json.RawMessage(`{"echo":null,"id":null,"name":"demo","replace_me":null,"tags":null}`),
			UnknownAfter: []string{"echo", "id"},
		}},
		Summary: plan.Summary{Create: 1},
	}

	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b1, err := os.ReadFile(path) //nolint:gosec // G304: path is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b1) == 0 || b1[len(b1)-1] != '\n' {
		t.Error("plan.json must end with a trailing newline")
	}
	if !strings.Contains(string(b1), `"format_version": "1.0"`) {
		t.Errorf("plan.json missing two-space-indented format_version:\n%s", b1)
	}

	// Determinism: writing the same plan again is byte-identical.
	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	b2, err := os.ReadFile(path) //nolint:gosec // G304: path is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Error("plan.json is not byte-identical across writes")
	}

	got, err := plan.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.StateSerial != 4 || got.EngineVersion != "0.1.0-dev" || len(got.Changes) != 1 {
		t.Errorf("Read round-trip mismatch: %+v", got)
	}
	if got.Changes[0].Address != "tchoritest_thing.demo" || got.Changes[0].Action != "create" {
		t.Errorf("Read change mismatch: %+v", got.Changes[0])
	}
}

func TestReadRejectsUnknownFormatVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"format_version":"9.9"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Read(path); err == nil {
		t.Fatal("Read accepted format_version 9.9, want error")
	}
}

// --- planner engine tests (against the Task 5 fake provider) -----------------

var testProviderBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tchori-plan-test")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mkdtemp:", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "terraform-provider-tchoritest")
	//nolint:gosec // G204: fixed "go build" argv; only variable part is bin, a t.TempDir-equivalent artifact path.
	cmd := exec.Command("go", "build", "-o", bin, "./internal/provider/testprovider")
	// go test runs with cwd = this package's source dir (internal/plan);
	// the build must run from the module root two levels up.
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "building test provider: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	testProviderBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testConfig builds an in-memory *config.Config with the fake provider and
// the given resources (key = address "type.name", value = raw JSON config).
func testConfig(t *testing.T, resources map[string]map[string]any) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]*config.ProviderConfig{
			"tchoritest": {Name: "tchoritest", Source: "tchori-labs/tchoritest", Version: "0.1.0"},
		},
		Resources: map[string]*config.Resource{},
	}
	for addr, raw := range resources {
		typ, name, ok := strings.Cut(addr, ".")
		if !ok {
			t.Fatalf("bad address %q", addr)
		}
		cfg.Resources[addr] = &config.Resource{
			Address:  addr,
			Type:     typ,
			Name:     name,
			Provider: "tchoritest",
			Config:   raw,
		}
	}
	return cfg
}

// stateWith builds an in-memory *state.State (key = address, value = the
// ctyjson-encoded attributes object exactly as apply would have stored it).
func stateWith(t *testing.T, serial uint64, resources map[string]string) *state.State {
	t.Helper()
	st := &state.State{FormatVersion: "1.0", Serial: serial, Resources: map[string]*state.ResourceState{}}
	for addr, attrs := range resources {
		typ, _, ok := strings.Cut(addr, ".")
		if !ok {
			t.Fatalf("bad address %q", addr)
		}
		st.Resources[addr] = &state.ResourceState{
			Type:       typ,
			Provider:   "tchoritest",
			Attributes: json.RawMessage(attrs),
		}
	}
	return st
}

// newPlanner launches the fake provider, fetches schemas, configures it, and
// returns a ready Planner (Refresh on, Destroy off).
func newPlanner(t *testing.T, cfg *config.Config, st *state.State) *plan.Planner {
	t.Helper()
	ctx := context.Background()
	client, err := provider.Launch(ctx, testProviderBin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	schemas, ds := client.Schemas(ctx)
	if ds.HasErrors() {
		t.Fatalf("Schemas: %+v", ds)
	}
	provCfg, ds := provider.Compose(map[string]any{}, schemas.Provider.Block.ImpliedType(), true, nil)
	if ds.HasErrors() {
		t.Fatalf("compose provider config: %+v", ds)
	}
	if ds := client.Configure(ctx, provCfg); ds.HasErrors() {
		t.Fatalf("Configure: %+v", ds)
	}
	return &plan.Planner{
		Config:        cfg,
		State:         st,
		Providers:     map[string]*provider.Client{"tchoritest": client},
		Schemas:       map[string]*provider.ProviderSchemas{"tchoritest": schemas},
		EngineVersion: "0.1.0-dev",
		Refresh:       true,
	}
}

// Apply-shaped state attributes, exactly as ctyjson.Marshal would emit them
// (compact, attribute keys sorted).
const demoApplied = `{"echo":"demo","id":"id-demo","name":"demo","replace_me":null,"tags":null}`
const demoAppliedOld = `{"echo":"demo","id":"id-demo","name":"demo","replace_me":"old","tags":null}`

func TestPlanCreateWithReference(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.alpha": {"name": "alpha"},
		"tchoritest_thing.beta":  {"name": "${tchoritest_thing.alpha.id}"},
	})
	st := stateWith(t, 0, nil)
	p := newPlanner(t, cfg, st)

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl.FormatVersion != "1.0" {
		t.Errorf("format_version = %q, want \"1.0\"", pl.FormatVersion)
	}
	if pl.StateSerial != 0 {
		t.Errorf("state_serial = %d, want 0", pl.StateSerial)
	}
	if len(pl.Changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(pl.Changes))
	}
	if pl.Changes[0].Address != "tchoritest_thing.alpha" || pl.Changes[1].Address != "tchoritest_thing.beta" {
		t.Fatalf("changes not sorted by address: %s, %s", pl.Changes[0].Address, pl.Changes[1].Address)
	}
	alpha, beta := pl.Changes[0], pl.Changes[1]
	for _, ch := range pl.Changes {
		if ch.Action != "create" {
			t.Errorf("%s: action = %q, want create", ch.Address, ch.Action)
		}
		if string(ch.Before) != "null" {
			t.Errorf("%s: before = %s, want null", ch.Address, ch.Before)
		}
		if len(ch.PlannedRaw) == 0 {
			t.Errorf("%s: planned_raw is empty", ch.Address)
		}
	}
	// Computed attrs are unknown at plan time; never faked.
	if got := fmt.Sprintf("%v", alpha.UnknownAfter); got != "[echo id]" {
		t.Errorf("alpha unknown_after = %v, want [echo id]", alpha.UnknownAfter)
	}
	// beta.name references alpha.id, which is unknown until apply — the
	// unknown must propagate through the resolver into beta's plan.
	if got := fmt.Sprintf("%v", beta.UnknownAfter); got != "[echo id name]" {
		t.Errorf("beta unknown_after = %v, want [echo id name]", beta.UnknownAfter)
	}
	wantAlphaAfter := `{"echo":null,"id":null,"name":"alpha","replace_me":null,"tags":null}`
	if string(alpha.After) != wantAlphaAfter {
		t.Errorf("alpha after = %s, want %s", alpha.After, wantAlphaAfter)
	}
	wantBetaAfter := `{"echo":null,"id":null,"name":null,"replace_me":null,"tags":null}`
	if string(beta.After) != wantBetaAfter {
		t.Errorf("beta after = %s, want %s", beta.After, wantBetaAfter)
	}
	if pl.Summary != (plan.Summary{Create: 2}) {
		t.Errorf("summary = %+v, want {Create:2}", pl.Summary)
	}
	if !pl.HasChanges() {
		t.Error("HasChanges() = false, want true (exit-code-2 case)")
	}

	// plan.json round trip for a provider-produced plan.
	out := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Write(pl, out); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := plan.Read(out)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.StateSerial != pl.StateSerial || len(got.Changes) != 2 || got.Changes[0].Action != "create" {
		t.Errorf("Read round-trip mismatch: %+v", got)
	}
}

func TestPlanNoOpAfterApply(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.demo": {"name": "demo"},
	})
	st := stateWith(t, 1, map[string]string{"tchoritest_thing.demo": demoApplied})
	p := newPlanner(t, cfg, st)

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl.StateSerial != 1 {
		t.Errorf("state_serial = %d, want 1", pl.StateSerial)
	}
	if len(pl.Changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(pl.Changes))
	}
	ch := pl.Changes[0]
	if ch.Action != "no-op" {
		t.Errorf("action = %q, want no-op", ch.Action)
	}
	if len(ch.UnknownAfter) != 0 {
		t.Errorf("unknown_after = %v, want empty", ch.UnknownAfter)
	}
	if string(ch.Before) != demoApplied {
		t.Errorf("before = %s, want %s", ch.Before, demoApplied)
	}
	if string(ch.After) != demoApplied {
		t.Errorf("after = %s, want %s", ch.After, demoApplied)
	}
	if pl.Summary != (plan.Summary{}) {
		t.Errorf("summary = %+v, want all zero", pl.Summary)
	}
	if pl.HasChanges() {
		t.Error("HasChanges() = true, want false (exit-code-0 case)")
	}
}

func TestPlanUpdateAndReplace(t *testing.T) {
	// Replace: replace_me differs from prior, so the fake provider returns it
	// in RequiresReplace and the planned value differs on that path.
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.demo": {"name": "demo", "replace_me": "new"},
	})
	st := stateWith(t, 2, map[string]string{"tchoritest_thing.demo": demoAppliedOld})
	p := newPlanner(t, cfg, st)
	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if len(pl.Changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(pl.Changes))
	}
	ch := pl.Changes[0]
	if ch.Action != "replace" {
		t.Errorf("action = %q, want replace", ch.Action)
	}
	if got := fmt.Sprintf("%v", ch.RequiresReplace); got != "[replace_me]" {
		t.Errorf("requires_replace = %v, want [replace_me]", ch.RequiresReplace)
	}
	wantAfter := `{"echo":"demo","id":"id-demo","name":"demo","replace_me":"new","tags":null}`
	if string(ch.After) != wantAfter {
		t.Errorf("after = %s, want %s", ch.After, wantAfter)
	}
	if pl.Summary != (plan.Summary{Replace: 1}) {
		t.Errorf("summary = %+v, want {Replace:1}", pl.Summary)
	}

	// Update: name changes (echo becomes unknown) but replace_me is unchanged,
	// so no replacement is forced.
	cfg2 := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.demo": {"name": "renamed", "replace_me": "old"},
	})
	st2 := stateWith(t, 2, map[string]string{"tchoritest_thing.demo": demoAppliedOld})
	p2 := newPlanner(t, cfg2, st2)
	pl2, ds2 := p2.Plan(context.Background())
	if ds2.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds2)
	}
	ch2 := pl2.Changes[0]
	if ch2.Action != "update" {
		t.Errorf("action = %q, want update", ch2.Action)
	}
	if len(ch2.RequiresReplace) != 0 {
		t.Errorf("requires_replace = %v, want empty", ch2.RequiresReplace)
	}
	if got := fmt.Sprintf("%v", ch2.UnknownAfter); got != "[echo]" {
		t.Errorf("unknown_after = %v, want [echo]", ch2.UnknownAfter)
	}
	if string(ch2.Before) != demoAppliedOld {
		t.Errorf("before = %s, want %s", ch2.Before, demoAppliedOld)
	}
	if pl2.Summary != (plan.Summary{Update: 1}) {
		t.Errorf("summary = %+v, want {Update:1}", pl2.Summary)
	}
}

func TestPlanDeleteRemovedFromConfig(t *testing.T) {
	cfg := testConfig(t, nil) // provider declared, resource removed from config
	st := stateWith(t, 3, map[string]string{"tchoritest_thing.demo": demoApplied})
	p := newPlanner(t, cfg, st)

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl.StateSerial != 3 {
		t.Errorf("state_serial = %d, want 3", pl.StateSerial)
	}
	if len(pl.Changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(pl.Changes))
	}
	ch := pl.Changes[0]
	if ch.Address != "tchoritest_thing.demo" || ch.Action != "delete" {
		t.Errorf("change = %s %s, want tchoritest_thing.demo delete", ch.Address, ch.Action)
	}
	if string(ch.Before) != demoApplied {
		t.Errorf("before = %s, want state attributes %s", ch.Before, demoApplied)
	}
	if string(ch.After) != "null" {
		t.Errorf("after = %s, want null", ch.After)
	}
	if pl.Summary != (plan.Summary{Delete: 1}) {
		t.Errorf("summary = %+v, want {Delete:1}", pl.Summary)
	}
	if !pl.HasChanges() {
		t.Error("HasChanges() = false, want true")
	}
}

func TestPlanDestroy(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.alpha": {"name": "alpha"},
		"tchoritest_thing.beta":  {"name": "${tchoritest_thing.alpha.id}"},
	})
	st := stateWith(t, 7, map[string]string{
		"tchoritest_thing.alpha": `{"echo":"alpha","id":"id-alpha","name":"alpha","replace_me":null,"tags":null}`,
		"tchoritest_thing.beta":  `{"echo":"id-alpha","id":"id-id-alpha","name":"id-alpha","replace_me":null,"tags":null}`,
	})
	p := newPlanner(t, cfg, st)
	p.Destroy = true

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl.StateSerial != 7 {
		t.Errorf("state_serial = %d, want 7 (stale detection input for Task 11)", pl.StateSerial)
	}
	if len(pl.Changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(pl.Changes))
	}
	// The document is address-sorted; the applier (Task 11) walks deletes in
	// REVERSE plan order, so beta (the dependent) is destroyed before alpha.
	if pl.Changes[0].Address != "tchoritest_thing.alpha" || pl.Changes[1].Address != "tchoritest_thing.beta" {
		t.Fatalf("changes not sorted by address: %s, %s", pl.Changes[0].Address, pl.Changes[1].Address)
	}
	for _, ch := range pl.Changes {
		if ch.Action != "delete" {
			t.Errorf("%s: action = %q, want delete", ch.Address, ch.Action)
		}
		if string(ch.After) != "null" {
			t.Errorf("%s: after = %s, want null", ch.Address, ch.After)
		}
		if string(ch.Before) == "null" {
			t.Errorf("%s: before must carry the state attributes, got null", ch.Address)
		}
		if len(ch.PlannedRaw) == 0 {
			t.Errorf("%s: planned_raw is empty (apply needs the planned null)", ch.Address)
		}
	}
	if pl.Summary != (plan.Summary{Delete: 2}) {
		t.Errorf("summary = %+v, want {Delete:2}", pl.Summary)
	}
	if !pl.HasChanges() {
		t.Error("HasChanges() = false, want true")
	}
}

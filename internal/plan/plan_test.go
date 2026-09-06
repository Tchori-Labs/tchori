package plan_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/privateblob"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/state"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
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

func TestWriteNewFileIsOwnerReadWriteOnly(t *testing.T) {
	pl := &plan.Plan{FormatVersion: plan.FormatVersion}
	path := filepath.Join(t.TempDir(), "plan.json")

	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertPlanContent(t, path, pl)
	assertOwnerReadWriteOnly(t, path)
}

func TestWriteOverwritePermissiveFileTightensMode(t *testing.T) {
	pl := populatedPlan()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil { //nolint:gosec // G306: intentionally reproduce an existing permissive plan artifact
		t.Fatalf("seed permissive plan: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: explicitly defeat umask to reproduce the permissive overwrite
		t.Fatalf("chmod permissive plan: %v", err)
	}

	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertPlanContent(t, path, pl)
	assertOwnerReadWriteOnly(t, path)

	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	assertPlanContent(t, path, pl)
	assertOwnerReadWriteOnly(t, path)

	got, err := plan.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Changes) != 2 || !bytes.Equal(got.Changes[0].PlannedRaw, pl.Changes[0].PlannedRaw) ||
		!bytes.Equal(got.Changes[1].Private, pl.Changes[1].Private) {
		t.Errorf("Write/Read round-trip lost plan payloads: %+v", got.Changes)
	}
}

func TestWriteSymlinkDestinationIsSafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires a permission-supporting platform")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "plan.json")
	sentinel := []byte("target must remain unchanged")
	if err := os.WriteFile(target, sentinel, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		if os.IsPermission(err) {
			t.Skipf("symlink creation is not permitted: %v", err)
		}
		t.Fatalf("create symlink: %v", err)
	}

	pl := populatedPlan()
	if err := plan.Write(pl, link); err != nil {
		if targetContent, readErr := os.ReadFile(target); readErr != nil { //nolint:gosec // G304: target is inside t.TempDir()
			t.Fatalf("read symlink target after rejected Write: %v", readErr)
		} else if !bytes.Equal(targetContent, sentinel) {
			t.Fatalf("rejected Write modified symlink target: got %q", targetContent)
		}
		return
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat replacement: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("destination mode = %v, want regular non-symlink file", info.Mode())
	}
	assertPlanContent(t, link, pl)
	assertOwnerReadWriteOnly(t, link)
	if targetContent, err := os.ReadFile(target); err != nil { //nolint:gosec // G304: target is inside t.TempDir()
		t.Fatalf("read original symlink target: %v", err)
	} else if !bytes.Equal(targetContent, sentinel) {
		t.Fatalf("Write followed symlink: target got %q, want %q", targetContent, sentinel)
	}
}

func TestWriteDirectoryDestinationReturnsWrappedError(t *testing.T) {
	dir := t.TempDir()
	if err := plan.Write(&plan.Plan{FormatVersion: plan.FormatVersion}, dir); err == nil {
		t.Fatal("Write to directory succeeded, want error")
	} else if !strings.Contains(err.Error(), "rename temp plan file") {
		t.Fatalf("Write error = %q, want actionable rename context", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dir), ".plan-*.tmp"))
	if err != nil {
		t.Fatalf("Glob temp plans: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("temporary plan files left behind after failure: %v", matches)
	}
}

func populatedPlan() *plan.Plan {
	return &plan.Plan{
		FormatVersion: plan.FormatVersion,
		EngineVersion: "0.1.0-dev",
		StateSerial:   4,
		Changes: []*plan.Change{
			{
				Address:    "tchoritest_thing.alpha",
				Action:     "update",
				Before:     json.RawMessage(`{"name":"before"}`),
				After:      json.RawMessage(`{"name":"after"}`),
				PlannedRaw: []byte{0x81, 0xa4, 'n', 'a', 'm', 'e'},
			},
			{
				Address:        "tchoritest_thing.beta",
				Action:         "create",
				Type:           "tchoritest_thing",
				Provider:       "tchoritest",
				ProviderSource: "tchori-labs/tchoritest",
				Before:         json.RawMessage("null"),
				After:          json.RawMessage(`{"token":null}`),
				Private:        []byte("provider-private-payload"),
			},
		},
		Summary: plan.Summary{Create: 1, Update: 1},
	}
}

func assertPlanContent(t *testing.T, path string, pl *plan.Plan) []byte {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: path is inside t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var decoded plan.Plan
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode plan content: %v", err)
	}
	// Normalize RawMessage whitespace without invoking the encrypting marshaler.
	type comparablePlan plan.Plan
	comparableJSON := func(value *plan.Plan) []byte {
		data, err := json.Marshal((*comparablePlan)(value))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(comparableJSON(&decoded), comparableJSON(pl)) {
		t.Fatal("plan content changed during Write/Read")
	}
	for i, change := range pl.Changes {
		if !bytes.Equal(decoded.Changes[i].Private, change.Private) {
			t.Fatalf("Write/Read changed private bytes for %s", change.Address)
		}
	}
	return got
}

func assertOwnerReadWriteOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("plan mode = %04o, want 0600", got)
	}
}

func TestPlanWriteReadDeterminism(t *testing.T) {
	pl := &plan.Plan{
		FormatVersion: plan.FormatVersion,
		EngineVersion: "0.1.0-dev",
		StateSerial:   4,
		Changes: []*plan.Change{{
			Address:      "tchoritest_thing.demo",
			Action:       "create",
			Before:       json.RawMessage("null"),
			After:        json.RawMessage(`{"echo":null,"id":null,"name":"demo","replace_me":null,"rules":null,"tags":null}`),
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
	if !strings.Contains(string(b1), `"format_version": "1.1"`) {
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
	if err := os.Setenv("TCHORI_ARTIFACT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32))); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "set artifact key:", err)
		os.Exit(1)
	}
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
			Type:           typ,
			Provider:       "tchoritest",
			Attributes:     json.RawMessage(attrs),
			ProviderSource: "tchori-labs/tchoritest",
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
	provCfg, ds := provider.Compose(map[string]any{}, schemas.Provider.Block.ImpliedType(), provider.EnvResolve, nil)
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
const demoApplied = `{"echo":"demo","id":"id-demo","name":"demo","replace_me":null,"rules":null,"tags":null}`
const demoAppliedOld = `{"echo":"demo","id":"id-demo","name":"demo","replace_me":"old","rules":null,"tags":null}`

func driftApplied(name, echo string) string {
	return fmt.Sprintf(`{"echo":%q,"id":%q,"name":%q,"replace_me":null,"rules":null,"tags":null}`, echo, "id-"+name, name)
}

func TestPlanRejectsUnboundOrChangedStateProviderSourceBeforeRefresh(t *testing.T) {
	const addr = "tchoritest_thing.demo"
	for _, tc := range []struct {
		name    string
		source  string
		summary string
	}{
		{name: "unbound", source: "", summary: "state has unbound provider source"},
		{name: "changed", source: "attacker.example/tchoritest", summary: "state does not match resource identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, map[string]map[string]any{addr: {"name": "demo"}})
			st := stateWith(t, 3, map[string]string{addr: demoApplied})
			st.Resources[addr].ProviderSource = tc.source
			_, ds := newPlanner(t, cfg, st).Plan(context.Background())
			if !ds.HasErrors() {
				t.Fatal("Plan sent stored state to a provider without a matching canonical source")
			}
			found := false
			for _, d := range ds {
				if d.Address == addr && d.Summary == tc.summary {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics = %+v, want %q", ds, tc.summary)
			}
		})
	}
}

func TestPlanRecordsRefreshDriftWithoutChangingExitSemantics(t *testing.T) {
	const addr = "tchoritest_thing.demo"
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "drift-a"}})
	p := newPlanner(t, cfg, stateWith(t, 3, map[string]string{addr: driftApplied("drift-a", "healthy")}))

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl.HasChanges() {
		t.Fatal("drift-only plan must not report pending changes")
	}
	if len(pl.Drift) != 1 || pl.Drift[0].Address != addr || !slices.Equal(pl.Drift[0].Paths, []string{"echo"}) {
		t.Fatalf("drift = %#v", pl.Drift)
	}
	if !bytes.Contains(pl.Drift[0].After, []byte(`"degraded:unhealthy"`)) {
		t.Fatalf("drift after = %s", pl.Drift[0].After)
	}
}

func TestPlanRefreshDriftDisabledAndMatching(t *testing.T) {
	const addr = "tchoritest_thing.demo"
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "drift-a"}})
	for _, test := range []struct {
		name    string
		echo    string
		refresh bool
	}{
		{name: "refresh disabled", echo: "healthy", refresh: false},
		{name: "refresh matches", echo: "degraded:unhealthy", refresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPlanner(t, cfg, stateWith(t, 1, map[string]string{addr: driftApplied("drift-a", test.echo)}))
			p.Refresh = test.refresh
			pl, ds := p.Plan(context.Background())
			if ds.HasErrors() {
				t.Fatalf("Plan diagnostics: %+v", ds)
			}
			if len(pl.Drift) != 0 {
				t.Fatalf("unexpected drift: %#v", pl.Drift)
			}
		})
	}
}

func TestPlanRefreshDriftSortedByAddress(t *testing.T) {
	resources := map[string]map[string]any{
		"tchoritest_thing.zed":   {"name": "drift-z"},
		"tchoritest_thing.alpha": {"name": "drift-a"},
	}
	states := map[string]string{
		"tchoritest_thing.zed":   driftApplied("drift-z", "healthy"),
		"tchoritest_thing.alpha": driftApplied("drift-a", "healthy"),
	}
	pl, ds := newPlanner(t, testConfig(t, resources), stateWith(t, 1, states)).Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if len(pl.Drift) != 2 || pl.Drift[0].Address != "tchoritest_thing.alpha" || pl.Drift[1].Address != "tchoritest_thing.zed" {
		t.Fatalf("drift order = %#v", pl.Drift)
	}
}

// TestPlanRefreshDriftRedactsLegacyPlaintextSecret guards against
// Tchori-Labs/tchori's high-severity drift-leak bug: a resource's state may
// still hold a plaintext value at a path this run considers sensitive (a
// legacy write predating the sensitivity declaration). Drift.Before must
// carry the same spec.Redact treatment as every other reporting artifact
// instead of replaying the recorded state's raw bytes.
func TestPlanRefreshDriftRedactsLegacyPlaintextSecret(t *testing.T) {
	const (
		addr     = "tchoritest_thing.demo"
		sentinel = "sekret-placeholder"
	)
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "drift-secret"}})
	cfg.Resources[addr].SensitiveAttributes = []string{"replace_me"}
	attrs := fmt.Sprintf(`{"echo":"healthy","id":"id-drift-secret","name":"drift-secret","replace_me":%q,"rules":null,"tags":null}`, sentinel)
	p := newPlanner(t, cfg, stateWith(t, 1, map[string]string{addr: attrs}))

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if len(pl.Drift) != 1 || pl.Drift[0].Address != addr {
		t.Fatalf("drift = %#v, want one entry for %s", pl.Drift, addr)
	}
	if bytes.Contains(pl.Drift[0].Before, []byte(sentinel)) {
		t.Fatalf("drift before leaked legacy plaintext secret: %s", pl.Drift[0].Before)
	}
	if !bytes.Contains(pl.Drift[0].Before, []byte(`"replace_me":null`)) {
		t.Fatalf("drift before missing redaction placeholder: %s", pl.Drift[0].Before)
	}
}

// TestPlanSensitivePathsUnionAcrossConfigAndState guards against
// Tchori-Labs/tchori's high-severity sensitive-path bug: removing a
// resource's sensitive_attributes declaration from config must not make the
// planner forget that state already recorded that path as sensitive on a
// prior run, and it must not let the resource's still-remembered secret
// print in plaintext in either the plan document or the re-persisted state.
// spec must be built from the union of config-declared and state-recorded
// sensitive paths, and the write-back to rs.SensitivePaths must never drop a
// path state already recalled.
func TestPlanSensitivePathsUnionAcrossConfigAndState(t *testing.T) {
	const (
		addr     = "tchoritest_thing.demo"
		sentinel = "sekret-placeholder"
	)
	// Config no longer declares "replace_me" sensitive at all.
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "drift-secret"}})
	attrs := fmt.Sprintf(`{"echo":"healthy","id":"id-drift-secret","name":"drift-secret","replace_me":%q,"rules":null,"tags":null}`, sentinel)
	st := stateWith(t, 1, map[string]string{addr: attrs})
	// State recalls "replace_me" as sensitive from a prior run.
	st.Resources[addr].SensitivePaths = []string{"replace_me"}
	p := newPlanner(t, cfg, st)

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}

	if len(pl.Changes) != 1 {
		t.Fatalf("changes=%d, want 1", len(pl.Changes))
	}
	if bytes.Contains(pl.Changes[0].Before, []byte(sentinel)) {
		t.Fatalf("change before leaked a secret config stopped declaring sensitive: %s", pl.Changes[0].Before)
	}
	if !bytes.Contains(pl.Changes[0].Before, []byte(`"replace_me":null`)) {
		t.Fatalf("change before missing redaction placeholder: %s", pl.Changes[0].Before)
	}

	if len(pl.Drift) != 1 || pl.Drift[0].Address != addr {
		t.Fatalf("drift = %#v, want one entry for %s", pl.Drift, addr)
	}
	if bytes.Contains(pl.Drift[0].Before, []byte(sentinel)) {
		t.Fatalf("drift before leaked a secret config stopped declaring sensitive: %s", pl.Drift[0].Before)
	}

	if !slices.Contains(st.Resources[addr].SensitivePaths, "replace_me") {
		t.Fatalf("state SensitivePaths after Plan = %v, want it to still recall \"replace_me\"", st.Resources[addr].SensitivePaths)
	}
}

func TestPlanGatewayHTMLRefreshDiagnostic(t *testing.T) {
	const (
		addr    = "tchoritest_thing.web"
		summary = "Error reading project"
		detail  = "decoding response: invalid character '<' looking for beginning of value"
	)
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "gateway_html"}})
	st := stateWith(t, 1, map[string]string{addr: `{"echo":"gateway_html","id":"id-gateway_html","name":"gateway_html","replace_me":null,"rules":null,"tags":null}`})

	pl, ds := newPlanner(t, cfg, st).Plan(context.Background())
	if pl != nil {
		t.Fatalf("Plan returned a plan despite refresh failure: %#v", pl)
	}
	if !ds.HasErrors() {
		t.Fatalf("Plan diagnostics have no error: %#v", ds)
	}
	if len(ds) != 2 {
		t.Fatalf("len(diagnostics) = %d, want provider error plus one hint: %#v", len(ds), ds)
	}
	if ds[0].Address != addr || ds[0].Summary != summary || ds[0].Detail != detail {
		t.Fatalf("provider diagnostic was not attributed verbatim: %#v", ds[0])
	}
	if ds[1].Severity != "warning" || ds[1].Address != addr || ds[1].Summary != "provider received a non-JSON response (HTML)" || !strings.Contains(ds[1].Detail, "identity-aware proxy") {
		t.Fatalf("advisory hint = %#v", ds[1])
	}
}

func TestPlanGatewayAttributeRefreshDiagnostic(t *testing.T) {
	const addr = "tchoritest_thing.web"
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "gateway_attr"}})
	st := stateWith(t, 1, map[string]string{addr: `{"echo":"gateway_attr","id":"id-gateway_attr","name":"gateway_attr","replace_me":null,"rules":null,"tags":null}`})
	_, ds := newPlanner(t, cfg, st).Plan(context.Background())
	if len(ds) != 1 || ds[0].Address != addr+".name" || ds[0].Summary != "invalid remote name" {
		t.Fatalf("attribute diagnostic = %#v", ds)
	}
}

func TestPlanValidateAndPlanDiagnosticsHaveResourceAddress(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		summary string
	}{
		{name: "validate RPC", value: "invalid", summary: "invalid name"},
		{name: "plan RPC", value: "invalid_plan", summary: "invalid planned name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const addr = "tchoritest_thing.web"
			cfg := testConfig(t, map[string]map[string]any{addr: {"name": test.value}})
			p := newPlanner(t, cfg, stateWith(t, 0, nil))
			p.Refresh = false
			_, ds := p.Plan(context.Background())
			if len(ds) != 1 || ds[0].Address != addr || ds[0].Summary != test.summary {
				t.Fatalf("diagnostics = %#v", ds)
			}
		})
	}
}

func TestPlanRedactsSensitiveArtifactsAndPlannedRaw(t *testing.T) {
	const sentinel = "tchori-e2e-super-secret-value"
	addr := "tchoritest_secretful.demo"
	cfg := testConfig(t, map[string]map[string]any{addr: {"name": "demo"}})
	attrs := `{"name":"demo","id":"secret-demo","client_secret":"` + sentinel + `","write_only_secret":null,"token":null,"note":null,"rules":null}`
	st := stateWith(t, 1, map[string]string{addr: attrs})
	p := newPlanner(t, cfg, st)
	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan: %#v", ds)
	}
	if len(pl.Changes) != 1 {
		t.Fatalf("changes=%d", len(pl.Changes))
	}
	ch := pl.Changes[0]
	if bytes.Contains(ch.Before, []byte(sentinel)) || bytes.Contains(ch.After, []byte(sentinel)) || bytes.Contains(ch.PlannedRaw, []byte(sentinel)) {
		t.Fatal("sentinel leaked into plan artifact")
	}
	sch, _, _ := p.Schemas["tchoritest"].LookupResourceType("tchoritest_secretful")
	v, err := ctymsgpack.Unmarshal(ch.PlannedRaw, sch.Block.ImpliedType())
	if err != nil {
		t.Fatal(err)
	}
	if v.GetAttr("client_secret").IsKnown() {
		t.Fatal("planned_raw client_secret is not unknown")
	}
	if ch.Action != "no-op" {
		t.Fatalf("action=%s, want no-op", ch.Action)
	}
	if !slices.Contains(ch.UnknownAfter, "client_secret") {
		t.Fatalf("unknown_after=%v", ch.UnknownAfter)
	}
}

func TestPlanStateOnlyDeleteUsesPersistedSensitivePaths(t *testing.T) {
	addr := "tchoritest_secretful.gone"
	st := stateWith(t, 1, map[string]string{addr: `{"name":"gone","id":"id","client_secret":null,"write_only_secret":null,"token":null,"note":"tchori-e2e-super-secret-value","rules":null}`})
	st.Resources[addr].SensitivePaths = []string{"note"}
	p := newPlanner(t, testConfig(t, nil), st)
	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan: %#v", ds)
	}
	if bytes.Contains(pl.Changes[0].Before, []byte("tchori-e2e-super-secret-value")) {
		t.Fatal("delete before leaked custom sensitive value")
	}
}

func TestPlanRejectsEmbeddedReference(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.tunnel": {"name": "tunnel"},
		"tchoritest_thing.wh": {
			"name": "wh",
			"tags": map[string]any{
				"content": "${tchoritest_thing.tunnel.id}.cfargotunnel.com",
			},
		},
	})
	p := newPlanner(t, cfg, stateWith(t, 0, nil))

	pl, ds := p.Plan(context.Background())
	if pl != nil {
		t.Fatalf("Plan = %+v, want nil for unresolved reference", pl)
	}
	found := false
	for _, d := range ds {
		if d.Summary == "unresolved reference" && strings.Contains(d.Detail, "tags.content") && strings.Contains(d.Detail, "${tchoritest_thing.tunnel.id}") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want unresolved reference at tags.content", ds)
	}
}

func TestPlanCreateWithResourceEnvWrapper(t *testing.T) {
	t.Setenv("TCHORI_TEST_NAME", "alpha")
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.demo": {"name": map[string]any{"env": "TCHORI_TEST_NAME"}},
	})
	p := newPlanner(t, cfg, stateWith(t, 0, nil))

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl == nil || len(pl.Changes) != 1 {
		t.Fatalf("plan changes = %+v, want exactly one change", pl)
	}
	var after map[string]any
	if err := json.Unmarshal(pl.Changes[0].After, &after); err != nil {
		t.Fatalf("decode planned after: %v", err)
	}
	if got := after["name"]; got != "alpha" {
		t.Errorf("planned name = %#v, want environment value %q", got, "alpha")
	}
	for _, path := range pl.Changes[0].UnknownAfter {
		if path == "name" {
			t.Errorf("unknown_after = %v, environment value must be concrete", pl.Changes[0].UnknownAfter)
		}
	}
}

func TestPlanResourceEnvWrappersNestedAndReferenced(t *testing.T) {
	t.Setenv("TCHORI_TEST_NAME", "alpha")
	t.Setenv("TCHORI_TEST_TAG", "secret-tag")
	t.Setenv("TCHORI_TEST_LABEL", "secret-label")
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.alpha": {
			"name": map[string]any{"env": "TCHORI_TEST_NAME"},
			"tags": map[string]any{"token": map[string]any{"env": "TCHORI_TEST_TAG"}},
		},
		"tchoritest_thing.beta": {
			"name": "${tchoritest_thing.alpha.name}",
		},
		"tchoritest_nested_thing.nested": {
			"name":     "nested",
			"settings": map[string]any{"label": map[string]any{"env": "TCHORI_TEST_LABEL"}},
		},
	})
	p := newPlanner(t, cfg, stateWith(t, 0, nil))

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	changes := make(map[string]map[string]any, len(pl.Changes))
	for _, change := range pl.Changes {
		var after map[string]any
		if err := json.Unmarshal(change.After, &after); err != nil {
			t.Fatalf("decode %s after: %v", change.Address, err)
		}
		changes[change.Address] = after
	}
	alpha := changes["tchoritest_thing.alpha"]
	if got := alpha["tags"].(map[string]any)["token"]; got != "secret-tag" {
		t.Errorf("planned map env value = %#v, want secret-tag", got)
	}
	if got := changes["tchoritest_thing.beta"]["name"]; got != "alpha" {
		t.Errorf("onward reference to env value = %#v, want alpha", got)
	}
	nested := changes["tchoritest_nested_thing.nested"]["settings"].(map[string]any)
	if got := nested["label"]; got != "secret-label" {
		t.Errorf("planned nested env value = %#v, want secret-label", got)
	}
}

func TestPlanResourceEnvWrapperUnset(t *testing.T) {
	const envName = "TCHORI_TEST_PLAN_UNSET"
	t.Setenv(envName, "placeholder")
	if err := os.Unsetenv(envName); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_thing.demo": {"name": map[string]any{"env": envName}},
	})
	p := newPlanner(t, cfg, stateWith(t, 0, nil))

	pl, ds := p.Plan(context.Background())
	if pl != nil {
		t.Errorf("plan = %+v, want no partial plan", pl)
	}
	if !ds.HasErrors() {
		t.Fatal("Plan succeeded; want unset environment diagnostic")
	}
	if ds[0].Summary != "environment variable not set" || !strings.Contains(ds[0].Detail, envName) {
		t.Errorf("diagnostics = %+v, want unset variable %q", ds, envName)
	}
}

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
	if pl.FormatVersion != plan.FormatVersion {
		t.Errorf("format_version = %q, want %q", pl.FormatVersion, plan.FormatVersion)
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
	wantAlphaAfter := `{"echo":null,"id":null,"name":"alpha","replace_me":null,"rules":null,"tags":null}`
	if string(alpha.After) != wantAlphaAfter {
		t.Errorf("alpha after = %s, want %s", alpha.After, wantAlphaAfter)
	}
	wantBetaAfter := `{"echo":null,"id":null,"name":null,"replace_me":null,"rules":null,"tags":null}`
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
	wantAfter := `{"echo":"demo","id":"id-demo","name":"demo","replace_me":"new","rules":null,"tags":null}`
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
		"tchoritest_thing.alpha": `{"echo":"alpha","id":"id-alpha","name":"alpha","replace_me":null,"rules":null,"tags":null}`,
		"tchoritest_thing.beta":  `{"echo":"id-alpha","id":"id-id-alpha","name":"id-alpha","replace_me":null,"rules":null,"tags":null}`,
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

// TestPlanUnsupportedResourceType guards the fix for issue #5: a config that
// references tchoritest_broken_thing (a resource type whose schema tchori
// cannot convert — see testprovider's brokenThingSchema) must still fail at
// Plan() time, but with a diagnostic that distinguishes "unsupported schema"
// (known type, unconvertible schema) from "unknown resource type" (the
// provider never defined it), naming the stored conversion detail. Every
// other test in this file plans configs that reference only
// tchoritest_thing, so the fake provider (rebuilt for every test in this
// package with the broken type present too) already exercises the
// "Schemas() succeeds despite one unsupported type" half of the fix.
func TestPlanUnsupportedResourceType(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_broken_thing.boom": {"name": "boom"},
	})
	st := stateWith(t, 0, nil)
	p := newPlanner(t, cfg, st)

	_, ds := p.Plan(context.Background())
	if !ds.HasErrors() {
		t.Fatal("Plan succeeded for a resource type with an unsupported (nested_type) schema, want error")
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

// serverAssignedApplied is tchoritest_server_assigned.tunnel exactly as apply
// would have stored it: the operator's "name" plus three attributes the
// remote API decided.
const serverAssignedApplied = `{"created_at":"2026-01-01T00:00:00Z","id":"id-tunnel","name":"tunnel","status":"inactive"}`

// TestPlanConvergesWhenProviderTrustsProposedNewState is the regression test
// for issue #59.
//
// Config declares only "name". The other three attributes are Computed, so
// the engine must propose the values already in state rather than null — a
// null there tells the provider the operator wants those fields cleared, and
// the plan never converges.
//
// tchoritest_server_assigned is used rather than tchoritest_thing precisely
// because it does NOT repair computed attributes on its own: it echoes the
// proposed new state back. That makes it behave like the providers this bug
// was reported against, where a perpetual diff turned into a PATCH carrying
// nulls.
func TestPlanConvergesWhenProviderTrustsProposedNewState(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_server_assigned.tunnel": {"name": "tunnel"},
	})
	st := stateWith(t, 1, map[string]string{"tchoritest_server_assigned.tunnel": serverAssignedApplied})
	p := newPlanner(t, cfg, st)

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if len(pl.Changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(pl.Changes))
	}
	ch := pl.Changes[0]
	if ch.Action != "no-op" {
		t.Errorf("action = %q, want no-op — an unchanged config must not plan an update\nafter: %s", ch.Action, ch.After)
	}
	if string(ch.After) != serverAssignedApplied {
		t.Errorf("after  = %s\nwant   = %s", ch.After, serverAssignedApplied)
	}
	if pl.HasChanges() {
		t.Error("HasChanges() = true, want false: this resource can never converge while it plans an update every run")
	}
}

// A second plan over the state a first apply produced must also be a no-op —
// the "every run" half of #59.
func TestPlanServerAssignedIsStableAcrossRuns(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_server_assigned.tunnel": {"name": "tunnel"},
	})
	st := stateWith(t, 1, map[string]string{"tchoritest_server_assigned.tunnel": serverAssignedApplied})

	for run := 1; run <= 2; run++ {
		p := newPlanner(t, cfg, st)
		pl, ds := p.Plan(context.Background())
		if ds.HasErrors() {
			t.Fatalf("run %d: Plan diagnostics: %+v", run, ds)
		}
		if pl.HasChanges() {
			t.Fatalf("run %d: plan reports changes for an unchanged config: %s", run, pl.Changes[0].After)
		}
	}
}

// Changing the one attribute the operator owns must still plan an update, and
// must carry the server-assigned attributes through untouched rather than
// clearing them.
func TestPlanServerAssignedUpdateKeepsComputedValues(t *testing.T) {
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_server_assigned.tunnel": {"name": "renamed"},
	})
	st := stateWith(t, 1, map[string]string{"tchoritest_server_assigned.tunnel": serverAssignedApplied})
	p := newPlanner(t, cfg, st)

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	ch := pl.Changes[0]
	if ch.Action != "update" {
		t.Fatalf("action = %q, want update", ch.Action)
	}
	var after map[string]any
	if err := json.Unmarshal(ch.After, &after); err != nil {
		t.Fatalf("cannot decode after: %v", err)
	}
	if after["name"] != "renamed" {
		t.Errorf("after.name = %v, want %q", after["name"], "renamed")
	}
	for attr, want := range map[string]any{
		"id":         "id-tunnel",
		"status":     "inactive",
		"created_at": "2026-01-01T00:00:00Z",
	} {
		if after[attr] != want {
			t.Errorf("after.%s = %v, want %v — a rename must not clear server-assigned fields", attr, after[attr], want)
		}
	}
}

func TestPlanRefreshMixedOptionalNestedObjects(t *testing.T) {
	const attrs = `{"id":"id-demo","name":"demo","ingress":[{"service":"http://one","origin_request":null},{"service":"http://two","origin_request":{"connect_timeout":null,"no_tls_verify":null}}]}`
	cfg := testConfig(t, map[string]map[string]any{
		"tchoritest_ingress_thing.demo": {
			"name": "demo",
			"ingress": []any{
				map[string]any{"service": "http://one"},
				map[string]any{"service": "http://two", "origin_request": map[string]any{}},
			},
		},
	})
	st := stateWith(t, 1, map[string]string{"tchoritest_ingress_thing.demo": attrs})
	p := newPlanner(t, cfg, st)
	p.Refresh = true

	pl, ds := p.Plan(context.Background())
	if ds.HasErrors() {
		t.Fatalf("Plan diagnostics: %+v", ds)
	}
	if pl == nil {
		t.Fatal("Plan returned nil without diagnostics")
	}
}

func TestPlanPrivateEncryptedAcrossJSONSeams(t *testing.T) {
	const sentinel = "plan-private-sentinel"
	t.Setenv("TCHORI_ARTIFACT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{32}, 32)))
	pl := &plan.Plan{
		FormatVersion: plan.FormatVersion,
		Changes: []*plan.Change{{
			Address: "test_thing.example", Type: "test_thing", Provider: "test",
			ProviderSource: "example.test/test",
			Action:         "create", Before: json.RawMessage("null"), After: json.RawMessage(`{}`),
			Private: []byte(sentinel),
		}},
	}
	data, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("json.Marshal = %v", err)
	}
	if bytes.Contains(data, []byte(sentinel)) ||
		bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString([]byte(sentinel)))) ||
		!bytes.Contains(data, []byte(`"private":{`)) {
		t.Fatalf("plan JSON did not protect private bytes: %s", data)
	}
	var decoded plan.Plan
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal = %v", err)
	}
	if len(decoded.Changes) != 1 || !bytes.Equal(decoded.Changes[0].Private, []byte(sentinel)) {
		t.Fatal("plan JSON round-trip lost exact private bytes")
	}

	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("Write = %v", err)
	}
	got, err := plan.Read(path)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if !bytes.Equal(got.Changes[0].Private, []byte(sentinel)) {
		t.Fatal("Write/Read round-trip lost exact private bytes")
	}
}

func TestPlanPrivateFailsClosed(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{33}, 32))
	t.Setenv("TCHORI_ARTIFACT_KEY", key)
	pl := &plan.Plan{
		FormatVersion: plan.FormatVersion,
		Changes: []*plan.Change{
			{Address: "test_thing.alpha", Type: "test_thing", Provider: "test", ProviderSource: "example.test/test", Action: "create", Before: json.RawMessage("null"), After: json.RawMessage(`{}`), Private: []byte("alpha-private")},
			{Address: "test_thing.beta", Type: "test_thing", Provider: "test", ProviderSource: "example.test/test", Action: "create", Before: json.RawMessage("null"), After: json.RawMessage(`{}`), Private: []byte("beta-private")},
		},
	}
	data, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing key marshal", func(t *testing.T) {
		if err := os.Unsetenv("TCHORI_ARTIFACT_KEY"); err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(pl); err == nil {
			t.Fatal("json.Marshal accepted private data without an artifact key")
		}
	})

	t.Run("wrong key unmarshal", func(t *testing.T) {
		t.Setenv("TCHORI_ARTIFACT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{34}, 32)))
		var got plan.Plan
		if err := json.Unmarshal(data, &got); err == nil {
			t.Fatal("json.Unmarshal accepted encrypted private data under the wrong key")
		}
	})

	t.Run("cross-change replay", func(t *testing.T) {
		t.Setenv("TCHORI_ARTIFACT_KEY", key)
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		changes := doc["changes"].([]any)
		alpha := changes[0].(map[string]any)
		beta := changes[1].(map[string]any)
		beta["private"] = alpha["private"]
		replayed, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var got plan.Plan
		if err := json.Unmarshal(replayed, &got); err == nil {
			t.Fatal("json.Unmarshal accepted a private envelope replayed at another address")
		}
	})

	t.Run("provider source tamper", func(t *testing.T) {
		t.Setenv("TCHORI_ARTIFACT_KEY", key)
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		changes := doc["changes"].([]any)
		changes[0].(map[string]any)["provider_source"] = "attacker.example/test"
		tampered, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var got plan.Plan
		if err := json.Unmarshal(tampered, &got); err == nil {
			t.Fatal("json.Unmarshal accepted private plan data under a different canonical provider source")
		}
	})

	t.Run("1.1 plaintext", func(t *testing.T) {
		t.Setenv("TCHORI_ARTIFACT_KEY", key)
		plaintext := `{"format_version":"1.1","engine_version":"dev","state_serial":0,"changes":[{"address":"test_thing.alpha","type":"test_thing","provider":"test","action":"create","before":null,"after":{},"private":"YWxwaGEtcHJpdmF0ZQ=="}],"summary":{"create":1,"update":0,"delete":0,"replace":0}}`
		var got plan.Plan
		if err := json.Unmarshal([]byte(plaintext), &got); err == nil {
			t.Fatal("json.Unmarshal accepted plaintext/base64 private data in format 1.1")
		}
	})
}

func TestReadUnbound11PrivateForExplicitMigration(t *testing.T) {
	t.Setenv("TCHORI_ARTIFACT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{35}, 32)))
	const sentinel = "pre-source-binding-plan-private"
	sealed, err := privateblob.Seal([]byte(sentinel), "plan\x00test_thing.example\x00test\x00test_thing")
	if err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf(
		`{"format_version":"1.1","engine_version":"dev","state_serial":2,"changes":[{"address":"test_thing.example","type":"test_thing","provider":"test","action":"update","before":{},"after":{},"private":%s}],"summary":{"create":0,"update":1,"delete":0,"replace":0}}`,
		sealed,
	)
	var got plan.Plan
	if err := json.Unmarshal([]byte(document), &got); err != nil {
		t.Fatalf("read pre-source-binding 1.1 plan: %v", err)
	}
	if got.Changes[0].ProviderSource != "" || !bytes.Equal(got.Changes[0].Private, []byte(sentinel)) {
		t.Fatal("pre-source-binding 1.1 plan did not remain readable for explicit migration refusal")
	}
}

func TestReadLegacyPlanPrivate(t *testing.T) {
	const sentinel = "legacy-plan-private"
	legacy := fmt.Sprintf(
		`{"format_version":"1.0","engine_version":"dev","state_serial":2,"changes":[{"address":"test_thing.example","action":"update","before":{},"after":{},"private":%q}],"summary":{"create":0,"update":1,"delete":0,"replace":0}}`,
		base64.StdEncoding.EncodeToString([]byte(sentinel)),
	)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := plan.Read(path)
	if err != nil {
		t.Fatalf("Read legacy plan = %v", err)
	}
	if !bytes.Equal(got.Changes[0].Private, []byte(sentinel)) {
		t.Fatal("legacy read lost private bytes")
	}
}

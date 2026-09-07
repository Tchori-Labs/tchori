package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// buildTestProvider compiles the Task 5 fake provider into a temp dir and
// returns the binary path. Using the module-qualified package path makes
// the build independent of the test's working directory.
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

// buildTestProvider5 compiles the protocol-5 fake provider (testprovider5)
// into a temp dir and returns the binary path.
func buildTestProvider5(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "terraform-provider-tchoritest5")
	cmd := exec.Command("go", "build", "-o", bin, //nolint:gosec // fixed command; bin is a t.TempDir artifact
		"github.com/tchori-labs/tchori/internal/provider/testprovider5")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building protocol-5 test provider: %v\n%s", err, out)
	}
	return bin
}

// TestLaunchProtocol5AndSchemas proves the tfplugin5 adapter end to end: a
// protocol-5-only provider (testprovider5) launches successfully through
// Launch's dual-protocol negotiation, and Schemas returns the same shapes
// TestLaunchAndSchemas checks for the protocol-6 fake provider — implied
// type equality across both fixtures proves the 5->6 schema conversion.
func TestLaunchProtocol5AndSchemas(t *testing.T) {
	bin := buildTestProvider5(t)
	ctx := context.Background()

	c, err := Launch(ctx, bin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = c.Close()
		}
	})

	schemas, ds := c.Schemas(ctx)
	if ds.HasErrors() {
		t.Fatalf("Schemas returned error diagnostics: %+v", ds)
	}
	if schemas == nil || schemas.Provider == nil || schemas.Provider.Block == nil {
		t.Fatal("nil provider schema")
	}

	prefix := schemas.Provider.Block.Attributes["prefix"]
	if prefix == nil {
		t.Fatal("provider schema missing attribute \"prefix\"")
	}
	if !prefix.Optional || !prefix.Type.Equals(cty.String) {
		t.Errorf("prefix = %+v, want optional string", prefix)
	}

	thing := schemas.ResourceTypes["tchoritest5_thing"]
	if thing == nil {
		t.Fatalf("missing tchoritest5_thing resource schema; got resource types %v",
			schemas.ResourceTypes)
	}

	want := cty.Object(map[string]cty.Type{
		"name":       cty.String,
		"tags":       cty.Map(cty.String),
		"replace_me": cty.String,
		"rules":      cty.List(cty.Object(map[string]cty.Type{"token_id": cty.String})),
		"id":         cty.String,
		"members":    cty.Set(cty.String),
		"echo":       cty.String,
	})
	if got := thing.Block.ImpliedType(); !got.Equals(want) {
		t.Errorf("ImpliedType = %#v, want %#v", got, want)
	}
	members := thing.Block.Attributes["members"]
	if members == nil || !members.Sensitive || members.NestedType != nil || !members.Type.Equals(cty.Set(cty.String)) {
		t.Fatalf("members = %+v, want sensitive flat set(string) without nested metadata", members)
	}

	// Close terminates the subprocess (the Stop mapping through the adapter).
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	deadline := time.Now().Add(5 * time.Second)
	for !c.plugin.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("protocol-5 provider subprocess still running 5s after Close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLaunchNegotiatesNeitherProtocolFails proves that a provider offering
// neither protocol 6 nor 5 still fails fast with the structured
// "provider protocol unsupported" diagnostic, now naming both supported
// protocols. This exercises Launch's default branch on
// pc.NegotiatedVersion(): the only way to force it deterministically from a
// real binary is a provider that speaks solely an unrelated protocol, which
// go-plugin itself rejects during the handshake before RPCs are ever
// attempted — the same "incompatible API version with plugin" failure Launch
// already classifies, just with go-plugin negotiating against neither 6 nor
// 5 instead of just 6.
func TestLaunchNegotiatesNeitherProtocolFails(t *testing.T) {
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "terraform-provider-protocol4only")
	cmd := exec.Command("go", "build", "-o", bin, //nolint:gosec // fixed command; bin is a t.TempDir artifact
		"github.com/tchori-labs/tchori/internal/provider/testprovider4only")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building protocol-4-only test provider: %v\n%s", err, out)
	}

	client, err := Launch(context.Background(), bin)
	if client != nil {
		t.Cleanup(func() { _ = client.Close() })
		t.Fatal("Launch returned a client for a provider offering neither protocol 6 nor 5")
	}
	if err == nil {
		t.Fatal("Launch returned nil error for a provider offering neither protocol 6 nor 5")
	}
	if !strings.Contains(err.Error(), "provider protocol unsupported") {
		t.Errorf("Launch error does not name the unsupported protocol: %q", err)
	}
	if !strings.Contains(err.Error(), "tfplugin6") || !strings.Contains(err.Error(), "tfplugin5") {
		t.Errorf("Launch error does not name both tfplugin6 and tfplugin5: %q", err)
	}
}

func TestLaunchAndSchemas(t *testing.T) {
	bin := buildTestProvider(t)
	ctx := context.Background()

	c, err := Launch(ctx, bin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})

	schemas, ds := c.Schemas(ctx)
	if ds.HasErrors() {
		t.Fatalf("Schemas returned error diagnostics: %+v", ds)
	}
	if schemas == nil || schemas.Provider == nil || schemas.Provider.Block == nil {
		t.Fatal("nil provider schema")
	}

	// Provider config schema: { "prefix": optional string }.
	prefix := schemas.Provider.Block.Attributes["prefix"]
	if prefix == nil {
		t.Fatal("provider schema missing attribute \"prefix\"")
	}
	if !prefix.Optional || !prefix.Type.Equals(cty.String) {
		t.Errorf("prefix = %+v, want optional string", prefix)
	}

	thing := schemas.ResourceTypes["tchoritest_thing"]
	if thing == nil {
		t.Fatalf("missing tchoritest_thing resource schema; got resource types %v",
			schemas.ResourceTypes)
	}
	attrs := thing.Block.Attributes

	name := attrs["name"]
	if name == nil {
		t.Fatal("missing attribute \"name\"")
	}
	if !name.Required || !name.Type.Equals(cty.String) {
		t.Errorf("name = %+v, want required string", name)
	}

	id := attrs["id"]
	if id == nil {
		t.Fatal("missing attribute \"id\"")
	}
	if !id.Computed || !id.Type.Equals(cty.String) {
		t.Errorf("id = %+v, want computed string", id)
	}

	tags := attrs["tags"]
	if tags == nil {
		t.Fatal("missing attribute \"tags\"")
	}
	if !tags.Optional || !tags.Type.Equals(cty.Map(cty.String)) {
		t.Errorf("tags = %+v, want optional map(string)", tags)
	}

	want := cty.Object(map[string]cty.Type{
		"name":       cty.String,
		"tags":       cty.Map(cty.String),
		"replace_me": cty.String,
		"rules":      cty.List(cty.Object(map[string]cty.Type{"token_id": cty.String})),
		"id":         cty.String,
		"echo":       cty.String,
	})
	if got := thing.Block.ImpliedType(); !got.Equals(want) {
		t.Errorf("ImpliedType = %#v, want %#v", got, want)
	}
	flat := schemas.ResourceTypes["tchoritest_flat_set_thing"]
	if flat == nil {
		t.Fatal("missing tchoritest_flat_set_thing resource schema")
	}
	flatMembers := flat.Block.Attributes["members"]
	wantMembers := cty.Set(cty.Object(map[string]cty.Type{
		"label": cty.String, "token": cty.String, "normalized": cty.String,
	}))
	if flatMembers == nil || flatMembers.NestedType != nil || !flatMembers.Type.Equals(wantMembers) {
		t.Fatalf("flat members = %+v, want flat protocol-6 set(object) without nested metadata", flatMembers)
	}

	// Schemas must cache: the second call returns the identical pointer.
	again, ds2 := c.Schemas(ctx)
	if ds2.HasErrors() {
		t.Fatalf("second Schemas call: %+v", ds2)
	}
	if again != schemas {
		t.Error("Schemas did not cache: second call returned a different pointer")
	}
}

// TestSchemasTolerateUnsupportedResourceType guards the fix for issue #5:
// one resource type whose schema tchori cannot convert (tchoritest_broken_thing
// carries a nested_type attribute using a nesting mode blockFromProto does
// not recognize — see testprovider's brokenThingSchema; nested_type itself
// is supported as of issue #7) must not fail Schemas() for the whole
// provider. The failure is recorded in UnsupportedResources instead, keyed
// by type name, so every other (supported) resource type — including
// tchoritest_nested_thing, a resource type that DOES use nested_type
// successfully — and the provider's own config schema, remain usable.
func TestSchemasTolerateUnsupportedResourceType(t *testing.T) {
	bin := buildTestProvider(t)
	ctx := context.Background()

	c, err := Launch(ctx, bin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})

	schemas, ds := c.Schemas(ctx)
	if ds.HasErrors() {
		t.Fatalf("Schemas returned error diagnostics for a provider with one unsupported resource type: %+v", ds)
	}
	if schemas == nil {
		t.Fatal("nil schemas")
	}

	if thing := schemas.ResourceTypes["tchoritest_thing"]; thing == nil {
		t.Fatal("tchoritest_thing missing from ResourceTypes; the good resource type must remain usable")
	}
	if broken := schemas.ResourceTypes["tchoritest_broken_thing"]; broken != nil {
		t.Errorf("tchoritest_broken_thing present in ResourceTypes = %+v, want absent (unconvertible schema)", broken)
	}

	detail, ok := schemas.UnsupportedResources["tchoritest_broken_thing"]
	if !ok {
		t.Fatalf("tchoritest_broken_thing missing from UnsupportedResources; got %v", schemas.UnsupportedResources)
	}
	if !strings.Contains(detail, "nested_type") {
		t.Errorf("UnsupportedResources detail = %q, want it to mention nested_type", detail)
	}

	// LookupResourceType must distinguish "known but unsupported" from
	// "unknown" (never defined by the provider at all).
	if s, unsupported, known := schemas.LookupResourceType("tchoritest_broken_thing"); s != nil || unsupported == "" || !known {
		t.Errorf("LookupResourceType(tchoritest_broken_thing) = (%v, %q, %v), want (nil, <non-empty>, true)",
			s, unsupported, known)
	}
	if s, unsupported, known := schemas.LookupResourceType("tchoritest_thing"); s == nil || unsupported != "" || !known {
		t.Errorf("LookupResourceType(tchoritest_thing) = (%v, %q, %v), want (<non-nil>, \"\", true)",
			s, unsupported, known)
	}
	if s, unsupported, known := schemas.LookupResourceType("tchoritest_nonexistent"); s != nil || unsupported != "" || known {
		t.Errorf("LookupResourceType(tchoritest_nonexistent) = (%v, %q, %v), want (nil, \"\", false)",
			s, unsupported, known)
	}
}

// TestSchemasConvertsNestedType guards issue #7 end to end through the real
// protocol client (not just blockFromProto directly, see schema_test.go):
// tchoritest_nested_thing's "settings" nested_type attribute (SchemaObject,
// SchemaObjectNestingModeSingle, two optional leaf attributes — see
// testprovider's nestedThingSchema) must convert successfully and land in
// ResourceTypes, not UnsupportedResources.
func TestSchemasConvertsNestedType(t *testing.T) {
	bin := buildTestProvider(t)
	ctx := context.Background()

	c, err := Launch(ctx, bin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	})

	schemas, ds := c.Schemas(ctx)
	if ds.HasErrors() {
		t.Fatalf("Schemas: %+v", ds)
	}

	nested := schemas.ResourceTypes["tchoritest_nested_thing"]
	if nested == nil {
		t.Fatalf("tchoritest_nested_thing missing from ResourceTypes (nested_type must convert); UnsupportedResources = %v",
			schemas.UnsupportedResources)
	}
	if _, unsupported := schemas.UnsupportedResources["tchoritest_nested_thing"]; unsupported {
		t.Errorf("tchoritest_nested_thing present in UnsupportedResources = %q, want absent",
			schemas.UnsupportedResources["tchoritest_nested_thing"])
	}

	settings := nested.Block.Attributes["settings"]
	if settings == nil {
		t.Fatal("missing attribute \"settings\"")
	}
	if !settings.Optional {
		t.Errorf("settings.Optional = false, want true")
	}
	want := cty.ObjectWithOptionalAttrs(map[string]cty.Type{
		"flag":  cty.Bool,
		"label": cty.String,
	}, []string{"flag", "label"})
	if !settings.Type.Equals(want) {
		t.Errorf("settings.Type = %#v, want %#v", settings.Type, want)
	}
}

func TestLaunchCanceledContext(t *testing.T) {
	bin := buildTestProvider(t)
	pidFile := filepath.Join(t.TempDir(), "provider.pid")
	t.Setenv("TCHORITEST_STALL_STARTUP", "1")
	t.Setenv("TCHORITEST_PID_FILE", pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type launchResult struct {
		client *Client
		err    error
	}
	resultCh := make(chan launchResult, 1)
	started := time.Now()
	go func() {
		client, err := Launch(ctx, bin)
		resultCh <- launchResult{client: client, err: err}
	}()

	pid := waitForProviderPID(t, pidFile, 5*time.Second)
	cancel()

	var result launchResult
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Launch did not return within 5s of context cancellation")
	}
	if result.client != nil {
		result.client.plugin.Kill()
		t.Fatal("Launch returned a client after context cancellation")
	}
	if result.err == nil {
		t.Fatal("Launch returned nil error after context cancellation")
	}
	if !errors.Is(result.err, context.Canceled) {
		t.Errorf("Launch error = %v, want wrapped context.Canceled", result.err)
	}
	if !strings.Contains(result.err.Error(), "provider: launching ") || !strings.Contains(result.err.Error(), " canceled: context canceled") {
		t.Errorf("Launch error = %q, want cancellation-classified launch error", result.err)
	}
	if elapsed := time.Since(started); elapsed >= providerStartTimeout {
		t.Errorf("Launch returned after %v, want less than providerStartTimeout (%v)", elapsed, providerStartTimeout)
	}

	waitForProviderExit(t, pid, 5*time.Second)
}

func TestCloseStopProviderStall(t *testing.T) {
	bin := buildTestProvider(t)
	t.Setenv("TCHORITEST_STALL_STOP", "1")

	c, err := Launch(context.Background(), bin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(c.plugin.Kill)

	closeResult := make(chan error, 1)
	started := time.Now()
	go func() {
		closeResult <- c.Close()
	}()

	var closeErr error
	select {
	case closeErr = <-closeResult:
	case <-time.After(2 * providerStopGrace):
		t.Fatalf("Close did not return within %v", 2*providerStopGrace)
	}
	if closeErr == nil {
		t.Fatal("Close returned nil error when StopProvider exceeded its grace period")
	}
	if code := status.Code(closeErr); code != codes.DeadlineExceeded {
		t.Errorf("Close error = %v (code %s), want DeadlineExceeded", closeErr, code)
	}
	if elapsed := time.Since(started); elapsed >= 2*providerStopGrace {
		t.Errorf("Close returned after %v, want less than %v", elapsed, 2*providerStopGrace)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !c.plugin.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("provider subprocess still running 5s after bounded Close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProviderPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the t.TempDir PID artifact supplied by this test
		if err == nil {
			trimmed := strings.TrimSpace(string(raw))
			// An empty file is not yet a written PID: the provider's
			// pidfile.Write stages content on a sibling path and renames it
			// into place, so this should not happen in practice, but treat
			// it as "not yet written" defensively rather than failing.
			if trimmed != "" {
				pid, parseErr := strconv.Atoi(trimmed)
				if parseErr != nil {
					t.Fatalf("parsing provider PID %q: %v", raw, parseErr)
				}
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reading provider PID file: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider did not write PID file within %v", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProviderExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		alive, err := providerProcessAlive(pid)
		if err != nil {
			t.Fatalf("checking provider process %d: %v", pid, err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider process %d still exists %v after Launch returned", pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseKillsProvider(t *testing.T) {
	bin := buildTestProvider(t)

	c, err := Launch(context.Background(), bin)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// plugin.Client.Kill blocks until exit, so this should already be true;
	// poll briefly to avoid platform flakiness.
	deadline := time.Now().Add(5 * time.Second)
	for !c.plugin.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("provider subprocess still running 5s after Close")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

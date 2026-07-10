package provider

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
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
		"id":         cty.String,
		"echo":       cty.String,
	})
	if got := thing.Block.ImpliedType(); !got.Equals(want) {
		t.Errorf("ImpliedType = %#v, want %#v", got, want)
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

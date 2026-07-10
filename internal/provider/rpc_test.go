package provider

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// buildFakeProviderForRPC compiles the Task 5 fake provider into a temp dir
// and returns the binary path. Named distinctively so it cannot collide with
// build helpers defined by client_test.go or build_test.go.
func buildFakeProviderForRPC(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "terraform-provider-tchoritest")
	//nolint:gosec // G204: fixed "go build" argv; only variable part is t.TempDir(), not external input.
	cmd := exec.Command("go", "build", "-o", bin, "./internal/provider/testprovider")
	// go test runs with cwd = this package's source dir (internal/provider);
	// the build must run from the module root two levels up.
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build testprovider: %v\n%s", err, out)
	}
	return bin
}

// thingVal builds a tchoritest_thing object value with all five attributes
// present (tags always null here), matching the resource's implied type.
func thingVal(name, replaceMe, id, echo cty.Value) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"echo":       echo,
		"id":         id,
		"name":       name,
		"replace_me": replaceMe,
		"tags":       cty.NullVal(cty.Map(cty.String)),
	})
}

func TestDottedPath(t *testing.T) {
	p := &tfplugin6.AttributePath{Steps: []*tfplugin6.AttributePath_Step{
		{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "tags"}},
		{Selector: &tfplugin6.AttributePath_Step_ElementKeyString{ElementKeyString: "env"}},
		{Selector: &tfplugin6.AttributePath_Step_ElementKeyInt{ElementKeyInt: 3}},
	}}
	if got, want := dottedPath(p), "tags.env.3"; got != want {
		t.Fatalf("dottedPath = %q, want %q", got, want)
	}
	if got := dottedPath(&tfplugin6.AttributePath{}); got != "" {
		t.Fatalf("dottedPath(empty) = %q, want empty string", got)
	}
}

func TestRPCDiagnosticsMapping(t *testing.T) {
	in := []*tfplugin6.Diagnostic{
		{
			Severity: tfplugin6.Diagnostic_ERROR,
			Summary:  "boom",
			Detail:   "it exploded",
			Attribute: &tfplugin6.AttributePath{Steps: []*tfplugin6.AttributePath_Step{
				{Selector: &tfplugin6.AttributePath_Step_AttributeName{AttributeName: "name"}},
			}},
		},
		{Severity: tfplugin6.Diagnostic_WARNING, Summary: "meh"},
		{Severity: tfplugin6.Diagnostic_INVALID, Summary: "wat"},
	}
	ds := rpcDiagnostics(in)
	if len(ds) != 3 {
		t.Fatalf("len(ds) = %d, want 3: %v", len(ds), ds)
	}
	if ds[0].Severity != diag.Error || ds[0].Summary != "boom" || ds[0].Detail != "it exploded" || ds[0].Address != "name" {
		t.Fatalf("ds[0] = %+v, want error/boom/it exploded/name", ds[0])
	}
	if ds[1].Severity != diag.Warning || ds[1].Address != "" {
		t.Fatalf("ds[1] = %+v, want warning with empty address", ds[1])
	}
	if ds[2].Severity != diag.Error {
		t.Fatalf("ds[2].Severity = %q, want error (INVALID fails closed)", ds[2].Severity)
	}
}

// TestProviderRPCDialogue drives the full RPC dialogue against the Task 5
// fake provider: Configure -> Validate (ok + error) -> Plan create (unknowns)
// -> Apply (unknowns resolved) -> Read (echo) -> Plan replace (RequiresReplace).
func TestProviderRPCDialogue(t *testing.T) {
	ctx := context.Background()
	bin := buildFakeProviderForRPC(t)

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
		t.Fatalf("Schemas: %v", ds)
	}
	thingSchema := schemas.ResourceTypes["tchoritest_thing"]
	if thingSchema == nil {
		t.Fatalf("resource type tchoritest_thing missing from schemas: %v", schemas.ResourceTypes)
	}
	thingTy := thingSchema.Block.ImpliedType()

	// 1. Configure with prefix="X" (surfaces later as the applied id prefix).
	provCfg := cty.ObjectVal(map[string]cty.Value{"prefix": cty.StringVal("X")})
	if ds := c.Configure(ctx, provCfg); ds.HasErrors() {
		t.Fatalf("Configure: %v", ds)
	}

	// 2. Validate ok: no diagnostics at all.
	okCfg := thingVal(cty.StringVal("foo"), cty.NullVal(cty.String),
		cty.NullVal(cty.String), cty.NullVal(cty.String))
	if ds := c.ValidateResource(ctx, "tchoritest_thing", okCfg); len(ds) != 0 {
		t.Fatalf("ValidateResource(ok): unexpected diagnostics %v", ds)
	}

	// 3. Validate name="invalid": error diagnostic with the fake's summary.
	badCfg := thingVal(cty.StringVal("invalid"), cty.NullVal(cty.String),
		cty.NullVal(cty.String), cty.NullVal(cty.String))
	ds = c.ValidateResource(ctx, "tchoritest_thing", badCfg)
	if !ds.HasErrors() {
		t.Fatalf("ValidateResource(invalid): want error diagnostics, got %v", ds)
	}
	if ds[0].Summary != "invalid name" {
		t.Fatalf("ValidateResource(invalid): summary = %q, want %q", ds[0].Summary, "invalid name")
	}

	// 4. Plan create: prior null => id and echo unknown in PlannedChange.State;
	// private bytes pass through PriorPrivate -> PlannedPrivate.
	prior := cty.NullVal(thingTy)
	pc, ds := c.PlanResource(ctx, "tchoritest_thing", prior, okCfg, okCfg, []byte("p1"))
	if ds.HasErrors() {
		t.Fatalf("PlanResource(create): %v", ds)
	}
	if pc.State.GetAttr("id").IsKnown() {
		t.Fatalf("PlanResource(create): id known %#v, want unknown", pc.State.GetAttr("id"))
	}
	if pc.State.GetAttr("echo").IsKnown() {
		t.Fatalf("PlanResource(create): echo known %#v, want unknown", pc.State.GetAttr("echo"))
	}
	if got := pc.State.GetAttr("name"); !got.RawEquals(cty.StringVal("foo")) {
		t.Fatalf("PlanResource(create): name = %#v, want %q", got, "foo")
	}
	if len(pc.RequiresReplace) != 0 {
		t.Fatalf("PlanResource(create): RequiresReplace = %v, want empty", pc.RequiresReplace)
	}
	if string(pc.Private) != "p1" {
		t.Fatalf("PlanResource(create): private = %q, want %q passed through", pc.Private, "p1")
	}

	// 5. Apply create: unknowns resolve; id = "<prefix>id-<name>" = "Xid-foo".
	applied, newPriv, ds := c.ApplyResource(ctx, "tchoritest_thing", prior, pc.State, okCfg, pc.Private)
	if ds.HasErrors() {
		t.Fatalf("ApplyResource: %v", ds)
	}
	if got := applied.GetAttr("id"); !got.RawEquals(cty.StringVal("Xid-foo")) {
		t.Fatalf("ApplyResource: id = %#v, want %q", got, "Xid-foo")
	}
	if got := applied.GetAttr("echo"); !got.RawEquals(cty.StringVal("foo")) {
		t.Fatalf("ApplyResource: echo = %#v, want %q", got, "foo")
	}
	if string(newPriv) != "p1" {
		t.Fatalf("ApplyResource: private = %q, want %q passed through", newPriv, "p1")
	}

	// 6. Read: fake provider echoes state and private unchanged.
	readBack, readPriv, ds := c.ReadResource(ctx, "tchoritest_thing", applied, newPriv)
	if ds.HasErrors() {
		t.Fatalf("ReadResource: %v", ds)
	}
	if !readBack.RawEquals(applied) {
		t.Fatalf("ReadResource: state = %#v, want echo of %#v", readBack, applied)
	}
	if string(readPriv) != "p1" {
		t.Fatalf("ReadResource: private = %q, want %q", readPriv, "p1")
	}

	// 7. Plan with changed replace_me against non-null prior: RequiresReplace
	// rendered dotted as ["replace_me"]; id keeps its prior (known) value.
	proposed2 := thingVal(cty.StringVal("foo"), cty.StringVal("changed"),
		cty.NullVal(cty.String), cty.NullVal(cty.String))
	pc2, ds := c.PlanResource(ctx, "tchoritest_thing", applied, proposed2, proposed2, newPriv)
	if ds.HasErrors() {
		t.Fatalf("PlanResource(replace): %v", ds)
	}
	if want := []string{"replace_me"}; !reflect.DeepEqual(pc2.RequiresReplace, want) {
		t.Fatalf("PlanResource(replace): RequiresReplace = %#v, want %#v", pc2.RequiresReplace, want)
	}
	if got := pc2.State.GetAttr("id"); !got.RawEquals(cty.StringVal("Xid-foo")) {
		t.Fatalf("PlanResource(replace): id = %#v, want prior %q kept", got, "Xid-foo")
	}
}

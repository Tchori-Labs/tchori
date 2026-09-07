package provider

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// fakeImportProviderClient embeds the (nil) generated interface so it only
// needs to implement ImportResourceState for these unit tests; any other
// method call would nil-panic, which is fine since ImportResource never
// calls them.
type fakeImportProviderClient struct {
	tfplugin6.ProviderClient
	resp *tfplugin6.ImportResourceState_Response
	err  error
}

func (f *fakeImportProviderClient) ImportResourceState(_ context.Context, _ *tfplugin6.ImportResourceState_Request, _ ...grpc.CallOption) (*tfplugin6.ImportResourceState_Response, error) {
	return f.resp, f.err
}

// fakePlanProviderClient embeds the (nil) generated interface so it only
// needs to implement PlanResourceChange for these unit tests.
type fakePlanProviderClient struct {
	tfplugin6.ProviderClient
	resp *tfplugin6.PlanResourceChange_Response
	err  error
}

func (f *fakePlanProviderClient) PlanResourceChange(_ context.Context, _ *tfplugin6.PlanResourceChange_Request, _ ...grpc.CallOption) (*tfplugin6.PlanResourceChange_Response, error) {
	return f.resp, f.err
}

// fakeApplyProviderClient embeds the (nil) generated interface so it only
// needs to implement ApplyResourceChange for these unit tests.
type fakeApplyProviderClient struct {
	tfplugin6.ProviderClient
	resp *tfplugin6.ApplyResourceChange_Response
	err  error
}

func (f *fakeApplyProviderClient) ApplyResourceChange(_ context.Context, _ *tfplugin6.ApplyResourceChange_Request, _ ...grpc.CallOption) (*tfplugin6.ApplyResourceChange_Response, error) {
	return f.resp, f.err
}

// fakeReadProviderClient embeds the (nil) generated interface so it only
// needs to implement ReadResource for these unit tests.
type fakeReadProviderClient struct {
	tfplugin6.ProviderClient
	resp *tfplugin6.ReadResource_Response
	err  error
}

func (f *fakeReadProviderClient) ReadResource(_ context.Context, _ *tfplugin6.ReadResource_Request, _ ...grpc.CallOption) (*tfplugin6.ReadResource_Response, error) {
	return f.resp, f.err
}

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

// thingVal builds a tchoritest_thing object value with all six attributes
// present (tags and rules always null here), matching the resource's
// implied type.
func thingVal(name, replaceMe, id, echo cty.Value) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"echo":       echo,
		"id":         id,
		"name":       name,
		"replace_me": replaceMe,
		"rules":      cty.NullVal(cty.List(cty.Object(map[string]cty.Type{"token_id": cty.String}))),
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

// TestImportResource covers the ImportResource wrapper's success and error
// paths against a fake ProviderClient (no real provider process needed).
func TestImportResource(t *testing.T) {
	ctx := context.Background()
	ty := cty.Object(map[string]cty.Type{"id": cty.String, "name": cty.String})
	stateVal := cty.ObjectVal(map[string]cty.Value{
		"id":   cty.StringVal("Xid-foo"),
		"name": cty.StringVal("foo"),
	})
	dv, err := EncodeDynamic(stateVal, ty)
	if err != nil {
		t.Fatalf("EncodeDynamic: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		c := &Client{grpc: &fakeImportProviderClient{resp: &tfplugin6.ImportResourceState_Response{
			ImportedResources: []*tfplugin6.ImportResourceState_ImportedResource{
				{TypeName: "tchoritest_thing", State: dv, Private: []byte("priv")},
			},
		}}}
		got, priv, ds := c.ImportResource(ctx, "tchoritest_thing", "Xid-foo", ty)
		if ds.HasErrors() {
			t.Fatalf("ImportResource: unexpected diagnostics %v", ds)
		}
		if !got.RawEquals(stateVal) {
			t.Fatalf("ImportResource: state = %#v, want %#v", got, stateVal)
		}
		if string(priv) != "priv" {
			t.Fatalf("ImportResource: private = %q, want %q", priv, "priv")
		}
	})

	t.Run("rpc error", func(t *testing.T) {
		c := &Client{grpc: &fakeImportProviderClient{err: errors.New("dial failed")}}
		_, _, ds := c.ImportResource(ctx, "tchoritest_thing", "Xid-foo", ty)
		if !ds.HasErrors() {
			t.Fatalf("ImportResource(rpc error): want error diagnostics, got %v", ds)
		}
	})

	t.Run("provider diagnostic", func(t *testing.T) {
		c := &Client{grpc: &fakeImportProviderClient{resp: &tfplugin6.ImportResourceState_Response{
			Diagnostics: []*tfplugin6.Diagnostic{
				{Severity: tfplugin6.Diagnostic_ERROR, Summary: "resource does not exist"},
			},
		}}}
		_, _, ds := c.ImportResource(ctx, "tchoritest_thing", "bogus", ty)
		if !ds.HasErrors() {
			t.Fatalf("ImportResource(provider diagnostic): want error diagnostics, got %v", ds)
		}
		if ds[0].Summary != "resource does not exist" {
			t.Fatalf("ImportResource(provider diagnostic): summary = %q, want %q", ds[0].Summary, "resource does not exist")
		}
	})

	t.Run("zero results", func(t *testing.T) {
		c := &Client{grpc: &fakeImportProviderClient{resp: &tfplugin6.ImportResourceState_Response{}}}
		_, _, ds := c.ImportResource(ctx, "tchoritest_thing", "Xid-foo", ty)
		if !ds.HasErrors() {
			t.Fatalf("ImportResource(zero results): want error diagnostics, got %v", ds)
		}
		if ds[0].Summary != "provider imported nothing" {
			t.Fatalf("ImportResource(zero results): summary = %q, want %q", ds[0].Summary, "provider imported nothing")
		}
	})

	t.Run("multi results", func(t *testing.T) {
		c := &Client{grpc: &fakeImportProviderClient{resp: &tfplugin6.ImportResourceState_Response{
			ImportedResources: []*tfplugin6.ImportResourceState_ImportedResource{
				{TypeName: "tchoritest_thing", State: dv},
				{TypeName: "tchoritest_other", State: dv},
			},
		}}}
		_, _, ds := c.ImportResource(ctx, "tchoritest_thing", "Xid-foo", ty)
		if !ds.HasErrors() {
			t.Fatalf("ImportResource(multi results): want error diagnostics, got %v", ds)
		}
		if ds[0].Summary != "multi-resource import not supported" {
			t.Fatalf("ImportResource(multi results): summary = %q, want %q", ds[0].Summary, "multi-resource import not supported")
		}
	})

	t.Run("deferred", func(t *testing.T) {
		// BUG: ImportResource ignores a non-nil Deferred marker and decodes
		// the (possibly stale or absent) imported state anyway. The engine
		// has no deferred-operation support, so a deferred response must be
		// rejected explicitly instead of silently accepted.
		c := &Client{grpc: &fakeImportProviderClient{resp: &tfplugin6.ImportResourceState_Response{
			ImportedResources: []*tfplugin6.ImportResourceState_ImportedResource{
				{TypeName: "tchoritest_thing", State: dv, Private: []byte("priv")},
			},
			Deferred: &tfplugin6.Deferred{Reason: tfplugin6.Deferred_ABSENT_PREREQ},
		}}}
		_, _, ds := c.ImportResource(ctx, "tchoritest_thing", "Xid-foo", ty)
		if !ds.HasErrors() {
			t.Fatalf("ImportResource(deferred): want error diagnostics rejecting the deferred response, got %v", ds)
		}
	})

	t.Run("type mismatch", func(t *testing.T) {
		// BUG: a single ImportedResource whose TypeName does not match the
		// requested typeName is accepted as long as its state happens to
		// decode at the caller's schema (schemas can coincidentally match
		// across resource types). ImportResource must validate TypeName.
		c := &Client{grpc: &fakeImportProviderClient{resp: &tfplugin6.ImportResourceState_Response{
			ImportedResources: []*tfplugin6.ImportResourceState_ImportedResource{
				{TypeName: "tchoritest_other", State: dv, Private: []byte("priv")},
			},
		}}}
		_, _, ds := c.ImportResource(ctx, "tchoritest_thing", "Xid-foo", ty)
		if !ds.HasErrors() {
			t.Fatalf("ImportResource(type mismatch): want error diagnostics rejecting a wrong-typed import, got %v", ds)
		}
	})
}

// TestPlanResourceRejectsDeferred covers BUG (1): PlanResource must reject a
// non-nil Deferred response explicitly rather than silently decoding a
// (likely absent) PlannedState as a null value. The engine has no
// deferred-operation support, so a deferred PlanResourceChange response is a
// protocol condition the caller cannot act on correctly and must surface as
// an error instead of feeding a bogus "create" plan downstream.
func TestPlanResourceRejectsDeferred(t *testing.T) {
	ctx := context.Background()
	ty := cty.Object(map[string]cty.Type{"id": cty.String})
	nullVal := cty.NullVal(ty)

	c := &Client{grpc: &fakePlanProviderClient{resp: &tfplugin6.PlanResourceChange_Response{
		// PlannedState deliberately left nil, matching what a deferring
		// provider commonly omits; the response is what a real deferred
		// PlanResourceChange looks like on the wire.
		Deferred: &tfplugin6.Deferred{Reason: tfplugin6.Deferred_RESOURCE_CONFIG_UNKNOWN},
	}}}
	pc, ds := c.PlanResource(ctx, "tchoritest_thing", nullVal, nullVal, nullVal, nil)
	if !ds.HasErrors() {
		t.Fatalf("PlanResource(deferred): want error diagnostics rejecting the deferred response, got pc=%#v ds=%v", pc, ds)
	}
}

// TestReadResourceRejectsDeferred covers BUG (1)'s ReadResource half: a
// deferred read with no NewState currently decodes to a null value, which
// callers read as "resource was deleted" and the planner then proposes a
// create instead of a no-op refresh. ReadResource must reject the deferred
// response before any null decoding happens.
func TestReadResourceRejectsDeferred(t *testing.T) {
	ctx := context.Background()
	current := cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("Xid-foo")})

	c := &Client{grpc: &fakeReadProviderClient{resp: &tfplugin6.ReadResource_Response{
		// NewState deliberately left nil: today this decodes to a null
		// value of current's type, which reads as "deleted" to the caller.
		Deferred: &tfplugin6.Deferred{Reason: tfplugin6.Deferred_PROVIDER_CONFIG_UNKNOWN},
	}}}
	got, _, ds := c.ReadResource(ctx, "tchoritest_thing", current, nil)
	if !ds.HasErrors() {
		t.Fatalf("ReadResource(deferred): want error diagnostics rejecting the deferred response, got state=%#v ds=%v", got, ds)
	}
}

// TestApplyResourcePreservesPartialStateOnError covers BUG (2): today
// ApplyResource discards any returned NewState/Private whenever the
// provider's diagnostics contain an error, returning cty.NilVal even when
// the provider deliberately returned a decodeable partial state (e.g. one
// nested object created before a later one failed, or — as tchori's own
// fake providers do for "explode"/"api_400" — the prior state kept
// specifically to disambiguate from deletion). The caller needs that
// recoverable state to persist it; only an undecodeable NewState should
// still yield cty.NilVal.
func TestApplyResourcePreservesPartialStateOnError(t *testing.T) {
	ctx := context.Background()
	ty := cty.Object(map[string]cty.Type{"id": cty.String, "name": cty.String})
	partial := cty.ObjectVal(map[string]cty.Value{
		"id":   cty.StringVal("Xid-foo"),
		"name": cty.StringVal("foo"),
	})
	dv, err := EncodeDynamic(partial, ty)
	if err != nil {
		t.Fatalf("EncodeDynamic: %v", err)
	}

	c := &Client{grpc: &fakeApplyProviderClient{resp: &tfplugin6.ApplyResourceChange_Response{
		NewState: dv,
		Private:  []byte("partial-priv"),
		Diagnostics: []*tfplugin6.Diagnostic{
			{Severity: tfplugin6.Diagnostic_ERROR, Summary: "apply exploded"},
		},
	}}}
	got, priv, ds := c.ApplyResource(ctx, "tchoritest_thing", cty.NullVal(ty), partial, partial, nil)
	if !ds.HasErrors() {
		t.Fatalf("ApplyResource(partial failure): want error diagnostics, got none")
	}
	if got == cty.NilVal || !got.RawEquals(partial) {
		t.Fatalf("ApplyResource(partial failure): state = %#v, want recoverable partial state %#v preserved despite the error", got, partial)
	}
	if string(priv) != "partial-priv" {
		t.Fatalf("ApplyResource(partial failure): private = %q, want %q preserved alongside the error", priv, "partial-priv")
	}
}

// TestApplyResourceUndecodeableStateStillNilOnError proves the fix does not
// paper over a genuinely broken NewState: if the bytes the provider returned
// alongside its error diagnostics fail to decode, ApplyResource must still
// surface a decode error and return cty.NilVal rather than fabricate state.
func TestApplyResourceUndecodeableStateStillNilOnError(t *testing.T) {
	ctx := context.Background()
	ty := cty.Object(map[string]cty.Type{"id": cty.String, "name": cty.String})
	planned := cty.ObjectVal(map[string]cty.Value{
		"id":   cty.StringVal("Xid-foo"),
		"name": cty.StringVal("foo"),
	})

	c := &Client{grpc: &fakeApplyProviderClient{resp: &tfplugin6.ApplyResourceChange_Response{
		NewState: &tfplugin6.DynamicValue{Msgpack: []byte("not valid msgpack")},
		Diagnostics: []*tfplugin6.Diagnostic{
			{Severity: tfplugin6.Diagnostic_ERROR, Summary: "apply exploded"},
		},
	}}}
	got, _, ds := c.ApplyResource(ctx, "tchoritest_thing", cty.NullVal(ty), planned, planned, nil)
	if !ds.HasErrors() {
		t.Fatalf("ApplyResource(undecodeable partial state): want error diagnostics, got none")
	}
	if got != cty.NilVal {
		t.Fatalf("ApplyResource(undecodeable partial state): state = %#v, want cty.NilVal", got)
	}
}

// TestProviderRPCImportResourceState drives ImportResource against the real
// fake provider process: apply a thing, import its own id back, and assert
// the imported state matches the applied state (import -> plan is a no-op).
// Also covers the provider's "id-" marker rejection (does-not-exist path).
func TestProviderRPCImportResourceState(t *testing.T) {
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
	thingTy := schemas.ResourceTypes["tchoritest_thing"].Block.ImpliedType()

	provCfg := cty.ObjectVal(map[string]cty.Value{"prefix": cty.StringVal("X")})
	if ds := c.Configure(ctx, provCfg); ds.HasErrors() {
		t.Fatalf("Configure: %v", ds)
	}

	okCfg := thingVal(cty.StringVal("foo"), cty.NullVal(cty.String),
		cty.NullVal(cty.String), cty.NullVal(cty.String))
	prior := cty.NullVal(thingTy)
	pc, ds := c.PlanResource(ctx, "tchoritest_thing", prior, okCfg, okCfg, nil)
	if ds.HasErrors() {
		t.Fatalf("PlanResource: %v", ds)
	}
	applied, _, ds := c.ApplyResource(ctx, "tchoritest_thing", prior, pc.State, okCfg, pc.Private)
	if ds.HasErrors() {
		t.Fatalf("ApplyResource: %v", ds)
	}
	id := applied.GetAttr("id")

	imported, _, ds := c.ImportResource(ctx, "tchoritest_thing", id.AsString(), thingTy)
	if ds.HasErrors() {
		t.Fatalf("ImportResource: %v", ds)
	}
	if !imported.RawEquals(applied) {
		t.Fatalf("ImportResource: state = %#v, want %#v (applied)", imported, applied)
	}

	_, _, ds = c.ImportResource(ctx, "tchoritest_thing", "no-marker-here", thingTy)
	if !ds.HasErrors() {
		t.Fatalf("ImportResource(no id- marker): want error diagnostics, got none")
	}
}

// buildFakeProvider5ForRPC compiles the protocol-5 fake provider
// (testprovider5) into a temp dir and returns the binary path.
func buildFakeProvider5ForRPC(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "terraform-provider-tchoritest5")
	//nolint:gosec // G204: fixed "go build" argv; only variable part is t.TempDir(), not external input.
	cmd := exec.Command("go", "build", "-o", bin, "./internal/provider/testprovider5")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build testprovider5: %v\n%s", err, out)
	}
	return bin
}

// TestProviderRPCDialogueProtocol5 drives the full RPC dialogue through the
// tfplugin5 adapter against testprovider5: Configure -> Validate (ok +
// error) -> Plan create -> Apply (unknowns resolved) -> Read -> Import,
// plus a diagnostics-carrying apply failure ("explode") proving
// Diagnostic/AttributePath conversion on a real wire exchange.
func TestProviderRPCDialogueProtocol5(t *testing.T) {
	ctx := context.Background()
	bin := buildFakeProvider5ForRPC(t)

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
	thingSchema := schemas.ResourceTypes["tchoritest5_thing"]
	if thingSchema == nil {
		t.Fatalf("resource type tchoritest5_thing missing from schemas: %v", schemas.ResourceTypes)
	}
	thingTy := thingSchema.Block.ImpliedType()

	// 1. Configure with prefix="X".
	provCfg := cty.ObjectVal(map[string]cty.Value{"prefix": cty.StringVal("X")})
	if ds := c.Configure(ctx, provCfg); ds.HasErrors() {
		t.Fatalf("Configure: %v", ds)
	}

	// 2. Validate ok: no diagnostics.
	okCfg, cds := Compose(map[string]any{"name": "foo"}, thingTy, EnvResolve, nil)
	if cds.HasErrors() {
		t.Fatalf("Compose(ok): %v", cds)
	}
	if ds := c.ValidateResource(ctx, "tchoritest5_thing", okCfg); len(ds) != 0 {
		t.Fatalf("ValidateResource(ok): unexpected diagnostics %v", ds)
	}

	// 3. Validate name="invalid": error diagnostic converted through the
	// adapter's Diagnostic/AttributePath translation.
	badCfg, cds := Compose(map[string]any{"name": "invalid"}, thingTy, EnvResolve, nil)
	if cds.HasErrors() {
		t.Fatalf("Compose(invalid): %v", cds)
	}
	ds = c.ValidateResource(ctx, "tchoritest5_thing", badCfg)
	if !ds.HasErrors() {
		t.Fatalf("ValidateResource(invalid): want error diagnostics, got %v", ds)
	}
	if ds[0].Summary != "invalid name" {
		t.Fatalf("ValidateResource(invalid): summary = %q, want %q", ds[0].Summary, "invalid name")
	}

	// 4. Plan create: id and echo unknown.
	prior := cty.NullVal(thingTy)
	pc, ds := c.PlanResource(ctx, "tchoritest5_thing", prior, okCfg, okCfg, []byte("p1"))
	if ds.HasErrors() {
		t.Fatalf("PlanResource(create): %v", ds)
	}
	if pc.State.GetAttr("id").IsKnown() {
		t.Fatalf("PlanResource(create): id known %#v, want unknown", pc.State.GetAttr("id"))
	}
	if string(pc.Private) != "p1" {
		t.Fatalf("PlanResource(create): private = %q, want %q passed through", pc.Private, "p1")
	}

	// 5. Apply create: id = "Xid-foo".
	applied, newPriv, ds := c.ApplyResource(ctx, "tchoritest5_thing", prior, pc.State, okCfg, pc.Private)
	if ds.HasErrors() {
		t.Fatalf("ApplyResource: %v", ds)
	}
	if got := applied.GetAttr("id"); !got.RawEquals(cty.StringVal("Xid-foo")) {
		t.Fatalf("ApplyResource: id = %#v, want %q", got, "Xid-foo")
	}
	if string(newPriv) != "p1" {
		t.Fatalf("ApplyResource: private = %q, want %q passed through", newPriv, "p1")
	}

	// 6. Read: fake provider echoes state and private unchanged.
	readBack, readPriv, ds := c.ReadResource(ctx, "tchoritest5_thing", applied, newPriv)
	if ds.HasErrors() {
		t.Fatalf("ReadResource: %v", ds)
	}
	if !readBack.RawEquals(applied) {
		t.Fatalf("ReadResource: state = %#v, want echo of %#v", readBack, applied)
	}
	if string(readPriv) != "p1" {
		t.Fatalf("ReadResource: private = %q, want %q", readPriv, "p1")
	}

	// 7. Import: importing the applied id back yields the same state
	// (import -> plan is a no-op).
	imported, _, ds := c.ImportResource(ctx, "tchoritest5_thing", applied.GetAttr("id").AsString(), thingTy)
	if ds.HasErrors() {
		t.Fatalf("ImportResource: %v", ds)
	}
	if !imported.RawEquals(applied) {
		t.Fatalf("ImportResource: state = %#v, want %#v (applied)", imported, applied)
	}

	// 8. Import "missing": not-found diagnostic, proving the adapter
	// converts a real provider-side error diagnostic correctly.
	_, _, ds = c.ImportResource(ctx, "tchoritest5_thing", "missing", thingTy)
	if !ds.HasErrors() {
		t.Fatalf("ImportResource(missing): want error diagnostics, got none")
	}

	// 9. Apply "explode": diagnostics-carrying apply failure, proving
	// Diagnostic conversion on a real wire exchange that actually fails.
	explodeCfg, cds := Compose(map[string]any{"name": "explode"}, thingTy, EnvResolve, nil)
	if cds.HasErrors() {
		t.Fatalf("Compose(explode): %v", cds)
	}
	pc2, ds := c.PlanResource(ctx, "tchoritest5_thing", prior, explodeCfg, explodeCfg, nil)
	if ds.HasErrors() {
		t.Fatalf("PlanResource(explode): %v", ds)
	}
	_, _, ds = c.ApplyResource(ctx, "tchoritest5_thing", prior, pc2.State, explodeCfg, pc2.Private)
	if !ds.HasErrors() {
		t.Fatalf("ApplyResource(explode): want error diagnostics, got none")
	}
	if ds[0].Summary != "apply exploded" {
		t.Fatalf("ApplyResource(explode): summary = %q, want %q", ds[0].Summary, "apply exploded")
	}
}

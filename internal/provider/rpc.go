// rpc.go implements the engine-side provider RPCs (Configure /
// ValidateResource / PlanResource / ApplyResource / ReadResource) directly
// over the generated tfplugin6 gRPC stubs.
//
// Encoding invariant: every cty.Value passed to these methods is already at
// the relevant schema implied type (provider config at the provider block's
// ImpliedType, resource values at the resource type's ImpliedType — composed
// by Compose or decoded from state; create/delete nulls are
// cty.NullVal(impliedType)). Therefore v.Type() IS that implied type and
// EncodeDynamic(v, v.Type()) encodes at the schema type as the protocol
// requires. Responses decode at the matching request types.
package provider

import (
	"context"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// PlannedChange is the engine-side result of a PlanResourceChange RPC.
type PlannedChange struct {
	State           cty.Value // planned new state (may contain unknowns)
	RequiresReplace []string  // attribute paths as dotted strings
	Private         []byte
}

// rpcTerraformVersion is presented to providers in ConfigureProvider. Real
// providers parse this as a semver and may gate features or minimum-version
// checks on it; the engine's own version string ("0.1.0-dev") would trip
// those checks, so we present the OpenTofu release whose protocol definition
// our stubs were generated from (pinned in research-plugin-protocol.md).
const rpcTerraformVersion = "1.12.3"

// Configure sends the composed provider configuration to the provider.
func (c *Client) Configure(ctx context.Context, config cty.Value) diag.Diagnostics {
	dv, ds := encodeRPCValue(config, "provider configuration")
	if ds.HasErrors() {
		return ds
	}
	resp, err := c.grpc.ConfigureProvider(ctx, &tfplugin6.ConfigureProvider_Request{
		TerraformVersion: rpcTerraformVersion,
		Config:           dv,
	})
	if err != nil {
		return diag.Diagnostics{diag.Errorf("", "ConfigureProvider RPC failed", err.Error())}
	}
	return rpcDiagnostics(resp.Diagnostics)
}

// ValidateResource asks the provider to validate a resource configuration.
func (c *Client) ValidateResource(ctx context.Context, typeName string, config cty.Value) diag.Diagnostics {
	dv, ds := encodeRPCValue(config, "resource configuration")
	if ds.HasErrors() {
		return ds
	}
	resp, err := c.grpc.ValidateResourceConfig(ctx, &tfplugin6.ValidateResourceConfig_Request{
		TypeName: typeName,
		Config:   dv,
	})
	if err != nil {
		return diag.Diagnostics{diag.Errorf("", "ValidateResourceConfig RPC failed", err.Error())}
	}
	return rpcDiagnostics(resp.Diagnostics)
}

// PlanResource runs PlanResourceChange. prior, proposed and config are all at
// the resource type's ImpliedType; prior is null of that type on create,
// proposed is null of that type on delete. Private bytes pass through.
func (c *Client) PlanResource(ctx context.Context, typeName string, prior, proposed, config cty.Value, priorPrivate []byte) (*PlannedChange, diag.Diagnostics) {
	req := &tfplugin6.PlanResourceChange_Request{
		TypeName:     typeName,
		PriorPrivate: priorPrivate,
	}
	var ds diag.Diagnostics
	if req.PriorState, ds = encodeRPCValue(prior, "prior state"); ds.HasErrors() {
		return nil, ds
	}
	if req.ProposedNewState, ds = encodeRPCValue(proposed, "proposed state"); ds.HasErrors() {
		return nil, ds
	}
	if req.Config, ds = encodeRPCValue(config, "resource configuration"); ds.HasErrors() {
		return nil, ds
	}
	resp, err := c.grpc.PlanResourceChange(ctx, req)
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf("", "PlanResourceChange RPC failed", err.Error())}
	}
	ds = rpcDiagnostics(resp.Diagnostics)
	if ds.HasErrors() {
		return nil, ds
	}
	planned, moreDs := decodeRPCState(resp.PlannedState, proposed.Type(), "planned state")
	ds = append(ds, moreDs...)
	if ds.HasErrors() {
		return nil, ds
	}
	var requiresReplace []string
	for _, p := range resp.RequiresReplace {
		requiresReplace = append(requiresReplace, dottedPath(p))
	}
	return &PlannedChange{
		State:           planned,
		RequiresReplace: requiresReplace,
		Private:         resp.PlannedPrivate,
	}, ds
}

// ApplyResource runs ApplyResourceChange and returns the new state value and
// new private bytes. planned may contain unknowns (the msgpack unknown
// extension); the provider resolves them.
func (c *Client) ApplyResource(ctx context.Context, typeName string, prior, planned, config cty.Value, private []byte) (cty.Value, []byte, diag.Diagnostics) {
	req := &tfplugin6.ApplyResourceChange_Request{
		TypeName:       typeName,
		PlannedPrivate: private,
	}
	var ds diag.Diagnostics
	if req.PriorState, ds = encodeRPCValue(prior, "prior state"); ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	if req.PlannedState, ds = encodeRPCValue(planned, "planned state"); ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	if req.Config, ds = encodeRPCValue(config, "resource configuration"); ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	resp, err := c.grpc.ApplyResourceChange(ctx, req)
	if err != nil {
		return cty.NilVal, nil, diag.Diagnostics{diag.Errorf("", "ApplyResourceChange RPC failed", err.Error())}
	}
	ds = rpcDiagnostics(resp.Diagnostics)
	if ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	newState, moreDs := decodeRPCState(resp.NewState, planned.Type(), "new state")
	ds = append(ds, moreDs...)
	if ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	return newState, resp.Private, ds
}

// ReadResource refreshes a resource. current is the state-decoded value at
// the resource type's ImpliedType.
func (c *Client) ReadResource(ctx context.Context, typeName string, current cty.Value, private []byte) (cty.Value, []byte, diag.Diagnostics) {
	dv, ds := encodeRPCValue(current, "current state")
	if ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	resp, err := c.grpc.ReadResource(ctx, &tfplugin6.ReadResource_Request{
		TypeName:     typeName,
		CurrentState: dv,
		Private:      private,
	})
	if err != nil {
		return cty.NilVal, nil, diag.Diagnostics{diag.Errorf("", "ReadResource RPC failed", err.Error())}
	}
	ds = rpcDiagnostics(resp.Diagnostics)
	if ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	newState, moreDs := decodeRPCState(resp.NewState, current.Type(), "refreshed state")
	ds = append(ds, moreDs...)
	if ds.HasErrors() {
		return cty.NilVal, nil, ds
	}
	return newState, resp.Private, ds
}

// --- helpers -----------------------------------------------------------------

// encodeRPCValue wraps EncodeDynamic (typeconv.go) with a diagnostic error.
// Values are encoded at their own type: by the package invariant, every value
// reaching an RPC is already at the relevant schema ImpliedType.
func encodeRPCValue(v cty.Value, what string) (*tfplugin6.DynamicValue, diag.Diagnostics) {
	dv, err := EncodeDynamic(v, v.Type())
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf("", "encoding "+what, err.Error())}
	}
	return dv, nil
}

// decodeRPCState decodes a returned DynamicValue at ty. A nil DynamicValue
// (a provider omitting the field) decodes to a null value of ty.
func decodeRPCState(dv *tfplugin6.DynamicValue, ty cty.Type, what string) (cty.Value, diag.Diagnostics) {
	if dv == nil {
		return cty.NullVal(ty), nil
	}
	v, err := DecodeDynamic(dv, ty)
	if err != nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf("", "decoding "+what, err.Error())}
	}
	return v, nil
}

// rpcDiagnostics maps wire diagnostics to engine diagnostics: WARNING maps to
// diag.Warning; ERROR — and INVALID or any unrecognized severity, failing
// closed — map to diag.Error. An attribute path, when present, is rendered
// dotted into Address.
func rpcDiagnostics(in []*tfplugin6.Diagnostic) diag.Diagnostics {
	var out diag.Diagnostics
	for _, d := range in {
		if d == nil {
			continue
		}
		addr := ""
		if d.Attribute != nil {
			addr = dottedPath(d.Attribute)
		}
		if d.Severity == tfplugin6.Diagnostic_WARNING {
			out = append(out, diag.Warnf(addr, d.Summary, d.Detail))
			continue
		}
		out = append(out, diag.Errorf(addr, d.Summary, d.Detail))
	}
	return out
}

// dottedPath renders a wire AttributePath with steps joined by "." —
// attribute names verbatim, string element keys verbatim, int element keys
// in decimal: "replace_me", "tags.env", "items.0.name". go-cty has no public
// path renderer (research-cty.md §6), so this stays local; the contract pins
// the dotted rendering for RequiresReplace and diagnostic addresses.
func dottedPath(p *tfplugin6.AttributePath) string {
	parts := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		switch sel := s.Selector.(type) {
		case *tfplugin6.AttributePath_Step_AttributeName:
			parts = append(parts, sel.AttributeName)
		case *tfplugin6.AttributePath_Step_ElementKeyString:
			parts = append(parts, sel.ElementKeyString)
		case *tfplugin6.AttributePath_Step_ElementKeyInt:
			parts = append(parts, strconv.FormatInt(sel.ElementKeyInt, 10))
		}
	}
	return strings.Join(parts, ".")
}

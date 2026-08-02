// SPDX-License-Identifier: MPL-2.0
//
// The message-conversion helpers in this file translate between the
// tfplugin5 (Terraform plugin protocol 5) and tfplugin6 (protocol 6) wire
// types vendored at OpenTofu tag v1.12.3 (see
// internal/provider/proto/tfplugin5/ and .../tfplugin6/). DynamicValue,
// Diagnostic, AttributePath, and Schema are structurally near-identical
// between the two protocols, so this is mechanical field-by-field
// translation — no engine semantics change here.

// Package provider launches Terraform plugin-protocol provider binaries
// (protocol 6 natively, protocol 5 via this adapter) over hashicorp/go-plugin
// and exposes their gRPC API to the rest of tchori.
package provider

import (
	"context"

	"google.golang.org/grpc"

	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin5"
	"github.com/tchori-labs/tchori/internal/provider/proto/tfplugin6"
)

// --- DynamicValue --------------------------------------------------------

// dynamicValue5to6 copies a tfplugin5 DynamicValue's opaque msgpack/json
// bytes into a tfplugin6 DynamicValue verbatim. A nil input stays nil (a
// provider that omits the field, e.g. a null state).
func dynamicValue5to6(dv *tfplugin5.DynamicValue) *tfplugin6.DynamicValue {
	if dv == nil {
		return nil
	}
	return &tfplugin6.DynamicValue{
		Msgpack: dv.Msgpack,
		Json:    dv.Json,
	}
}

// dynamicValue6to5 is the inverse of dynamicValue5to6, for requests the
// engine sends toward a protocol-5 provider.
func dynamicValue6to5(dv *tfplugin6.DynamicValue) *tfplugin5.DynamicValue {
	if dv == nil {
		return nil
	}
	return &tfplugin5.DynamicValue{
		Msgpack: dv.Msgpack,
		Json:    dv.Json,
	}
}

// --- Diagnostic ------------------------------------------------------------

// diagnosticSeverity5to6 maps a tfplugin5 severity to tfplugin6. The two
// enums share the same underlying values (INVALID=0, ERROR=1, WARNING=2),
// but the mapping is written as an explicit switch rather than relied on
// implicitly, and fails closed to ERROR for any value it does not
// recognize — mirroring rpcDiagnostics' fail-closed behavior in rpc.go.
func diagnosticSeverity5to6(s tfplugin5.Diagnostic_Severity) tfplugin6.Diagnostic_Severity {
	switch s {
	case tfplugin5.Diagnostic_WARNING:
		return tfplugin6.Diagnostic_WARNING
	case tfplugin5.Diagnostic_ERROR, tfplugin5.Diagnostic_INVALID:
		return tfplugin6.Diagnostic_ERROR
	default:
		return tfplugin6.Diagnostic_ERROR
	}
}

// diagnostic5to6 converts a single wire diagnostic.
func diagnostic5to6(d *tfplugin5.Diagnostic) *tfplugin6.Diagnostic {
	if d == nil {
		return nil
	}
	return &tfplugin6.Diagnostic{
		Severity:  diagnosticSeverity5to6(d.Severity),
		Summary:   d.Summary,
		Detail:    d.Detail,
		Attribute: attributePath5to6(d.Attribute),
	}
}

// diagnostics5to6 converts a diagnostic slice, preserving nil-vs-empty and
// order; nil entries are dropped (matching rpcDiagnostics' own nil guard).
func diagnostics5to6(in []*tfplugin5.Diagnostic) []*tfplugin6.Diagnostic {
	if in == nil {
		return nil
	}
	out := make([]*tfplugin6.Diagnostic, 0, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		out = append(out, diagnostic5to6(d))
	}
	return out
}

// --- AttributePath -----------------------------------------------------------

// attributePathStep5to6 converts one AttributePath_Step selector. An
// AttributePath_Step with no selector set (the zero value) converts to a
// step with no selector set — callers never see this in practice since
// providers only ever populate populated steps, but the conversion stays
// total either way.
func attributePathStep5to6(s *tfplugin5.AttributePath_Step) *tfplugin6.AttributePath_Step {
	if s == nil {
		return nil
	}
	out := &tfplugin6.AttributePath_Step{}
	switch sel := s.Selector.(type) {
	case *tfplugin5.AttributePath_Step_AttributeName:
		out.Selector = &tfplugin6.AttributePath_Step_AttributeName{AttributeName: sel.AttributeName}
	case *tfplugin5.AttributePath_Step_ElementKeyString:
		out.Selector = &tfplugin6.AttributePath_Step_ElementKeyString{ElementKeyString: sel.ElementKeyString}
	case *tfplugin5.AttributePath_Step_ElementKeyInt:
		out.Selector = &tfplugin6.AttributePath_Step_ElementKeyInt{ElementKeyInt: sel.ElementKeyInt}
	}
	return out
}

// attributePath5to6 converts a whole AttributePath (a list of steps). A nil
// path stays nil (dottedPath in rpc.go treats that as "no address").
func attributePath5to6(p *tfplugin5.AttributePath) *tfplugin6.AttributePath {
	if p == nil {
		return nil
	}
	steps := make([]*tfplugin6.AttributePath_Step, 0, len(p.Steps))
	for _, s := range p.Steps {
		steps = append(steps, attributePathStep5to6(s))
	}
	return &tfplugin6.AttributePath{Steps: steps}
}

// --- Schema ------------------------------------------------------------------

// schemaNestingMode5to6 maps a tfplugin5 nesting mode to its tfplugin6
// counterpart by explicit switch. An unrecognized mode passes through as the
// tfplugin6 zero value (INVALID) so blockFromProto (schema.go) produces its
// existing "unsupported nesting mode" error rather than this layer
// mis-mapping a mode it does not understand.
func schemaNestingMode5to6(m tfplugin5.Schema_NestedBlock_NestingMode) tfplugin6.Schema_NestedBlock_NestingMode {
	switch m {
	case tfplugin5.Schema_NestedBlock_SINGLE:
		return tfplugin6.Schema_NestedBlock_SINGLE
	case tfplugin5.Schema_NestedBlock_LIST:
		return tfplugin6.Schema_NestedBlock_LIST
	case tfplugin5.Schema_NestedBlock_SET:
		return tfplugin6.Schema_NestedBlock_SET
	case tfplugin5.Schema_NestedBlock_MAP:
		return tfplugin6.Schema_NestedBlock_MAP
	case tfplugin5.Schema_NestedBlock_GROUP:
		return tfplugin6.Schema_NestedBlock_GROUP
	default:
		return tfplugin6.Schema_NestedBlock_INVALID
	}
}

// schemaAttribute5to6 converts one Schema_Attribute. tfplugin5 attributes
// always carry JSON-encoded cty type bytes in Type and never have a
// NestedType (protocol 5 predates nested_type attributes) — the produced
// tfplugin6.Schema_Attribute therefore always leaves NestedType nil, which
// suits blockFromProto's existing flat-type parsing path (schema.go).
func schemaAttribute5to6(a *tfplugin5.Schema_Attribute) *tfplugin6.Schema_Attribute {
	if a == nil {
		return nil
	}
	return &tfplugin6.Schema_Attribute{
		Name:               a.Name,
		Type:               a.Type,
		Description:        a.Description,
		Required:           a.Required,
		Optional:           a.Optional,
		Computed:           a.Computed,
		Sensitive:          a.Sensitive,
		DescriptionKind:    tfplugin6.StringKind(a.DescriptionKind),
		Deprecated:         a.Deprecated,
		WriteOnly:          a.WriteOnly,
		DeprecationMessage: a.DeprecationMessage,
	}
}

// schemaNestedBlock5to6 converts one Schema_NestedBlock, recursing into its
// inner block.
func schemaNestedBlock5to6(nb *tfplugin5.Schema_NestedBlock) *tfplugin6.Schema_NestedBlock {
	if nb == nil {
		return nil
	}
	return &tfplugin6.Schema_NestedBlock{
		TypeName: nb.TypeName,
		Block:    schemaBlock5to6(nb.Block),
		Nesting:  schemaNestingMode5to6(nb.Nesting),
		MinItems: nb.MinItems,
		MaxItems: nb.MaxItems,
	}
}

// schemaBlock5to6 converts one Schema_Block, recursing into attributes and
// nested block types.
func schemaBlock5to6(b *tfplugin5.Schema_Block) *tfplugin6.Schema_Block {
	if b == nil {
		return nil
	}
	attrs := make([]*tfplugin6.Schema_Attribute, 0, len(b.Attributes))
	for _, a := range b.Attributes {
		attrs = append(attrs, schemaAttribute5to6(a))
	}
	blockTypes := make([]*tfplugin6.Schema_NestedBlock, 0, len(b.BlockTypes))
	for _, nb := range b.BlockTypes {
		blockTypes = append(blockTypes, schemaNestedBlock5to6(nb))
	}
	return &tfplugin6.Schema_Block{
		Version:            b.Version,
		Attributes:         attrs,
		BlockTypes:         blockTypes,
		Description:        b.Description,
		DescriptionKind:    tfplugin6.StringKind(b.DescriptionKind),
		Deprecated:         b.Deprecated,
		DeprecationMessage: b.DeprecationMessage,
	}
}

// schema5to6 converts a whole Schema (version + top-level block).
func schema5to6(s *tfplugin5.Schema) *tfplugin6.Schema {
	if s == nil {
		return nil
	}
	return &tfplugin6.Schema{
		Version: s.Version,
		Block:   schemaBlock5to6(s.Block),
	}
}

// --- protocol5Adapter ------------------------------------------------------

// protocol5Adapter wraps a negotiated protocol-5 tfplugin5.ProviderClient and
// satisfies providerGRPC by translating each of the eight RPCs the engine
// calls into its tfplugin5 counterpart, converting requests going out and
// responses coming back with the helpers above. Streaming and
// data-source/function/ephemeral-resource RPCs are deliberately out of scope
// (providerGRPC never declares them, so protocol5Adapter never needs to
// implement them).
type protocol5Adapter struct {
	client tfplugin5.ProviderClient
}

var _ providerGRPC = (*protocol5Adapter)(nil)

// GetProviderSchema maps to tfplugin5's GetSchema. DataSourceSchemas and
// server capabilities are dropped in the tfplugin6 response the engine sees
// — schema.go's Schemas only reads Provider, ResourceSchemas, and
// Diagnostics.
func (a *protocol5Adapter) GetProviderSchema(ctx context.Context, in *tfplugin6.GetProviderSchema_Request, opts ...grpc.CallOption) (*tfplugin6.GetProviderSchema_Response, error) {
	resp, err := a.client.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{}, opts...)
	if err != nil {
		return nil, err
	}
	resourceSchemas := make(map[string]*tfplugin6.Schema, len(resp.ResourceSchemas))
	for name, s := range resp.ResourceSchemas {
		resourceSchemas[name] = schema5to6(s)
	}
	return &tfplugin6.GetProviderSchema_Response{
		Provider:        schema5to6(resp.Provider),
		ResourceSchemas: resourceSchemas,
		Diagnostics:     diagnostics5to6(resp.Diagnostics),
	}, nil
}

// ConfigureProvider maps to tfplugin5's Configure. TerraformVersion and
// Config pass through; ClientCapabilities is never set by the engine on
// either protocol.
func (a *protocol5Adapter) ConfigureProvider(ctx context.Context, in *tfplugin6.ConfigureProvider_Request, opts ...grpc.CallOption) (*tfplugin6.ConfigureProvider_Response, error) {
	resp, err := a.client.Configure(ctx, &tfplugin5.Configure_Request{
		TerraformVersion: in.TerraformVersion,
		Config:           dynamicValue6to5(in.Config),
	}, opts...)
	if err != nil {
		return nil, err
	}
	return &tfplugin6.ConfigureProvider_Response{Diagnostics: diagnostics5to6(resp.Diagnostics)}, nil
}

// ValidateResourceConfig maps to tfplugin5's ValidateResourceTypeConfig.
func (a *protocol5Adapter) ValidateResourceConfig(ctx context.Context, in *tfplugin6.ValidateResourceConfig_Request, opts ...grpc.CallOption) (*tfplugin6.ValidateResourceConfig_Response, error) {
	resp, err := a.client.ValidateResourceTypeConfig(ctx, &tfplugin5.ValidateResourceTypeConfig_Request{
		TypeName: in.TypeName,
		Config:   dynamicValue6to5(in.Config),
	}, opts...)
	if err != nil {
		return nil, err
	}
	return &tfplugin6.ValidateResourceConfig_Response{Diagnostics: diagnostics5to6(resp.Diagnostics)}, nil
}

// PlanResourceChange maps to tfplugin5's PlanResourceChange.
// LegacyTypeSystem is passed through unconditionally — legacy-SDK providers
// (oracle/oci, the classic null/random/time/local) set it routinely and it
// must never be treated as an error.
func (a *protocol5Adapter) PlanResourceChange(ctx context.Context, in *tfplugin6.PlanResourceChange_Request, opts ...grpc.CallOption) (*tfplugin6.PlanResourceChange_Response, error) {
	resp, err := a.client.PlanResourceChange(ctx, &tfplugin5.PlanResourceChange_Request{
		TypeName:         in.TypeName,
		PriorState:       dynamicValue6to5(in.PriorState),
		ProposedNewState: dynamicValue6to5(in.ProposedNewState),
		Config:           dynamicValue6to5(in.Config),
		PriorPrivate:     in.PriorPrivate,
	}, opts...)
	if err != nil {
		return nil, err
	}
	requiresReplace := make([]*tfplugin6.AttributePath, 0, len(resp.RequiresReplace))
	for _, p := range resp.RequiresReplace {
		requiresReplace = append(requiresReplace, attributePath5to6(p))
	}
	return &tfplugin6.PlanResourceChange_Response{
		PlannedState:     dynamicValue5to6(resp.PlannedState),
		RequiresReplace:  requiresReplace,
		PlannedPrivate:   resp.PlannedPrivate,
		Diagnostics:      diagnostics5to6(resp.Diagnostics),
		LegacyTypeSystem: resp.LegacyTypeSystem,
	}, nil
}

// ApplyResourceChange maps to tfplugin5's ApplyResourceChange, with the same
// LegacyTypeSystem pass-through treatment as PlanResourceChange.
func (a *protocol5Adapter) ApplyResourceChange(ctx context.Context, in *tfplugin6.ApplyResourceChange_Request, opts ...grpc.CallOption) (*tfplugin6.ApplyResourceChange_Response, error) {
	resp, err := a.client.ApplyResourceChange(ctx, &tfplugin5.ApplyResourceChange_Request{
		TypeName:       in.TypeName,
		PriorState:     dynamicValue6to5(in.PriorState),
		PlannedState:   dynamicValue6to5(in.PlannedState),
		Config:         dynamicValue6to5(in.Config),
		PlannedPrivate: in.PlannedPrivate,
	}, opts...)
	if err != nil {
		return nil, err
	}
	return &tfplugin6.ApplyResourceChange_Response{
		NewState:         dynamicValue5to6(resp.NewState),
		Private:          resp.Private,
		Diagnostics:      diagnostics5to6(resp.Diagnostics),
		LegacyTypeSystem: resp.LegacyTypeSystem,
	}, nil
}

// ReadResource maps to tfplugin5's ReadResource.
func (a *protocol5Adapter) ReadResource(ctx context.Context, in *tfplugin6.ReadResource_Request, opts ...grpc.CallOption) (*tfplugin6.ReadResource_Response, error) {
	resp, err := a.client.ReadResource(ctx, &tfplugin5.ReadResource_Request{
		TypeName:     in.TypeName,
		CurrentState: dynamicValue6to5(in.CurrentState),
		Private:      in.Private,
	}, opts...)
	if err != nil {
		return nil, err
	}
	return &tfplugin6.ReadResource_Response{
		NewState:    dynamicValue5to6(resp.NewState),
		Diagnostics: diagnostics5to6(resp.Diagnostics),
		Private:     resp.Private,
		Deferred:    deferred5to6(resp.Deferred),
	}, nil
}

// ImportResourceState maps to tfplugin5's ImportResourceState. A non-nil
// tfplugin5 Deferred response is mapped to a non-nil tfplugin6 Deferred so
// any engine-side protocol-violation check on a deferred import still fires
// against the adapted response exactly as it would against a native
// protocol-6 provider.
func (a *protocol5Adapter) ImportResourceState(ctx context.Context, in *tfplugin6.ImportResourceState_Request, opts ...grpc.CallOption) (*tfplugin6.ImportResourceState_Response, error) {
	resp, err := a.client.ImportResourceState(ctx, &tfplugin5.ImportResourceState_Request{
		TypeName: in.TypeName,
		Id:       in.Id,
	}, opts...)
	if err != nil {
		return nil, err
	}
	imported := make([]*tfplugin6.ImportResourceState_ImportedResource, 0, len(resp.ImportedResources))
	for _, r := range resp.ImportedResources {
		if r == nil {
			continue
		}
		imported = append(imported, &tfplugin6.ImportResourceState_ImportedResource{
			TypeName: r.TypeName,
			State:    dynamicValue5to6(r.State),
			Private:  r.Private,
		})
	}
	return &tfplugin6.ImportResourceState_Response{
		ImportedResources: imported,
		Diagnostics:       diagnostics5to6(resp.Diagnostics),
		Deferred:          deferred5to6(resp.Deferred),
	}, nil
}

// StopProvider maps to tfplugin5's Stop.
func (a *protocol5Adapter) StopProvider(ctx context.Context, in *tfplugin6.StopProvider_Request, opts ...grpc.CallOption) (*tfplugin6.StopProvider_Response, error) {
	resp, err := a.client.Stop(ctx, &tfplugin5.Stop_Request{}, opts...)
	if err != nil {
		return nil, err
	}
	return &tfplugin6.StopProvider_Response{Error: resp.Error}, nil
}

// deferred5to6 converts a tfplugin5 Deferred marker into its tfplugin6
// counterpart. A nil input (the common case: no deferral) stays nil.
func deferred5to6(d *tfplugin5.Deferred) *tfplugin6.Deferred {
	if d == nil {
		return nil
	}
	var reason tfplugin6.Deferred_Reason
	switch d.Reason {
	case tfplugin5.Deferred_RESOURCE_CONFIG_UNKNOWN:
		reason = tfplugin6.Deferred_RESOURCE_CONFIG_UNKNOWN
	case tfplugin5.Deferred_PROVIDER_CONFIG_UNKNOWN:
		reason = tfplugin6.Deferred_PROVIDER_CONFIG_UNKNOWN
	case tfplugin5.Deferred_ABSENT_PREREQ:
		reason = tfplugin6.Deferred_ABSENT_PREREQ
	default:
		reason = tfplugin6.Deferred_UNKNOWN
	}
	return &tfplugin6.Deferred{Reason: reason}
}

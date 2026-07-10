// Package main implements the tchori fake test provider: a minimal, honest
// tfprotov6.ProviderServer used only as a test rig for the engine's own
// protocol client. Hand-rolled against terraform-plugin-go v0.31.0 — no
// terraform-plugin-framework, no terraform-plugin-sdk.
package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// providerType is the wire shape of the provider configuration block.
var providerType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"prefix": tftypes.String,
	},
}

var providerSchema = &tfprotov6.Schema{
	Block: &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "prefix", Type: tftypes.String, Optional: true},
		},
	},
}

// thingType is the wire shape of the tchoritest_thing resource.
var thingType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"echo":       tftypes.String,                           // Computed: always equals name
		"id":         tftypes.String,                           // Computed: "<prefix>id-<name>" at apply
		"name":       tftypes.String,                           // Required
		"replace_me": tftypes.String,                           // Optional: change forces replacement
		"tags":       tftypes.Map{ElementType: tftypes.String}, // Optional
	},
}

var thingSchema = &tfprotov6.Schema{
	Version: 0,
	Block: &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "echo", Type: tftypes.String, Computed: true},
			{Name: "id", Type: tftypes.String, Computed: true},
			{Name: "name", Type: tftypes.String, Required: true},
			{Name: "replace_me", Type: tftypes.String, Optional: true},
			{Name: "tags", Type: tftypes.Map{ElementType: tftypes.String}, Optional: true},
		},
	},
}

// server implements tfprotov6.ProviderServer. In terraform-plugin-go v0.31.0
// that interface requires 23 methods (6 provider RPCs + ResourceServer 9 +
// DataSourceServer 2 + FunctionServer 2 + EphemeralResourceServer 4). The
// RPCs this rig does not exercise are honest empty-response stubs — never
// panics.
type server struct {
	prefix string // captured by ConfigureProvider, used in ApplyResourceChange
}

var _ tfprotov6.ProviderServer = (*server)(nil)

// --- Provider-level RPCs ----------------------------------------------------

func (s *server) GetMetadata(ctx context.Context, req *tfprotov6.GetMetadataRequest) (*tfprotov6.GetMetadataResponse, error) {
	return &tfprotov6.GetMetadataResponse{
		ServerCapabilities: &tfprotov6.ServerCapabilities{
			GetProviderSchemaOptional: true,
		},
		Resources: []tfprotov6.ResourceMetadata{
			{TypeName: "tchoritest_thing"},
		},
	}, nil
}

func (s *server) GetProviderSchema(ctx context.Context, req *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	return &tfprotov6.GetProviderSchemaResponse{
		Provider: providerSchema,
		ResourceSchemas: map[string]*tfprotov6.Schema{
			"tchoritest_thing": thingSchema,
		},
		DataSourceSchemas: map[string]*tfprotov6.Schema{},
		Functions:         map[string]*tfprotov6.Function{},
	}, nil
}

func (s *server) GetResourceIdentitySchemas(ctx context.Context, req *tfprotov6.GetResourceIdentitySchemasRequest) (*tfprotov6.GetResourceIdentitySchemasResponse, error) {
	// No resource-identity support: empty response, honest stub.
	return &tfprotov6.GetResourceIdentitySchemasResponse{}, nil
}

func (s *server) ValidateProviderConfig(ctx context.Context, req *tfprotov6.ValidateProviderConfigRequest) (*tfprotov6.ValidateProviderConfigResponse, error) {
	return &tfprotov6.ValidateProviderConfigResponse{
		PreparedConfig: req.Config,
	}, nil
}

func (s *server) ConfigureProvider(ctx context.Context, req *tfprotov6.ConfigureProviderRequest) (*tfprotov6.ConfigureProviderResponse, error) {
	cfg, err := req.Config.Unmarshal(providerType)
	if err != nil {
		return nil, err
	}
	if !cfg.IsNull() {
		var attrs map[string]tftypes.Value
		if err := cfg.As(&attrs); err != nil {
			return nil, err
		}
		if p := attrs["prefix"]; p.IsKnown() && !p.IsNull() {
			if err := p.As(&s.prefix); err != nil {
				return nil, err
			}
		}
	}
	return &tfprotov6.ConfigureProviderResponse{}, nil
}

func (s *server) StopProvider(ctx context.Context, req *tfprotov6.StopProviderRequest) (*tfprotov6.StopProviderResponse, error) {
	return &tfprotov6.StopProviderResponse{}, nil
}

// --- ResourceServer ----------------------------------------------------------

func (s *server) ValidateResourceConfig(ctx context.Context, req *tfprotov6.ValidateResourceConfigRequest) (*tfprotov6.ValidateResourceConfigResponse, error) {
	cfg, err := req.Config.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	if cfg.IsNull() {
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	var attrs map[string]tftypes.Value
	if err := cfg.As(&attrs); err != nil {
		return nil, err
	}
	var name string
	if n := attrs["name"]; n.IsKnown() && !n.IsNull() {
		if err := n.As(&name); err != nil {
			return nil, err
		}
	}
	if name == "invalid" {
		return &tfprotov6.ValidateResourceConfigResponse{
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "invalid name",
				Detail:   `the name "invalid" is not allowed`,
			}},
		}, nil
	}
	return &tfprotov6.ValidateResourceConfigResponse{}, nil
}

func (s *server) UpgradeResourceState(ctx context.Context, req *tfprotov6.UpgradeResourceStateRequest) (*tfprotov6.UpgradeResourceStateResponse, error) {
	// Schema version is 0 and never bumped: reinterpret the raw state as-is.
	val, err := req.RawState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	dv, err := tfprotov6.NewDynamicValue(thingType, val)
	if err != nil {
		return nil, err
	}
	return &tfprotov6.UpgradeResourceStateResponse{UpgradedState: &dv}, nil
}

func (s *server) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	// No backing store: echo current state (and private) unchanged.
	return &tfprotov6.ReadResourceResponse{
		NewState: req.CurrentState,
		Private:  req.Private,
	}, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	// Delete: proposed new state is null; plan the null through.
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{
			PlannedState:   req.ProposedNewState,
			PlannedPrivate: req.PriorPrivate,
		}, nil
	}

	prior, err := req.PriorState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}

	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}

	var priorAttrs map[string]tftypes.Value
	if !prior.IsNull() {
		if err := prior.As(&priorAttrs); err != nil {
			return nil, err
		}
	}

	if prior.IsNull() {
		// Create: both computed attributes are decided at apply time.
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		attrs["echo"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		// Update: id never changes once created; echo is unknown only when
		// name changes, else it keeps its prior value.
		attrs["id"] = priorAttrs["id"]
		if attrs["name"].Equal(priorAttrs["name"]) {
			attrs["echo"] = priorAttrs["echo"]
		} else {
			attrs["echo"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		}
	}

	var requiresReplace []*tftypes.AttributePath
	if !prior.IsNull() && !attrs["replace_me"].Equal(priorAttrs["replace_me"]) {
		requiresReplace = append(requiresReplace,
			tftypes.NewAttributePath().WithAttributeName("replace_me"))
	}

	plannedDV, err := tfprotov6.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{
		PlannedState:    &plannedDV,
		RequiresReplace: requiresReplace,
		PlannedPrivate:  req.PriorPrivate,
	}, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	// Destroy: planned state is null; acknowledge the deletion.
	if planned.IsNull() {
		return &tfprotov6.ApplyResourceChangeResponse{NewState: req.PlannedState}, nil
	}
	var attrs map[string]tftypes.Value
	if err := planned.As(&attrs); err != nil {
		return nil, err
	}
	var name string
	if err := attrs["name"].As(&name); err != nil {
		return nil, err
	}
	// Deliberate failure hook for apply-time error handling tests (Task 11):
	// a "thing" named "explode" always fails to apply.
	if name == "explode" {
		return &tfprotov6.ApplyResourceChangeResponse{
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "apply exploded",
				Detail:   `the name "explode" always fails to apply`,
			}},
		}, nil
	}
	if !attrs["id"].IsKnown() {
		attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"id-"+name)
	}
	if !attrs["echo"].IsKnown() {
		attrs["echo"] = tftypes.NewValue(tftypes.String, name)
	}
	newDV, err := tfprotov6.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{
		NewState: &newDV,
		Private:  req.PlannedPrivate,
	}, nil
}

func (s *server) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	return &tfprotov6.ImportResourceStateResponse{}, nil
}

func (s *server) MoveResourceState(ctx context.Context, req *tfprotov6.MoveResourceStateRequest) (*tfprotov6.MoveResourceStateResponse, error) {
	return &tfprotov6.MoveResourceStateResponse{}, nil
}

func (s *server) UpgradeResourceIdentity(ctx context.Context, req *tfprotov6.UpgradeResourceIdentityRequest) (*tfprotov6.UpgradeResourceIdentityResponse, error) {
	return &tfprotov6.UpgradeResourceIdentityResponse{}, nil
}

func (s *server) GenerateResourceConfig(ctx context.Context, req *tfprotov6.GenerateResourceConfigRequest) (*tfprotov6.GenerateResourceConfigResponse, error) {
	// Mandatory to compile as of terraform-plugin-go v0.31.0; never invoked
	// because ServerCapabilities does not advertise it.
	return &tfprotov6.GenerateResourceConfigResponse{}, nil
}

// --- DataSourceServer ---------------------------------------------------------

func (s *server) ValidateDataResourceConfig(ctx context.Context, req *tfprotov6.ValidateDataResourceConfigRequest) (*tfprotov6.ValidateDataResourceConfigResponse, error) {
	return &tfprotov6.ValidateDataResourceConfigResponse{}, nil
}

func (s *server) ReadDataSource(ctx context.Context, req *tfprotov6.ReadDataSourceRequest) (*tfprotov6.ReadDataSourceResponse, error) {
	return &tfprotov6.ReadDataSourceResponse{State: req.Config}, nil
}

// --- FunctionServer -----------------------------------------------------------

func (s *server) CallFunction(ctx context.Context, req *tfprotov6.CallFunctionRequest) (*tfprotov6.CallFunctionResponse, error) {
	return &tfprotov6.CallFunctionResponse{}, nil
}

func (s *server) GetFunctions(ctx context.Context, req *tfprotov6.GetFunctionsRequest) (*tfprotov6.GetFunctionsResponse, error) {
	return &tfprotov6.GetFunctionsResponse{Functions: map[string]*tfprotov6.Function{}}, nil
}

// --- EphemeralResourceServer ----------------------------------------------------

func (s *server) ValidateEphemeralResourceConfig(ctx context.Context, req *tfprotov6.ValidateEphemeralResourceConfigRequest) (*tfprotov6.ValidateEphemeralResourceConfigResponse, error) {
	return &tfprotov6.ValidateEphemeralResourceConfigResponse{}, nil
}

func (s *server) OpenEphemeralResource(ctx context.Context, req *tfprotov6.OpenEphemeralResourceRequest) (*tfprotov6.OpenEphemeralResourceResponse, error) {
	return &tfprotov6.OpenEphemeralResourceResponse{Result: req.Config}, nil
}

func (s *server) RenewEphemeralResource(ctx context.Context, req *tfprotov6.RenewEphemeralResourceRequest) (*tfprotov6.RenewEphemeralResourceResponse, error) {
	return &tfprotov6.RenewEphemeralResourceResponse{}, nil
}

func (s *server) CloseEphemeralResource(ctx context.Context, req *tfprotov6.CloseEphemeralResourceRequest) (*tfprotov6.CloseEphemeralResourceResponse, error) {
	return &tfprotov6.CloseEphemeralResourceResponse{}, nil
}

func main() {
	err := tf6server.Serve(
		"registry.opentofu.org/tchori-labs/tchoritest",
		func() tfprotov6.ProviderServer { return &server{} },
	)
	if err != nil {
		log.Fatal(err)
	}
}

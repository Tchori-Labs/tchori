// Package main implements the tchori protocol-5 fake test provider: a
// minimal, honest tfprotov5.ProviderServer used as a test rig for the
// engine's tfplugin5 adapter (see internal/provider/tfplugin5_adapter.go).
// Hand-rolled against terraform-plugin-go v0.31.0 — no
// terraform-plugin-framework, no terraform-plugin-sdk — mirroring
// internal/provider/testprovider/main.go's tchoritest_thing behavior
// exactly, but declared over protocol 5 as provider tchoritest5 with
// resource type tchoritest5_thing.
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5/tf5server"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// providerType is the wire shape of the provider configuration block.
var providerType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"prefix": tftypes.String,
	},
}

var providerSchema = &tfprotov5.Schema{
	Block: &tfprotov5.SchemaBlock{
		Attributes: []*tfprotov5.SchemaAttribute{
			{Name: "prefix", Type: tftypes.String, Optional: true},
		},
	},
}

// thingRuleType is the wire shape of one element of the optional "rules"
// list, kept in sync with testprovider's protocol-6 fixture.
var thingRuleType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"token_id": tftypes.String,
	},
}

// thingType is the wire shape of the tchoritest5_thing resource — the same
// attribute set as tchoritest_thing (internal/provider/testprovider).
var thingType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"echo":       tftypes.String,                           // Computed: always equals name
		"id":         tftypes.String,                           // Computed: "<prefix>id-<name>" at apply
		"name":       tftypes.String,                           // Required
		"replace_me": tftypes.String,                           // Optional: change forces replacement
		"rules":      tftypes.List{ElementType: thingRuleType}, // Optional: list-of-object
		"tags":       tftypes.Map{ElementType: tftypes.String}, // Optional
	},
}

var thingSchema = &tfprotov5.Schema{
	Version: 0,
	Block: &tfprotov5.SchemaBlock{
		Attributes: []*tfprotov5.SchemaAttribute{
			{Name: "echo", Type: tftypes.String, Computed: true},
			{Name: "id", Type: tftypes.String, Computed: true},
			{Name: "name", Type: tftypes.String, Required: true},
			{Name: "replace_me", Type: tftypes.String, Optional: true},
			{Name: "rules", Type: tftypes.List{ElementType: thingRuleType}, Optional: true},
			{Name: "tags", Type: tftypes.Map{ElementType: tftypes.String}, Optional: true},
		},
	},
}

// server implements tfprotov5.ProviderServer. The RPCs this rig does not
// exercise are honest empty-response stubs — never panics.
type server struct {
	prefix string // captured by ConfigureProvider, used in ApplyResourceChange
}

var _ tfprotov5.ProviderServer = (*server)(nil)

// --- Provider-level RPCs ----------------------------------------------------

func (s *server) GetMetadata(ctx context.Context, req *tfprotov5.GetMetadataRequest) (*tfprotov5.GetMetadataResponse, error) {
	return &tfprotov5.GetMetadataResponse{
		ServerCapabilities: &tfprotov5.ServerCapabilities{
			GetProviderSchemaOptional: true,
		},
		Resources: []tfprotov5.ResourceMetadata{
			{TypeName: "tchoritest5_thing"},
		},
	}, nil
}

func (s *server) GetProviderSchema(ctx context.Context, req *tfprotov5.GetProviderSchemaRequest) (*tfprotov5.GetProviderSchemaResponse, error) {
	return &tfprotov5.GetProviderSchemaResponse{
		Provider: providerSchema,
		ResourceSchemas: map[string]*tfprotov5.Schema{
			"tchoritest5_thing": thingSchema,
		},
		DataSourceSchemas: map[string]*tfprotov5.Schema{},
		Functions:         map[string]*tfprotov5.Function{},
	}, nil
}

func (s *server) GetResourceIdentitySchemas(ctx context.Context, req *tfprotov5.GetResourceIdentitySchemasRequest) (*tfprotov5.GetResourceIdentitySchemasResponse, error) {
	// No resource-identity support: empty response, honest stub.
	return &tfprotov5.GetResourceIdentitySchemasResponse{}, nil
}

func (s *server) PrepareProviderConfig(ctx context.Context, req *tfprotov5.PrepareProviderConfigRequest) (*tfprotov5.PrepareProviderConfigResponse, error) {
	return &tfprotov5.PrepareProviderConfigResponse{
		PreparedConfig: req.Config,
	}, nil
}

func (s *server) ConfigureProvider(ctx context.Context, req *tfprotov5.ConfigureProviderRequest) (*tfprotov5.ConfigureProviderResponse, error) {
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
	return &tfprotov5.ConfigureProviderResponse{}, nil
}

func (s *server) StopProvider(ctx context.Context, req *tfprotov5.StopProviderRequest) (*tfprotov5.StopProviderResponse, error) {
	if os.Getenv("TCHORITEST5_STALL_STOP") != "" {
		time.Sleep(30 * time.Second)
	}
	return &tfprotov5.StopProviderResponse{}, nil
}

// --- ResourceServer ----------------------------------------------------------

func (s *server) ValidateResourceTypeConfig(ctx context.Context, req *tfprotov5.ValidateResourceTypeConfigRequest) (*tfprotov5.ValidateResourceTypeConfigResponse, error) {
	cfg, err := req.Config.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	if cfg.IsNull() {
		return &tfprotov5.ValidateResourceTypeConfigResponse{}, nil
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
		return &tfprotov5.ValidateResourceTypeConfigResponse{
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
				Summary:  "invalid name",
				Detail:   `the name "invalid" is not allowed`,
			}},
		}, nil
	}
	return &tfprotov5.ValidateResourceTypeConfigResponse{}, nil
}

func (s *server) UpgradeResourceState(ctx context.Context, req *tfprotov5.UpgradeResourceStateRequest) (*tfprotov5.UpgradeResourceStateResponse, error) {
	// Schema version is 0 and never bumped: reinterpret the raw state as-is.
	val, err := req.RawState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	dv, err := tfprotov5.NewDynamicValue(thingType, val)
	if err != nil {
		return nil, err
	}
	return &tfprotov5.UpgradeResourceStateResponse{UpgradedState: &dv}, nil
}

func (s *server) ReadResource(ctx context.Context, req *tfprotov5.ReadResourceRequest) (*tfprotov5.ReadResourceResponse, error) {
	// Test hook: refreshing a resource named "vanish" reports it gone
	// (null NewState), exercising the engine's null-on-refresh-after-import
	// rejection.
	cur, err := req.CurrentState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	if !cur.IsNull() {
		var attrs map[string]tftypes.Value
		if err := cur.As(&attrs); err == nil {
			var name string
			if n := attrs["name"]; n.IsKnown() && !n.IsNull() {
				_ = n.As(&name)
			}
			if name == "vanish" {
				nullDV, err := tfprotov5.NewDynamicValue(thingType, tftypes.NewValue(thingType, nil))
				if err != nil {
					return nil, err
				}
				return &tfprotov5.ReadResourceResponse{NewState: &nullDV}, nil
			}
			// TC-050 / issue #52 reproduction hook: mirror the protocol-6
			// fake's unpathed Coolify HTML decode failure byte-for-byte.
			if name == "gateway_html" {
				return &tfprotov5.ReadResourceResponse{Diagnostics: []*tfprotov5.Diagnostic{{
					Severity: tfprotov5.DiagnosticSeverityError,
					Summary:  "Error reading project",
					Detail:   "decoding response: invalid character '<' looking for beginning of value",
				}}}, nil
			}
			if strings.HasPrefix(name, "drift-") {
				attrs["echo"] = tftypes.NewValue(tftypes.String, "degraded:unhealthy")
				newState, err := tfprotov5.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
				if err != nil {
					return nil, err
				}
				return &tfprotov5.ReadResourceResponse{NewState: &newState, Private: req.Private}, nil
			}
		}
	}
	// No backing store: echo current state (and private) unchanged.
	return &tfprotov5.ReadResourceResponse{
		NewState: req.CurrentState,
		Private:  req.Private,
	}, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfprotov5.PlanResourceChangeRequest) (*tfprotov5.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	// Delete: proposed new state is null; plan the null through.
	if proposed.IsNull() {
		return &tfprotov5.PlanResourceChangeResponse{
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

	plannedDV, err := tfprotov5.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov5.PlanResourceChangeResponse{
		PlannedState:    &plannedDV,
		RequiresReplace: requiresReplace,
		PlannedPrivate:  req.PriorPrivate,
	}, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfprotov5.ApplyResourceChangeRequest) (*tfprotov5.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	// Destroy: planned state is null; acknowledge the deletion.
	if planned.IsNull() {
		return &tfprotov5.ApplyResourceChangeResponse{NewState: req.PlannedState}, nil
	}
	var attrs map[string]tftypes.Value
	if err := planned.As(&attrs); err != nil {
		return nil, err
	}
	var name string
	if err := attrs["name"].As(&name); err != nil {
		return nil, err
	}
	// TC-052 / issue #53 reproduction hook: mirror Coolify's unpathed,
	// bodyless API rejection byte-for-byte.
	if name == "api_400" {
		return &tfprotov5.ApplyResourceChangeResponse{
			NewState: req.PriorState,
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
				Summary:  "Error updating service",
				Detail:   "api error (status 400): Invalid request",
			}},
		}, nil
	}
	// Deliberate failure hook for apply-time error handling tests: a
	// "thing" named "explode" always fails to apply.
	if name == "explode" {
		return &tfprotov5.ApplyResourceChangeResponse{
			// A nil NewState alongside an apply error reads as deletion to
			// the client; keep prior state to disambiguate.
			NewState: req.PriorState,
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
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
	newDV, err := tfprotov5.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov5.ApplyResourceChangeResponse{
		NewState: &newDV,
		Private:  req.PlannedPrivate,
	}, nil
}

// ImportResourceState adopts an existing tchoritest5_thing by ID: it derives
// name from the substring after the last "id-" marker, matching
// ApplyResourceChange's id = "<prefix>id-<name>" convention, and returns a
// fully populated state so import -> plan is a clean no-op. IDs without an
// "id-" marker, or the literal "missing", are rejected so the CLI's
// "resource does not exist" path is testable.
func (s *server) ImportResourceState(ctx context.Context, req *tfprotov5.ImportResourceStateRequest) (*tfprotov5.ImportResourceStateResponse, error) {
	if req.TypeName != "tchoritest5_thing" {
		return &tfprotov5.ImportResourceStateResponse{
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
				Summary:  "import not supported",
				Detail:   "resource type " + req.TypeName + " does not support import",
			}},
		}, nil
	}
	if req.ID == "missing" {
		return &tfprotov5.ImportResourceStateResponse{
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
				Summary:  "resource does not exist",
				Detail:   `id "missing" has no matching remote resource`,
			}},
		}, nil
	}
	const marker = "id-"
	idx := strings.LastIndex(req.ID, marker)
	if idx < 0 {
		return &tfprotov5.ImportResourceStateResponse{
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
				Summary:  "resource does not exist",
				Detail:   `id "` + req.ID + `" has no "id-" marker`,
			}},
		}, nil
	}
	name := req.ID[idx+len(marker):]
	if name == "" {
		return &tfprotov5.ImportResourceStateResponse{
			Diagnostics: []*tfprotov5.Diagnostic{{
				Severity: tfprotov5.DiagnosticSeverityError,
				Summary:  "resource does not exist",
				Detail:   `id "` + req.ID + `" has an empty name after "id-"`,
			}},
		}, nil
	}
	attrs := map[string]tftypes.Value{
		"id":         tftypes.NewValue(tftypes.String, req.ID),
		"name":       tftypes.NewValue(tftypes.String, name),
		"echo":       tftypes.NewValue(tftypes.String, name),
		"replace_me": tftypes.NewValue(tftypes.String, nil),
		"rules":      tftypes.NewValue(tftypes.List{ElementType: thingRuleType}, nil),
		"tags":       tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil),
	}
	dv, err := tfprotov5.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov5.ImportResourceStateResponse{
		ImportedResources: []*tfprotov5.ImportedResource{{
			TypeName: req.TypeName,
			State:    &dv,
		}},
	}, nil
}

func (s *server) MoveResourceState(ctx context.Context, req *tfprotov5.MoveResourceStateRequest) (*tfprotov5.MoveResourceStateResponse, error) {
	return &tfprotov5.MoveResourceStateResponse{}, nil
}

func (s *server) UpgradeResourceIdentity(ctx context.Context, req *tfprotov5.UpgradeResourceIdentityRequest) (*tfprotov5.UpgradeResourceIdentityResponse, error) {
	return &tfprotov5.UpgradeResourceIdentityResponse{}, nil
}

func (s *server) GenerateResourceConfig(ctx context.Context, req *tfprotov5.GenerateResourceConfigRequest) (*tfprotov5.GenerateResourceConfigResponse, error) {
	// Mandatory to compile as of terraform-plugin-go v0.31.0; never invoked
	// because ServerCapabilities does not advertise it.
	return &tfprotov5.GenerateResourceConfigResponse{}, nil
}

// --- DataSourceServer ---------------------------------------------------------

func (s *server) ValidateDataSourceConfig(ctx context.Context, req *tfprotov5.ValidateDataSourceConfigRequest) (*tfprotov5.ValidateDataSourceConfigResponse, error) {
	return &tfprotov5.ValidateDataSourceConfigResponse{}, nil
}

func (s *server) ReadDataSource(ctx context.Context, req *tfprotov5.ReadDataSourceRequest) (*tfprotov5.ReadDataSourceResponse, error) {
	return &tfprotov5.ReadDataSourceResponse{State: req.Config}, nil
}

// --- FunctionServer -----------------------------------------------------------

func (s *server) CallFunction(ctx context.Context, req *tfprotov5.CallFunctionRequest) (*tfprotov5.CallFunctionResponse, error) {
	return &tfprotov5.CallFunctionResponse{}, nil
}

func (s *server) GetFunctions(ctx context.Context, req *tfprotov5.GetFunctionsRequest) (*tfprotov5.GetFunctionsResponse, error) {
	return &tfprotov5.GetFunctionsResponse{Functions: map[string]*tfprotov5.Function{}}, nil
}

// --- EphemeralResourceServer ----------------------------------------------------

func (s *server) ValidateEphemeralResourceConfig(ctx context.Context, req *tfprotov5.ValidateEphemeralResourceConfigRequest) (*tfprotov5.ValidateEphemeralResourceConfigResponse, error) {
	return &tfprotov5.ValidateEphemeralResourceConfigResponse{}, nil
}

func (s *server) OpenEphemeralResource(ctx context.Context, req *tfprotov5.OpenEphemeralResourceRequest) (*tfprotov5.OpenEphemeralResourceResponse, error) {
	return &tfprotov5.OpenEphemeralResourceResponse{Result: req.Config}, nil
}

func (s *server) RenewEphemeralResource(ctx context.Context, req *tfprotov5.RenewEphemeralResourceRequest) (*tfprotov5.RenewEphemeralResourceResponse, error) {
	return &tfprotov5.RenewEphemeralResourceResponse{}, nil
}

func (s *server) CloseEphemeralResource(ctx context.Context, req *tfprotov5.CloseEphemeralResourceRequest) (*tfprotov5.CloseEphemeralResourceResponse, error) {
	return &tfprotov5.CloseEphemeralResourceResponse{}, nil
}

func main() {
	if pidFile := os.Getenv("TCHORITEST5_PID_FILE"); pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { //nolint:gosec // G703: test-only path is explicitly provided by the lifecycle test
			log.Fatal(err)
		}
	}
	if os.Getenv("TCHORITEST5_STALL_STARTUP") != "" {
		time.Sleep(24 * time.Hour)
	}

	err := tf5server.Serve(
		"registry.opentofu.org/tchori-labs/tchoritest5",
		func() tfprotov5.ProviderServer { return &server{} },
	)
	if err != nil {
		log.Fatal(err)
	}
}

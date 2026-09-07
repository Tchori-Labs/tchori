// Package main implements the tchori fake test provider: a minimal, honest
// tfprotov6.ProviderServer used only as a test rig for the engine's own
// protocol client. Hand-rolled against terraform-plugin-go v0.31.0 — no
// terraform-plugin-framework, no terraform-plugin-sdk.
package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/tchori-labs/tchori/internal/provider/testprovider/pidfile"
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
// thingRuleType is the wire shape of one element of thingType's "rules"
// list attribute: a single string leaf, token_id, mirroring the cloudflare
// access-policy shape from issue #11 (policies[].include[].service_token.
// token_id) closely enough to reproduce the bug at the fake-provider level.
var thingRuleType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"token_id": tftypes.String,
	},
}

var thingType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"echo":       tftypes.String,                           // Computed: always equals name
		"id":         tftypes.String,                           // Computed: "<prefix>id-<name>" at apply
		"name":       tftypes.String,                           // Required
		"replace_me": tftypes.String,                           // Optional: change forces replacement
		"rules":      tftypes.List{ElementType: thingRuleType}, // Optional: list-of-object, TC-033 fixture
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
			{Name: "rules", Type: tftypes.List{ElementType: thingRuleType}, Optional: true},
			{Name: "tags", Type: tftypes.Map{ElementType: tftypes.String}, Optional: true},
		},
	},
}

// nestedSettingsType is the wire shape of tchoritest_nested_thing's
// "settings" nested_type attribute (SchemaObjectNestingModeSingle, see
// nestedThingSchema below).
var nestedSettingsType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"flag":  tftypes.Bool,
		"label": tftypes.String,
	},
}

// nestedThingType is the wire shape of the tchoritest_nested_thing resource:
// the same "id"/"name" shape as tchoritest_thing, plus a nested_type
// "settings" attribute that this engine's schema.go must convert to a cty
// object instead of rejecting (issue #7). This fake provider passes
// "settings" through Plan/Apply completely unmodified — only "id" is
// computed — so tests can assert the nested value (or its null absence)
// survives the Compose -> msgpack -> provider -> state round trip intact.
var nestedThingType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"id":       tftypes.String,
		"name":     tftypes.String,
		"settings": nestedSettingsType,
	},
}

// nestedThingSchema declares tchoritest_nested_thing: a resource type whose
// "settings" attribute uses nested_type (tfprotov6.SchemaObject in an
// attribute's NestedType field, SchemaObjectNestingModeSingle) with two
// optional leaf attributes. It is the acceptance fixture for issue #7:
// tchori's engine must convert this schema (not tolerate it as unsupported)
// and round-trip both an omitted ("settings" absent from config, i.e. null)
// and a populated "settings" value through validate/plan/apply.
var nestedThingSchema = &tfprotov6.Schema{
	Version: 0,
	Block: &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "id", Type: tftypes.String, Computed: true},
			{Name: "name", Type: tftypes.String, Required: true},
			{
				Name:     "settings",
				Optional: true,
				NestedType: &tfprotov6.SchemaObject{
					Nesting: tfprotov6.SchemaObjectNestingModeSingle,
					Attributes: []*tfprotov6.SchemaAttribute{
						{Name: "flag", Type: tftypes.Bool, Optional: true},
						{Name: "label", Type: tftypes.String, Optional: true},
					},
				},
			},
		},
	},
}

// ingressThingType and ingressThingSchema reproduce issue #50 / TC-055: one
// ingress list element may leave origin_request null while a sibling supplies
// it. Plan and apply pass ingress through unchanged and mint only id.
var ingressOriginRequestType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"connect_timeout": tftypes.String,
		"no_tls_verify":   tftypes.Bool,
	},
}

var ingressElementType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"service":        tftypes.String,
		"origin_request": ingressOriginRequestType,
	},
}

var ingressThingType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"id":      tftypes.String,
		"name":    tftypes.String,
		"ingress": tftypes.List{ElementType: ingressElementType},
	},
}

var ingressThingSchema = &tfprotov6.Schema{
	Version: 0,
	Block: &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "id", Type: tftypes.String, Computed: true},
			{Name: "name", Type: tftypes.String, Required: true},
			{
				Name:     "ingress",
				Optional: true,
				NestedType: &tfprotov6.SchemaObject{
					Nesting: tfprotov6.SchemaObjectNestingModeList,
					Attributes: []*tfprotov6.SchemaAttribute{
						{Name: "service", Type: tftypes.String, Optional: true},
						{
							Name:     "origin_request",
							Optional: true,
							NestedType: &tfprotov6.SchemaObject{
								Nesting: tfprotov6.SchemaObjectNestingModeSingle,
								Attributes: []*tfprotov6.SchemaAttribute{
									{Name: "connect_timeout", Type: tftypes.String, Optional: true},
									{Name: "no_tls_verify", Type: tftypes.Bool, Optional: true},
								},
							},
						},
					},
				},
			},
		},
	},
}

// serverAssignedType is the wire shape of tchoritest_server_assigned.
var serverAssignedType = tftypes.Object{
	AttributeTypes: map[string]tftypes.Type{
		"name":       tftypes.String,
		"id":         tftypes.String,
		"status":     tftypes.String,
		"created_at": tftypes.String,
	},
}

// serverAssignedSchema declares tchoritest_server_assigned: one required
// attribute the operator writes and three the remote API decides. It is the
// fixture for issue #59.
//
// The difference from tchoritest_thing is deliberate and is the whole point:
// tchoritest_thing's PlanResourceChange repairs its own computed attributes
// by hand (see the `attrs["id"] = priorAttrs["id"]` branch), so it converges
// even when handed a proposed new state full of nulls. Real providers do not
// do that. Ones built on terraform-plugin-framework lean on plan modifiers
// like UseStateForUnknown, which fire only for UNKNOWN values and so never
// run against a null. That is why the engine bug in #59 survived a green test
// suite: every fixture was too well behaved to expose it.
//
// This resource is the honest naive case — its plan echoes ProposedNewState
// unchanged — so a planner test against it fails the moment the engine
// proposes null for an attribute the operator never wrote.
var serverAssignedSchema = &tfprotov6.Schema{
	Version: 0,
	Block: &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "name", Type: tftypes.String, Required: true},
			{Name: "id", Type: tftypes.String, Computed: true},
			{Name: "status", Type: tftypes.String, Computed: true},
			{Name: "created_at", Type: tftypes.String, Computed: true},
		},
	},
}

const secretSentinel = "tchori-e2e-super-secret-value" //nolint:gosec // deliberate fake credential sentinel proving absence from artifacts

var secretRuleType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{"token": tftypes.String}}
var secretfulType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"name": tftypes.String, "id": tftypes.String, "client_secret": tftypes.String,
	"write_only_secret": tftypes.String, "token": tftypes.String, "note": tftypes.String,
	"rules": tftypes.List{ElementType: secretRuleType},
}}
var secretfulSchema = &tfprotov6.Schema{Version: 0, Block: &tfprotov6.SchemaBlock{
	Attributes: []*tfprotov6.SchemaAttribute{
		{Name: "name", Type: tftypes.String, Required: true}, {Name: "id", Type: tftypes.String, Computed: true},
		{Name: "client_secret", Type: tftypes.String, Computed: true, Sensitive: true},
		{Name: "write_only_secret", Type: tftypes.String, Computed: true, Sensitive: true},
		{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true}, {Name: "note", Type: tftypes.String, Optional: true},
	},
	BlockTypes: []*tfprotov6.SchemaNestedBlock{{TypeName: "rules", Nesting: tfprotov6.SchemaNestedBlockNestingModeList, Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true}}}}},
}}
var refreshSensitiveType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"name": tftypes.String, "id": tftypes.String, "secret": tftypes.String,
	"members": tftypes.List{ElementType: tftypes.String}, "note": tftypes.String,
}}

var refreshSensitiveSchema = &tfprotov6.Schema{Version: 0, Block: &tfprotov6.SchemaBlock{
	Attributes: []*tfprotov6.SchemaAttribute{
		{Name: "name", Type: tftypes.String, Required: true},
		{Name: "id", Type: tftypes.String, Computed: true},
		{Name: "secret", Type: tftypes.String, Optional: true, Computed: true, Sensitive: true},
		{Name: "members", Type: tftypes.List{ElementType: tftypes.String}, Optional: true, Computed: true, Sensitive: true},
		{Name: "note", Type: tftypes.String, Optional: true, Computed: true},
	},
}}

// brokenThingSchema declares tchoritest_broken_thing: a resource type whose
// "settings" attribute is nested_type, but with a nesting mode
// blockFromProto/nestedObjectType does not recognize (none of
// SINGLE/LIST/SET/MAP — see internal/provider/schema.go's nestedObjectType),
// which tchori's engine genuinely cannot convert. nested_type itself is now
// supported (issue #7); this fixture instead exercises the case that stays
// out of scope: a schema whose nested shape this engine does not understand.
// It exists purely as a test fixture for the tolerate-until-used machinery
// from issue #5: it is never configured, planned, or applied by any test —
// only its presence in GetProviderSchema exercises that Schemas() must still
// succeed for the whole provider, and only a config that actually references
// this type may fail, with the stored per-type diagnostic.
var brokenThingSchema = &tfprotov6.Schema{
	Version: 0,
	Block: &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "name", Type: tftypes.String, Required: true},
			{
				Name:     "settings",
				Optional: true,
				NestedType: &tfprotov6.SchemaObject{
					Nesting: tfprotov6.SchemaObjectNestingMode(99), // unrecognized nesting mode
					Attributes: []*tfprotov6.SchemaAttribute{
						{Name: "flag", Type: tftypes.Bool, Optional: true},
					},
				},
			},
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

func knownResourceType(typeName string) bool {
	switch typeName {
	case "tchoritest_thing", "tchoritest_lossy", "tchoritest_nested_thing", "tchoritest_ingress_thing", "tchoritest_server_assigned", "tchoritest_secretful", "tchoritest_refresh_sensitive", "tchoritest_set_thing", "tchoritest_flat_set_thing", "tchoritest_broken_thing":
		return true
	default:
		return false
	}
}

func unknownResourceTypeDiagnostic(typeName string) *tfprotov6.Diagnostic {
	return &tfprotov6.Diagnostic{Severity: tfprotov6.DiagnosticSeverityError, Summary: "unknown resource type", Detail: "resource type " + typeName + " is not registered"}
}

// --- Provider-level RPCs ----------------------------------------------------

func (s *server) GetMetadata(ctx context.Context, req *tfprotov6.GetMetadataRequest) (*tfprotov6.GetMetadataResponse, error) {
	return &tfprotov6.GetMetadataResponse{
		ServerCapabilities: &tfprotov6.ServerCapabilities{
			GetProviderSchemaOptional: true,
		},
		Resources: []tfprotov6.ResourceMetadata{
			{TypeName: "tchoritest_thing"},
			{TypeName: "tchoritest_lossy"},
			{TypeName: "tchoritest_nested_thing"},
			{TypeName: "tchoritest_ingress_thing"},
			{TypeName: "tchoritest_server_assigned"},
			{TypeName: "tchoritest_secretful"},
			{TypeName: "tchoritest_refresh_sensitive"},
			{TypeName: "tchoritest_set_thing"},
			{TypeName: "tchoritest_flat_set_thing"},
			{TypeName: "tchoritest_broken_thing"},
		},
	}, nil
}

func (s *server) GetProviderSchema(ctx context.Context, req *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	return &tfprotov6.GetProviderSchemaResponse{
		Provider: providerSchema,
		ResourceSchemas: map[string]*tfprotov6.Schema{
			"tchoritest_thing":             thingSchema,
			"tchoritest_lossy":             lossySchema,
			"tchoritest_nested_thing":      nestedThingSchema,
			"tchoritest_ingress_thing":     ingressThingSchema,
			"tchoritest_server_assigned":   serverAssignedSchema,
			"tchoritest_secretful":         secretfulSchema,
			"tchoritest_refresh_sensitive": refreshSensitiveSchema,
			"tchoritest_set_thing":         setThingSchema,
			"tchoritest_flat_set_thing":    flatSetThingSchema,
			"tchoritest_broken_thing":      brokenThingSchema,
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
	// TC-050 / issue #52 reproduction hook: emulate a provider decoding an
	// identity-aware proxy's HTML response during configuration.
	if s.prefix == "gateway_html" {
		return &tfprotov6.ConfigureProviderResponse{Diagnostics: []*tfprotov6.Diagnostic{{
			Severity: tfprotov6.DiagnosticSeverityError,
			Summary:  "Error reading project",
			Detail:   "decoding response: invalid character '<' looking for beginning of value",
		}}}, nil
	}
	return &tfprotov6.ConfigureProviderResponse{}, nil
}

func (s *server) StopProvider(ctx context.Context, req *tfprotov6.StopProviderRequest) (*tfprotov6.StopProviderResponse, error) {
	if path := os.Getenv("TCHORITEST_STOP_FILE"); path != "" {
		if err := os.WriteFile(path, []byte("stopping"), 0o600); err != nil { //nolint:gosec // test-only provider writes caller-selected marker
			return nil, err
		}
	}
	if os.Getenv("TCHORITEST_STALL_STOP") != "" {
		time.Sleep(30 * time.Second)
	}
	return &tfprotov6.StopProviderResponse{}, nil
}

// --- ResourceServer ----------------------------------------------------------

func (s *server) ValidateResourceConfig(ctx context.Context, req *tfprotov6.ValidateResourceConfigRequest) (*tfprotov6.ValidateResourceConfigResponse, error) {
	if !knownResourceType(req.TypeName) {
		return &tfprotov6.ValidateResourceConfigResponse{Diagnostics: []*tfprotov6.Diagnostic{unknownResourceTypeDiagnostic(req.TypeName)}}, nil
	}
	if req.TypeName == "tchoritest_lossy" {
		if _, err := req.Config.Unmarshal(lossyType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_nested_thing" {
		if _, err := req.Config.Unmarshal(nestedThingType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_ingress_thing" {
		if _, err := req.Config.Unmarshal(ingressThingType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_server_assigned" {
		if _, err := req.Config.Unmarshal(serverAssignedType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_secretful" {
		if _, err := req.Config.Unmarshal(secretfulType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_refresh_sensitive" {
		if _, err := req.Config.Unmarshal(refreshSensitiveType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_set_thing" {
		if _, err := req.Config.Unmarshal(setThingType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
	if req.TypeName == "tchoritest_flat_set_thing" {
		if _, err := req.Config.Unmarshal(flatSetThingType); err != nil {
			return nil, err
		}
		return &tfprotov6.ValidateResourceConfigResponse{}, nil
	}
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
	if !knownResourceType(req.TypeName) {
		return &tfprotov6.UpgradeResourceStateResponse{Diagnostics: []*tfprotov6.Diagnostic{unknownResourceTypeDiagnostic(req.TypeName)}}, nil
	}
	// Schema version is 0 and never bumped for any resource type: reinterpret
	// the raw state as-is, just against the requested type's own wire shape.
	ty := thingType
	switch req.TypeName {
	case "tchoritest_lossy":
		ty = lossyType
	case "tchoritest_nested_thing":
		ty = nestedThingType
	case "tchoritest_ingress_thing":
		ty = ingressThingType
	case "tchoritest_server_assigned":
		ty = serverAssignedType
	case "tchoritest_secretful":
		ty = secretfulType
	case "tchoritest_refresh_sensitive":
		ty = refreshSensitiveType
	case "tchoritest_set_thing":
		ty = setThingType
	case "tchoritest_flat_set_thing":
		ty = flatSetThingType
	}
	val, err := req.RawState.Unmarshal(ty)
	if err != nil {
		return nil, err
	}
	dv, err := tfprotov6.NewDynamicValue(ty, val)
	if err != nil {
		return nil, err
	}
	return &tfprotov6.UpgradeResourceStateResponse{UpgradedState: &dv}, nil
}

func (s *server) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	if !knownResourceType(req.TypeName) {
		return &tfprotov6.ReadResourceResponse{Diagnostics: []*tfprotov6.Diagnostic{unknownResourceTypeDiagnostic(req.TypeName)}}, nil
	}
	if req.TypeName == "tchoritest_thing" {
		cur, err := req.CurrentState.Unmarshal(thingType)
		if err != nil {
			return nil, err
		}
		if !cur.IsNull() {
			var attrs map[string]tftypes.Value
			if err := cur.As(&attrs); err != nil {
				return nil, err
			}
			var name string
			if n := attrs["name"]; n.IsKnown() && !n.IsNull() {
				if err := n.As(&name); err != nil {
					return nil, err
				}
			}
			// TC-050 / issue #52 reproduction hooks: gateway_html mirrors the
			// Coolify provider's unpathed decode error byte-for-byte, while
			// gateway_attr proves provider attribute paths are qualified.
			switch name {
			case "gateway_html":
				return &tfprotov6.ReadResourceResponse{Diagnostics: []*tfprotov6.Diagnostic{{
					Severity: tfprotov6.DiagnosticSeverityError,
					Summary:  "Error reading project",
					Detail:   "decoding response: invalid character '<' looking for beginning of value",
				}}}, nil
			case "gateway_attr":
				return &tfprotov6.ReadResourceResponse{Diagnostics: []*tfprotov6.Diagnostic{{
					Severity:  tfprotov6.DiagnosticSeverityError,
					Summary:   "invalid remote name",
					Detail:    "the remote API rejected this attribute",
					Attribute: tftypes.NewAttributePath().WithAttributeName("name"),
				}}}, nil
			}
			if strings.HasPrefix(name, "drift-") {
				attrs["echo"] = tftypes.NewValue(tftypes.String, "degraded:unhealthy")
				newState, err := tfprotov6.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
				if err != nil {
					return nil, err
				}
				return &tfprotov6.ReadResourceResponse{NewState: &newState, Private: req.Private}, nil
			}
		}
	}
	if req.TypeName == "tchoritest_refresh_sensitive" {
		return s.readRefreshSensitive(req)
	}
	if req.TypeName == "tchoritest_flat_set_thing" {
		return s.readFlatSetThing(req)
	}
	// No backing store: echo current state (and private) unchanged.
	return &tfprotov6.ReadResourceResponse{
		NewState: req.CurrentState,
		Private:  req.Private,
	}, nil
}
func (s *server) readRefreshSensitive(req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	current, err := req.CurrentState.Unmarshal(refreshSensitiveType)
	if err != nil {
		return nil, err
	}
	if current.IsNull() {
		return &tfprotov6.ReadResourceResponse{NewState: req.CurrentState, Private: req.Private}, nil
	}
	var attrs map[string]tftypes.Value
	if err := current.As(&attrs); err != nil {
		return nil, err
	}
	var name string
	if err := attrs["name"].As(&name); err != nil {
		return nil, err
	}
	if name == "omit-sensitive" {
		attrs["secret"] = tftypes.NewValue(tftypes.String, nil)
		attrs["members"] = tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, tftypes.UnknownValue)
		attrs["note"] = tftypes.NewValue(tftypes.String, "remote-note")
		newState, err := tfprotov6.NewDynamicValue(refreshSensitiveType, tftypes.NewValue(refreshSensitiveType, attrs))
		if err != nil {
			return nil, err
		}
		return &tfprotov6.ReadResourceResponse{NewState: &newState, Private: req.Private}, nil
	}
	return &tfprotov6.ReadResourceResponse{NewState: req.CurrentState, Private: req.Private}, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	if !knownResourceType(req.TypeName) {
		return &tfprotov6.PlanResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{unknownResourceTypeDiagnostic(req.TypeName)}}, nil
	}
	if req.TypeName == "tchoritest_lossy" {
		return s.planLossy(req)
	}
	if req.TypeName == "tchoritest_nested_thing" {
		return s.planNestedThing(req)
	}
	if req.TypeName == "tchoritest_ingress_thing" {
		return s.planIngressThing(req)
	}
	if req.TypeName == "tchoritest_server_assigned" {
		return s.planServerAssigned(req)
	}
	if req.TypeName == "tchoritest_refresh_sensitive" {
		return s.planRefreshSensitive(req)
	}
	if req.TypeName == "tchoritest_secretful" {
		return s.planSecretful(req)
	}
	if req.TypeName == "tchoritest_set_thing" {
		return s.planSetThing(req)
	}
	if req.TypeName == "tchoritest_flat_set_thing" {
		return s.planFlatSetThing(req)
	}
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
	var name string
	if n := attrs["name"]; n.IsKnown() && !n.IsNull() {
		if err := n.As(&name); err != nil {
			return nil, err
		}
	}
	// TC-050 fixture for PlanResourceChange attribution, paired with the
	// existing "invalid" ValidateResourceConfig hook.
	if name == "invalid_plan" {
		return &tfprotov6.PlanResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{{
			Severity: tfprotov6.DiagnosticSeverityError,
			Summary:  "invalid planned name",
			Detail:   `the name "invalid_plan" cannot be planned`,
		}}}, nil
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

func (s *server) planRefreshSensitive(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(refreshSensitiveType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{PlannedState: req.ProposedNewState, PlannedPrivate: req.PriorPrivate}, nil
	}
	prior, err := req.PriorState.Unmarshal(refreshSensitiveType)
	if err != nil {
		return nil, err
	}
	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}
	if prior.IsNull() {
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		var priorAttrs map[string]tftypes.Value
		if err := prior.As(&priorAttrs); err != nil {
			return nil, err
		}
		attrs["id"] = priorAttrs["id"]
		if attrs["name"].IsNull() || !attrs["name"].IsKnown() {
			attrs["name"] = priorAttrs["name"]
		}
		if attrs["note"].IsNull() || !attrs["note"].IsKnown() {
			attrs["note"] = priorAttrs["note"]
		}
		if !priorAttrs["secret"].IsKnown() || priorAttrs["secret"].IsNull() ||
			!priorAttrs["members"].IsKnown() || priorAttrs["members"].IsNull() {
			return &tfprotov6.PlanResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "sensitive refresh state was not recovered",
				Detail:   "the provider did not receive complete persisted sensitive state",
			}}}, nil
		}
		attrs["secret"] = priorAttrs["secret"]
		attrs["members"] = priorAttrs["members"]
	}
	planned, err := tfprotov6.NewDynamicValue(refreshSensitiveType, tftypes.NewValue(refreshSensitiveType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{PlannedState: &planned, PlannedPrivate: req.PriorPrivate}, nil
}

func (s *server) planSecretful(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(secretfulType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{PlannedState: req.ProposedNewState, PlannedPrivate: req.PriorPrivate}, nil
	}
	prior, err := req.PriorState.Unmarshal(secretfulType)
	if err != nil {
		return nil, err
	}
	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}
	if prior.IsNull() {
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		var p map[string]tftypes.Value
		if err := prior.As(&p); err != nil {
			return nil, err
		}
		attrs["id"] = p["id"]
	}
	attrs["client_secret"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	attrs["write_only_secret"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	dv, err := tfprotov6.NewDynamicValue(secretfulType, tftypes.NewValue(secretfulType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{PlannedState: &dv, PlannedPrivate: req.PriorPrivate}, nil
}

func (s *server) applySecretful(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(secretfulType)
	if err != nil {
		return nil, err
	}
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
	attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"secret-"+name)
	attrs["client_secret"] = tftypes.NewValue(tftypes.String, secretSentinel)
	attrs["write_only_secret"] = tftypes.NewValue(tftypes.String, nil)
	dv, err := tfprotov6.NewDynamicValue(secretfulType, tftypes.NewValue(secretfulType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{NewState: &dv, Private: req.PlannedPrivate}, nil
}

func (s *server) applyRefreshSensitive(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(refreshSensitiveType)
	if err != nil {
		return nil, err
	}
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
	if !attrs["id"].IsKnown() {
		attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"refresh-"+name)
	}
	attrs["secret"] = tftypes.NewValue(tftypes.String, "refresh-sensitive-secret")
	attrs["members"] = tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{
		tftypes.NewValue(tftypes.String, "refresh-member-one"),
		tftypes.NewValue(tftypes.String, "refresh-member-two"),
	})
	newState, err := tfprotov6.NewDynamicValue(refreshSensitiveType, tftypes.NewValue(refreshSensitiveType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{NewState: &newState, Private: req.PlannedPrivate}, nil
}

// planNestedThing plans a tchoritest_nested_thing change. "settings" (the
// nested_type attribute — see nestedThingSchema) is never touched: it flows
// through from proposed to planned exactly as composed, whether null
// (omitted from config) or populated, so tests can assert the round trip
// through this fake provider preserves it untouched. Only "id" is computed.
func (s *server) planNestedThing(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(nestedThingType)
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

	prior, err := req.PriorState.Unmarshal(nestedThingType)
	if err != nil {
		return nil, err
	}

	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}

	if prior.IsNull() {
		// Create: id is decided at apply time.
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		var priorAttrs map[string]tftypes.Value
		if err := prior.As(&priorAttrs); err != nil {
			return nil, err
		}
		// Update: id never changes once created.
		attrs["id"] = priorAttrs["id"]
	}

	plannedDV, err := tfprotov6.NewDynamicValue(nestedThingType, tftypes.NewValue(nestedThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{
		PlannedState:   &plannedDV,
		PlannedPrivate: req.PriorPrivate,
	}, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	if path := os.Getenv("TCHORITEST_VERIFY_STATE_LOCK"); path != "" {
		lock := flock.New(path + ".lock")
		defer func() { _ = lock.Close() }()
		locked, err := lock.TryLock()
		if err != nil {
			return nil, err
		}
		if locked {
			return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "concurrent state mutation possible",
				Detail:   "state lock was released during provider apply",
			}}}, nil
		}
	}
	if !knownResourceType(req.TypeName) {
		return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{unknownResourceTypeDiagnostic(req.TypeName)}}, nil
	}
	if req.TypeName == "tchoritest_lossy" {
		return s.applyLossy(req)
	}
	if req.TypeName == "tchoritest_nested_thing" {
		return s.applyNestedThing(req)
	}
	if req.TypeName == "tchoritest_ingress_thing" {
		return s.applyIngressThing(req)
	}
	if req.TypeName == "tchoritest_server_assigned" {
		return s.applyServerAssigned(req)
	}
	if req.TypeName == "tchoritest_secretful" {
		return s.applySecretful(req)
	}
	if req.TypeName == "tchoritest_refresh_sensitive" {
		return s.applyRefreshSensitive(req)
	}
	if req.TypeName == "tchoritest_set_thing" {
		return s.applySetThing(req)
	}
	if req.TypeName == "tchoritest_flat_set_thing" {
		return s.applyFlatSetThing(req)
	}
	planned, err := req.PlannedState.Unmarshal(thingType)
	if err != nil {
		return nil, err
	}
	// Destroy: planned state is null. The explode_destroy fixture returns a
	// provider diagnostic so TC-050 can prove the destroy call-site context;
	// all other resources acknowledge deletion unchanged.
	if planned.IsNull() {
		prior, err := req.PriorState.Unmarshal(thingType)
		if err != nil {
			return nil, err
		}
		if !prior.IsNull() {
			var attrs map[string]tftypes.Value
			if err := prior.As(&attrs); err != nil {
				return nil, err
			}
			var name string
			if err := attrs["name"].As(&name); err != nil {
				return nil, err
			}
			if name == "partial_destroy_null" {
				return &tfprotov6.ApplyResourceChangeResponse{
					NewState: req.PlannedState,
					Private:  []byte("destroy-partial-recovery"),
					Diagnostics: []*tfprotov6.Diagnostic{{
						Severity: tfprotov6.DiagnosticSeverityError,
						Summary:  "destroy partially failed",
						Detail:   "the remote object was deleted before a follow-up operation failed",
					}},
				}, nil
			}
			if name == "partial_destroy_state" {
				attrs["echo"] = tftypes.NewValue(tftypes.String, "destroy-side-effect")
				partial, err := tfprotov6.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
				if err != nil {
					return nil, err
				}
				return &tfprotov6.ApplyResourceChangeResponse{
					NewState: &partial,
					Private:  []byte("destroy-partial-recovery"),
					Diagnostics: []*tfprotov6.Diagnostic{{
						Severity: tfprotov6.DiagnosticSeverityError,
						Summary:  "destroy partially failed",
						Detail:   "the remote object changed before a follow-up operation failed",
					}},
				}, nil
			}
			if name == "explode_destroy" {
				return &tfprotov6.ApplyResourceChangeResponse{
					NewState: req.PriorState,
					Diagnostics: []*tfprotov6.Diagnostic{{
						Severity: tfprotov6.DiagnosticSeverityError,
						Summary:  "destroy exploded",
						Detail:   `the name "explode_destroy" always fails to destroy`,
					}},
				}, nil
			}
		}
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
	// TC-052 / issue #53 reproduction hook: mirror Coolify's unpathed,
	// bodyless API rejection byte-for-byte.
	if name == "api_400" {
		return &tfprotov6.ApplyResourceChangeResponse{
			NewState: req.PriorState,
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "Error updating service",
				Detail:   "api error (status 400): Invalid request",
			}},
		}, nil
	}
	// Deliberate failure hook for apply-time error handling tests (Task 11):
	// a "thing" named "explode" always fails to apply.
	if name == "explode" {
		return &tfprotov6.ApplyResourceChangeResponse{
			// A nil NewState alongside an apply error reads as deletion to the client; keep prior state to disambiguate.
			NewState: req.PriorState,
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
	if name == "partial_failure" {
		return &tfprotov6.ApplyResourceChangeResponse{
			NewState: &newDV,
			Private:  []byte("partial-recovery"),
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "post-create operation failed",
				Detail:   "resource exists, but its follow-up operation failed",
			}},
		}, nil
	}
	return &tfprotov6.ApplyResourceChangeResponse{
		NewState: &newDV,
		Private:  req.PlannedPrivate,
	}, nil
}

// planIngressThing passes ingress through untouched and computes only id.
func (s *server) planIngressThing(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(ingressThingType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{PlannedState: req.ProposedNewState, PlannedPrivate: req.PriorPrivate}, nil
	}
	prior, err := req.PriorState.Unmarshal(ingressThingType)
	if err != nil {
		return nil, err
	}
	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}
	if prior.IsNull() {
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		var priorAttrs map[string]tftypes.Value
		if err := prior.As(&priorAttrs); err != nil {
			return nil, err
		}
		attrs["id"] = priorAttrs["id"]
	}
	planned, err := tfprotov6.NewDynamicValue(ingressThingType, tftypes.NewValue(ingressThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{PlannedState: &planned, PlannedPrivate: req.PriorPrivate}, nil
}

// planServerAssigned plans a tchoritest_server_assigned change the way an
// ordinary provider does: it echoes the proposed new state back, except on
// create, where the three server-assigned attributes become unknown.
//
// It deliberately does NOT repair those attributes from prior state. A client
// that proposes null for them gets null back in the planned state, which is
// what makes this resource a faithful detector for issue #59.
func (s *server) planServerAssigned(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(serverAssignedType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{
			PlannedState:   req.ProposedNewState,
			PlannedPrivate: req.PriorPrivate,
		}, nil
	}
	prior, err := req.PriorState.Unmarshal(serverAssignedType)
	if err != nil {
		return nil, err
	}
	if !prior.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{
			PlannedState:   req.ProposedNewState,
			PlannedPrivate: req.PriorPrivate,
		}, nil
	}
	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}
	for _, n := range []string{"id", "status", "created_at"} {
		attrs[n] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	}
	plannedDV, err := tfprotov6.NewDynamicValue(serverAssignedType, tftypes.NewValue(serverAssignedType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{
		PlannedState:   &plannedDV,
		PlannedPrivate: req.PriorPrivate,
	}, nil
}

// applyServerAssigned mints the three server-assigned attributes when they
// arrive unknown, standing in for the remote API deciding them.
func (s *server) applyServerAssigned(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(serverAssignedType)
	if err != nil {
		return nil, err
	}
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
	if !attrs["id"].IsKnown() {
		attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"id-"+name)
	}
	if !attrs["status"].IsKnown() {
		attrs["status"] = tftypes.NewValue(tftypes.String, "inactive")
	}
	if !attrs["created_at"].IsKnown() {
		attrs["created_at"] = tftypes.NewValue(tftypes.String, "2026-01-01T00:00:00Z")
	}
	newDV, err := tfprotov6.NewDynamicValue(serverAssignedType, tftypes.NewValue(serverAssignedType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{
		NewState: &newDV,
		Private:  req.PlannedPrivate,
	}, nil
}

// applyNestedThing applies a tchoritest_nested_thing change. Like
// planNestedThing, "settings" is never touched — only "id" is minted when
// unknown — so a populated "settings" value must come back out of Apply
// exactly as it went into Plan.
func (s *server) applyNestedThing(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(nestedThingType)
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
	if !attrs["id"].IsKnown() {
		attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"id-"+name)
	}
	newDV, err := tfprotov6.NewDynamicValue(nestedThingType, tftypes.NewValue(nestedThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{
		NewState: &newDV,
		Private:  req.PlannedPrivate,
	}, nil
}

// applyIngressThing passes ingress through untouched and mints only id.
func (s *server) applyIngressThing(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(ingressThingType)
	if err != nil {
		return nil, err
	}
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
	if !attrs["id"].IsKnown() {
		attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"id-"+name)
	}
	newState, err := tfprotov6.NewDynamicValue(ingressThingType, tftypes.NewValue(ingressThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{NewState: &newState, Private: req.PlannedPrivate}, nil
}

// ImportResourceState adopts an existing tchoritest_thing by ID: it derives
// name from the substring after the last "id-" marker, matching
// ApplyResourceChange's id = "<prefix>id-<name>" convention, and returns a
// fully populated state so import -> plan is a clean no-op. IDs without an
// "id-" marker are rejected so the CLI's "resource does not exist" path is
// testable.
func (s *server) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	if req.TypeName == "tchoritest_refresh_sensitive" {
		return s.importRefreshSensitive(req)
	}
	if req.TypeName == "tchoritest_set_thing" {
		return s.importSetThing(req)
	}
	if req.TypeName != "tchoritest_thing" {
		return &tfprotov6.ImportResourceStateResponse{
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "import not supported",
				Detail:   "resource type " + req.TypeName + " does not support import",
			}},
		}, nil
	}
	const marker = "id-"
	idx := strings.LastIndex(req.ID, marker)
	if idx < 0 {
		return &tfprotov6.ImportResourceStateResponse{
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "resource does not exist",
				Detail:   `id "` + req.ID + `" has no "id-" marker`,
			}},
		}, nil
	}
	name := req.ID[idx+len(marker):]
	if name == "" {
		return &tfprotov6.ImportResourceStateResponse{
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
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
	dv, err := tfprotov6.NewDynamicValue(thingType, tftypes.NewValue(thingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ImportResourceStateResponse{
		ImportedResources: []*tfprotov6.ImportedResource{{
			TypeName: req.TypeName,
			State:    &dv,
		}},
	}, nil
}

func (s *server) importRefreshSensitive(req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	const marker = "id-"
	idx := strings.LastIndex(req.ID, marker)
	if idx < 0 || idx+len(marker) == len(req.ID) {
		return &tfprotov6.ImportResourceStateResponse{Diagnostics: []*tfprotov6.Diagnostic{{
			Severity: tfprotov6.DiagnosticSeverityError,
			Summary:  "resource does not exist",
			Detail:   "the refresh-sensitive fixture requires an id- name",
		}}}, nil
	}
	name := req.ID[idx+len(marker):]
	value := tftypes.NewValue(refreshSensitiveType, map[string]tftypes.Value{
		"id":      tftypes.NewValue(tftypes.String, req.ID),
		"name":    tftypes.NewValue(tftypes.String, name),
		"secret":  tftypes.NewValue(tftypes.String, "refresh-sensitive-secret"),
		"members": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, "refresh-member-one"), tftypes.NewValue(tftypes.String, "refresh-member-two")}),
		"note":    tftypes.NewValue(tftypes.String, "imported-note"),
	})
	dynamic, err := tfprotov6.NewDynamicValue(refreshSensitiveType, value)
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ImportResourceStateResponse{ImportedResources: []*tfprotov6.ImportedResource{{
		TypeName: req.TypeName,
		State:    &dynamic,
	}}}, nil
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
	if pidFile := os.Getenv("TCHORITEST_PID_FILE"); pidFile != "" {
		if err := pidfile.Write(pidFile, os.Getpid()); err != nil {
			log.Fatal(err)
		}
	}
	if os.Getenv("TCHORITEST_STALL_STARTUP") != "" {
		time.Sleep(24 * time.Hour)
	}

	err := tf6server.Serve(
		"registry.opentofu.org/tchori-labs/tchoritest",
		func() tfprotov6.ProviderServer { return &server{} },
	)
	if err != nil {
		log.Fatal(err)
	}
}

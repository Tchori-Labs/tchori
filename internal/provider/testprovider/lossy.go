package main

// TC-046 / Tchori-Labs/tchori-internal#47 fixture. The create/update asymmetry is
// deliberate: create mimics a provider POST payload silently dropping authored
// fields, while update mimics the corresponding PATCH honouring them.

import (
	"strings"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

var lossyCredentialsType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"user": tftypes.String, "token": tftypes.String,
}}
var lossyEndpointType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"host": tftypes.String, "api_key": tftypes.String,
}}
var lossyProbeType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{"path": tftypes.String}}

var lossyType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"id":          tftypes.String,
	"name":        tftypes.String,
	"flag":        tftypes.Bool,
	"tags":        tftypes.Map{ElementType: tftypes.String},
	"secret":      tftypes.String,
	"replace_me":  tftypes.String,
	"credentials": lossyCredentialsType,
	"endpoints":   tftypes.List{ElementType: lossyEndpointType},
	"probes":      tftypes.List{ElementType: lossyProbeType},
}}

var lossySchema = &tfprotov6.Schema{Version: 0, Block: &tfprotov6.SchemaBlock{
	Attributes: []*tfprotov6.SchemaAttribute{
		{Name: "id", Type: tftypes.String, Computed: true},
		{Name: "name", Type: tftypes.String, Required: true},
		{Name: "flag", Type: tftypes.Bool, Optional: true},
		{Name: "tags", Type: tftypes.Map{ElementType: tftypes.String}, Optional: true},
		{Name: "secret", Type: tftypes.String, Optional: true, Sensitive: true},
		{Name: "replace_me", Type: tftypes.String, Optional: true},
	},
	BlockTypes: []*tfprotov6.SchemaNestedBlock{
		{TypeName: "credentials", Nesting: tfprotov6.SchemaNestedBlockNestingModeSingle, Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "user", Type: tftypes.String, Optional: true},
			{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true},
		}}},
		{TypeName: "endpoints", Nesting: tfprotov6.SchemaNestedBlockNestingModeList, Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "host", Type: tftypes.String, Optional: true},
			{Name: "api_key", Type: tftypes.String, Optional: true, Sensitive: true},
		}}},
		{TypeName: "probes", Nesting: tfprotov6.SchemaNestedBlockNestingModeList, Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "path", Type: tftypes.String, Optional: true},
		}}},
	},
}}

func (s *server) planLossy(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(lossyType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{PlannedState: req.ProposedNewState, PlannedPrivate: req.PriorPrivate}, nil
	}
	prior, err := req.PriorState.Unmarshal(lossyType)
	if err != nil {
		return nil, err
	}
	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}
	var priorAttrs map[string]tftypes.Value
	if prior.IsNull() {
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		if err := prior.As(&priorAttrs); err != nil {
			return nil, err
		}
		attrs["id"] = priorAttrs["id"]
	}
	var replace []*tftypes.AttributePath
	if !prior.IsNull() && !attrs["replace_me"].Equal(priorAttrs["replace_me"]) {
		replace = append(replace, tftypes.NewAttributePath().WithAttributeName("replace_me"))
	}
	dv, err := tfprotov6.NewDynamicValue(lossyType, tftypes.NewValue(lossyType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{PlannedState: &dv, PlannedPrivate: req.PriorPrivate, RequiresReplace: replace}, nil
}

func (s *server) applyLossy(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(lossyType)
	if err != nil {
		return nil, err
	}
	if planned.IsNull() {
		return &tfprotov6.ApplyResourceChangeResponse{NewState: req.PlannedState}, nil
	}
	prior, err := req.PriorState.Unmarshal(lossyType)
	if err != nil {
		return nil, err
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
		attrs["id"] = tftypes.NewValue(tftypes.String, "lossy-"+name)
	}
	if prior.IsNull() {
		if !attrs["flag"].IsNull() {
			attrs["flag"] = tftypes.NewValue(tftypes.Bool, false)
		}
		if !attrs["tags"].IsNull() && attrs["tags"].IsKnown() {
			var tags map[string]tftypes.Value
			if err := attrs["tags"].As(&tags); err != nil {
				return nil, err
			}
			if _, ok := tags["nullify"]; ok {
				attrs["tags"] = tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
			} else {
				delete(tags, "dropped")
				_, inject := tags["inject"]
				if len(tags) == 0 || inject {
					tags["injected"] = tftypes.NewValue(tftypes.String, "by-provider")
				}
				attrs["tags"] = tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, tags)
			}
		}
		if !attrs["secret"].IsNull() && attrs["secret"].IsKnown() {
			var secret string
			if err := attrs["secret"].As(&secret); err != nil {
				return nil, err
			}
			attrs["secret"] = tftypes.NewValue(tftypes.String, strings.ToUpper(secret))
		}
		if !attrs["credentials"].IsNull() && attrs["credentials"].IsKnown() {
			var credentials map[string]tftypes.Value
			if err := attrs["credentials"].As(&credentials); err != nil {
				return nil, err
			}
			var user string
			if credentials["user"].IsKnown() && !credentials["user"].IsNull() {
				if err := credentials["user"].As(&user); err != nil {
					return nil, err
				}
			}
			if user == "nullify" {
				attrs["credentials"] = tftypes.NewValue(lossyCredentialsType, nil)
			} else {
				credentials["user"] = tftypes.NewValue(tftypes.String, "")
				if credentials["token"].IsKnown() && !credentials["token"].IsNull() {
					var token string
					if err := credentials["token"].As(&token); err != nil {
						return nil, err
					}
					credentials["token"] = tftypes.NewValue(tftypes.String, strings.ToUpper(token))
				}
				attrs["credentials"] = tftypes.NewValue(lossyCredentialsType, credentials)
			}
		}
		for name, ty := range map[string]tftypes.Type{
			"endpoints": tftypes.List{ElementType: lossyEndpointType},
			"probes":    tftypes.List{ElementType: lossyProbeType},
		} {
			if attrs[name].IsKnown() && !attrs[name].IsNull() {
				var elems []tftypes.Value
				if err := attrs[name].As(&elems); err != nil {
					return nil, err
				}
				if len(elems) > 0 {
					elems = elems[:len(elems)-1]
				}
				attrs[name] = tftypes.NewValue(ty, elems)
			}
		}
	}
	dv, err := tfprotov6.NewDynamicValue(lossyType, tftypes.NewValue(lossyType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{NewState: &dv, Private: req.PlannedPrivate}, nil
}

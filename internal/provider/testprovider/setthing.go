package main

import (
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

var setThingDetailType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"kind":   tftypes.String,
	"secret": tftypes.String,
}}

var setThingMemberType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"label":   tftypes.String,
	"token":   tftypes.String,
	"details": tftypes.List{ElementType: setThingDetailType},
}}

var setThingType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"id":                tftypes.String,
	"name":              tftypes.String,
	"attribute_members": tftypes.Set{ElementType: setThingMemberType},
	"block_members":     tftypes.Set{ElementType: setThingMemberType},
}}

var setThingMemberAttributes = []*tfprotov6.SchemaAttribute{
	{Name: "label", Type: tftypes.String, Optional: true},
	{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true},
}

func setThingDetailBlock() *tfprotov6.SchemaNestedBlock {
	return &tfprotov6.SchemaNestedBlock{
		TypeName: "details",
		Nesting:  tfprotov6.SchemaNestedBlockNestingModeList,
		Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
			{Name: "kind", Type: tftypes.String, Optional: true},
			{Name: "secret", Type: tftypes.String, Optional: true, Sensitive: true},
		}},
	}
}

var setThingSchema = &tfprotov6.Schema{Version: 0, Block: &tfprotov6.SchemaBlock{
	Attributes: []*tfprotov6.SchemaAttribute{
		{Name: "id", Type: tftypes.String, Computed: true},
		{Name: "name", Type: tftypes.String, Required: true},
		{
			Name:     "attribute_members",
			Optional: true,
			NestedType: &tfprotov6.SchemaObject{
				Nesting: tfprotov6.SchemaObjectNestingModeSet,
				Attributes: []*tfprotov6.SchemaAttribute{
					{Name: "label", Type: tftypes.String, Optional: true},
					{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true},
					{
						Name:     "details",
						Optional: true,
						NestedType: &tfprotov6.SchemaObject{
							Nesting: tfprotov6.SchemaObjectNestingModeList,
							Attributes: []*tfprotov6.SchemaAttribute{
								{Name: "kind", Type: tftypes.String, Optional: true},
								{Name: "secret", Type: tftypes.String, Optional: true, Sensitive: true},
							},
						},
					},
				},
			},
		},
	},
	BlockTypes: []*tfprotov6.SchemaNestedBlock{{
		TypeName: "block_members",
		Nesting:  tfprotov6.SchemaNestedBlockNestingModeSet,
		Block: &tfprotov6.SchemaBlock{
			Attributes: setThingMemberAttributes,
			BlockTypes: []*tfprotov6.SchemaNestedBlock{setThingDetailBlock()},
		},
	}},
}}

func (s *server) planSetThing(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(setThingType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{PlannedState: req.ProposedNewState, PlannedPrivate: req.PriorPrivate}, nil
	}
	prior, err := req.PriorState.Unmarshal(setThingType)
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
	planned, err := tfprotov6.NewDynamicValue(setThingType, tftypes.NewValue(setThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{PlannedState: &planned, PlannedPrivate: req.PriorPrivate}, nil
}

func (s *server) applySetThing(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(setThingType)
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
	newState, err := tfprotov6.NewDynamicValue(setThingType, tftypes.NewValue(setThingType, attrs))
	if err != nil {
		return nil, err
	}
	if name == "partial-set-failure" {
		return &tfprotov6.ApplyResourceChangeResponse{
			NewState: &newState,
			Diagnostics: []*tfprotov6.Diagnostic{{
				Severity: tfprotov6.DiagnosticSeverityError,
				Summary:  "set create partially failed",
				Detail:   "the object exists and requires a recovery checkpoint",
			}},
		}, nil
	}
	return &tfprotov6.ApplyResourceChangeResponse{NewState: &newState, Private: req.PlannedPrivate}, nil
}

func (s *server) importSetThing(req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	detail := func(secret string) tftypes.Value {
		return tftypes.NewValue(setThingDetailType, map[string]tftypes.Value{
			"kind":   tftypes.NewValue(tftypes.String, "same"),
			"secret": tftypes.NewValue(tftypes.String, secret),
		})
	}
	member := func(token, secret string) tftypes.Value {
		return tftypes.NewValue(setThingMemberType, map[string]tftypes.Value{
			"label": tftypes.NewValue(tftypes.String, "same"),
			"token": tftypes.NewValue(tftypes.String, token),
			"details": tftypes.NewValue(tftypes.List{ElementType: setThingDetailType}, []tftypes.Value{
				detail(secret),
			}),
		})
	}
	members := func(prefix string) tftypes.Value {
		return tftypes.NewValue(tftypes.Set{ElementType: setThingMemberType}, []tftypes.Value{
			member(prefix+"-token-one", prefix+"-detail-one"),
			member(prefix+"-token-two", prefix+"-detail-two"),
		})
	}
	value := tftypes.NewValue(setThingType, map[string]tftypes.Value{
		"id":                tftypes.NewValue(tftypes.String, req.ID),
		"name":              tftypes.NewValue(tftypes.String, "imported"),
		"attribute_members": members("imported-attribute"),
		"block_members":     members("imported-block"),
	})
	dynamic, err := tfprotov6.NewDynamicValue(setThingType, value)
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ImportResourceStateResponse{ImportedResources: []*tfprotov6.ImportedResource{{
		TypeName: req.TypeName,
		State:    &dynamic,
	}}}, nil
}

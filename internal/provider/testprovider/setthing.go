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
	"label":      tftypes.String,
	"token":      tftypes.String,
	"normalized": tftypes.String,
	"details":    tftypes.List{ElementType: setThingDetailType},
}}

var setThingDeclaredMemberType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"label": tftypes.String,
	"token": tftypes.String,
}}

var setThingType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"id":                tftypes.String,
	"name":              tftypes.String,
	"attribute_members": tftypes.Set{ElementType: setThingMemberType},
	"block_members":     tftypes.Set{ElementType: setThingMemberType},
	"declared_members":  tftypes.Set{ElementType: setThingDeclaredMemberType},
}}

var setThingMemberAttributes = []*tfprotov6.SchemaAttribute{
	{Name: "label", Type: tftypes.String, Optional: true},
	{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true},
	{Name: "normalized", Type: tftypes.String, Computed: true},
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
					{Name: "normalized", Type: tftypes.String, Computed: true},
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
		{
			Name:     "declared_members",
			Optional: true,
			NestedType: &tfprotov6.SchemaObject{
				Nesting: tfprotov6.SchemaObjectNestingModeSet,
				Attributes: []*tfprotov6.SchemaAttribute{
					{Name: "label", Type: tftypes.String, Optional: true},
					{Name: "token", Type: tftypes.String, Optional: true},
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

var flatSetMemberType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"label":      tftypes.String,
	"token":      tftypes.String,
	"normalized": tftypes.String,
}}

var flatSetNestedMemberType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"token": tftypes.String,
}}

var flatSetGroupType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"members": tftypes.Set{ElementType: flatSetNestedMemberType},
}}

var flatSetThingType = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"id":      tftypes.String,
	"name":    tftypes.String,
	"members": tftypes.Set{ElementType: flatSetMemberType},
	"groups":  tftypes.Map{ElementType: flatSetGroupType},
}}

// flatSetThingSchema deliberately uses a flat Type with no NestedType. This is
// the representation protocol 5 always produces and protocol 6 also permits.
var flatSetThingSchema = &tfprotov6.Schema{Version: 0, Block: &tfprotov6.SchemaBlock{
	Attributes: []*tfprotov6.SchemaAttribute{
		{Name: "id", Type: tftypes.String, Computed: true},
		{Name: "name", Type: tftypes.String, Required: true},
		{Name: "members", Type: tftypes.Set{ElementType: flatSetMemberType}, Optional: true},
		{Name: "groups", Type: tftypes.Map{ElementType: flatSetGroupType}, Optional: true},
	},
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
	for _, name := range []string{"attribute_members", "block_members"} {
		attrs[name], err = normalizeSetThingMembers(attrs[name])
		if err != nil {
			return nil, err
		}
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
func normalizeSetThingMembers(value tftypes.Value) (tftypes.Value, error) {
	if value.IsNull() || !value.IsKnown() {
		return value, nil
	}
	var members []tftypes.Value
	if err := value.As(&members); err != nil {
		return tftypes.Value{}, err
	}
	for i, member := range members {
		var attrs map[string]tftypes.Value
		if err := member.As(&attrs); err != nil {
			return tftypes.Value{}, err
		}
		normalized := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		if label := attrs["label"]; label.IsKnown() && !label.IsNull() {
			var raw string
			if err := label.As(&raw); err != nil {
				return tftypes.Value{}, err
			}
			normalized = tftypes.NewValue(tftypes.String, "normalized-"+raw)
		}
		attrs["normalized"] = normalized
		members[i] = tftypes.NewValue(setThingMemberType, attrs)
	}
	return tftypes.NewValue(tftypes.Set{ElementType: setThingMemberType}, members), nil
}

func (s *server) planFlatSetThing(req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	proposed, err := req.ProposedNewState.Unmarshal(flatSetThingType)
	if err != nil {
		return nil, err
	}
	if proposed.IsNull() {
		return &tfprotov6.PlanResourceChangeResponse{
			PlannedState: req.ProposedNewState, PlannedPrivate: req.PriorPrivate,
		}, nil
	}
	prior, err := req.PriorState.Unmarshal(flatSetThingType)
	if err != nil {
		return nil, err
	}
	var attrs map[string]tftypes.Value
	if err := proposed.As(&attrs); err != nil {
		return nil, err
	}
	attrs["members"], err = normalizeFlatSetMembers(attrs["members"])
	if err != nil {
		return nil, err
	}
	var replace []*tftypes.AttributePath
	if prior.IsNull() {
		attrs["id"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	} else {
		var priorAttrs map[string]tftypes.Value
		if err := prior.As(&priorAttrs); err != nil {
			return nil, err
		}
		attrs["id"] = priorAttrs["id"]
		replace = append(replace, tftypes.NewAttributePath().WithAttributeName("members"))
	}
	planned, err := tfprotov6.NewDynamicValue(flatSetThingType, tftypes.NewValue(flatSetThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.PlanResourceChangeResponse{
		PlannedState: &planned, PlannedPrivate: req.PriorPrivate, RequiresReplace: replace,
	}, nil
}

func normalizeFlatSetMembers(value tftypes.Value) (tftypes.Value, error) {
	if value.IsNull() || !value.IsKnown() {
		return value, nil
	}
	var members []tftypes.Value
	if err := value.As(&members); err != nil {
		return tftypes.Value{}, err
	}
	for i, member := range members {
		var attrs map[string]tftypes.Value
		if err := member.As(&attrs); err != nil {
			return tftypes.Value{}, err
		}
		normalized := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		if label := attrs["label"]; label.IsKnown() && !label.IsNull() {
			var raw string
			if err := label.As(&raw); err != nil {
				return tftypes.Value{}, err
			}
			normalized = tftypes.NewValue(tftypes.String, "normalized-"+raw)
		}
		attrs["normalized"] = normalized
		members[i] = tftypes.NewValue(flatSetMemberType, attrs)
	}
	return tftypes.NewValue(tftypes.Set{ElementType: flatSetMemberType}, members), nil
}

func (s *server) applyFlatSetThing(req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	planned, err := req.PlannedState.Unmarshal(flatSetThingType)
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
	if members := attrs["members"]; members.IsKnown() && !members.IsNull() {
		var values []tftypes.Value
		if err := members.As(&values); err != nil {
			return nil, err
		}
		for _, member := range values {
			var fields map[string]tftypes.Value
			if err := member.As(&fields); err != nil {
				return nil, err
			}
			if !fields["token"].IsKnown() {
				return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{{
					Severity: tfprotov6.DiagnosticSeverityError,
					Summary:  "unresolved flat set member",
					Detail:   "members.token must be concrete before ApplyResourceChange",
				}}}, nil
			}
		}
	}
	if groups := attrs["groups"]; !groups.IsKnown() {
		return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{{
			Severity: tfprotov6.DiagnosticSeverityError,
			Summary:  "unresolved flat set group",
			Detail:   "groups must be concrete before ApplyResourceChange",
		}}}, nil
	} else if !groups.IsNull() {
		var values map[string]tftypes.Value
		if err := groups.As(&values); err != nil {
			return nil, err
		}
		for _, group := range values {
			var fields map[string]tftypes.Value
			if err := group.As(&fields); err != nil {
				return nil, err
			}
			var members []tftypes.Value
			if err := fields["members"].As(&members); err != nil {
				return nil, err
			}
			for _, member := range members {
				var memberFields map[string]tftypes.Value
				if err := member.As(&memberFields); err != nil {
					return nil, err
				}
				if !memberFields["token"].IsKnown() {
					return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: []*tfprotov6.Diagnostic{{
						Severity: tfprotov6.DiagnosticSeverityError,
						Summary:  "unresolved flat set group",
						Detail:   "groups.members.token must be concrete before ApplyResourceChange",
					}}}, nil
				}
			}
		}
	}
	var name string
	if err := attrs["name"].As(&name); err != nil {
		return nil, err
	}
	if !attrs["id"].IsKnown() {
		attrs["id"] = tftypes.NewValue(tftypes.String, s.prefix+"id-"+name)
	}
	if name == "unknown-group-result" {
		attrs["groups"] = tftypes.NewValue(tftypes.Map{ElementType: flatSetGroupType}, map[string]tftypes.Value{
			"private-key": tftypes.NewValue(flatSetGroupType, map[string]tftypes.Value{
				"members": tftypes.NewValue(tftypes.Set{ElementType: flatSetNestedMemberType}, tftypes.UnknownValue),
			}),
		})
	}
	newState, err := tfprotov6.NewDynamicValue(flatSetThingType, tftypes.NewValue(flatSetThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ApplyResourceChangeResponse{NewState: &newState, Private: req.PlannedPrivate}, nil
}

func (s *server) readFlatSetThing(req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	current, err := req.CurrentState.Unmarshal(flatSetThingType)
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
	if name != "drift-flat-members" {
		return &tfprotov6.ReadResourceResponse{NewState: req.CurrentState, Private: req.Private}, nil
	}
	attrs["members"] = tftypes.NewValue(tftypes.Set{ElementType: flatSetMemberType}, []tftypes.Value{
		tftypes.NewValue(flatSetMemberType, map[string]tftypes.Value{
			"label":      tftypes.NewValue(tftypes.String, "same"),
			"token":      tftypes.NewValue(tftypes.String, "remote-private"),
			"normalized": tftypes.NewValue(tftypes.String, "normalized-same"),
		}),
	})
	newState, err := tfprotov6.NewDynamicValue(flatSetThingType, tftypes.NewValue(flatSetThingType, attrs))
	if err != nil {
		return nil, err
	}
	return &tfprotov6.ReadResourceResponse{NewState: &newState, Private: req.Private}, nil
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
			"label":      tftypes.NewValue(tftypes.String, "same"),
			"normalized": tftypes.NewValue(tftypes.String, "normalized-same"),
			"token":      tftypes.NewValue(tftypes.String, token),
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
		"declared_members":  tftypes.NewValue(tftypes.Set{ElementType: setThingDeclaredMemberType}, nil),
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

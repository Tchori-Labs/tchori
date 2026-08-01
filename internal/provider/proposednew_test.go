package provider

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// tunnelBlock mirrors the shape of cloudflare_zero_trust_tunnel_cloudflared:
// three attributes the operator writes, the rest assigned by the API.
func tunnelBlock() *SchemaBlock {
	return &SchemaBlock{
		Attributes: map[string]*Attr{
			"account_id": {Type: cty.String, Required: true},
			"name":       {Type: cty.String, Required: true},
			"config_src": {Type: cty.String, Optional: true},

			"id":            {Type: cty.String, Computed: true},
			"status":        {Type: cty.String, Computed: true},
			"created_at":    {Type: cty.String, Computed: true},
			"remote_config": {Type: cty.Bool, Computed: true},
			"tunnel_secret": {Type: cty.String, Optional: true, Computed: true},
		},
		Blocks: map[string]*NestedBlock{},
	}
}

func TestProposedNewCarriesComputedAttributesForward(t *testing.T) {
	block := tunnelBlock()
	ty := block.ImpliedType()

	prior := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("5c114a37"),
		"name":          cty.StringVal("waha-botlab"),
		"config_src":    cty.StringVal("cloudflare"),
		"id":            cty.StringVal("93903ed0-abb1-40b4-911a-695429cc6395"),
		"status":        cty.StringVal("inactive"),
		"created_at":    cty.StringVal("2026-07-30T16:07:53Z"),
		"remote_config": cty.True,
		"tunnel_secret": cty.NullVal(cty.String),
	})

	// What Compose produces from the config: the three declared attributes,
	// null everywhere else.
	config := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("5c114a37"),
		"name":          cty.StringVal("waha-botlab"),
		"config_src":    cty.StringVal("cloudflare"),
		"id":            cty.NullVal(cty.String),
		"status":        cty.NullVal(cty.String),
		"created_at":    cty.NullVal(cty.String),
		"remote_config": cty.NullVal(cty.Bool),
		"tunnel_secret": cty.NullVal(cty.String),
	})

	got := ProposedNew(block, prior, config)

	if !got.Type().Equals(ty) {
		t.Fatalf("proposed type = %s, want %s", got.Type().FriendlyName(), ty.FriendlyName())
	}

	// The whole point of #59: an unchanged config must propose the object it
	// already is, not an object with every server-assigned field cleared.
	if !got.RawEquals(prior) {
		t.Errorf("proposed new state differs from prior for an unchanged config\n got: %#v\nwant: %#v", got, prior)
	}

	for _, name := range []string{"id", "status", "created_at", "remote_config"} {
		if got.GetAttr(name).IsNull() {
			t.Errorf("computed attribute %q proposed as null; the provider would read that as a request to clear it", name)
		}
	}
}

func TestProposedNewPrefersConfigOverPrior(t *testing.T) {
	block := tunnelBlock()

	prior := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("5c114a37"),
		"name":          cty.StringVal("old-name"),
		"config_src":    cty.StringVal("local"),
		"id":            cty.StringVal("93903ed0"),
		"status":        cty.StringVal("inactive"),
		"created_at":    cty.StringVal("2026-07-30T16:07:53Z"),
		"remote_config": cty.False,
		"tunnel_secret": cty.NullVal(cty.String),
	})
	config := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("5c114a37"),
		"name":          cty.StringVal("new-name"),
		"config_src":    cty.StringVal("cloudflare"),
		"id":            cty.NullVal(cty.String),
		"status":        cty.NullVal(cty.String),
		"created_at":    cty.NullVal(cty.String),
		"remote_config": cty.NullVal(cty.Bool),
		"tunnel_secret": cty.NullVal(cty.String),
	})

	got := ProposedNew(block, prior, config)

	if v := got.GetAttr("name").AsString(); v != "new-name" {
		t.Errorf("name = %q, want the config value %q", v, "new-name")
	}
	if v := got.GetAttr("config_src").AsString(); v != "cloudflare" {
		t.Errorf("config_src = %q, want the config value %q", v, "cloudflare")
	}
	if v := got.GetAttr("id").AsString(); v != "93903ed0" {
		t.Errorf("id = %q, want the prior value carried forward", v)
	}
}

// An Optional+Computed attribute the operator DID set must take the config
// value — otherwise the operator could never change it once set.
func TestProposedNewOptionalComputedSetInConfigWins(t *testing.T) {
	block := tunnelBlock()

	prior := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("acct"),
		"name":          cty.StringVal("t"),
		"config_src":    cty.NullVal(cty.String),
		"id":            cty.StringVal("abc"),
		"status":        cty.StringVal("inactive"),
		"created_at":    cty.StringVal("t0"),
		"remote_config": cty.True,
		"tunnel_secret": cty.StringVal("prior-secret"),
	})
	config := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("acct"),
		"name":          cty.StringVal("t"),
		"config_src":    cty.NullVal(cty.String),
		"id":            cty.NullVal(cty.String),
		"status":        cty.NullVal(cty.String),
		"created_at":    cty.NullVal(cty.String),
		"remote_config": cty.NullVal(cty.Bool),
		"tunnel_secret": cty.StringVal("new-secret"),
	})

	got := ProposedNew(block, prior, config)

	if v := got.GetAttr("tunnel_secret").AsString(); v != "new-secret" {
		t.Errorf("tunnel_secret = %q, want the config value %q", v, "new-secret")
	}
}

// On create there is no prior, so computed attributes stay null and the
// provider is the one that marks them unknown.
func TestProposedNewOnCreateLeavesComputedNull(t *testing.T) {
	block := tunnelBlock()
	ty := block.ImpliedType()

	config := cty.ObjectVal(map[string]cty.Value{
		"account_id":    cty.StringVal("acct"),
		"name":          cty.StringVal("t"),
		"config_src":    cty.StringVal("cloudflare"),
		"id":            cty.NullVal(cty.String),
		"status":        cty.NullVal(cty.String),
		"created_at":    cty.NullVal(cty.String),
		"remote_config": cty.NullVal(cty.Bool),
		"tunnel_secret": cty.NullVal(cty.String),
	})

	got := ProposedNew(block, cty.NullVal(ty), config)

	if !got.RawEquals(config) {
		t.Errorf("on create the proposal must equal the config\n got: %#v\nwant: %#v", got, config)
	}
}

func TestProposedNewNullConfigIsPassedThrough(t *testing.T) {
	block := tunnelBlock()
	ty := block.ImpliedType()

	got := ProposedNew(block, cty.NullVal(ty), cty.NullVal(ty))
	if !got.IsNull() {
		t.Errorf("a null config must propose null, got %#v", got)
	}
}

func TestProposedNewRecursesIntoSingleAndListBlocks(t *testing.T) {
	inner := &SchemaBlock{
		Attributes: map[string]*Attr{
			"name":     {Type: cty.String, Required: true},
			"assigned": {Type: cty.String, Computed: true},
		},
		Blocks: map[string]*NestedBlock{},
	}
	block := &SchemaBlock{
		Attributes: map[string]*Attr{
			"id": {Type: cty.String, Computed: true},
		},
		Blocks: map[string]*NestedBlock{
			"meta":   {Nesting: "single", Block: inner},
			"policy": {Nesting: "list", Block: inner},
		},
	}

	elem := func(name, assigned cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"name": name, "assigned": assigned})
	}

	prior := cty.ObjectVal(map[string]cty.Value{
		"id":     cty.StringVal("root-id"),
		"meta":   elem(cty.StringVal("m"), cty.StringVal("meta-assigned")),
		"policy": cty.ListVal([]cty.Value{elem(cty.StringVal("p0"), cty.StringVal("p0-assigned"))}),
	})
	config := cty.ObjectVal(map[string]cty.Value{
		"id":     cty.NullVal(cty.String),
		"meta":   elem(cty.StringVal("m"), cty.NullVal(cty.String)),
		"policy": cty.ListVal([]cty.Value{elem(cty.StringVal("p0"), cty.NullVal(cty.String))}),
	})

	got := ProposedNew(block, prior, config)

	if v := got.GetAttr("meta").GetAttr("assigned"); v.IsNull() || v.AsString() != "meta-assigned" {
		t.Errorf("single block: assigned = %#v, want the prior value carried forward", v)
	}
	p0 := got.GetAttr("policy").Index(cty.NumberIntVal(0))
	if v := p0.GetAttr("assigned"); v.IsNull() || v.AsString() != "p0-assigned" {
		t.Errorf("list block: assigned = %#v, want the prior value carried forward", v)
	}
	if v := got.GetAttr("id"); v.IsNull() || v.AsString() != "root-id" {
		t.Errorf("id = %#v, want the prior value carried forward", v)
	}
}

// A list that grew has no prior element to pair with the new entry; the new
// one must still compose, with its computed attributes left null.
func TestProposedNewListGrewBeyondPrior(t *testing.T) {
	inner := &SchemaBlock{
		Attributes: map[string]*Attr{
			"name":     {Type: cty.String, Required: true},
			"assigned": {Type: cty.String, Computed: true},
		},
		Blocks: map[string]*NestedBlock{},
	}
	block := &SchemaBlock{
		Attributes: map[string]*Attr{},
		Blocks:     map[string]*NestedBlock{"policy": {Nesting: "list", Block: inner}},
	}
	elem := func(name, assigned cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"name": name, "assigned": assigned})
	}

	prior := cty.ObjectVal(map[string]cty.Value{
		"policy": cty.ListVal([]cty.Value{elem(cty.StringVal("p0"), cty.StringVal("p0-assigned"))}),
	})
	config := cty.ObjectVal(map[string]cty.Value{
		"policy": cty.ListVal([]cty.Value{
			elem(cty.StringVal("p0"), cty.NullVal(cty.String)),
			elem(cty.StringVal("p1"), cty.NullVal(cty.String)),
		}),
	})

	got := ProposedNew(block, prior, config)
	list := got.GetAttr("policy")

	if list.LengthInt() != 2 {
		t.Fatalf("policy length = %d, want 2", list.LengthInt())
	}
	if v := list.Index(cty.NumberIntVal(0)).GetAttr("assigned"); v.IsNull() || v.AsString() != "p0-assigned" {
		t.Errorf("policy[0].assigned = %#v, want the prior value carried forward", v)
	}
	if v := list.Index(cty.NumberIntVal(1)).GetAttr("assigned"); !v.IsNull() {
		t.Errorf("policy[1].assigned = %#v, want null — there is no prior element to carry", v)
	}
}

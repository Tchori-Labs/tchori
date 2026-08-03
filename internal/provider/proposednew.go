package provider

import (
	"github.com/zclconf/go-cty/cty"
)

// ProposedNew builds the proposed new state for one resource from its prior
// state and its config value, following the same rule Terraform applies in
// objchange.ProposedNewObject.
//
// The distinction this exists to preserve: a provider's PlanResourceChange
// receives BOTH a Config and a ProposedNewState, and they mean different
// things. Config is what the author wrote — an attribute absent there is
// null, and for an Optional attribute that null means "unset me". Proposed
// new state is Terraform's guess at the post-apply object, which carries
// forward values the provider itself computed on a previous run.
//
// Passing the config value as both arguments erases that distinction, and the
// consequence is not cosmetic: every Computed attribute absent from config
// arrives as null, so the provider sees the operator asking to clear
// server-assigned fields on every single plan. The resource can then never
// converge — it plans as an update forever, and providers that turn a diff
// into a PATCH send a body full of nulls (Tchori-Labs/tchori#59, where the
// resulting PATCH against a Cloudflare tunnel returns 404).
//
// Note also that plan-modifier helpers providers rely on, such as the
// framework's UseStateForUnknown, only fire for UNKNOWN values. A null is not
// unknown, so those modifiers never run and cannot paper over the mistake.
//
// The rule, per attribute: if the attribute is Computed and config left it
// null, take the prior value; otherwise take the config value. On create the
// prior is null, so computed attributes propose null and the provider fills
// them with unknowns — which is exactly what Terraform does.
func ProposedNew(block *SchemaBlock, prior, config cty.Value) cty.Value {
	if block == nil || config.IsNull() || !config.IsKnown() {
		return config
	}
	if !prior.IsKnown() {
		return config
	}

	attrs := make(map[string]cty.Value, len(block.Attributes)+len(block.Blocks))

	for name, attr := range block.Attributes {
		configV := config.GetAttr(name)
		// Attr.Type can carry conversion-only optional markers. A marked null
		// mixed with a concrete sibling makes cty collection construction panic
		// (issue #50), so proposed values always use the marker-free type.
		priorV := cty.NullVal(attr.Type.WithoutOptionalAttributesDeep())
		if !prior.IsNull() {
			priorV = prior.GetAttr(name)
		}
		// Optional+Computed behaves the same as Computed here: the operator
		// may set it, but when they don't, the value the provider chose last
		// time is the better guess than "clear it".
		if attr.Computed && configV.IsNull() {
			attrs[name] = priorV
			continue
		}
		attrs[name] = configV
	}

	for name, nb := range block.Blocks {
		configV := config.GetAttr(name)
		priorV := cty.NullVal(configV.Type())
		if !prior.IsNull() {
			priorV = prior.GetAttr(name)
		}
		attrs[name] = proposedNewBlock(nb, priorV, configV)
	}

	return cty.ObjectVal(attrs)
}

// proposedNewBlock recurses into one nested block, correlating prior and
// config elements so that computed attributes inside the block are carried
// forward the same way top-level ones are.
func proposedNewBlock(nb *NestedBlock, prior, config cty.Value) cty.Value {
	if config.IsNull() || !config.IsKnown() {
		return config
	}

	switch nb.Nesting {
	case "single":
		return ProposedNew(nb.Block, prior, config)

	case "list":
		if config.LengthInt() == 0 {
			return config
		}
		elems := make([]cty.Value, 0, config.LengthInt())
		for it := config.ElementIterator(); it.Next(); {
			idx, cv := it.Element()
			pv := cty.NullVal(cv.Type())
			// Correlate by index: the only correlation a list offers. An
			// insertion in the middle shifts the pairing, which is why
			// Terraform treats this as a guess the provider may override
			// rather than as truth.
			if !prior.IsNull() && prior.IsKnown() && prior.HasIndex(idx).True() {
				pv = prior.Index(idx)
			}
			elems = append(elems, ProposedNew(nb.Block, pv, cv))
		}
		return cty.ListVal(elems)

	case "map":
		if config.LengthInt() == 0 {
			return config
		}
		elems := make(map[string]cty.Value, config.LengthInt())
		for it := config.ElementIterator(); it.Next(); {
			k, cv := it.Element()
			pv := cty.NullVal(cv.Type())
			if !prior.IsNull() && prior.IsKnown() && prior.HasIndex(k).True() {
				pv = prior.Index(k)
			}
			elems[k.AsString()] = ProposedNew(nb.Block, pv, cv)
		}
		return cty.MapVal(elems)

	case "set":
		// A set has no stable element identity, so prior and config elements
		// cannot be correlated: any pairing would be arbitrary and could
		// carry one element's computed values onto a different element.
		// Terraform takes config unchanged here and so do we.
		return config
	}

	return config
}

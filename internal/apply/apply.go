// Package apply executes a saved plan against live providers: creates,
// updates and replaces run in dependency order (the config's topological
// sort, cfg.Order() — NOT pl.Changes' document order, which is merely
// sorted alphabetically by address for plan.json determinism), deletes run
// last in reverse dependency order. The state file is re-saved after every
// successful provider call so a mid-sequence failure never loses the
// resources already applied (partial-state safety).
package apply

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/state"
)

// Apply verifies pl.StateSerial == st.Serial (else stale-plan error diag),
// verifies every non-delete plan change still has a matching address in cfg
// (else "plan does not match configuration" error diag — see the
// configuration-drift guard below), then executes changes: creates/updates/
// replaces in dependency order, deletes last in reverse dependency order.
// After EVERY successful ApplyResource the state is updated in memory AND
// saved to statePath (partial-state safety). First error aborts remaining
// changes but keeps completed ones saved.
//
// Ordering note: pl.Changes is sorted alphabetically by address (see
// plan.finalize) purely so plan.json is byte-for-byte deterministic — that
// order carries no dependency information and must never drive execution.
// A dependent whose address sorts before its dependency (e.g.
// tchoritest_thing.a_first referencing ${tchoritest_thing.z_second.id})
// would otherwise be applied before the resource it depends on exists.
// Execution order is instead derived from cfg.Order(), the same
// topological sort the planner itself uses.
//
// Ordering note 2: state.Save increments Serial on every call, so staleness
// MUST be (and is) validated before the first save — the plan's
// state_serial is compared against the serial captured at plan time, which
// only matches the pre-apply state.
func Apply(ctx context.Context, pl *plan.Plan, cfg *config.Config, providers map[string]*provider.Client, schemas map[string]*provider.ProviderSchemas, st *state.State, statePath string) diag.Diagnostics {
	if pl.StateSerial != st.Serial {
		return diag.Diagnostics{diag.Errorf("", "stale plan", fmt.Sprintf(
			"plan was created against state serial %d but the current state serial is %d; run plan again",
			pl.StateSerial, st.Serial))}
	}

	ex := &executor{cfg: cfg, providers: providers, schemas: schemas, st: st, statePath: statePath}

	byAddr := make(map[string]*plan.Change, len(pl.Changes))
	for _, ch := range pl.Changes {
		byAddr[ch.Address] = ch
	}

	// order is the config's topological sort (dependencies before
	// dependents). A nil cfg (defensively supported by applyChange below)
	// yields an empty order, same as an empty config.
	var order []string
	if cfg != nil {
		var ods diag.Diagnostics
		order, ods = cfg.Order()
		if ods.HasErrors() {
			// Should be unreachable in practice: a config that already
			// produced this plan necessarily passed Order() during planning
			// too (see plan.Planner.Plan). Surfacing rather than assuming
			// keeps this defensive.
			return ods
		}
	}
	inConfigOrder := make(map[string]bool, len(order))
	for _, addr := range order {
		inConfigOrder[addr] = true
	}

	// Creates, updates and replaces first, in cfg.Order() sequence.
	// Addresses in order with no corresponding plan change (no-op, or the
	// plan simply predates them) are skipped.
	var ordered []*plan.Change
	for _, addr := range order {
		ch := byAddr[addr]
		if ch == nil {
			continue
		}
		switch ch.Action {
		case "create", "update", "replace":
			ordered = append(ordered, ch)
		}
	}

	// … then deletes, last, so nothing is destroyed while a resource that
	// still references it is being created or updated. Addresses present in
	// cfg.Order() (e.g. `tchori destroy` on a config that still declares the
	// resource) run in reverse topological order — dependents destroyed
	// before their dependencies. Addresses absent from config (resources
	// removed from the config file) carry no dependency information —
	// Order() only walks config resources — so they run in reverse-lexical
	// order, after the config-known deletes. No-op changes are skipped
	// entirely.
	for i := len(order) - 1; i >= 0; i-- {
		if ch := byAddr[order[i]]; ch != nil && ch.Action == "delete" {
			ordered = append(ordered, ch)
		}
	}
	var stateOnlyDeletes []*plan.Change
	for _, ch := range pl.Changes {
		if ch.Action == "delete" && !inConfigOrder[ch.Address] {
			stateOnlyDeletes = append(stateOnlyDeletes, ch)
		}
	}
	sort.Slice(stateOnlyDeletes, func(i, j int) bool {
		return stateOnlyDeletes[i].Address > stateOnlyDeletes[j].Address
	})
	ordered = append(ordered, stateOnlyDeletes...)

	// Guard against configuration drift since the plan was created. The
	// create/update/replace leg above is built by walking cfg.Order() (i.e.
	// cfg.Resources' addresses) and looking up a matching plan change — see
	// the "Ordering note" comments — so a non-delete change whose address is
	// no longer in cfg.Resources (the config file was edited to remove the
	// resource after the plan was saved) never appears in `order` and would
	// otherwise silently vanish from `ordered`, applying nothing with zero
	// diagnostics. Refuse the whole apply instead, before any provider call
	// or state save, mirroring the stale-plan check's all-or-nothing
	// posture: a plan that no longer matches configuration is not
	// partially actionable.
	var driftDiags diag.Diagnostics
	for _, ch := range pl.Changes {
		if ch.Action == "delete" {
			continue
		}
		if cfg == nil || cfg.Resources[ch.Address] == nil {
			driftDiags = append(driftDiags, diag.Errorf(ch.Address, "plan does not match configuration",
				fmt.Sprintf(
					"plan has a %q change for %q, but the address is no longer present in the loaded configuration; the configuration changed since the plan was created — run plan again",
					ch.Action, ch.Address)))
		}
	}
	if driftDiags.HasErrors() {
		return driftDiags
	}

	var ds diag.Diagnostics
	for _, ch := range ordered {
		step := ex.applyChange(ctx, ch)
		ds = append(ds, step...)
		if step.HasErrors() {
			// Abort remaining changes. Everything already applied has been
			// saved to statePath change by change, so completed work is kept.
			return ds
		}
	}
	return ds
}

// executor carries the shared apply context so the per-change helpers do
// not need seven-parameter signatures.
type executor struct {
	cfg       *config.Config
	providers map[string]*provider.Client
	schemas   map[string]*provider.ProviderSchemas
	st        *state.State
	statePath string
}

// applyChange executes one plan change and persists its result.
func (ex *executor) applyChange(ctx context.Context, ch *plan.Change) diag.Diagnostics {
	addr := ch.Address

	// Type and provider come from config when the resource is present; a
	// delete of a resource that was removed from config falls back to the
	// state entry (which records both).
	var typeName, providerName string
	switch {
	case ex.cfg != nil && ex.cfg.Resources[addr] != nil:
		typeName = ex.cfg.Resources[addr].Type
		providerName = ex.cfg.Resources[addr].Provider
	case ex.st.Resources[addr] != nil:
		typeName = ex.st.Resources[addr].Type
		providerName = ex.st.Resources[addr].Provider
	default:
		return diag.Diagnostics{diag.Errorf(addr, "unknown resource",
			"address appears in the plan but in neither configuration nor state")}
	}

	client := ex.providers[providerName]
	if client == nil {
		return diag.Diagnostics{diag.Errorf(addr, "provider not running",
			fmt.Sprintf("no launched provider client for %q", providerName))}
	}
	ps := ex.schemas[providerName]
	if ps == nil {
		return diag.Diagnostics{diag.Errorf(addr, "missing resource schema",
			fmt.Sprintf("provider %q has no schema for resource type %q", providerName, typeName))}
	}
	schema, unsupported, known := ps.LookupResourceType(typeName)
	if !known {
		return diag.Diagnostics{diag.Errorf(addr, "missing resource schema",
			fmt.Sprintf("provider %q has no schema for resource type %q", providerName, typeName))}
	}
	if schema == nil {
		return diag.Diagnostics{diag.Errorf(addr,
			fmt.Sprintf("unsupported schema for resource type %q", typeName), unsupported)}
	}
	ty := schema.Block.ImpliedType()

	// Prior value and private bytes come from state (null/nil if absent) —
	// except for "create", where the plan document is trusted over state
	// instead. A "create" action means the planner determined there is no
	// live prior object (classify: prior.IsNull() => "create"); state.json
	// can still hold a stale entry for addr if that determination happened
	// during refresh in a separate `plan` invocation, since refresh mutates
	// only the planner's in-memory state.State and plan never re-saves it
	// (see plan.Planner.Plan's out-of-band-deletion branch). Decoding that
	// stale entry here and handing the provider a non-null prior would make
	// it treat the apply as an Update against an object that no longer
	// exists — nothing to converge toward, so the apply can never succeed.
	// Delete/replace/update all still decode from state: they need the real
	// recorded object to destroy or diff against. See
	// TestApplyCreateIgnoresStalePriorState.
	prior := cty.NullVal(ty)
	var priorPrivate []byte
	if ch.Action != "create" {
		if rs := ex.st.Resources[addr]; rs != nil {
			v, err := ctyjson.Unmarshal(rs.Attributes, ty)
			if err != nil {
				return diag.Diagnostics{diag.Errorf(addr, "corrupt state attributes", err.Error())}
			}
			prior = v
			priorPrivate = rs.Private
		}
	}

	switch ch.Action {
	case "delete":
		return ex.destroy(ctx, client, typeName, addr, ty, prior, priorPrivate)
	case "replace":
		// Destroy-then-create: two explicit ApplyResource calls. The state
		// entry is removed (and saved) after the destroy leg, then written
		// back (and saved) after the create leg.
		if ds := ex.destroy(ctx, client, typeName, addr, ty, prior, priorPrivate); ds.HasErrors() {
			return ds
		}
		return ex.createOrUpdate(ctx, client, typeName, providerName, addr, ty, cty.NullVal(ty), ch)
	default: // "create", "update"
		return ex.createOrUpdate(ctx, client, typeName, providerName, addr, ty, prior, ch)
	}
}

// destroy applies a null planned value — the plugin-protocol convention for
// "destroy this object" — then removes the resource from state and saves.
func (ex *executor) destroy(ctx context.Context, client *provider.Client, typeName, addr string, ty cty.Type, prior cty.Value, priorPrivate []byte) diag.Diagnostics {
	newState, _, ds := client.ApplyResource(ctx, typeName, prior, cty.NullVal(ty), cty.NullVal(ty), priorPrivate)
	if ds.HasErrors() {
		return ds
	}
	if !newState.IsNull() {
		return append(ds, diag.Errorf(addr, "provider did not destroy resource",
			"ApplyResource returned a non-null state for a null planned value"))
	}
	delete(ex.st.Resources, addr)
	if err := ex.st.Save(ex.statePath); err != nil {
		return append(ds, diag.Errorf(addr, "saving state", err.Error()))
	}
	return ds
}

// createOrUpdate decodes the planned value captured at plan time (msgpack,
// may contain unknowns) at the schema's implied type, composes the resource
// config with references resolved against current state, applies, then
// records the provider's returned state and saves.
func (ex *executor) createOrUpdate(ctx context.Context, client *provider.Client, typeName, providerName, addr string, ty cty.Type, prior cty.Value, ch *plan.Change) diag.Diagnostics {
	var res *config.Resource
	if ex.cfg != nil {
		res = ex.cfg.Resources[addr]
	}
	if res == nil {
		return diag.Diagnostics{diag.Errorf(addr, "resource missing from configuration",
			fmt.Sprintf("%s change for %q but the address is not in the loaded configuration", ch.Action, addr))}
	}

	planned, err := ctymsgpack.Unmarshal(ch.PlannedRaw, ty)
	if err != nil {
		return diag.Diagnostics{diag.Errorf(addr, "corrupt planned value", err.Error())}
	}

	cfgVal, ds := provider.Compose(res.Config, ty, false, ex.resolveRef)
	if ds.HasErrors() {
		return ds
	}

	// ch.PlannedRaw was captured at plan time: an attribute whose raw config
	// is a ${..} reference to a resource that had not yet applied within
	// that same plan (e.g. two new resources created together, one
	// referencing the other's computed id) is still unknown there — the
	// referenced resource's real value did not exist yet. By now,
	// dependency-ordered execution (see Apply) guarantees any resource
	// addr's config references has already applied, so cfgVal — recomposed
	// above against current state — holds the concrete value. Overlay it
	// onto planned's unknown leaves before applying; leaves the provider's
	// own computed attributes (absent from raw config, hence null in
	// cfgVal, e.g. "id") unknown for the provider itself to resolve, same
	// as before.
	planned = resolvePlannedUnknowns(planned, cfgVal)

	newState, newPrivate, applyDs := client.ApplyResource(ctx, typeName, prior, planned, cfgVal, ch.Private)
	ds = append(ds, applyDs...)
	if ds.HasErrors() {
		return ds
	}

	attrs, err := ctyjson.Marshal(newState, ty)
	if err != nil {
		return append(ds, diag.Errorf(addr, "encoding new state", err.Error()))
	}
	ex.st.Resources[addr] = &state.ResourceState{
		Type:       typeName,
		Provider:   providerName,
		Attributes: attrs,
		Private:    newPrivate,
	}
	if err := ex.st.Save(ex.statePath); err != nil {
		return append(ds, diag.Errorf(addr, "saving state", err.Error()))
	}
	return ds
}

// resolvePlannedUnknowns walks planned and cfgVal together (both at the same
// schema type) and replaces any unknown leaf in planned with the
// corresponding value from cfgVal, provided cfgVal actually has a concrete
// (known, non-null) value there. Composite values (objects, maps) recurse
// per-element; lists, sets and primitives are returned unchanged since
// nothing in this MVP nests a forward reference inside an ordered
// collection and per-index merging would be unsound without a stronger
// correspondence guarantee.
func resolvePlannedUnknowns(planned, cfgVal cty.Value) cty.Value {
	if !planned.IsKnown() {
		if cfgVal.IsKnown() && !cfgVal.IsNull() {
			return cfgVal
		}
		return planned
	}
	if planned.IsNull() {
		return planned
	}

	ty := planned.Type()
	switch {
	case ty.IsObjectType():
		atys := ty.AttributeTypes()
		attrs := make(map[string]cty.Value, len(atys))
		for name, aty := range atys {
			sub := cty.NullVal(aty)
			if cfgVal.IsKnown() && !cfgVal.IsNull() {
				sub = cfgVal.GetAttr(name)
			}
			attrs[name] = resolvePlannedUnknowns(planned.GetAttr(name), sub)
		}
		return cty.ObjectVal(attrs)

	case ty.IsMapType():
		if planned.LengthInt() == 0 {
			return planned
		}
		cfgElems := map[string]cty.Value{}
		if cfgVal.IsKnown() && !cfgVal.IsNull() {
			cfgElems = cfgVal.AsValueMap()
		}
		elems := make(map[string]cty.Value, planned.LengthInt())
		for it := planned.ElementIterator(); it.Next(); {
			k, v := it.Element()
			key := k.AsString()
			sub, ok := cfgElems[key]
			if !ok {
				sub = cty.NullVal(ty.ElementType())
			}
			elems[key] = resolvePlannedUnknowns(v, sub)
		}
		return cty.MapVal(elems)

	default:
		return planned
	}
}

// resolveRef resolves a ${type.name.attr} reference against the current
// in-memory state. Apply executes creates/updates/replaces in cfg.Order()'s
// topological order (not pl.Changes' address-sorted document order — see
// Apply), so by the time a dependent applies, the resource it references
// has already applied and its post-apply value already recorded in state
// (and saved).
func (ex *executor) resolveRef(ref config.Ref) (cty.Value, diag.Diagnostics) {
	rs := ex.st.Resources[ref.Address]
	if rs == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "reference to missing resource",
			fmt.Sprintf("cannot resolve ${%s.%s}: resource has no state", ref.Address, ref.Attr))}
	}
	ps := ex.schemas[rs.Provider]
	if ps == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "missing resource schema",
			fmt.Sprintf("provider %q has no schema for resource type %q", rs.Provider, rs.Type))}
	}
	schema, unsupported, known := ps.LookupResourceType(rs.Type)
	if !known {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "missing resource schema",
			fmt.Sprintf("provider %q has no schema for resource type %q", rs.Provider, rs.Type))}
	}
	if schema == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address,
			fmt.Sprintf("unsupported schema for resource type %q", rs.Type), unsupported)}
	}
	v, err := ctyjson.Unmarshal(rs.Attributes, schema.Block.ImpliedType())
	if err != nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "corrupt state attributes", err.Error())}
	}

	for _, seg := range strings.Split(ref.Attr, ".") {
		if v.IsNull() {
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "reference through null value",
				fmt.Sprintf("cannot resolve %q in ${%s.%s}: intermediate value is null", seg, ref.Address, ref.Attr))}
		}
		vty := v.Type()
		switch {
		case vty.IsObjectType() && vty.HasAttribute(seg):
			v = v.GetAttr(seg)
		case vty.IsMapType() && v.HasIndex(cty.StringVal(seg)).True():
			v = v.Index(cty.StringVal(seg))
		default:
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "unresolvable reference",
				fmt.Sprintf("cannot resolve %q in ${%s.%s}", seg, ref.Address, ref.Attr))}
		}
	}
	return v, nil
}

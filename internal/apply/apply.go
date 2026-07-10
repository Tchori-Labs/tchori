// Package apply executes a saved plan against live providers: creates,
// updates and replaces run in plan order, deletes run last in reverse plan
// order. The state file is re-saved after every successful provider call so
// a mid-sequence failure never loses the resources already applied
// (partial-state safety).
package apply

import (
	"context"
	"fmt"
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
// then executes changes: creates/updates/replaces in plan order, deletes
// last in reverse order. After EVERY successful ApplyResource the state is
// updated in memory AND saved to statePath (partial-state safety). First
// error aborts remaining changes but keeps completed ones saved.
//
// Ordering note: state.Save increments Serial on every call, so staleness
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

	// Creates, updates and replaces first, in plan order …
	var ordered []*plan.Change
	for _, ch := range pl.Changes {
		switch ch.Action {
		case "create", "update", "replace":
			ordered = append(ordered, ch)
		}
	}
	// … then deletes, last, in reverse plan order (dependents first). No-op
	// changes are skipped entirely.
	for i := len(pl.Changes) - 1; i >= 0; i-- {
		if pl.Changes[i].Action == "delete" {
			ordered = append(ordered, pl.Changes[i])
		}
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
	if ps == nil || ps.ResourceTypes[typeName] == nil {
		return diag.Diagnostics{diag.Errorf(addr, "missing resource schema",
			fmt.Sprintf("provider %q has no schema for resource type %q", providerName, typeName))}
	}
	ty := ps.ResourceTypes[typeName].Block.ImpliedType()

	// Prior value and private bytes come from state (null/nil if absent).
	prior := cty.NullVal(ty)
	var priorPrivate []byte
	if rs := ex.st.Resources[addr]; rs != nil {
		v, err := ctyjson.Unmarshal(rs.Attributes, ty)
		if err != nil {
			return diag.Diagnostics{diag.Errorf(addr, "corrupt state attributes", err.Error())}
		}
		prior = v
		priorPrivate = rs.Private
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

// resolveRef resolves a ${type.name.attr} reference against the current
// in-memory state. Plan order puts dependencies before their dependents, so
// by the time a dependent applies, the referenced resource's post-apply
// value has already been recorded (and saved).
func (ex *executor) resolveRef(ref config.Ref) (cty.Value, diag.Diagnostics) {
	rs := ex.st.Resources[ref.Address]
	if rs == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "reference to missing resource",
			fmt.Sprintf("cannot resolve ${%s.%s}: resource has no state", ref.Address, ref.Attr))}
	}
	ps := ex.schemas[rs.Provider]
	if ps == nil || ps.ResourceTypes[rs.Type] == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "missing resource schema",
			fmt.Sprintf("provider %q has no schema for resource type %q", rs.Provider, rs.Type))}
	}
	v, err := ctyjson.Unmarshal(rs.Attributes, ps.ResourceTypes[rs.Type].Block.ImpliedType())
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

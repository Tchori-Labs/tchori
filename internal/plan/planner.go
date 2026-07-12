package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/state"
)

// Planner builds a Plan by diffing desired configuration against recorded
// state through provider PlanResourceChange RPCs.
type Planner struct {
	Config        *config.Config
	State         *state.State
	Providers     map[string]*provider.Client          // key = provider local name
	Schemas       map[string]*provider.ProviderSchemas // key = provider local name
	EngineVersion string
	Refresh       bool // default true; ReadResource before diffing
	Destroy       bool // destroy mode: plan deletes for everything in state
}

// Plan iterates config resources in dependency order, plans each through its
// provider, and classifies the resulting change. Resources present in state
// but absent from config become delete changes. In destroy mode every state
// resource becomes a delete change. The document's Changes are always sorted
// by address; delete-time sequencing is the applier's job (Task 11 executes
// deletes last, in reverse plan order).
func (p *Planner) Plan(ctx context.Context) (*Plan, diag.Diagnostics) {
	var ds diag.Diagnostics
	pl := &Plan{
		FormatVersion: FormatVersion,
		EngineVersion: p.EngineVersion,
		StateSerial:   p.State.Serial,
		Changes:       []*Change{},
	}

	if p.Destroy {
		for _, addr := range sortedStateAddrs(p.State) {
			ch, cds := p.stateDeleteChange(addr)
			ds = append(ds, cds...)
			if cds.HasErrors() {
				return nil, ds
			}
			pl.Changes = append(pl.Changes, ch)
		}
		finalize(pl)
		return pl, ds
	}

	order, ods := p.Config.Order()
	ds = append(ds, ods...)
	if ods.HasErrors() {
		return nil, ds
	}

	// plannedValues carries the planned (possibly partially unknown) value of
	// every already-planned resource, so ${type.name.attr} references in later
	// resources resolve against planned data and unknowns propagate.
	plannedValues := map[string]cty.Value{}
	resolve := func(ref config.Ref) (cty.Value, diag.Diagnostics) {
		pv, ok := plannedValues[ref.Address]
		if !ok {
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address,
				"reference to unplanned resource",
				fmt.Sprintf("no planned value recorded for %s", ref.Address))}
		}
		v, err := attrPath(ref.Attr).Apply(pv)
		if err != nil {
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address,
				"cannot resolve reference",
				fmt.Sprintf("%s.%s: %s", ref.Address, ref.Attr, err))}
		}
		return v, nil
	}

	for _, addr := range order {
		res := p.Config.Resources[addr]
		client, schema, lds := p.lookup(addr, res.Provider, res.Type)
		ds = append(ds, lds...)
		if lds.HasErrors() {
			return nil, ds
		}
		ty := schema.Block.ImpliedType()

		// Prior value from state, decoded against the schema's implied type.
		prior := cty.NullVal(ty)
		var priorPrivate []byte
		rs, hasPrior := p.State.Resources[addr]
		if hasPrior {
			pv, err := ctyjson.Unmarshal(rs.Attributes, ty)
			if err != nil {
				ds = append(ds, diag.Errorf(addr, "invalid state attributes", err.Error()))
				return nil, ds
			}
			prior = pv
			priorPrivate = rs.Private
		}

		// Refresh: re-read the real object, use the result as prior, and keep
		// the in-memory state copy in sync.
		if p.Refresh && hasPrior {
			rv, rpriv, rds := client.ReadResource(ctx, res.Type, prior, priorPrivate)
			ds = append(ds, rds...)
			if rds.HasErrors() {
				return nil, ds
			}
			if rv.IsNull() {
				// The object vanished out of band: plan from a null prior.
				delete(p.State.Resources, addr)
				prior = cty.NullVal(ty)
				priorPrivate = nil
			} else {
				attrs, err := ctyjson.Marshal(rv, ty)
				if err != nil {
					ds = append(ds, diag.Errorf(addr, "cannot encode refreshed state", err.Error()))
					return nil, ds
				}
				rs.Attributes = attrs
				rs.Private = rpriv
				prior = rv
				priorPrivate = rpriv
			}
		}

		// Proposed value from raw config; refs resolve to planned values.
		proposed, cds := provider.Compose(res.Config, ty, false, resolve)
		ds = append(ds, cds...)
		if cds.HasErrors() {
			return nil, ds
		}

		vds := client.ValidateResource(ctx, res.Type, proposed)
		ds = append(ds, vds...)
		if vds.HasErrors() {
			return nil, ds
		}

		pc, pds := client.PlanResource(ctx, res.Type, prior, proposed, proposed, priorPrivate)
		ds = append(ds, pds...)
		if pds.HasErrors() {
			return nil, ds
		}
		planned := pc.State
		plannedValues[addr] = planned

		ch, err := newChange(addr, prior, planned, ty, pc)
		if err != nil {
			ds = append(ds, diag.Errorf(addr, "cannot encode change", err.Error()))
			return nil, ds
		}
		pl.Changes = append(pl.Changes, ch)
	}

	// Resources present in state but absent from config: destroy-style deletes.
	for _, addr := range sortedStateAddrs(p.State) {
		if _, inConfig := p.Config.Resources[addr]; inConfig {
			continue
		}
		ch, cds := p.stateDeleteChange(addr)
		ds = append(ds, cds...)
		if cds.HasErrors() {
			return nil, ds
		}
		pl.Changes = append(pl.Changes, ch)
	}

	finalize(pl)
	return pl, ds
}

// lookup resolves the launched client and resource-type schema for a resource.
func (p *Planner) lookup(addr, providerName, typeName string) (*provider.Client, *provider.Schema, diag.Diagnostics) {
	client, ok := p.Providers[providerName]
	if !ok {
		return nil, nil, diag.Diagnostics{diag.Errorf(addr, "provider not launched",
			fmt.Sprintf("no client for provider %q", providerName))}
	}
	schemas, ok := p.Schemas[providerName]
	if !ok {
		return nil, nil, diag.Diagnostics{diag.Errorf(addr, "provider schema missing",
			fmt.Sprintf("no schemas for provider %q", providerName))}
	}
	schema, unsupported, known := schemas.LookupResourceType(typeName)
	if !known {
		return nil, nil, diag.Diagnostics{diag.Errorf(addr, "unknown resource type",
			fmt.Sprintf("provider %q has no resource type %q", providerName, typeName))}
	}
	if schema == nil {
		return nil, nil, diag.Diagnostics{diag.Errorf(addr,
			fmt.Sprintf("unsupported schema for resource type %q", typeName), unsupported)}
	}
	return client, schema, nil
}

// stateDeleteChange synthesizes a delete change for a resource that exists in
// state: Before is the stored attributes verbatim, the planned value is null.
// No provider plan RPC is needed to plan a deletion in the MVP.
func (p *Planner) stateDeleteChange(addr string) (*Change, diag.Diagnostics) {
	rs := p.State.Resources[addr]
	_, schema, lds := p.lookup(addr, rs.Provider, rs.Type)
	if lds.HasErrors() {
		return nil, lds
	}
	ty := schema.Block.ImpliedType()
	raw, err := msgpack.Marshal(cty.NullVal(ty), ty)
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf(addr, "cannot encode planned value", err.Error())}
	}
	return &Change{
		Address:    addr,
		Action:     "delete",
		Before:     append(json.RawMessage(nil), rs.Attributes...),
		After:      json.RawMessage("null"),
		PlannedRaw: raw,
		Private:    rs.Private,
	}, lds
}

// newChange classifies and serializes one provider-planned resource change.
func newChange(addr string, prior, planned cty.Value, ty cty.Type, pc *provider.PlannedChange) (*Change, error) {
	before := json.RawMessage("null") // JSON null for create
	if !prior.IsNull() {
		b, err := ctyjson.Marshal(prior, ty)
		if err != nil {
			return nil, fmt.Errorf("before: %w", err)
		}
		before = b
	}

	after := json.RawMessage("null") // JSON null for delete
	var unknownAfter []string
	if !planned.IsNull() {
		sanitized, paths, err := nullOutUnknowns(planned)
		if err != nil {
			return nil, fmt.Errorf("after: %w", err)
		}
		unknownAfter = paths
		b, err := ctyjson.Marshal(sanitized, ty)
		if err != nil {
			return nil, fmt.Errorf("after: %w", err)
		}
		after = b
	}

	// PlannedRaw keeps the exact planned value, unknowns included, for the
	// applier: cty/msgpack encodes unknowns as its extension type 0.
	raw, err := msgpack.Marshal(planned, ty)
	if err != nil {
		return nil, fmt.Errorf("planned_raw: %w", err)
	}

	return &Change{
		Address:         addr,
		Action:          classify(prior, planned, pc.RequiresReplace),
		Before:          before,
		After:           after,
		UnknownAfter:    unknownAfter,
		RequiresReplace: pc.RequiresReplace,
		PlannedRaw:      raw,
		Private:         pc.Private,
	}, nil
}

// classify implements the contract's action classification: no prior =>
// create; prior and null planned => delete; RequiresReplace non-empty AND
// planned differs on those paths => replace; planned == prior => no-op;
// else update.
func classify(prior, planned cty.Value, requiresReplace []string) string {
	switch {
	case prior.IsNull():
		return "create"
	case planned.IsNull():
		return "delete"
	case replaceRequired(prior, planned, requiresReplace):
		return "replace"
	case planned.RawEquals(prior):
		return "no-op"
	default:
		return "update"
	}
}

// replaceRequired reports whether planned differs from prior on any of the
// provider's RequiresReplace paths. An unknown planned value on such a path
// counts as differing (the provider cannot promise it stays the same).
func replaceRequired(prior, planned cty.Value, paths []string) bool {
	for _, ps := range paths {
		path := attrPath(ps)
		pv, perr := path.Apply(prior)
		nv, nerr := path.Apply(planned)
		if perr != nil || nerr != nil {
			return true // path not comparable on both sides: assume changed
		}
		eq := pv.Equals(nv)
		if !eq.IsKnown() || eq.False() {
			return true
		}
	}
	return false
}

// attrPath converts a dotted attribute path ("replace_me", "triggers.foo")
// into a cty.Path of GetAttr steps.
func attrPath(dotted string) cty.Path {
	parts := strings.Split(dotted, ".")
	path := cty.GetAttrPath(parts[0])
	for _, part := range parts[1:] {
		path = path.GetAttr(part)
	}
	return path
}

// nullOutUnknowns is the research-digest workaround for ctyjson.Marshal
// rejecting unknown values: replace every unknown with a typed null and
// record its dotted path for the plan's unknown_after list. Paths are
// stringified inside the callback, so no cty.Path.Copy is needed (the
// backing array is only reused after the callback returns).
func nullOutUnknowns(v cty.Value) (cty.Value, []string, error) {
	var paths []string
	out, err := cty.Transform(v, func(p cty.Path, val cty.Value) (cty.Value, error) {
		if !val.IsKnown() {
			paths = append(paths, pathString(p))
			return cty.NullVal(val.Type()), nil
		}
		return val, nil
	})
	if err != nil {
		return cty.NilVal, nil, err
	}
	sort.Strings(paths)
	return out, paths, nil
}

// pathString renders a cty.Path as a dotted attribute path: "id",
// "triggers.foo", `tags["env"]`, "items[0]". Adapted from the algorithm of
// Terraform's internal tfdiags.FormatCtyPath (internal/tfdiags, BUSL-1.1,
// not importable — reimplemented per research-cty.md §6), without the
// leading dot.
func pathString(path cty.Path) string {
	var buf strings.Builder
	for i, step := range path {
		switch ts := step.(type) {
		case cty.GetAttrStep:
			if i > 0 {
				buf.WriteByte('.')
			}
			buf.WriteString(ts.Name)
		case cty.IndexStep:
			key := ts.Key
			switch {
			case key.Type() == cty.Number && key.IsKnown() && !key.IsNull():
				_, _ = fmt.Fprintf(&buf, "[%s]", key.AsBigFloat().Text('g', -1))
			case key.Type() == cty.String && key.IsKnown() && !key.IsNull():
				_, _ = fmt.Fprintf(&buf, "[%s]", strconv.Quote(key.AsString()))
			default:
				buf.WriteString("[...]")
			}
		}
	}
	return buf.String()
}

// sortedStateAddrs returns the state's resource addresses in sorted order.
func sortedStateAddrs(st *state.State) []string {
	addrs := make([]string, 0, len(st.Resources))
	for addr := range st.Resources {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	return addrs
}

// finalize sorts the changes by address (document determinism) and counts
// the summary; no-op changes are listed but never counted.
func finalize(pl *Plan) {
	sort.Slice(pl.Changes, func(i, j int) bool { return pl.Changes[i].Address < pl.Changes[j].Address })
	for _, ch := range pl.Changes {
		switch ch.Action {
		case "create":
			pl.Summary.Create++
		case "update":
			pl.Summary.Update++
		case "delete":
			pl.Summary.Delete++
		case "replace":
			pl.Summary.Replace++
		}
	}
}

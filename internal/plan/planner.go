package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/msgpack"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/sensitive"
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
		rs, hasPrior := p.State.Resources[addr]
		providerConfig := p.Config.Providers[res.Provider]
		if providerConfig == nil || providerConfig.Source == "" {
			ds = append(ds, diag.Errorf(addr, "provider source unavailable",
				fmt.Sprintf("provider %q has no canonical source in configuration", res.Provider)))
			return nil, ds
		}
		if hasPrior {
			ids := validateStateIdentity(addr, rs, res.Type, res.Provider, providerConfig.Source)
			ds = append(ds, ids...)
			if ids.HasErrors() {
				return nil, ds
			}
		}
		client, schema, lds := p.lookup(addr, res.Provider, res.Type)
		ds = append(ds, lds...)
		if lds.HasErrors() {
			return nil, ds
		}
		ty := schema.Block.ImpliedType()

		var persistedPaths []string
		if hasPrior {
			persistedPaths = rs.SensitivePaths
		}
		spec, specDs := sensitive.ResolveWithPersisted(
			schema.Block, res.SensitiveAttributes, persistedPaths, res.Config,
		)
		ds = append(ds, specDs...)
		if specDs.HasErrors() {
			return nil, ds
		}

		// Prior value from state. Sensitive-set recovery is applied before cty
		// decoding so provider operations receive the original set membership.
		prior := cty.NullVal(ty)
		var priorPrivate []byte
		var recordedAttrs json.RawMessage
		if hasPrior {
			pv, err := spec.RestoreProjected(rs.Attributes, rs.SensitiveSetRecovery, ty, rs.SensitivePaths, rs.SensitiveRecoveryVersion)
			if err != nil {
				ds = append(ds, diag.Errorf(addr, "invalid state attributes", err.Error()))
				return nil, ds
			}
			prior = pv
			priorPrivate = rs.Private
			var projectedRedacted []string
			recordedAttrs, projectedRedacted, _, err = spec.Project(prior)
			if err != nil {
				return nil, append(ds, diag.Errorf(addr, "cannot project recorded state", err.Error()))
			}
			publicDifference, err := newDrift(addr, rs.Attributes, recordedAttrs)
			if err != nil {
				return nil, append(ds, diag.Errorf(addr, "cannot inspect sensitive state", err.Error()))
			}
			if publicDifference != nil {
				_, legacyPaths, redactErr := sensitive.RedactJSON(rs.Attributes, projectedRedacted)
				if redactErr != nil {
					return nil, append(ds, diag.Errorf(addr, "cannot inspect sensitive state", redactErr.Error()))
				}
				if len(legacyPaths) != 0 {
					ds = append(ds, diag.Warnf(addr, "plaintext sensitive value already exists in state", fmt.Sprintf("rotate credentials at paths %s and purge state.json, state.json.backup, and git history; plan writes no state and a no-op apply saves nothing, so manual purge may be required", strings.Join(legacyPaths, ", "))))
				}
			}
		}

		// Refresh: re-read the real object, use the result as prior, and keep
		// the in-memory state copy in sync.
		if p.Refresh && hasPrior {
			// recordedAttrs was projected without rebuilding sensitive sets.
			rv, rpriv, rds := client.ReadResource(ctx, res.Type, prior, priorPrivate)
			rds = provider.Context(addr, rds)
			ds = append(ds, rds...)
			if rds.HasErrors() {
				return nil, ds
			}
			if rv.IsNull() {
				// The object vanished out of band: plan from a null prior.
				pl.Drift = append(pl.Drift, &Drift{
					Address: addr,
					Before:  recordedAttrs,
					After:   json.RawMessage("null"),
				})
				delete(p.State.Resources, addr)
				prior = cty.NullVal(ty)
				priorPrivate = nil
			} else {
				attrs, redactedPaths, recovery, err := spec.Project(rv)
				if err != nil {
					return nil, append(ds, diag.Errorf(addr, "cannot redact refreshed state", err.Error()))
				}
				drift, err := newTypedDrift(addr, recordedAttrs, attrs, prior, rv, spec)
				if err != nil {
					ds = append(ds, diag.Errorf(addr, "cannot compare refreshed state", err.Error()))
					return nil, ds
				}
				if drift != nil {
					pl.Drift = append(pl.Drift, drift)
				}
				rs.Attributes = attrs
				rs.Private = rpriv
				rs.SensitiveSetRecovery = recovery
				rs.SensitiveRecoveryVersion = sensitive.RecoveryVersion
				rs.Redacted = redactedPaths
				// spec was built from the union above, so spec.Paths() is
				// itself the monotonic union of what rs.SensitivePaths held
				// on entry and this run's declarations: this assignment can
				// only add newly schema- or config-sensitive paths, never
				// drop a still-schema-valid recorded one.
				rs.SensitivePaths = spec.Paths()
				rs.SensitiveScanned = true
				prior = rv
				priorPrivate = rpriv
			}
		}

		// Config value from raw config; refs resolve to planned values. An
		// attribute absent from config is null here, which is what "the
		// author did not write this" means to a provider.
		configVal, cds := provider.Compose(res.Config, ty, provider.EnvResolve, resolve)
		ds = append(ds, cds...)
		if cds.HasErrors() {
			return nil, ds
		}

		vds := client.ValidateResource(ctx, res.Type, configVal)
		vds = provider.Context(addr, vds)
		ds = append(ds, vds...)
		if vds.HasErrors() {
			return nil, ds
		}

		// Proposed new state is a different thing from config: it is the
		// guess at the post-apply object, so Computed attributes the author
		// left unset keep the value the provider assigned last run instead of
		// reverting to null. Sending config for both makes every plan read as
		// "clear all server-assigned fields" (Tchori-Labs/tchori-internal#59).
		proposed := provider.ProposedNew(schema.Block, prior, configVal)

		pc, pds := client.PlanResource(ctx, res.Type, prior, proposed, configVal, priorPrivate)
		pds = provider.Context(addr, pds)
		ds = append(ds, pds...)
		if pds.HasErrors() {
			return nil, ds
		}
		planned := pc.State
		plannedValues[addr] = planned

		ch, err := newChange(addr, res.Provider, providerConfig.Source, res.Type, prior, planned, ty, pc, spec)
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
	providerConfig := p.Config.Providers[rs.Provider]
	if providerConfig == nil || providerConfig.Source == "" {
		return nil, diag.Diagnostics{diag.Errorf(addr, "provider source unavailable",
			fmt.Sprintf("provider %q has no canonical source in configuration", rs.Provider))}
	}
	ids := validateStateIdentity(addr, rs, rs.Type, rs.Provider, providerConfig.Source)
	if ids.HasErrors() {
		return nil, ids
	}
	_, schema, lds := p.lookup(addr, rs.Provider, rs.Type)
	if lds.HasErrors() {
		return nil, lds
	}
	ty := schema.Block.ImpliedType()
	spec, sds := sensitive.ResolveWithPersisted(schema.Block, nil, rs.SensitivePaths, nil)
	lds = append(lds, sds...)
	if sds.HasErrors() {
		return nil, lds
	}
	p.State.NoteSensitive(addr, spec.Paths())
	prior, err := spec.RestoreProjected(rs.Attributes, rs.SensitiveSetRecovery, ty, rs.SensitivePaths, rs.SensitiveRecoveryVersion)
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf(addr, "cannot restore delete state", err.Error())}
	}
	// A state-only delete has no raw config and therefore no stable literal
	// instance exemptions; spec is already resolved without any exemptions.
	before, _, _, err := spec.Project(prior)
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf(addr, "cannot sanitize delete state", err.Error())}
	}
	raw, err := msgpack.Marshal(cty.NullVal(ty), ty)
	if err != nil {
		return nil, diag.Diagnostics{diag.Errorf(addr, "cannot encode planned value", err.Error())}
	}
	return &Change{
		Address:        addr,
		Type:           rs.Type,
		Provider:       rs.Provider,
		ProviderSource: rs.ProviderSource,
		Action:         "delete",
		Before:         before,
		After:          json.RawMessage("null"),
		PlannedRaw:     raw,
		Private:        rs.Private,
	}, lds
}

func validateStateIdentity(addr string, rs *state.ResourceState, resourceType, providerName, providerSource string) diag.Diagnostics {
	if rs == nil {
		return nil
	}
	if rs.ProviderSource == "" {
		return diag.Diagnostics{diag.Errorf(addr, "state has unbound provider source",
			"the stored resource predates canonical provider-source binding; run tchori state sanitize before planning")}
	}
	if rs.Type != resourceType || rs.Provider != providerName || rs.ProviderSource != providerSource {
		return diag.Diagnostics{diag.Errorf(addr, "state does not match resource identity",
			"the stored resource type, provider alias, or canonical provider source differs from configuration")}
	}
	return nil
}

// newChange classifies and serializes one provider-planned resource change.
func newChange(addr, providerName, providerSource, typeName string, prior, planned cty.Value, ty cty.Type, pc *provider.PlannedChange, spec *sensitive.Spec) (*Change, error) {
	before := json.RawMessage("null") // JSON null for create
	if !prior.IsNull() {
		b, _, _, err := spec.Marshal(prior)
		if err != nil {
			return nil, fmt.Errorf("before: %w", err)
		}
		before = b
	}

	after := json.RawMessage("null") // JSON null for delete
	var unknownAfter []string
	plannedForArtifact := planned
	if !planned.IsNull() {
		var err error
		plannedForArtifact, err = spec.Unknown(planned)
		if err != nil {
			return nil, fmt.Errorf("sensitive unknowns: %w", err)
		}
		b, _, paths, err := spec.Marshal(plannedForArtifact)
		if err != nil {
			return nil, fmt.Errorf("after: %w", err)
		}
		unknownAfter = paths
		after = b
	}

	// PlannedRaw keeps the exact planned value, unknowns included, for the
	// applier: cty/msgpack encodes unknowns as its extension type 0.
	raw, err := msgpack.Marshal(plannedForArtifact, ty)
	if err != nil {
		return nil, fmt.Errorf("planned_raw: %w", err)
	}

	return &Change{
		Address:         addr,
		Type:            typeName,
		Provider:        providerName,
		ProviderSource:  providerSource,
		Action:          Classify(prior, planned, pc.RequiresReplace, spec),
		Before:          before,
		After:           after,
		UnknownAfter:    unknownAfter,
		RequiresReplace: pc.RequiresReplace,
		PlannedRaw:      raw,
		Private:         pc.Private,
	}, nil
}

// Classify implements the plan/apply action contract: no prior => create;
// prior and null planned => delete; RequiresReplace non-empty and changed on
// one of those paths => replace; planned equal to prior => no-op; else update.
// Apply reuses this after concretizing an unknown-bearing set re-plan so a
// reviewed replacement cannot destroy an object after its action changes.
func Classify(prior, planned cty.Value, requiresReplace []string, spec *sensitive.Spec) string {
	if prior.IsNull() {
		return "create"
	}
	if planned.IsNull() {
		return "delete"
	}
	maskedPrior, err1 := spec.Mask(prior)
	maskedPlanned, err2 := spec.Mask(planned)
	if err1 != nil || err2 != nil {
		return "update"
	}
	switch {
	case replaceRequired(maskedPrior, maskedPlanned, requiresReplace):
		return "replace"
	case maskedPlanned.RawEquals(maskedPrior):
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

// attrPath retains the package-private call site while sharing one renderer/parser.
func attrPath(dotted string) cty.Path { return sensitive.AttrPath(dotted) }

// PathString is retained for callers outside plan; implementation lives in the leaf package.
func PathString(path cty.Path) string { return sensitive.PathString(path) }

// sortedStateAddrs returns the state's resource addresses in sorted order.
func sortedStateAddrs(st *state.State) []string {
	addrs := make([]string, 0, len(st.Resources))
	for addr := range st.Resources {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	return addrs
}

func newDrift(address string, before, after json.RawMessage) (*Drift, error) {
	beforeValue, err := decodeJSON(before)
	if err != nil {
		return nil, fmt.Errorf("decode recorded value: %w", err)
	}
	afterValue, err := decodeJSON(after)
	if err != nil {
		return nil, fmt.Errorf("decode refreshed value: %w", err)
	}
	paths := changedPaths(beforeValue, afterValue)
	if len(paths) == 0 {
		return nil, nil
	}
	return &Drift{
		Address: address,
		Before:  append(json.RawMessage(nil), before...),
		After:   append(json.RawMessage(nil), after...),
		Paths:   paths,
	}, nil
}

func newTypedDrift(address string, before, after json.RawMessage, prior, refreshed cty.Value, spec *sensitive.Spec) (*Drift, error) {
	maskedPrior, err := spec.Mask(prior)
	if err != nil {
		return nil, fmt.Errorf("mask recorded value: %w", err)
	}
	maskedRefreshed, err := spec.Mask(refreshed)
	if err != nil {
		return nil, fmt.Errorf("mask refreshed value: %w", err)
	}
	if maskedPrior.RawEquals(maskedRefreshed) {
		return nil, nil
	}
	drift, err := newDrift(address, before, after)
	if err != nil || drift != nil {
		return drift, err
	}
	// Sensitive set membership can change while both review projections remain
	// identical. Preserve the truthful drift event without exposing the hidden
	// member difference.
	return &Drift{
		Address: address,
		Before:  append(json.RawMessage(nil), before...),
		After:   append(json.RawMessage(nil), after...),
	}, nil
}

// finalize sorts changes and drift by address (document determinism) and
// counts the summary; no-op changes and drift are never counted.
func finalize(pl *Plan) {
	sort.Slice(pl.Changes, func(i, j int) bool { return pl.Changes[i].Address < pl.Changes[j].Address })
	sort.Slice(pl.Drift, func(i, j int) bool { return pl.Drift[i].Address < pl.Drift[j].Address })
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

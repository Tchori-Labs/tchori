// Package apply executes a saved plan against live providers: creates,
// updates and replaces run in dependency order (the config's topological
// sort, cfg.Order() — NOT pl.Changes' document order, which is merely
// sorted alphabetically by address for plan.json determinism), deletes run
// last in reverse dependency order. State is marked incomplete on disk before
// the first provider call (or apply refuses to run), re-saved after every
// successful provider call, finalized after all dependency-eligible work on a
// failed run, and cleared only after full completion. A successful apply with
// N provider-change saves therefore
// advances serial by N+2; a failed non-empty apply advances it by at least 2.
// Pre-flight refusals do not mutate state. Unresolved reference-shaped values
// are rejected before every config-bearing provider call, including before the
// destroy leg of a replace; provider-returned values remain outside that
// validation boundary.
//
// After create, update, and the create leg of replace, apply also checks the
// returned state against concretely-authored planned values. Shallow-known
// object/map containers and positionally aligned lists/tuples are traversed
// despite unknown computed descendants; an authored map owns its complete key
// set. Null or unknown resource roots fail before persistence, and other
// unknown/unencodable results fail without writing that address. Destroy
// retains its separate null-result guard.
package apply

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/config"
	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
	"github.com/tchori-labs/tchori/internal/privateblob"
	"github.com/tchori-labs/tchori/internal/provider"
	"github.com/tchori-labs/tchori/internal/sensitive"
	"github.com/tchori-labs/tchori/internal/state"
)

// Apply verifies pl.StateSerial == st.Serial (else stale-plan error diag),
// verifies every non-delete plan change still has a matching address in cfg
// (else "plan does not match configuration" error diag — see the
// configuration-drift guard below), then executes changes: creates/updates/
// replaces in dependency order, deletes last in reverse dependency order.
// Before the first ApplyResource, Apply durably marks statePath incomplete and
// refuses to execute if that save fails. After EVERY successful ApplyResource
// the state is updated in memory AND saved (partial-state safety). A failed
// change blocks only changes in its dependency closure; independent changes
// continue, and each blocked change receives an address-specific diagnostic.
// Failed runs finalize the marker and keep completed work saved; full
// completion clears the marker. A non-empty successful run
// with N provider-change saves advances serial by N+2. A failed run advances it
// by at least 2, so callers must compute a new plan before retrying. Stale-plan,
// ordering, and configuration-drift refusals happen before any save.
//
// Ordering note: pl.Changes is sorted alphabetically by address (see
// plan.finalize) for stable document ordering; it carries no dependency
// information and must never drive execution.
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
// Result accounts for plan changes that completed successfully and changes
// that dependency failures prevented Apply from attempting.
type Result struct {
	Created     int
	Updated     int
	Deleted     int
	Replaced    int
	NotExecuted []NotExecutedChange
}

// NotExecutedChange describes a planned change skipped because another change
// in the required dependency direction failed or was itself blocked.
type NotExecutedChange struct {
	Address string
	Action  string
	Reason  string
}

// Apply returns truthful execution accounting alongside all diagnostics.
func Apply(ctx context.Context, pl *plan.Plan, cfg *config.Config, providers map[string]*provider.Client, schemas map[string]*provider.ProviderSchemas, st *state.State, statePath string) (Result, diag.Diagnostics) {
	var result Result
	if err := privateblob.ValidateKey(); err != nil {
		return result, diag.Diagnostics{diag.Errorf("", "invalid artifact key", err.Error())}
	}
	hadMarker := st.Incomplete != nil
	if pl.StateSerial != st.Serial {
		return result, diag.Diagnostics{diag.Errorf("", "stale plan", fmt.Sprintf(
			"plan was created against state serial %d but the current state serial is %d; run plan again",
			pl.StateSerial, st.Serial))}
	}

	ex := &executor{cfg: cfg, providers: providers, schemas: schemas, st: st, statePath: statePath, applied: map[string]cty.Value{}}
	st.SetSensitiveResolver(func(addr string, rs *state.ResourceState) (state.Resolution, bool) {
		providerName, typeName := rs.Provider, rs.Type
		providerSource := rs.ProviderSource
		var declared []string
		var rawCfg map[string]any
		if cfg != nil {
			if res := cfg.Resources[addr]; res != nil {
				providerName, typeName = res.Provider, res.Type
				declared, rawCfg = res.SensitiveAttributes, res.Config
			}
			if pc := cfg.Providers[providerName]; pc != nil {
				providerSource = pc.Source
			}
		}
		ps := schemas[providerName]
		if ps == nil {
			return state.Resolution{}, false
		}
		sch, _, known := ps.LookupResourceType(typeName)
		if !known || sch == nil {
			return state.Resolution{}, false
		}
		spec, rds := sensitive.ResolveWithPersisted(sch.Block, declared, rs.SensitivePaths, rawCfg)
		if rds.HasErrors() {
			return state.Resolution{}, false
		}
		return state.Resolution{
			Paths:              spec.Paths(),
			ProviderSource:     providerSource,
			SanitizeAttributes: spec.Sanitizer(sch.Block.ImpliedType()),
			SanitizeBackup:     spec.Effective(nil).Sanitizer(sch.Block.ImpliedType()),
		}, true
	})

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
			return result, ods
		}
	}
	dependencies := map[string][]string{}
	if cfg != nil {
		var dds diag.Diagnostics
		dependencies, dds = cfg.Dependencies()
		if dds.HasErrors() {
			return result, dds
		}
	}
	dependents := reverseEdges(dependencies)
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
	var driftDiags diag.Diagnostics
	for _, ch := range pl.Changes {
		if ch.Action == "no-op" {
			continue
		}
		if ch.Action != "delete" && (cfg == nil || cfg.Resources[ch.Address] == nil) {
			driftDiags = append(driftDiags, diag.Errorf(ch.Address, "plan does not match configuration",
				fmt.Sprintf(
					"plan has a %q change for %q, but the address is no longer present in the loaded configuration; the configuration changed since the plan was created — run plan again",
					ch.Action, ch.Address)))
			continue
		}
		if ch.Type == "" || ch.Provider == "" || ch.ProviderSource == "" {
			driftDiags = append(driftDiags, diag.Errorf(ch.Address, "plan has unbound resource identity",
				"every executable change must record its resource type, provider alias, and canonical provider source; run plan again"))
			continue
		}
		typeName, providerName, providerSource, known := resourceIdentity(cfg, st, ch.Address)
		if !known || ch.Type != typeName || ch.Provider != providerName || ch.ProviderSource != providerSource {
			driftDiags = append(driftDiags, diag.Errorf(ch.Address, "plan does not match resource identity",
				"the resource type, provider alias, or canonical provider source changed since the plan was created; run plan again"))
			continue
		}
		if ch.Action != "create" {
			if rs := st.Resources[ch.Address]; rs != nil {
				if rs.ProviderSource == "" {
					driftDiags = append(driftDiags, diag.Errorf(ch.Address, "state has unbound provider source",
						"the stored resource predates canonical provider-source binding; run tchori state sanitize before applying"))
					continue
				}
				if rs.Type != typeName || rs.Provider != providerName || rs.ProviderSource != providerSource {
					driftDiags = append(driftDiags, diag.Errorf(ch.Address, "state does not match resource identity",
						"the stored resource type, provider alias, or canonical provider source differs from the provider selected by configuration"))
					continue
				}
			}
		}
		if ch.Action == "delete" {
			continue
		}
	}
	if driftDiags.HasErrors() {
		return result, driftDiags
	}

	unlock, err := st.Lock(ctx, statePath)
	if err != nil {
		return result, diag.Diagnostics{diag.Errorf("", "locking state", err.Error())}
	}
	defer unlock()

	marked := len(ordered) > 0
	if marked {
		remaining := make([]string, len(ordered))
		for i, ch := range ordered {
			remaining[i] = ch.Address
		}
		previousMarker, previousSerial := st.Incomplete, st.Serial
		st.Incomplete = &state.IncompleteApply{Applied: []string{}, Remaining: remaining}
		if err := ex.save(); err != nil {
			st.Incomplete, st.Serial = previousMarker, previousSerial
			return result, diag.Diagnostics{diag.Errorf("", "marking state incomplete",
				fmt.Sprintf("apply refused to run because non-convergence could not be recorded before the first provider call: %s", err))}
		}
	}

	var ds diag.Diagnostics
	outcomes := make(map[string]string, len(ordered))
	var completed, unfinished []*plan.Change
	failedAddress := ""
	persistenceFailedAt := ""
	for _, ch := range ordered {
		blocker, relationship := "", ""
		stateOnlyDelete := ch.Action == "delete" && !inConfigOrder[ch.Address]
		if persistenceFailedAt != "" && !stateOnlyDelete {
			blocker, relationship = persistenceFailedAt, "state persistence failure at"
		} else if ch.Action != "delete" {
			blocker = closureBlocker(ch.Address, dependencies, outcomes)
			relationship = "dependency"
		} else if inConfigOrder[ch.Address] {
			blocker = closureBlocker(ch.Address, dependents, outcomes)
			relationship = "dependent"
		}
		// A state-only delete cannot have a config-side dependent: Order rejects
		// every reference to an undeclared address. It is therefore always safe
		// to attempt, even when another config change failed.
		if blocker != "" {
			reason := fmt.Sprintf("action %q for %s was not executed because %s %s failed or was not executed", ch.Action, ch.Address, relationship, blocker)
			if relationship == "state persistence failure at" {
				reason = fmt.Sprintf("action %q for %s was not executed because state persistence failed while executing %s", ch.Action, ch.Address, blocker)
			}
			result.NotExecuted = append(result.NotExecuted, NotExecutedChange{Address: ch.Address, Action: ch.Action, Reason: reason})
			ds = append(ds, diag.Errorf(ch.Address, "planned change not executed", reason))
			outcomes[ch.Address] = "blocked"
			unfinished = append(unfinished, ch)
			continue
		}

		mutationCount := len(ex.mutations)
		step := ex.applyChange(ctx, ch)
		ds = append(ds, step...)
		executed := len(ex.mutations)-mutationCount == 1
		if ch.Action == "replace" {
			executed = len(ex.mutations)-mutationCount == 2
		}
		if executed {
			result.record(ch.Action)
		}
		if step.HasErrors() {
			outcomes[ch.Address] = "failed"
			unfinished = append(unfinished, ch)
			if failedAddress == "" {
				failedAddress = ch.Address
			}
			if diagnosticsContainSummary(step, "saving state") {
				persistenceFailedAt = ch.Address
			}
			continue
		}
		outcomes[ch.Address] = "completed"
		completed = append(completed, ch)
		st.Incomplete.Applied = addresses(completed)
	}

	if ds.HasErrors() {
		// Per-change saves happen inside applyChange. This final save records the
		// authoritative successful/unfinished split after independent work ran.
		st.Incomplete.FailedAddress = failedAddress
		st.Incomplete.Applied = addresses(completed)
		st.Incomplete.Remaining = addresses(unfinished)
		if err := ex.save(); err != nil {
			ds = append(ds, diag.Errorf(failedAddress, "saving incomplete state", err.Error()))
		}
		ds = append(ds, diag.Warnf(failedAddress, "state left incomplete",
			fmt.Sprintf("state is marked non-converged after failure at %s; run tchori state status for details", failedAddress)))
		return result, ds
	}

	st.Incomplete = nil
	if marked || ex.saveCount > 0 || hadMarker {
		if err := ex.save(); err != nil {
			return result, append(ds, diag.Errorf("", "clearing incomplete state", err.Error()))
		}
	}
	return result, ds
}

func (r *Result) record(action string) {
	switch action {
	case "create":
		r.Created++
	case "update":
		r.Updated++
	case "delete":
		r.Deleted++
	case "replace":
		r.Replaced++
	}
}

func reverseEdges(dependencies map[string][]string) map[string][]string {
	dependents := make(map[string][]string, len(dependencies))
	for addr := range dependencies {
		dependents[addr] = []string{}
	}
	for addr, deps := range dependencies {
		for _, dependency := range deps {
			dependents[dependency] = append(dependents[dependency], addr)
		}
	}
	for addr := range dependents {
		sort.Strings(dependents[addr])
	}
	return dependents
}

func closureBlocker(addr string, edges map[string][]string, outcomes map[string]string) string {
	seen := map[string]bool{}
	var visit func(string) string
	visit = func(current string) string {
		for _, next := range edges[current] {
			if seen[next] {
				continue
			}
			seen[next] = true
			if outcomes[next] == "failed" || outcomes[next] == "blocked" {
				return next
			}
			if blocker := visit(next); blocker != "" {
				return blocker
			}
		}
		return ""
	}
	return visit(addr)
}

func diagnosticsContainSummary(ds diag.Diagnostics, summary string) bool {
	for _, d := range ds {
		if d.Summary == summary {
			return true
		}
	}
	return false
}

func addresses(changes []*plan.Change) []string {
	out := make([]string, len(changes))
	for i, ch := range changes {
		out[i] = ch.Address
	}
	return out
}

// executor carries the shared apply context so the per-change helpers do
// not need seven-parameter signatures.
type executor struct {
	cfg       *config.Config
	providers map[string]*provider.Client
	schemas   map[string]*provider.ProviderSchemas
	st        *state.State
	statePath string
	applied   map[string]cty.Value // full in-process values for same-run references
	mutations []stateMutation
	saveCount int
}

func (ex *executor) save() error {
	if err := ex.st.Save(ex.statePath); err != nil {
		return err
	}
	ex.saveCount++
	return nil
}

// resourceIdentity is shared by preflight and execution so authenticated plan
// metadata is checked against the exact provider that will receive the call.
func resourceIdentity(cfg *config.Config, st *state.State, address string) (resourceType, providerName, providerSource string, known bool) {
	if cfg != nil && cfg.Resources[address] != nil {
		resource := cfg.Resources[address]
		providerConfig := cfg.Providers[resource.Provider]
		if providerConfig == nil || providerConfig.Source == "" {
			return resource.Type, resource.Provider, "", false
		}
		return resource.Type, resource.Provider, providerConfig.Source, true
	}
	if resource := st.Resources[address]; resource != nil {
		source := resource.ProviderSource
		if cfg != nil && cfg.Providers[resource.Provider] != nil {
			source = cfg.Providers[resource.Provider].Source
		}
		if source == "" {
			return resource.Type, resource.Provider, "", false
		}
		return resource.Type, resource.Provider, source, true
	}
	return "", "", "", false
}

// applyChange executes one plan change and persists its result. For TC-048,
// every non-delete config is composed before action dispatch so replace cannot
// destroy the prior object before an unresolved-reference failure is known.
func (ex *executor) applyChange(ctx context.Context, ch *plan.Change) diag.Diagnostics {
	addr := ch.Address

	typeName, providerName, providerSource, known := resourceIdentity(ex.cfg, ex.st, addr)
	if !known {
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
	// Register full effective paths before any destroy removes the entry; the
	// same save's backup must still scrub the departing resource.
	var declared, persisted []string
	var rawCfg map[string]any
	if ex.cfg != nil && ex.cfg.Resources[addr] != nil {
		declared = ex.cfg.Resources[addr].SensitiveAttributes
		rawCfg = ex.cfg.Resources[addr].Config
	}
	if rs := ex.st.Resources[addr]; rs != nil {
		persisted = rs.SensitivePaths
	}
	spec, sds := sensitive.ResolveWithPersisted(schema.Block, declared, persisted, rawCfg)
	if sds.HasErrors() {
		return sds
	}
	ex.st.NoteSensitive(addr, spec.Paths())

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
			v, err := spec.RestoreProjected(rs.Attributes, rs.SensitiveSetRecovery, ty, rs.SensitivePaths, rs.SensitiveRecoveryVersion)
			if err != nil {
				return diag.Diagnostics{diag.Errorf(addr, "corrupt state attributes", err.Error())}
			}
			prior = v
			priorPrivate = rs.Private
		}
	}

	if ch.Action == "delete" {
		return ex.destroy(ctx, client, typeName, providerName, providerSource, addr, ch.Action, schema.Block, ty, prior, priorPrivate)
	}

	// Compose before selecting create/update/replace so an unresolved value
	// cannot let replace destroy a live object and only then fail. A resource's
	// own state entry cannot be one of its reference targets: self-references
	// are cycles rejected by Config.Order.
	cfgVal, ds := ex.composeConfig(addr, ty)
	if ds.HasErrors() {
		return ds
	}
	// Decode the reviewed plan. Sensitive-set plans deliberately carry
	// unknown secret leaves, so apply re-plans them with the now-concrete
	// composed configuration rather than replacing the entire provider plan
	// with raw config. The re-plan is non-mutating and must satisfy the
	// reviewed known-value contract as a multiset before any replace destroy.
	reviewed, err := provider.DecodeMsgpack(ch.PlannedRaw, ty)
	if err != nil {
		return append(ds, diag.Errorf(addr, "corrupt planned value", err.Error()))
	}
	planned := reviewed
	plannedPrivate := ch.Private
	if reviewedSetNeedsReplan(reviewed) {
		proposed := provider.ProposedNew(schema.Block, prior, cfgVal)
		replanned, pds := client.PlanResource(ctx, typeName, prior, proposed, cfgVal, priorPrivate)
		pds = provider.Context(addr, pds)
		ds = append(ds, pds...)
		if pds.HasErrors() {
			return ds
		}
		replannedAction := plan.Classify(prior, replanned.State, replanned.RequiresReplace, spec)
		if !plannedContractMatches(reviewed, replanned.State) ||
			!sameStrings(ch.RequiresReplace, replanned.RequiresReplace) ||
			replannedAction != ch.Action {
			return append(ds, diag.Errorf(addr, "set preflight diverged from reviewed plan",
				fmt.Sprintf("provider re-plan changed a reviewed non-sensitive value, collection membership, replacement requirement, or action (%s to %s); no resource mutation was attempted", ch.Action, replannedAction)))
		}
		planned = replanned.State
		plannedPrivate = replanned.Private
	} else {
		planned = resolvePlannedUnknowns(reviewed, cfgVal)
	}
	if planned.IsNull() || !planned.IsKnown() {
		return append(ds, diag.Errorf(addr, "invalid planned value", "create, update, and replace require a known, non-null resource object"))
	}
	for _, finding := range provider.FindUnresolvedReferences(planned) {
		ds = append(ds, provider.UnresolvedReferenceDiagnostic(addr, finding))
	}
	if ds.HasErrors() {
		return ds
	}

	switch ch.Action {
	case "replace":
		// Destroy-then-create: two explicit ApplyResource calls. The state
		// entry is removed (and saved) after the destroy leg, then written
		// back (and saved) after the create leg.
		destroyDs := ex.destroy(ctx, client, typeName, providerName, providerSource, addr, ch.Action, schema.Block, ty, prior, priorPrivate)
		ds = append(ds, destroyDs...)
		if destroyDs.HasErrors() {
			return ds
		}
		return append(ds, ex.createOrUpdate(ctx, client, typeName, providerName, providerSource, addr, schema.Block, ty, cty.NullVal(ty), cfgVal, planned, plannedPrivate, ch, spec)...)
	default: // "create", "update"
		return append(ds, ex.createOrUpdate(ctx, client, typeName, providerName, providerSource, addr, schema.Block, ty, prior, cfgVal, planned, plannedPrivate, ch, spec)...)
	}
}

// destroy applies a null planned value — the plugin-protocol convention for
// "destroy this object" — then removes the resource from state and saves.
func (ex *executor) destroy(ctx context.Context, client *provider.Client, typeName, providerName, providerSource, addr, action string, block *provider.SchemaBlock, ty cty.Type, prior cty.Value, priorPrivate []byte) diag.Diagnostics {
	newState, newPrivate, ds := client.ApplyResource(ctx, typeName, prior, cty.NullVal(ty), cty.NullVal(ty), priorPrivate)
	ds = provider.Context(addr, ds)
	failed := ds.HasErrors()
	if failed {
		if newState == cty.NilVal || !newState.IsKnown() {
			return ds
		}
		old := ex.st.Resources[addr]
		if newState.IsNull() {
			delete(ex.st.Resources, addr)
			if err := ex.save(); err != nil {
				if old != nil {
					ex.st.Resources[addr] = old
				}
				return append(ds, diag.Errorf(addr, "saving state", err.Error()))
			}
			return append(ds, unresolvedWarnings(ex.st)...)
		}
		if old != nil && newState.RawEquals(prior) && (newPrivate == nil || bytes.Equal(newPrivate, old.Private)) {
			return ds
		}

		var declared, persisted []string
		var rawCfg map[string]any
		if ex.cfg != nil && ex.cfg.Resources[addr] != nil {
			declared = append(declared, ex.cfg.Resources[addr].SensitiveAttributes...)
			rawCfg = ex.cfg.Resources[addr].Config
		}
		if old != nil {
			persisted = old.SensitivePaths
		}
		spec, specDs := sensitive.ResolveWithPersisted(block, declared, persisted, rawCfg)
		ds = append(ds, specDs...)
		if specDs.HasErrors() {
			return ds
		}
		attrs, redactedPaths, recovery, err := spec.Project(newState)
		if err != nil {
			return append(ds, diag.Errorf(addr, "redacting partial destroy state", err.Error()))
		}
		ex.st.NoteSensitive(addr, spec.Paths())
		ex.st.Resources[addr] = &state.ResourceState{
			Type:                     typeName,
			Provider:                 providerName,
			ProviderSource:           providerSource,
			Attributes:               attrs,
			Private:                  newPrivate,
			SensitiveSetRecovery:     recovery,
			SensitiveRecoveryVersion: sensitive.RecoveryVersion,
			Redacted:                 redactedPaths,
			SensitivePaths:           spec.Paths(),
			SensitiveScanned:         true,
		}
		if err := ex.save(); err != nil {
			if old != nil {
				ex.st.Resources[addr] = old
			} else {
				delete(ex.st.Resources, addr)
			}
			return append(ds, diag.Errorf(addr, "saving state", err.Error()))
		}
		return append(ds, unresolvedWarnings(ex.st)...)
	}
	if !newState.IsNull() {
		return append(ds, diag.Errorf(addr, "provider did not destroy resource",
			"ApplyResource returned a non-null state for a null planned value"))
	}
	delete(ex.st.Resources, addr)
	if err := ex.save(); err != nil {
		return append(ds, diag.Errorf(addr, "saving state", err.Error()))
	}
	ex.mutations = append(ex.mutations, stateMutation{addr: addr, action: action, removed: true})
	return append(ds, unresolvedWarnings(ex.st)...)
}

// createOrUpdate applies the value prepared before any replace destroy leg,
// then records the provider's returned state and saves.
func (ex *executor) createOrUpdate(ctx context.Context, client *provider.Client, typeName, providerName, providerSource, addr string, block *provider.SchemaBlock, ty cty.Type, prior, cfgVal, planned cty.Value, plannedPrivate []byte, ch *plan.Change, spec *sensitive.Spec) diag.Diagnostics {
	var ds diag.Diagnostics

	newState, newPrivate, applyDs := client.ApplyResource(ctx, typeName, prior, planned, cfgVal, plannedPrivate)
	applyDs = provider.Context(addr, applyDs)
	ds = append(ds, applyDs...)
	failed := applyDs.HasErrors()
	if failed {
		ds = append(ds, attemptedChangeSummary(addr, ch.Action, block, spec, prior, planned)...)
		if newState == cty.NilVal || newState.IsNull() || !newState.IsKnown() {
			return ds
		}
		// Unchanged prior state needs no extra checkpoint. A changed partial
		// result must survive so the next run can recover the remote object.
		if old := ex.st.Resources[addr]; old != nil && newState.RawEquals(prior) && bytes.Equal(newPrivate, old.Private) {
			return ds
		}
	}

	// Compute consistency diagnostics before encoding. Degenerate null or
	// shallow-unknown resource roots return their named errors immediately;
	// a partially unknown object is still walked for actionable leaf errors,
	// then the encoding guard below prevents it from being persisted.
	var consistencyDs diag.Diagnostics
	if !failed {
		consistencyDs = checkResultConsistency(addr, block, spec, planned, cfgVal, newState)
	}
	if newState.IsNull() || !newState.IsKnown() {
		return append(ds, consistencyDs...)
	}
	// Keep the full result only in memory so same-run references resolve even
	// though the durable state withholds the source value.
	if !failed {
		ex.applied[addr] = newState
	}
	attrs, redactedPaths, recovery, err := spec.Project(newState)
	if err != nil {
		return append(ds, diag.Errorf(addr, "redacting new state", err.Error()))
	}
	ex.st.NoteSensitive(addr, spec.Paths())
	ex.st.Resources[addr] = &state.ResourceState{
		Type:                     typeName,
		Provider:                 providerName,
		ProviderSource:           providerSource,
		Attributes:               attrs,
		Private:                  newPrivate,
		SensitiveSetRecovery:     recovery,
		SensitiveRecoveryVersion: sensitive.RecoveryVersion,
		Redacted:                 redactedPaths,
		SensitivePaths:           spec.Paths(),
		SensitiveScanned:         true,
	}
	if len(redactedPaths) != 0 {
		ds = append(ds, diag.Warnf(addr, "sensitive attributes withheld from state", fmt.Sprintf("withheld paths: %s; capture provider-issued credentials in a secret store", strings.Join(redactedPaths, ", "))))
	}
	if err := ex.save(); err != nil {
		return append(ds, diag.Errorf(addr, "saving state", err.Error()))
	}
	if !failed {
		ex.mutations = append(ex.mutations, stateMutation{addr: addr, action: ch.Action})
	}
	ds = append(ds, unresolvedWarnings(ex.st)...)
	return append(ds, consistencyDs...)
}

func unresolvedWarnings(st *state.State) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, addr := range st.UnresolvedSensitiveAddresses() {
		ds = append(ds, diag.Warnf(addr, "state entry could not be checked for sensitive values", "provider schema or live configuration was unavailable; inspect and rotate any credentials manually"))
	}
	return ds
}

// resolvePlannedUnknowns walks planned and cfgVal together (both at the same
// schema type) and replaces any unknown leaf in planned with the
// corresponding value from cfgVal, provided cfgVal actually has a concrete
// (known, non-null) value there. Composite values recurse per-element:
// objects and maps per-attribute/per-key (as before), and — fixing
// Tchori-Labs/tchori-internal#11 (TC-033) — lists and tuples per-index.
// Planned and cfgVal decode the same raw config at the same schema type, so
// positional collections can resolve unknown references only when lengths
// agree. Sets intentionally have no merge rule: their elements have no stable
// identity. Sensitive sets are concretized by a provider re-plan plus the
// reviewed-contract check in applyChange.
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

	case ty.IsListType():
		if planned.LengthInt() == 0 {
			return planned
		}
		elemTy := ty.ElementType()
		var cfgElems []cty.Value
		haveCfg := cfgVal.IsKnown() && !cfgVal.IsNull() && cfgVal.LengthInt() == planned.LengthInt()
		if haveCfg {
			cfgElems = cfgVal.AsValueSlice()
		}
		if !haveCfg {
			// No sound positional correspondence (cfgVal unknown/null, or a
			// length mismatch) — recurse each element against null so any
			// provider-computed unknowns still pass through unchanged, but
			// never risk misaligning elements across differing lengths.
			elems := make([]cty.Value, 0, planned.LengthInt())
			for it := planned.ElementIterator(); it.Next(); {
				_, v := it.Element()
				elems = append(elems, resolvePlannedUnknowns(v, cty.NullVal(elemTy)))
			}
			return cty.ListVal(elems)
		}
		elems := make([]cty.Value, 0, planned.LengthInt())
		i := 0
		for it := planned.ElementIterator(); it.Next(); i++ {
			_, v := it.Element()
			elems = append(elems, resolvePlannedUnknowns(v, cfgElems[i]))
		}
		return cty.ListVal(elems)

	case ty.IsTupleType():
		atys := ty.TupleElementTypes()
		if len(atys) == 0 {
			return planned
		}
		plannedElems := planned.AsValueSlice()
		haveCfg := cfgVal.IsKnown() && !cfgVal.IsNull()
		var cfgElems []cty.Value
		if haveCfg {
			cfgElems = cfgVal.AsValueSlice()
			haveCfg = len(cfgElems) == len(plannedElems)
		}
		if !haveCfg {
			elems := make([]cty.Value, len(plannedElems))
			for i, v := range plannedElems {
				elems[i] = resolvePlannedUnknowns(v, cty.NullVal(atys[i]))
			}
			return cty.TupleVal(elems)
		}
		elems := make([]cty.Value, len(plannedElems))
		for i, v := range plannedElems {
			elems[i] = resolvePlannedUnknowns(v, cfgElems[i])
		}
		return cty.TupleVal(elems)

	case ty.IsSetType():
		return planned

	default:
		return planned
	}
}

// reviewedSetNeedsReplan reports whether a reviewed unknown is at or below a
// set boundary. Sets have no positional merge rule, so every such value must
// be concretized by asking the provider to plan again.
func reviewedSetNeedsReplan(value cty.Value) bool {
	return unknownAtOrBelowSet(value, false)
}

func unknownAtOrBelowSet(value cty.Value, insideSet bool) bool {
	ty := value.Type()
	if !value.IsKnown() {
		return insideSet || typeContainsSet(ty)
	}
	if value.IsNull() {
		return false
	}
	if ty.IsSetType() {
		if !value.IsWhollyKnown() {
			return true
		}
		for it := value.ElementIterator(); it.Next(); {
			_, element := it.Element()
			if unknownAtOrBelowSet(element, true) {
				return true
			}
		}
		return false
	}
	switch {
	case ty.IsObjectType():
		for name := range ty.AttributeTypes() {
			if unknownAtOrBelowSet(value.GetAttr(name), insideSet) {
				return true
			}
		}
	case ty.IsMapType(), ty.IsListType(), ty.IsTupleType():
		for it := value.ElementIterator(); it.Next(); {
			_, element := it.Element()
			if unknownAtOrBelowSet(element, insideSet) {
				return true
			}
		}
	}
	return false
}

func typeContainsSet(ty cty.Type) bool {
	switch {
	case ty.IsSetType():
		return true
	case ty.IsListType(), ty.IsMapType():
		return typeContainsSet(ty.ElementType())
	case ty.IsTupleType():
		for _, elementType := range ty.TupleElementTypes() {
			if typeContainsSet(elementType) {
				return true
			}
		}
	case ty.IsObjectType():
		for _, attributeType := range ty.AttributeTypes() {
			if typeContainsSet(attributeType) {
				return true
			}
		}
	}
	return false
}

// plannedContractMatches checks that an apply-time provider re-plan preserves
// every reviewed known value and collection membership. Unknown reviewed
// leaves are wildcards. Sets use a full bipartite match rather than index
// association, so duplicate public projections cannot coalesce.
func plannedContractMatches(reviewed, current cty.Value) bool {
	if reviewed == cty.NilVal || current == cty.NilVal || !reviewed.Type().Equals(current.Type()) {
		return false
	}
	if !reviewed.IsKnown() {
		return true
	}
	if !current.IsKnown() || reviewed.IsNull() != current.IsNull() {
		return false
	}
	if reviewed.IsNull() {
		return true
	}

	ty := reviewed.Type()
	switch {
	case ty.IsObjectType():
		for name := range ty.AttributeTypes() {
			if !plannedContractMatches(reviewed.GetAttr(name), current.GetAttr(name)) {
				return false
			}
		}
		return true
	case ty.IsMapType():
		reviewedValues, currentValues := reviewed.AsValueMap(), current.AsValueMap()
		if len(reviewedValues) != len(currentValues) {
			return false
		}
		for key, value := range reviewedValues {
			other, ok := currentValues[key]
			if !ok || !plannedContractMatches(value, other) {
				return false
			}
		}
		return true
	case ty.IsListType(), ty.IsTupleType():
		reviewedValues, currentValues := reviewed.AsValueSlice(), current.AsValueSlice()
		if len(reviewedValues) != len(currentValues) {
			return false
		}
		for i := range reviewedValues {
			if !plannedContractMatches(reviewedValues[i], currentValues[i]) {
				return false
			}
		}
		return true
	case ty.IsSetType():
		reviewedValues, currentValues := reviewed.AsValueSlice(), current.AsValueSlice()
		if len(reviewedValues) != len(currentValues) {
			return false
		}
		edges := make([][]int, len(reviewedValues))
		for i := range reviewedValues {
			for j := range currentValues {
				if plannedContractMatches(reviewedValues[i], currentValues[j]) {
					edges[i] = append(edges[i], j)
				}
			}
		}
		return perfectBipartiteMatch(edges, len(currentValues))
	default:
		return reviewed.RawEquals(current)
	}
}

// perfectBipartiteMatch uses augmenting paths to find a complete assignment in
// O(VE). It never enumerates permutations of wildcard-compatible set members.
func perfectBipartiteMatch(edges [][]int, rightCount int) bool {
	if len(edges) != rightCount {
		return false
	}
	rightOwner := make([]int, rightCount)
	for i := range rightOwner {
		rightOwner[i] = -1
	}
	var augment func(int, []bool) bool
	augment = func(left int, seen []bool) bool {
		for _, right := range edges[left] {
			if right < 0 || right >= rightCount || seen[right] {
				continue
			}
			seen[right] = true
			if rightOwner[right] == -1 || augment(rightOwner[right], seen) {
				rightOwner[right] = left
				return true
			}
		}
		return false
	}
	for left := range edges {
		if !augment(left, make([]bool, rightCount)) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resolveRef resolves a ${type.name.attr} reference against the current
// in-memory state. Apply executes creates/updates/replaces in cfg.Order()'s
// topological order (not pl.Changes' address-sorted document order — see
// Apply), so by the time a dependent applies, the resource it references
// has already applied and its post-apply value already recorded in state
// (and saved).
func (ex *executor) resolveRef(ref config.Ref) (cty.Value, diag.Diagnostics) {
	if full, ok := ex.applied[ref.Address]; ok {
		return resolveRefValue(full, ref)
	}
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
	v, err := provider.DecodeJSON(rs.Attributes, schema.Block.ImpliedType())
	if err != nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "corrupt state attributes", err.Error())}
	}

	return resolveRefValue(v, ref)
}

func resolveRefValue(v cty.Value, ref config.Ref) (cty.Value, diag.Diagnostics) {
	for _, seg := range strings.Split(ref.Attr, ".") {
		if v.IsNull() {
			return cty.NilVal, diag.Diagnostics{diag.Errorf(ref.Address, "value was withheld from state",
				fmt.Sprintf("cannot resolve ${%s.%s}: the value is null or was withheld; provide it again through configuration or a secret store", ref.Address, ref.Attr))}
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

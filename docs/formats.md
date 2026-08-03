# Format reference: plan.json and state.json

tchori has exactly two machine-readable, on-disk artifacts: the plan
document (`plan.json`, or whatever path `-out` names) and the state file
(`state.json`, fixed name in the working directory). Both are schema-
versioned, deterministic JSON, designed to be committed, diffed, and read by
agents as easily as by humans. This page documents their fields, semantics,
and guarantees as implemented — it is not a design proposal.

Source of truth: `internal/plan/plan.go`, `internal/plan/planner.go`,
`internal/state/state.go`, and their tests.

## File purposes

| File | Written by | Read by | Purpose |
| --- | --- | --- | --- |
| `plan.json` | `tchori plan -out FILE`, `tchori destroy -out FILE` | `tchori apply FILE` | The reviewable, PR-able artifact: exactly the set of changes an apply will execute. There is no plan-less apply. |
| `state.json` | `tchori apply`, `tchori destroy` (via `Save`) | `tchori plan`, `tchori apply`, `tchori state list/show`, `tchori mcp` | The record of what tchori believes is really deployed: one entry per managed resource, keyed by address. |

## Unresolved-reference safety

References use the exact whole-string form `${type.name.attr}`. If a
reference-shaped `${...}` fragment survives in a resolved resource config,
provider config, or planned value that tchori is about to send to a provider
RPC, tchori refuses the call with an `unresolved reference` error. This guard
applies to validate, plan, apply (including replace before its destroy leg),
and provider configuration. As a result, tchori never persists a matching
value that it composed and sent itself.

Composition happens without a resource address in scope, so validate and plan
diagnostics identify the offending attribute path and matched `${...}`
substring but leave `address` empty. Apply performs an address-aware guard and
also attaches the resource address. Diagnostics never print the complete
attribute value, which may have come from state or an environment variable.

The guarantee has three deliberate boundaries:

- Non-reference template strings such as `${HOME}` are valid literals and may
  legitimately appear in config or state.
- Values returned by a provider are not scanned; provider responses remain
  authoritative and may contain text that resembles a reference.
- A matching value already present in a pre-existing `state.json` is not
  rewritten or cleaned. It becomes a hard error when it next feeds an
  outgoing value.

## plan.json

### Top-level fields (`Plan`)

| Field | JSON type | Meaning |
| --- | --- | --- |
| `format_version` | string | Plan document schema version. Currently always `"1.0"` (`plan.FormatVersion`). |
| `engine_version` | string | The tchori binary version that produced the plan (e.g. `"0.1.0-dev"`), from `internal/version.Version`. |
| `state_serial` | integer | The state file's `serial` at the moment this plan was computed (`p.State.Serial`). `apply` compares this against the live state's serial to detect staleness — see below. |
| `changes` | array of `Change` | Always sorted by `address` (`plan.finalize`). Document order is for byte-stability only; it carries no dependency information (`apply.Apply`'s ordering notes call this out explicitly — execution order comes from the config's topological sort, not from this array). |
| `summary` | object | Counts of `create`/`update`/`delete`/`replace` changes. All four keys are always present, even at zero (`Summary` has no `omitempty` tags). `no-op` changes are never counted. |

### Change fields

| Field | JSON type | Meaning |
| --- | --- | --- |
| `address` | string | Resource address, `type.name` (e.g. `tchoritest_thing.a`). |
| `action` | string | One of `create`, `update`, `delete`, `replace`, `no-op` — see Action semantics below. |
| `before` | object or `null` | Prior value, ctyjson-encoded. `null` for `create` (no prior object existed). |
| `after` | object or `null` | Planned value, ctyjson-encoded, with every attribute unknown at plan time rendered as JSON `null`. `null` for `delete`. |
| `unknown_after` | array of strings, omitted if empty | Dotted attribute paths inside `after` whose real value won't be known until apply (see Unknowns below). |
| `requires_replace` | array of strings, omitted if empty | Attribute paths the provider says force replacement *if their value differs from prior*. Presence here does not by itself mean `action` is `replace` — see Action semantics. |
| `planned_raw` | base64 string, omitted if empty | The exact planned value (including real unknowns), msgpack-encoded via `cty/msgpack`. Opaque; consumed by `apply` to reconstruct the planned state precisely — not meant for humans to read. |
| `private` | base64 string, omitted if empty | Opaque per-resource provider private data, round-tripped from the provider's plan RPC into its apply RPC. |

`before`/`after`/`planned_raw`/`private` are typed `json.RawMessage` or
`[]byte` in Go; `encoding/json`'s default handling renders `[]byte` as
standard base64 in the JSON document.

> **Environment-sourced resource values:** An `{"env": "VAR"}` value in resource
> config is resolved to a concrete string at plan time. It is persisted in
> `plan.json` (visibly in `after` and inside `planned_raw`; base64 is encoding,
> not encryption) and returned by the MCP `plan` tool. Values at paths marked
> sensitive by the provider or `sensitive_attributes` follow the normal
> redaction rules; treat unmarked plan values and plan results as sensitive.

### Action semantics (`plan.classify`)

| Action | When |
| --- | --- |
| `create` | No prior state for this address. |
| `delete` | Prior state exists and the planned value is null (resource removed from config, or `destroy` mode). |
| `replace` | `requires_replace` is non-empty **and** the planned value actually differs from prior on at least one of those paths (an unknown planned value on such a path counts as differing — the provider cannot promise it stays the same). |
| `update` | Planned value differs from prior, but not on a path that forces replacement. |
| `no-op` | Planned value equals prior exactly (`cty.Value.RawEquals`). |

`no-op` changes are listed in `changes` (so the document always accounts for
every config resource) but never counted in `summary`, and `tchori plan`'s
human-readable stdout output filters them out — only the JSON document keeps
them.

### How unknowns are represented

An attribute a provider can't determine until apply (e.g. a cloud-assigned
ID on create) is planned as *unknown*, not as a guessed value. Since JSON has
no "unknown" type, `newChange` (`internal/plan/planner.go`) walks the planned
value and:

- writes JSON `null` for that attribute inside `after`, and
- records its dotted path in `unknown_after`.

Paths use the same dotted/bracket notation for nested attributes and map
keys, e.g. `echo`, `id`, or `tags["parent"]` for a map key. `planned_raw`
preserves provider unknowns for apply, but sensitive non-exempt leaves are
also encoded there as unknown rather than concrete values. `after` is the
reviewable JSON projection; `planned_raw` is the executable one.

At apply time, an unknown left over from planning that turns out to be a
`${...}` reference to another resource created earlier in the same run is
resolved against that resource's real, just-applied value before the
provider is called — this covers references nested arbitrarily deep inside
lists, sets, tuples, objects, and maps (e.g. a policy list whose element
holds a reference inside a further-nested object), not just top-level or
object/map-nested attributes, so a single `tchori apply` suffices even when
the reference is nested inside an ordered collection (Tchori-Labs/tchori#11).

### Exit-code contract

| Command | 0 | 2 | 1 |
| --- | --- | --- | --- |
| `tchori plan [-out FILE]` | no changes (`Plan.HasChanges()` false) | changes pending | error (config/provider/runtime failure; diagnostics on stderr) |
| `tchori destroy -out FILE` | nothing to destroy | destroy plan has deletions | error |
| `tchori apply PLANFILE` | applied successfully | *(not used — apply is terminal)* | error: stale plan, configuration drift, or a provider apply failure |
| `tchori state status` | state is converged | *(not used)* | state carries `incomplete_apply`, or cannot be read |

`HasChanges()` is simply `create + update + delete + replace > 0` from
`summary` — `no-op`-only plans exit `0`.

Diagnostics do not alter this exit-code contract. Every provider-RPC failure
carries the resource or provider address that issued the RPC. Warning-severity
`apply aborted` and `attempted change` diagnostics add
[partial-apply accounting](#partial-apply-and-abort-accounting) without changing
`HasErrors()` or the exit code. See the [diagnostic contract](diagnostics.md)
for the JSON shape, pretty rendering, address qualification, and advisory
non-JSON-response hint.

### format_version compatibility

`plan.Read` rejects any plan whose `format_version` is not exactly the
version this build of tchori writes (currently `"1.0"`) — a plan written by
a future, schema-incompatible tchori is refused with an explicit error
rather than silently misinterpreted.

### Example

Generated from a real `plan.Planner.Plan()` run against the in-repo test
provider (`tchoritest`, prefix `demo-`), for the two-resource config from the
README quickstart plus a `tags` reference so a nested unknown is visible
(`tchoritest_thing.b.tags.parent` references `tchoritest_thing.a.id`, which
doesn't exist yet on a first plan):

```json
{
  "format_version": "1.0",
  "engine_version": "0.1.0-dev",
  "state_serial": 0,
  "changes": [
    {
      "address": "tchoritest_thing.a",
      "action": "create",
      "before": null,
      "after": {
        "echo": null,
        "id": null,
        "name": "alpha",
        "replace_me": null,
        "tags": null
      },
      "unknown_after": [
        "echo",
        "id"
      ],
      "planned_raw": "haRlY2hv1AAAomlk1AAApG5hbWWlYWxwaGGqcmVwbGFjZV9tZcCkdGFnc8A="
    },
    {
      "address": "tchoritest_thing.b",
      "action": "create",
      "before": null,
      "after": {
        "echo": null,
        "id": null,
        "name": "beta",
        "replace_me": null,
        "tags": {
          "parent": null
        }
      },
      "unknown_after": [
        "echo",
        "id",
        "tags[\"parent\"]"
      ],
      "planned_raw": "haRlY2hv1AAAomlk1AAApG5hbWWkYmV0YapyZXBsYWNlX21lwKR0YWdzgaZwYXJlbnTUAAA="
    }
  ],
  "summary": {
    "create": 2,
    "update": 0,
    "delete": 0,
    "replace": 0
  }
}
```

This plan would exit `2` (`tchori plan -out plan.json`): both resources are
creates.

## state.json

### Top-level fields (`State`)

| Field | JSON type | Meaning |
| --- | --- | --- |
| `format_version` | string | State document schema version. Currently always `"1.0"` (unexported `state.formatVersion`). |
| `serial` | integer | Monotonically incremented once per successful `Save` call — see Serial semantics below. |
| `resources` | object | Map of resource address (`type.name`) to `ResourceState`. |
| `incomplete_apply` | object, omitted when converged | Durable evidence that the last apply did not complete; see below. |

### ResourceState fields

| Field | JSON type | Meaning |
| --- | --- | --- |
| `type` | string | Provider resource type, e.g. `tchoritest_thing`. |
| `provider` | string | Provider local name from config, e.g. `tchoritest`. |
| `attributes` | object | ctyjson-encoded applied values. Every withheld sensitive leaf is JSON `null`; state never stores unknown values. |
| `private` | base64 string, omitted if empty | Opaque provider data, round-tripped untouched. Tchori does not inspect or redact this blob. |
| `redacted` | array of strings, omitted if empty | Sorted paths whose values are withheld, explaining why the corresponding `attributes` leaf is `null`. |
| `sensitive_paths` | array of strings, omitted if empty | Sorted effective, index-insensitive sensitivity contract. It survives config removal and drives backups, delete plans, orphan handling, and provider-free read masking. |
| `sensitive_scanned` | boolean, omitted when false | Provenance marker set after live schema/config resolution, including for a definitively non-sensitive resource. Read surfaces use it to distinguish checked entries from legacy entries with unknown provenance. |

### Incomplete apply lifecycle

`incomplete_apply` contains `failed_address` (omitted while a run is still in
flight), `applied`, and `remaining`. The two lists are always JSON arrays,
including when empty. The record contains resource **addresses only**: never
attribute values, provider responses, private data, diagnostic details that
might echo values, or timestamps.

For every non-empty apply, tchori writes this marker to disk **before the first
provider call**. If that pre-flight save fails, apply refuses to issue any
provider request. Per-change saves preserve the marker while work proceeds. On
the first failure, a final save records the exact failed address and
applied/remaining split; on full success, a terminal save removes the marker.
A process killed mid-run or a failed finalizing save therefore still leaves an
artifact that admits it is non-converged. Stale-plan, configuration-order, and
configuration-drift refusals write nothing. A zero-change apply writes no new
marker, although it does clear a stale marker from an earlier run.

Use `tchori state status` as the convergence gate: exit 0 means converged and
exit 1 means incomplete. `plan`, `apply`, and `destroy` warn when loading a
marked file, while planning remains available for recovery.

An environment-sourced resource config value is written into the applied
resource's `attributes` in `state.json`, just like any other concrete configured
value. Values at sensitive paths follow the normal state redaction rules; treat
unmarked state values as sensitive.

`attributes` is encoded at the resource schema's deeply marker-free implied
cty type. Optional-attribute markers belong only to schema conversion targets;
they are never part of a value type constructed, decoded, or persisted by the
engine.

### Serial semantics

- `state.Load` on a missing path returns a fresh, empty state:
  `format_version: "1.0"`, `serial: 0`, `resources: {}` — not an error.
- Each successful `Save` increments `Serial`, regardless of whether the
  resource data actually changed. A save rejected because another process
  committed from the same base does not increment it.
- A non-empty successful apply with N provider change-leg saves advances
  `serial` by **N+2**: one durable pre-flight marker save, N per-leg saves,
  and one terminal clear. A failed apply advances it by at least 2 even if
  no resource completed (pre-flight marker plus failure finalizer). A replace
  has two provider legs and therefore two per-leg saves. Recompute the plan
  before retrying any failed apply because its original state serial is stale.

### Locking, backup, and durability behavior

`Save` (`internal/state/state.go`):

1. Acquires an flock-based lock at `path+".lock"` (`github.com/gofrs/flock`),
   polling every 50ms up to a 10-second timeout, and releases it via `defer`.
   If the name exists, `Save` first requires it to be a regular file on every
   platform. On POSIX, no-follow and nonblocking open flags additionally make
   a symlink or filesystem FIFO raced in after that check fail fast rather
   than be followed or hang. After acquisition on every platform, the held
   descriptor must be regular and the same inode as a fresh `Lstat` of the
   name. An existing regular lock is reused untouched: it is not replaced,
   truncated, or chmod'ed, so its permissions remain as found.
2. Re-reads the on-disk serial and compares it with the base serial observed by
   `Load` or the preceding successful `Save`. If another process committed in
   the meantime, `Save` returns `state.ErrConcurrentModification` with the
   state path and re-run guidance; neither the state nor its backup is touched.
3. Parses the prior document and sanitizes every entry using persisted paths,
   live resolution, and effective-path hints before writing `path+".backup"`.
   With no known sensitive path the copy stays byte-identical; otherwise it is
   canonically re-serialized. Existing `redacted` markers are unioned with
   newly changed paths. A parse failure aborts rather than copying uninspected
   bytes. The backup deliberately applies no literal-instance exemptions and
   retains previously persisted paths, so the prior document is scrubbed under
   the rules that wrote it even when the current declaration was removed.
   Because apply now performs bracketing saves, the backup left by a successful
   apply normally contains a marker-carrying intermediate, not the pre-apply
   state. The fresh-temp-and-rename symlink, directory, and `0600` hardening
   remains unchanged.
4. Sanitizes every live state entry, including resources untouched by this
   apply. A resolvable entry uses current schema/config paths as authoritative,
   honors per-instance literal exemptions, intersects stale markers with the
   current set, then unions newly redacted paths. An unresolvable entry falls
   back to persisted paths and hints without exemptions and is reported. A
   resolved empty path set means definitively non-sensitive and records
   `sensitive_scanned`; no resolver means persisted-hints-only behavior with no
   provenance writes or unresolved warnings.
5. Increments `Serial`, marshals with `MarshalIndent`, writes a temp file
   (`.state-*.tmp`) in the same directory, and fsyncs the complete file before
   closing it. It atomically renames the temp file over `path`, then runs the
   platform's directory-durability barrier before reporting success. On POSIX
   this fsyncs the containing directory; on Windows the barrier is a documented
   no-op because directory fsync is not a supported primitive and NTFS journals
   rename metadata. Failures before rename remove the temp file and leave the
   in-memory serial unchanged. A post-rename directory-sync failure returns
   without deleting the newly committed state; the in-memory serial and
   compare-and-swap base advance to match that visible replacement so a retry
   does not report a false concurrent modification.

Together, the file fsync and the platform directory-durability barrier mean a
`nil` return confirms the state contents reached stable storage across abrupt
process or host failure — on POSIX this additionally confirms the atomic
directory-entry replacement itself was fsynced; on Windows the rename's
durability is covered by the NTFS metadata journal instead.

#### Symlink handling

The sidecar guarantee is deliberately path- and platform-specific:

1. Writes to `state.json` and `state.json.backup` never traverse a symlink on
   any platform. Each uses a fresh, same-directory `O_EXCL` temporary file with
   mode `0600` requested and a rename that replaces the destination name. A
   symlink target is never truncated or modified, and the result is a new
   regular file. Replacement is atomic on POSIX. Windows `os.Rename` uses
   `MoveFileEx` replacement semantics, but does not have the same formal
   atomicity guarantee and may fail when another process has the destination
   open. On POSIX, umask can only clear requested bits, so permissions are
   owner-only and never broader than `0600`; Windows mode bits do not describe
   the resulting ACL and the platform's permission semantics apply.
2. A non-regular lock entry present during the preflight `Lstat` is rejected
   before opening on every platform. On POSIX, `O_NOFOLLOW|O_NONBLOCK` also
   refuses a symlink raced in before open and makes a raced filesystem FIFO
   return promptly. Everywhere, after acquisition, `Save` verifies that the
   held descriptor is regular and the same inode as the lock name. Where the
   guard constant is zero, notably Windows, a raced entry can be traversed
   before this verification detects it; a dangling symlink target may already
   have been created empty, but existing data cannot be destroyed because the
   lock open has no `O_TRUNC`. Windows named pipes use the `\\.\pipe\`
   namespace rather than filesystem FIFO paths.
3. Existing regular lock files are reused exactly as found, including their
   contents, inode, and permissions. The lock stores no state data, and
   replacing or mutating an inode held by another process would weaken flock's
   mutual exclusion.
4. `Load` intentionally uses `os.ReadFile`, so it follows a symlink at the
   state path for a read-only operation performed with the invoking user's
   privileges.

Hardlinks and symlinked parent-directory components are outside this mechanism.
State and backup permission bits remain subject to umask as the owner-only upper
bound described above. The nonblocking claim is measured for filesystem FIFOs
on the shipped Linux and Darwin targets; it is not a claim that arbitrary device
nodes cannot block. The guard flags apply to every unix build, but on the
unshipped aix, solaris, and illumos targets flock opens with `O_RDWR`, and POSIX
leaves `O_RDWR|O_NONBLOCK` FIFO-open behavior undefined.

### Determinism

- Both files are written with `encoding/json.MarshalIndent(v, "", "  ")`
  plus a trailing newline.
- `plan.json`'s `changes` array is explicitly sorted by `address`
  (`plan.finalize`).
- `state.json`'s `resources` is a Go map, but `encoding/json` always
  marshals map keys in sorted order — so the file's byte content does not
  depend on Go map insertion order. A converged marker is a pointer with
  `omitempty`, so converged files retain the established key set and order;
  an incomplete marker has no timestamp. `TestSaveDeterministicAcrossInsertionOrder`
  pins this down directly: two states built by inserting the same three
  resources in different orders `Save` to byte-identical files.
- Together, re-running plan or save against unchanged input reproduces the
  same bytes (`TestPlanWriteReadDeterminism`, `TestSaveLoadRoundTrip`) — this
  is what makes `git diff plan.json` / `git diff state.json` show only real
  changes, never formatting noise or nondeterministic key order.

### format_version compatibility

`state.Load` applies the same rule as `plan.Read`: an *existing* state file
must carry `format_version` exactly `"1.0"`, including rejecting a missing
or empty field — a state file tchori itself wrote always carries `"1.0"`
(see `Save`), so anything else is a file this engine did not write and
should not guess about. A missing file is not subject to this check at all
(it synthesizes a fresh empty state instead). `incomplete_apply` is an additive,
optional field, so `format_version` remains `"1.0"`. An older tchori binary can
read a marked file but will silently drop the marker if it re-saves that state.

### Example

The state produced by applying the plan.json example above (test provider,
prefix `demo-`):

```json
{
  "format_version": "1.0",
  "serial": 4,
  "resources": {
    "tchoritest_thing.a": {
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "attributes": {
        "echo": "alpha",
        "id": "demo-id-alpha",
        "name": "alpha",
        "replace_me": null,
        "tags": null
      }
    },
    "tchoritest_thing.b": {
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "attributes": {
        "echo": "beta",
        "id": "demo-id-beta",
        "name": "beta",
        "replace_me": null,
        "tags": {
          "parent": "demo-id-alpha"
        }
      }
    }
  }
}
```

Note `serial: 4`: apply saved the pre-flight marker, saved once after each of
the two resources applied, then saved once more to clear the marker. Planning
against this state again produces two `no-op` changes and exits `0`.

## Staleness and configuration drift at apply

`apply.Apply` refuses to run, entirely and before any provider call or
state save, in two situations:

1. **Stale plan.** `pl.state_serial != st.Serial` — the state has moved on
   since this plan was computed (someone else applied in the meantime, or
   it's simply an old plan file). This check runs before the first state
   save of the apply, because `Save` itself increments `Serial` — comparing
   after any save would compare against a serial the apply itself just
   changed. The error names both serials and says to plan again.
2. **Configuration drift.** Every non-delete change's address must still
   exist in the configuration loaded fresh at apply time. If the config was
   edited (e.g. a resource declaration removed) after the plan was written,
   that change would otherwise silently vanish from the execution order
   with zero diagnostics. Apply instead refuses the whole run with a "plan
   does not match configuration" diagnostic, mirroring the stale-plan
   check's all-or-nothing posture — a plan that no longer matches
   configuration is not partially actionable.

Both refusals are exit code `1`, with a structured diagnostic on stderr
naming the problem; the fix in both cases is to run `plan` again.

## Result consistency at apply

After create, update, and the create leg of replace, tchori checks the
provider's returned state against the plan for values the configuration
concretely authored. Deletes are excluded; their existing `provider did not
destroy resource` guard checks for a null result. A configured node must be
non-null and wholly known, and a planned wholesale value must be wholly known,
to make a value promise. A wholly-known planned null is compared wholesale.
Computed and unauthored attributes, null or unknown configured containers,
and unknown values at nodes that must be compared wholesale are not checked.
Sets have no stable element correspondence and remain wholesale values.

Object and map containers are traversed whenever they are shallow-known and
non-null, even when they contain unknown computed descendants such as an
`id` or `uuid`. Lists and tuples are likewise traversed by index when config,
plan, and result lengths agree, so an unknown element does not suppress checks
of concrete siblings. If those lengths differ, positional correspondence is
not sound and the collection is compared wholesale instead. Thus one unknown
computed descendant never suppresses checks of concrete siblings where safe
correspondence exists. A returned null or shallow-unknown container is instead
reported once at its own named path and is not traversed.

A configured map container authors its complete key set. Tchori walks the
union of planned and applied keys, reporting dropped keys as `applied absent`,
provider-invented keys as `planned absent`, and an authored empty map that
comes back populated. Individual shared-key values are compared only when the
configuration concretely authored that key. A key invented by the provider at
plan time (present in plan but absent from config) remains out of scope.

A null resource object fails before the attribute walk with `provider returned
no state after apply`; a shallow-unknown resource object similarly fails with
`provider returned unknown state after apply`. Every attribute divergence path
is named. Apply exits `1` with a structured diagnostic. State records exactly
the provider result when that result is non-null and JSON-encodable, even when
a consistency diagnostic is raised; null, root-unknown, and other
not-wholly-known/unencodable results write nothing for that address. Tchori
never substitutes the planned value. A provider that honours an attribute on
update can therefore converge on a second plan and apply.

Consistency diagnostic values follow the redaction rules below.

## Partial apply and abort accounting

Apply stops at the first erroring change, but every completed provider change
has already been saved to `state.json`. The provider's error remains verbatim
and in its original severity. Tchori then emits one warning-severity diagnostic
with summary `apply aborted`, addressed to the failing resource. Its multi-line
detail names the failing address and action, lists each completed change saved
before the failure, lists every later change that was not attempted, and says
to run `tchori plan` again. Empty lists are explicit: `nothing was applied ...`
for a first-change failure and `no further changes were pending; nothing was
left unattempted` for a last-change failure.

Saved changes distinguish `recorded in state` from `removed from state`. The
latter matters for a replace whose destroy leg succeeded and whose create leg
failed: the resource is absent from durable state, rather than untouched or
successfully replaced. Apply's durable incomplete marker also advances the
state serial on a failed run, including a first-change failure. A saved plan
therefore no longer matches `state.json`; run `tchori plan` before the next
apply. The `apply aborted` warning is emitted once per failed execution loop;
stale-plan, configuration-ordering, and configuration-drift refusals happen
before that loop and do not emit it.

When an update reaches `ApplyResourceChange` and the provider rejects it,
tchori also emits one warning-severity diagnostic with summary `attempted
change`, at the same resource address. Its detail lists the changed attribute
paths as `path: before -> after`, using the prior state and the resolved planned
value actually handed to the provider. Object and map paths are listed
individually; ordered collections and sets are rendered at their container
path. Unknowns and null transitions are explicit. Creates, replace create
legs, and updates with no value difference have no before/after list and do
not emit this warning.

Both sides of every attempted-change entry use the same fail-closed schema
redaction as the consistency diagnostic described above. A sensitive attribute
renders `(sensitive value) -> (sensitive value)`. A nested block rendered as a
whole is redacted when any descendant is sensitive, and an unresolvable schema
path is redacted rather than exposed.

An `api error (status <code>)` diagnostic is provider text describing a
provider/API-side rejection. Tchori preserves that text, attributes it, and
adds the surrounding abort and attempted-value accounting; it does not build,
inspect, or repair the HTTP payload constructed inside a third-party provider.
Because both additions are warnings, they never change `HasErrors()` or the
[exit-code contract](#exit-code-contract): the original provider error still
makes apply exit `1`.

## Sensitivity

Tchori derives sensitive paths from provider schema `Sensitive` flags plus a
resource's optional `sensitive_attributes` list. Provider-computed values at
those paths are withheld from state and plan artifacts. State stores a typed
JSON `null` plus the three metadata fields above; plans replace the value with
an unknown in both `after` and decoded `planned_raw`, list it in
`unknown_after`, and mask it during update/replacement classification so a
withheld computed value does not cause a perpetual diff.

Sensitivity matching ignores collection indices, while the raw-literal
exemption is a fully index-qualified instance. Thus one repeated-block element
may retain an authored literal while a sibling containing a `${...}` reference
is withheld. Literal authorship is determined from raw config syntax only;
references, explicit nulls, absent values, and `{"env":"..."}` wrappers are
never exempt. Set-nested blocks have no stable element identity and therefore
fail closed with no exemptions. Per-attribute sensitivity inside a provider
`nested_type` is not retained by current schema conversion; use
`sensitive_attributes` for that path.

Every save sanitizes the whole state document. Live, resolvable entries are
instance-aware and use current schema/config as authoritative, so literals
survive unrelated saves and removing a declaration takes effect on the next
save-producing apply. Backups, delete `before` values, orphans, and read
rendering are conservative path-level copies with no exemption. The backup is
the one consumer that also retains prior paths, ensuring the preceding file is
scrubbed even when a declaration was just removed. Backup markers are unioned;
live markers are intersected with current paths before newly changed paths are
unioned.

`state show` and MCP `state_show` mask from persisted `sensitive_paths` in
memory and never save. An entry with `sensitive_scanned: true` and no paths is
known non-sensitive. A pre-change entry with neither field cannot be classified
without launching a provider, so provider-free reads render it as stored with a
note. Likewise, `plan` never writes state and a no-op apply saves nothing:
legacy plaintext remains on disk until a changed apply/import or manual purge.
If plaintext was previously committed, rotate the credential and purge
`state.json`, `state.json.backup`, and repository history.

The consistency diagnostic and the
[`attempted change`](#partial-apply-and-abort-accounting) diagnostic follow the
same schema sensitivity rules and never print sensitive values. Provider
`private` blobs remain opaque and are not inspected; providers must not rely on
tchori to redact secrets stored there.

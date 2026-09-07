# Format reference: plan.json and state.json

tchori has exactly two machine-readable, on-disk artifacts: the plan
document (`plan.json`, or whatever path `-out` names) and the state file
(`state.json`, fixed name in the working directory). Both are schema-
versioned JSON designed to be reviewed by humans and agents. Attribute and
field ordering is stable; encrypted private payloads use fresh random nonces,
so ciphertext is intentionally not byte-deterministic. This page documents
implemented fields, semantics, and guarantees, not a design proposal.

Source of truth: `internal/plan/plan.go`, `internal/plan/planner.go`,
`internal/state/state.go`, and their tests.

## File purposes

| File | Written by | Read by | Purpose |
| --- | --- | --- | --- |
| `plan.json` | `tchori plan -out FILE`, `tchori destroy -out FILE` | `tchori apply FILE` | The reviewable, PR-able artifact: the exact changes an apply will execute plus optional informational refresh drift. There is no plan-less apply. |
| `state.json` | `tchori apply`, `tchori import`, `tchori state sanitize` | `tchori plan`, `tchori apply`, `tchori state list/show/status`, `tchori mcp` | The record of what tchori believes is deployed, keyed by resource address. |

## Unresolved-reference safety

References use the exact whole-string form `${type.name.attr}`. If a
reference-shaped `${...}` fragment survives in a resolved resource config,
provider config, or planned value that tchori is about to send to a provider
RPC, tchori refuses the call with an `unresolved reference` error. This guard
applies to validate, plan, apply (including replace before its destroy leg),
and provider configuration. As a result, tchori never persists a matching
value that it composed and sent itself.

Replacement planned values are decoded and validated before the destroy leg.
Corrupt MessagePack, null resource roots, and unresolved references refuse the
replacement without deleting the existing object.

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
| `format_version` | string | Plan document schema version. New writes use `"1.2"` (`plan.FormatVersion`). |
| `engine_version` | string | The tchori binary version that produced the plan (e.g. `"0.1.0-dev"`), from `internal/version.Version`. |
| `state_serial` | integer | The state file's `serial` at the moment this plan was computed (`p.State.Serial`). `apply` compares this against the live state's serial to detect staleness — see below. |
| `changes` | array of `Change` | Always sorted by `address` (`plan.finalize`). Document order is for byte-stability only; it carries no dependency information (`apply.Apply`'s ordering notes call this out explicitly — execution order comes from the config's topological sort, not from this array). |
| `drift` | array of `Drift`, omitted if empty | Informational out-of-band differences between the value recorded in `state.json` and the value returned by refresh. Sorted by `address`. `apply` ignores this field; it never contributes to `summary` or `HasChanges()`. |
| `summary` | object | Counts of `create`/`update`/`delete`/`replace` changes. All four keys are always present, even at zero (`Summary` has no `omitempty` tags). `no-op` changes are never counted. |

### Change fields

| Field | JSON type | Meaning |
| --- | --- | --- |
| `address` | string | Resource address, `type.name` (e.g. `tchoritest_thing.a`). |
| `type` | string, omitted when absent in legacy input | Provider resource type; required for every executable change and checked against the execution target. |
| `provider` | string, omitted when absent in legacy input | Provider local name; required for every executable change and checked against the execution target. |
| `provider_source` | string, omitted when absent in legacy input | Canonical registry source of the provider selected at plan time; required for every executable change and authenticated with private data. |
| `action` | string | One of `create`, `update`, `delete`, `replace`, `no-op` — see Action semantics below. |
| `before` | object or `null` | Prior public projection. `null` for `create`; sensitive-set arrays retain one entry per authoritative element. |
| `after` | object or `null` | Planned public projection, with every attribute unknown at plan time rendered as JSON `null`. `null` for `delete`; projected set duplicates are retained. |
| `unknown_after` | array of strings, omitted if empty | Dotted attribute paths inside `after` whose real value won't be known until apply (see Unknowns below). |
| `requires_replace` | array of strings, omitted if empty | Attribute paths the provider says force replacement *if their value differs from prior*. Presence here does not by itself mean `action` is `replace` — see Action semantics. |
| `planned_raw` | base64 string, omitted if empty | The exact provider-planned shape, msgpack-encoded via `cty/msgpack`. Provider unknowns remain unknown. Sensitive non-exempt leaves are also encoded as unknown, including inside sets, so config/env secrets are not frozen into the executable artifact. |
| `private` | object, omitted if empty | Authenticated encrypted envelope for opaque provider recovery data; decrypted bytes are passed unchanged to provider RPCs. Legacy `1.0` used a plaintext base64 string. |

`before` and `after` are deterministic JSON projections. A set is sorted by
its projected element bytes but duplicate projections remain duplicate array
entries, preserving cardinality when elements differ only in sensitive leaves.
`planned_raw` is base64-encoded MessagePack. Current `private` is an envelope
with integer `version: 1`, base64 `nonce` (12 bytes), and base64 `ciphertext`
(including the 16-byte authentication tag). It uses AES-256-GCM and
authenticates the resource address, resource type, provider alias, canonical
provider source, and artifact kind as additional data. It is not
interchangeable with `planned_raw`, nor may a plaintext private string appear
in a `1.1` or `1.2` document.

### Drift fields

| Field | JSON type | Meaning |
| --- | --- | --- |
| `address` | string | Resource address whose refreshed representation differs from the recorded state. |
| `before` | object | The reporting-safe attributes recorded in `state.json` before refresh. |
| `after` | object or `null` | The reporting-safe refreshed attributes, or `null` when the provider reports that the object no longer exists. |
| `paths` | array of strings, omitted if empty | Sorted changed leaf paths. A vanished object has no paths because the whole object is absent. |

A drift entry is a refresh observation, not an action. It is preserved by
`plan.Write`/`plan.Read` and returned by the MCP `plan()` tool, but apply does
not consume it. Sensitive leaves in `before` and `after` use the same
schema/config-driven redaction as change values. A drift-free plan omits the
field completely, preserving the bytes written before this field existed.

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
| `replace` | `requires_replace` is non-empty **and** the comparison value differs from prior on at least one of those paths (an unknown planned value on such a path counts as differing — the provider cannot promise it stays the same). |
| `update` | The comparison value differs from prior, but not on a path that forces replacement. |
| `no-op` | Prior and planned values are equal after masking ordinary sensitive leaves. Sensitive leaves inside sets remain part of the in-memory comparison because they determine membership. |

`no-op` changes are listed in `changes` (so the document always accounts for
every config resource) but never counted in `summary`, and `tchori plan`'s
human-readable stdout output filters them out — only the JSON document keeps
them.

### Human plan output

Human `plan` and `destroy` output keeps one action-symbol header per non-no-op
change and prints changed leaf attributes under every non-delete header.
Creates use `+`, removals use `-`, and updates use `~ before -> after`.
Provider unknowns render as `(known after apply)`, replacement paths end with
`# forces replacement`, and delete changes print only their header. Strings
are quoted so `""` and `null` remain distinct; strings longer than 120 runes
are truncated with an ellipsis and their full rune count.

Schema-sensitive paths, including leaves under sensitive nested attributes,
render as `(sensitive value)` on both sides. If an address cannot be resolved
to a schema, human rendering fails closed and redacts its values. Drift uses
the same formatting, truncation, and redaction path. It appears before planned
changes under `Note: objects have changed outside tchori since the last
apply.` A vanished object is shown as `<address> (object no longer exists)`.
The note is printed even for a drift-only plan, followed by the existing `No
changes. Configuration matches state.` line.

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
also encoded there as unknown rather than concrete values. A sensitive set
keeps every unknown-bearing element, so add/remove/member-change transitions
remain distinguishable without persisting the secret. `after` is the
reviewable JSON projection; `planned_raw` is the executable one.

At apply time, an unknown left over from planning that turns out to be a
`${...}` reference to another resource created earlier in the same run is
resolved against that resource's real, just-applied value before the provider
is called. Objects, maps, lists, and tuples use their key or positional
correspondence. Sets have no stable positional correspondence, so tchori never
substitutes raw configuration for a reviewed unknown at or below any set,
whether sensitive or not. It asks the provider to plan again with the concrete
configuration and builds the set compatibility graph once; augmenting-path
matching must find a perfect multiset match against every reviewed known value
and collection membership. Replacement paths and the shared value-sensitive
action classification must also remain identical. Any divergence fails before
the provider's apply RPC, including the destroy leg of a replacement.

### Exit-code contract

| Command | 0 | 2 | 1 |
| --- | --- | --- | --- |
| `tchori plan [-out FILE]` | no changes (`Plan.HasChanges()` false) | changes pending | error (config/provider/runtime failure; diagnostics on stderr) |
| `tchori destroy -out FILE` | nothing to destroy | destroy plan has deletions | error |
| `tchori apply PLANFILE` | applied successfully | *(not used — apply is terminal)* | error: stale plan, configuration drift, or a provider apply failure |
| `tchori state status` | state is converged | *(not used)* | state carries `incomplete_apply`, or cannot be read |

`HasChanges()` is simply `create + update + delete + replace > 0` from
`summary` — `no-op`-only and drift-only plans exit `0`. Drift is output, not a
diagnostic: it does not change `HasErrors()` or any exit code.

Diagnostics do not alter this exit-code contract. Every provider-RPC failure
carries the resource or provider address that issued the RPC. Error-severity
`planned change not executed` diagnostics provide per-address
[failure-isolation accounting](#failure-isolation-at-apply), while the
warning-severity `attempted change` diagnostic records values sent in a failed
update. See the [diagnostic contract](diagnostics.md)
for the JSON shape, pretty rendering, address qualification, and advisory
non-JSON-response hint.

### format_version compatibility

`plan.Read` accepts current format `"1.2"`, encrypted-private format `"1.1"`,
and legacy `"1.0"` for migration. Missing, empty, and unsupported versions are
rejected rather than guessed. New writes always use `"1.2"`. This prevents an
older engine from applying a plan and then coalescing sensitive set elements
while it writes state.

The optional `drift` field remains informational and ignored by apply.
Authenticated private storage drove `1.1`; identity-safe sensitive set
persistence drives `1.2`.

Legacy plans, and early `1.1` plans, with private data but no complete bound
type/provider/source identity remain readable for diagnosis but cannot be
applied: recompute the plan with this engine. Apply requires complete identity
for every executable change, even when its private payload is empty, and
compares it against live config/state routing before any checkpoint or provider
mutation. Changing an alias or its canonical source cannot redirect
authenticated private data to another provider.

### Example

Generated from a real `plan.Planner.Plan()` run against the in-repo test
provider (`tchoritest`, prefix `demo-`), for the two-resource config from the
README quickstart plus a `tags` reference so a nested unknown is visible
(`tchoritest_thing.b.tags.parent` references `tchoritest_thing.a.id`, which
doesn't exist yet on a first plan):

```json
{
  "format_version": "1.2",
  "engine_version": "0.1.0-dev",
  "state_serial": 0,
  "changes": [
    {
      "address": "tchoritest_thing.a",
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "provider_source": "tchori-labs/tchoritest",
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
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "provider_source": "tchori-labs/tchoritest",
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
| `format_version` | string | State document schema version. New writes use `"1.3"`. |
| `serial` | integer | Monotonically incremented once per successful `Save` call — see Serial semantics below. |
| `resources` | object | Map of resource address (`type.name`) to `ResourceState`. |
| `incomplete_apply` | object, omitted when converged | Durable evidence that the last apply did not complete; see below. |

### ResourceState fields

| Field | JSON type | Meaning |
| --- | --- | --- |
| `type` | string | Provider resource type, e.g. `tchoritest_thing`. |
| `provider` | string | Provider local name from config, e.g. `tchoritest`. |
| `provider_source` | string, omitted in legacy/early `1.1` input | Canonical provider registry source. New state binds this value into encrypted-envelope authentication and checks it before provider RPCs. |
| `attributes` | object | Deterministic public projection of applied values. Every withheld sensitive leaf is JSON `null`; sensitive sets retain element count and non-sensitive association as array entries. A sensitive map inside a captured set is withheld as a whole so its keys do not leak. A sensitive map above an affected set is also whole-value `null` and uses authenticated recovery. State never stores unknown values. |
| `private` | object, omitted if empty | Authenticated encrypted envelope with `version`, `nonce`, and `ciphertext`, as described for plans. The opaque plaintext is preserved only in memory for provider RPCs. |
| `sensitive_set_recovery` | object, omitted if empty | Separate authenticated encrypted envelope containing complete outermost sets whose identity depends on sensitive descendants and any smallest sensitive map boundary needed to hide dynamic keys above such a set. It is opened before typed state decoding and never included in read projections. |
| `sensitive_recovery_version` | integer | Mandatory per-resource sensitive projection generation in state format `1.3`; `0` identifies a legacy projection and `3` identifies the current recovery contract. Presence is the non-downgradable resource boundary; nonzero values are also authenticated as part of the recovery envelope's AES-GCM additional data. |
| `redacted` | array of strings, omitted if empty | Sorted paths whose values are withheld, explaining why the corresponding `attributes` leaf is `null`. |
| `sensitive_paths` | array of strings, omitted if empty | Sorted effective, index-insensitive sensitivity contract. It survives config removal and drives backups, delete plans, orphan handling, and provider-free read masking. |
| `sensitive_scanned` | boolean, omitted when false | Provenance marker set after live schema/config resolution, including for a definitively non-sensitive resource. Read surfaces use it to distinguish checked entries from legacy entries with unknown provenance. |

Standalone resource JSON used by CLI/MCP read surfaces omits both encrypted
fields and the state-only generation marker. The state document serializer,
not an ordinary resource JSON dump, writes provider-private and sensitive-set
recovery envelopes plus the mandatory marker.

The recovery envelope has a purpose distinct from provider `private` and
authenticates the resource address, type, provider alias, canonical source, and
nonzero `sensitive_recovery_version` as AES-GCM additional data. Its plaintext
is versioned and contains a SHA-256 digest of the exact public projection plus
structured attribute/map/list paths to msgpack-encoded authoritative values.
Recovery payload version 3 stores outermost affected sets and sensitive map
boundaries in `values`; structural collection traversal distinguishes
`map(set(...))` from `set(map(...))`, and map recovery preserves null, empty,
keys, structure, and nested set membership exactly. Version 2 stores sets in
`sets` and records the sensitivity paths that produced the projection, so
expanding sensitivity can authenticate and restore old set membership before
emitting a new projection/recovery pair. Version 1 payloads use the resource's
recorded `sensitive_paths` for the same migration. Valid version 1 and 2
projections are checked with their generation-time map projection semantics,
then rotated to the confidential version 3 representation.
Restoration validates the generation marker, generation paths, complete
recovery path set, projection digest, and typed generation-time projection
before replacing public placeholders and decoding the authoritative cty value.
Moving either envelope to another address/type/source/purpose, changing the
key or generation marker, editing the public projection, or removing recovery
required by a current-generation marker fails before a provider mutation or
state checkpoint.

### Incomplete apply lifecycle

`incomplete_apply` contains `failed_address` (omitted while a run is still in
flight), `applied`, and `remaining`. The two lists are always JSON arrays,
including when empty. The record contains resource **addresses only**: never
attribute values, provider responses, private data, diagnostic details that
might echo values, or timestamps.

For every non-empty apply, tchori writes this marker to disk **before the first
provider call**. If that pre-flight save fails, apply refuses to issue any
provider request. Per-change saves preserve the marker while work proceeds. On
a failed run, a final save records the first failed address and the exact
completed/unfinished split after independent work; on full success, a terminal
save removes the marker.
A process killed mid-run or a failed finalizing save therefore still leaves an
artifact that admits it is non-converged. Stale-plan, configuration-order, and
configuration-drift refusals write nothing. A zero-change apply writes no new
marker, although it does clear a stale marker from an earlier run.

An early `1.1` resource with no `provider_source` remains readable so operators
can inspect and migrate it, but planning/apply refuse to send its state or
private bytes to a provider. `tchori state sanitize` validates the stored
type/provider alias against live configuration and schema, binds the canonical
source, and re-encrypts state and backup under the stronger identity. A
nonempty stored source that differs from resolved configuration is refused
before provider discovery and before state or backup mutation; it is never
rewritten as a migration. A nonempty redacted sensitive set without recovery is
rejected explicitly: membership cannot be reconstructed safely from the public
projection.

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
  `format_version: "1.3"`, `serial: 0`, `resources: {}` — not an error.
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

Apply and import hold the same state sidecar lock for their complete mutating
operation, including provider RPCs and every checkpoint. Lock acquisition is
bounded by ten seconds and observes command cancellation. The serial is
checked after acquiring the lock, before remote mutation. Saves within the
operation reuse the held lock; concurrent writers cannot interleave remote
side effects between state checkpoints.

`Save` (`internal/state/state.go`):

1. Reuses the operation's flock-based lock at `path+".lock"` or acquires one
   for the individual save, polling every 50ms up to a 10-second timeout.
   A standalone save releases its own lock via `defer`.
   If the name exists, acquisition requires it to be a regular file on every
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
   When schema is available, sensitive-set recovery is authenticated, restored,
   and reprojected with a matching rotated recovery payload. Without schema, an
   existing public-projection/recovery pair is preserved byte-for-byte rather
   than changing one half and invalidating the other. With no known sensitive
   path the copy stays byte-identical; otherwise it is canonically re-serialized.
   Existing `redacted` markers are unioned with newly changed paths. A parse or
   envelope-authentication failure aborts rather than copying uninspected bytes.
   The backup deliberately applies no literal-instance exemptions and retains
   previously persisted paths, so the prior document is scrubbed under the
   rules that wrote it even when the current declaration was removed. Because
   apply performs bracketing saves, the backup left by a successful apply
   normally contains a marker-carrying intermediate, not the pre-apply state.
   The fresh-temp-and-rename symlink, directory, and `0600` hardening remains
   unchanged.
4. Sanitizes every live state entry, including resources untouched by this
   apply. Current schema/config paths are unioned with persisted paths and
   effective hints: removing a declaration does not declassify stored secrets.
   Live resolution restores affected sets under their recorded generation
   paths before typed decoding, then emits a new public projection and recovery
   payload under current policy. It honors per-instance literal exemptions
   outside sets. An unresolved entry without set recovery falls back to
   persisted paths and hints without exemptions and is reported. An unresolved
   entry with set recovery is rejected: schema is required to keep the
   authenticated pair valid. An empty combined path set is definitively
   non-sensitive only after successful resolution, which records
   `sensitive_scanned`.
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

### Stable ordering and encrypted payloads

Both files use two-space JSON indentation and a trailing newline. Plan changes
are sorted by address; state resource map keys are sorted during JSON
encoding. Incomplete markers have no timestamp. Public set projections are
sorted by projected element bytes while retaining duplicates. These guarantees
keep the reviewable structure stable rather than depending on map or
secret-dependent set iteration order.

Provider-private and sensitive-set recovery encryption use fresh random nonces
on each serialization and distinct authenticated purposes. Both bind resource
address, type, provider alias, and canonical source. Identical plaintext
therefore produces different ciphertext; a ciphertext-only diff does not imply
infrastructure drift. No deterministic nonce is derived from content, serial,
or address. Artifacts with neither encrypted payload remain deterministic.

### format_version compatibility

`state.Load` accepts current `"1.3"`, recovery-envelope format `"1.2"`,
encrypted-private format `"1.1"`, and legacy `"1.0"`; it rejects missing,
empty, and unsupported versions. A nonexistent file instead yields a fresh
empty state. Every new save upgrades the document and backup to `"1.3"`.
Format `1.1` introduced encrypted provider-private storage. Format `1.2`
added encrypted sensitive-set recovery. Format `1.3` requires every resource
to carry `sensitive_recovery_version`, so deleting both a current recovery
envelope and its generation marker cannot be interpreted as legacy state.
Older readers refuse newer plans/state rather than silently discarding or
coalescing authoritative membership.

### Example

The state produced by applying the plan.json example above (test provider,
prefix `demo-`):

```json
{
  "format_version": "1.3",
  "serial": 4,
  "resources": {
    "tchoritest_thing.a": {
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "provider_source": "tchori-labs/tchoritest",
      "attributes": {
        "echo": "alpha",
        "id": "demo-id-alpha",
        "name": "alpha",
        "replace_me": null,
        "tags": null
      },
      "sensitive_recovery_version": 0
    },
    "tchoritest_thing.b": {
      "type": "tchoritest_thing",
      "provider": "tchoritest",
      "provider_source": "tchori-labs/tchoritest",
      "attributes": {
        "echo": "beta",
        "id": "demo-id-beta",
        "name": "beta",
        "replace_me": null,
        "tags": {
          "parent": "demo-id-alpha"
        }
      },
      "sensitive_recovery_version": 0
    }
  }
}
```

Note `serial: 4`: apply saved the pre-flight marker, saved once after each of
the two resources applied, then saved once more to clear the marker. Planning
against this state again produces two `no-op` changes and exits `0`.

## Staleness and configuration drift at apply

This section's **configuration drift** means that config changed after a plan
was written. It is distinct from the plan document's informational `drift`
array, which records provider refresh differences from `state.json`.

`apply.Apply` refuses to run, entirely and before any provider call or
state save, in two situations:

1. **Stale plan.** `pl.state_serial != st.Serial` — the state has moved on
   since this plan was computed (someone else applied in the meantime, or
   it's simply an old plan file). This check runs before the first state
   save of the apply, because `Save` itself increments `Serial` — comparing
   after any save would compare against a serial the apply itself just
   changed. The error names both serials and says to plan again.
2. **Configuration and resource-identity drift.** Every non-delete change's
   address must still exist in freshly loaded configuration. Every executable
   change must record type, provider alias, and canonical provider source even
   when private data is empty. Apply compares all three values against the
   provider selected by live configuration (or the configured provider behind
   a state-only delete), and separately checks stored state identity before any
   provider call. Missing legacy source metadata directs the operator to
   `tchori state sanitize`; changed routing directs the operator to recompute
   the plan. Every refusal occurs before the state lock/save boundary.

Both refusal classes are exit code `1`, with a structured diagnostic on stderr
naming the problem.

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

## Failure isolation at apply

Apply does not stop the entire run when one change fails. Creates, updates, and
replaces continue unless a failed or already-blocked resource appears anywhere
in their transitive dependency closure. Config-known deletes use the reverse
rule: a delete is blocked when a transitive dependent failed or was blocked,
so dependencies are never destroyed while a failed dependent still needs them.
Independent changes continue in the existing deterministic order, and every
successful provider result is saved to `state.json` before execution advances.

A delete for an address removed from configuration is always attempted. Such
an address cannot have a configuration-side dependent: configuration ordering
rejects any reference to an undeclared resource before apply begins. This is
why an unrelated failing create or update cannot starve a planned state-only
delete.

Every blocked change produces its own error-severity `planned change not
executed` diagnostic. The diagnostic is addressed to that resource and names
its planned action and the failed or blocked address that prevented execution.
The provider diagnostic for every attempted failure remains verbatim and at
its original severity. Nothing is silently skipped.

The stdout outcome line reports completed work, not the plan document's
summary. A successful run prints `Apply complete` (or `Destroy complete`) with
executed counts. A run containing errors still prints `Apply incomplete` (or
`Destroy incomplete`) with executed create, update, delete, and replace counts
plus the number of dependency-blocked changes, then exits `1`. A provider call
that failed was attempted but is neither reported as completed work nor as
"not executed". If a failed destroy response contains a decodable authoritative
state, tchori checkpoints it before continuing failure handling: explicit null
removes the state entry, while a changed non-null state and its private bytes
replace the prior entry. The action remains unfinished and uncounted. A
consistency error after a provider result was durably recorded does count that
executed mutation while still making the run fail.

Apply's durable incomplete marker records the first failed address, all
successfully completed addresses, and every failed or blocked address still
unfinished after independent work runs. A replace whose destroy leg returned
null with an error remains absent from durable state but is not counted as a
completed replacement. Every failed run that enters execution advances the
state serial when it records the incomplete marker, so run `tchori plan` before
retrying.
Stale-plan, configuration-ordering, and configuration-drift refusals happen
before the execution loop and therefore have no partial outcome.

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
adds the surrounding failure-isolation and attempted-value accounting; it does
not build, inspect, or repair the HTTP payload constructed inside a third-party
provider. The attempted-change addition is a warning and does not change
`HasErrors()`. A dependency-blocked change is an error in its own right; the
original provider error already makes apply exit `1` under the
[exit-code contract](#exit-code-contract).

## Sensitivity

Tchori derives sensitive paths from provider schema `Sensitive` flags plus a
resource's optional `sensitive_attributes` list. Provider-computed values at
those paths are withheld from state and plan artifacts. State stores a typed
JSON `null` plus the three metadata fields above; plans replace the value with
an unknown in both `after` and decoded `planned_raw`, list it in
`unknown_after`, and mask it during update/replacement classification so a
withheld computed value does not cause a perpetual diff.

Sensitivity matching derives logical paths directly from structured cty path
steps, so brackets, quotes, backslashes, or Unicode in map keys cannot alter the
schema path being redacted. The raw-literal exemption is a fully
index-qualified instance bound to the exact authored scalar value. Thus one
repeated-block element may retain an authored literal while a sibling
containing a `${...}` reference is withheld; a provider-substituted value at the
same instance is redacted. References, explicit nulls, absent values, and
`{"env":"..."}` wrappers are never exempt.

Set elements have no stable identity after a sensitive leaf becomes null or
unknown: distinct elements can become equal and coalesce. Tchori therefore
retains each redacted public element and separately encrypts the authoritative
outermost affected set. Flat cty types are traversed independently of provider
`nested_type` metadata, including sets nested in objects, maps, lists, tuples,
or other sets; protocol 5 and flat protocol 6 schemas therefore use the same
boundaries. Provider-declared sensitivity on the whole set, an explicit
whole-set declaration, and a sensitive ancestor all use the same recovery
mechanism. Per-attribute sensitivity inside provider `nested_type` objects,
lists, maps, and sets is retained recursively.
Sensitive maps are withheld as whole values rather than exposing their dynamic
keys with null leaves. Inside a captured set the set recovery already preserves
the map. Above a captured set, the smallest sensitive map boundary is itself
authenticated and recovered, including null versus empty.

Every save sanitizes the whole state document. Live, resolvable entries use the
provider schema's cty type, preserving map elements even when a map key matches
an attribute name, and honor only value-bound literal exemptions. Current
sensitivity is unioned with previously recorded paths, so removing a
declaration cannot restore a previously withheld secret. Backups, delete
`before` values, orphans, and read rendering are conservative path-level copies
with no exemption.

Refresh drift and state-only delete projections are derived from authenticated,
restored typed state before applying the set-aware public projection. A
legitimate recovered set is not treated as legacy plaintext; legacy plaintext
detection remains a separate comparison against the persisted public bytes.
Typed masked comparison still reports genuine hidden membership drift.

Default `state show` and MCP `state_show` mask persisted `sensitive_paths`
and legacy `redacted` hints in memory and never save or launch providers. An entry with
`sensitive_scanned: true` and no paths is known non-sensitive. A legacy entry
with neither field cannot be classified through that provider-free path,
which warns rather than claiming the entry is safe.

`tchori state show ADDRESS --discover-sensitive` opts into provider/schema
discovery and masks legacy output without writing state or refreshing remote
resources. `tchori state sanitize` resolves sensitivity and rewrites current
state plus its backup without applying infrastructure changes. It preserves
the state lock, serial/CAS and durability contract. Schema discovery rejects
resource type/provider mismatches and attributes incompatible with the schema
without writing. Hinted orphans are conservatively scrubbed in state and backup,
but sanitization exits `1` because their remaining values could not be checked.
Entries with neither configuration nor hints are rejected without writing.
Normal plan and no-op apply are not legacy cleanup commands. Previously
committed plaintext still requires credential rotation and repository-history
cleanup.

The consistency diagnostic and the
[`attempted change`](#failure-isolation-at-apply) diagnostic follow the
same schema sensitivity rules and never print sensitive values. Provider
`private` blobs remain opaque in memory, but their artifact representation is
authenticated encryption, not redaction or plaintext base64. See
[artifact key management](configuration.md#artifact-encryption-key).

When an apply RPC reports errors alongside a valid, changed partial resource
state, the create/update leg checkpoints that state and private recovery data.
The apply remains incomplete, the change is not counted as completed, and
dependent changes remain blocked. Missing or undecodable error-response state
does not fabricate a replacement state. Deferred read, plan, and import
responses are rejected explicitly rather than interpreted as deletion or an
executable plan.

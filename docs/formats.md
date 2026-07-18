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
keys, e.g. `echo`, `id`, or `tags["parent"]` for a map key. The exact,
non-null planned value (unknowns included) is preserved separately in
`planned_raw` for apply to use — `after` is the reviewable, JSON-native
view; `planned_raw` is the executable one.

### Exit-code contract

| Command | 0 | 2 | 1 |
| --- | --- | --- | --- |
| `tchori plan [-out FILE]` | no changes (`Plan.HasChanges()` false) | changes pending | error (config/provider/runtime failure; diagnostics on stderr) |
| `tchori destroy -out FILE` | nothing to destroy | destroy plan has deletions | error |
| `tchori apply PLANFILE` | applied successfully | *(not used — apply is terminal)* | error: stale plan, configuration drift, or a provider apply failure |

`HasChanges()` is simply `create + update + delete + replace > 0` from
`summary` — `no-op`-only plans exit `0`.

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

### ResourceState fields

| Field | JSON type | Meaning |
| --- | --- | --- |
| `type` | string | Provider resource type, e.g. `tchoritest_thing`. |
| `provider` | string | Provider local name from config, e.g. `tchoritest`. |
| `attributes` | object | ctyjson-encoded object of the resource's real, applied attribute values. Unlike a plan's `after`, everything here is concrete — state never stores unknowns. |
| `private` | base64 string, omitted if empty | Opaque per-resource provider private data, round-tripped through the provider's plan/apply RPCs untouched by tchori. |

### Serial semantics

- `state.Load` on a missing path returns a fresh, empty state:
  `format_version: "1.0"`, `serial: 0`, `resources: {}` — not an error.
- Each successful `Save` increments `Serial`, regardless of whether the
  resource data actually changed. A save rejected because another process
  committed from the same base does not increment it.
- Apply saves state after *each* successfully applied resource, not once
  per `apply` invocation — so an apply that creates two resources bumps
  `serial` by 2 (visible in the worked example below: two creates take the
  file from serial 0 to serial 2). This is also what makes partial-apply
  safety possible: if a later resource in the same apply fails, everything
  already applied is already saved under its own incremented serial.

### Locking, backup, and durability behavior

`Save` (`internal/state/state.go`):

1. Acquires an flock-based lock at `path+".lock"` (`github.com/gofrs/flock`),
   polling every 50ms up to a 10-second timeout, and releases it via
   `defer`.
2. Re-reads the on-disk serial and compares it with the base serial observed
   by `Load` or the preceding successful `Save`. If another process committed
   in the meantime, `Save` returns `state.ErrConcurrentModification` with the
   state path and re-run guidance; neither the state nor its backup is touched.
3. Copies the current file at `path` to `path+".backup"` before overwriting
   — a no-op on the very first save, since there's nothing to back up yet.
4. Increments `Serial`, marshals with `MarshalIndent`, writes a temp file
   (`.state-*.tmp`) in the same directory, and fsyncs the complete file before
   closing it.
5. Atomically renames the temp file over `path`, then fsyncs the containing
   directory before reporting success. Failures before rename remove the temp
   file and leave the in-memory serial unchanged. A post-rename directory-sync
   failure is returned without deleting the newly committed state; the
   in-memory serial and compare-and-swap base advance to match that visible
   replacement so a retry does not report a false concurrent modification. A
   successful commit becomes the next base, so apply's per-resource saves can
   continue sequentially.

Together, the file and directory fsync barriers mean a `nil` return confirms
both the state contents and the atomic directory-entry replacement reached
stable storage across abrupt process or host failure.

### Determinism

- Both files are written with `encoding/json.MarshalIndent(v, "", "  ")`
  plus a trailing newline.
- `plan.json`'s `changes` array is explicitly sorted by `address`
  (`plan.finalize`).
- `state.json`'s `resources` is a Go map, but `encoding/json` always
  marshals map keys in sorted order — so the file's byte content does not
  depend on Go map insertion order. `TestSaveDeterministicAcrossInsertionOrder`
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
(it synthesizes a fresh empty state instead).

### Example

The state produced by applying the plan.json example above (test provider,
prefix `demo-`):

```json
{
  "format_version": "1.0",
  "serial": 2,
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

Note `serial: 2`, not `1`: `apply` saved once after `tchoritest_thing.a`
applied and again after `tchoritest_thing.b` applied. Planning against this
state again produces two `no-op` changes and exits `0`.

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

## Sensitivity

Provider responses are stored verbatim in `state.json` and `plan.json`, so
values a provider *derives* from env-sourced secrets can end up recorded
there too — treat both files as sensitive (redaction is a recorded post-MVP
item, not yet implemented).

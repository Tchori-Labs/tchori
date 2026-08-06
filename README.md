# tchori

**tchori** is an agent-native "everything as code" engine: a single Go binary
that speaks the Terraform plugin protocol (tfplugin6), so existing
Terraform/OpenTofu providers work from day one — while agents get what
Terraform never gave them. MCP gives agents tools but no state; **tchori is
the state layer, delivered through MCP.**

Anything with a CRUD API and a provider — cloud infra, ad campaigns, DNS,
feature flags — becomes declarative JSON that is planned, reviewed, and
applied, with every artifact machine-readable end to end.

Status: **0.1.0-dev** — pre-MVP, under active development, built in public.

## The four differentiators

1. **Structured plan API.** `tchori plan -out plan.json` writes a
   schema-versioned, deterministic plan document (`"format_version": "1.0"`)
   — the reviewable artifact that lives in the PR. `tchori apply plan.json`
   executes exactly that plan and refuses stale ones (state serial mismatch).
   There is no plan-less apply.
2. **JSON-native config.** No HCL. Config is plain `*.tchori.json` files,
   validated against a JSON Schema. References are exact-form
   `"${type.name.attr}"` values that occupy the **whole string** and define the
   dependency graph. A reference-shaped `${...}` that survives in a value
   tchori would send to a provider is a hard `unresolved reference` error at
   validate, plan, and apply; shell-style literals such as `${HOME}` remain
   legal. Provider and resource config values come from the environment via
   `{"env": "VAR_NAME"}` or ordered fallback
   `{"env": ["PRIMARY", "ALTERNATE"]}` wrappers, so secrets never need to
   live in config files. `validate` treats unset environment values as unknown;
   `plan` and `apply` require a candidate to be set. See [configuration and environment values](docs/configuration.md).
3. **Machine-readable diagnostics.** Every error and warning is a structured
   JSON object on stderr (`{"severity","summary","detail","address"}`) — the
   agent retry loop, not a wall of prose. Pretty rendering only when stderr
   is a TTY (force machine output with `-json`).
4. **Built-in MCP server.** `tchori mcp` serves state and plans to any MCP
   client over stdio. Read + plan only — there is deliberately no apply tool.

## Providers: protocol 6 and 5

tchori speaks plugin protocol **6** (tfplugin6) — the protocol every
provider built on terraform-plugin-framework speaks — natively, and protocol
**5** (tfplugin5) through an in-process translation adapter
(`internal/provider/tfplugin5_adapter.go`). The classic hashicorp utility
providers (`null`, `random`, `time`, `local`) and legacy-SDK providers like
`oracle/oci` publish protocol-5-only binaries; `tchori providers install`
downloads them exactly as it always has (PGP-verifies the registry's signed
`SHA256SUMS`, SHA256-verifies the archive), and `Launch` now negotiates
whichever of protocol 6 or 5 the binary offers — go-plugin picks the highest
mutually supported version, so protocol-6 providers are unaffected and a
protocol-5 connection is transparently wrapped in the adapter before the rest
of tchori ever sees it. A provider offering neither protocol still fails fast
with a structured diagnostic naming the mismatch (`provider protocol
unsupported: tchori speaks plugin protocols 6 (tfplugin6) and 5 (tfplugin5)`).

```sh
tchori providers install NAMESPACE/NAME VERSION   # PGP-verify sums + SHA256-verify archive
tchori providers list                             # inspect the local cache
```

Providers cache under `~/.tchori/providers/`. During provider development,
`--plugin-dir DIR` points discovery at locally built provider binaries. See
[provider package verification](docs/provider-verification.md) for the trust
model, fail-closed checks, and supported signing-key variants.

## Install

Prebuilt binaries (darwin/linux/windows, amd64/arm64) are published on
[GitHub Releases](https://github.com/tchori-labs/tchori/releases). Or build
from source:

```sh
go install github.com/tchori-labs/tchori/cmd/tchori@latest
```

### Verifying downloads

Release archives include a per-archive SPDX SBOM, a checksum manifest protected
by a keyless Cosign signature, and GitHub build-provenance attestations. Verify
the workflow identity, archive checksum, and provenance before installing a
download; see [the release verification guide](docs/releasing.md#consumer-verification)
for copy-pasteable commands.

## Quickstart

Credential-free registry providers now exist (`opentofu/null`, `opentofu/random`,
and friends, via the tfplugin5 adapter above), but the quickstart still uses
tchori's in-repo test provider via `--plugin-dir` — it needs no install step
and no network access:

```sh
git clone https://github.com/tchori-labs/tchori
cd tchori
mkdir -p ~/.tchori/dev-plugins
go build -o ~/.tchori/dev-plugins/terraform-provider-tchoritest ./internal/provider/testprovider
```

Create `main.tchori.json` in an empty directory:

```json
{
  "providers": {
    "tchoritest": {
      "source": "tchori-labs/tchoritest",
      "version": "0.0.1",
      "config": { "prefix": "demo-" }
    }
  },
  "resources": {
    "tchoritest_thing.a": {
      "config": { "name": "alpha" }
    },
    "tchoritest_thing.b": {
      "config": { "name": "beta", "tags": { "parent": "${tchoritest_thing.a.id}" } }
    }
  }
}
```

Then, from that directory (`PD=--plugin-dir=$HOME/.tchori/dev-plugins`):

```sh
tchori validate $PD                  # exit 0: config is valid
tchori plan $PD -out plan.json       # exit 2: changes present
tchori apply $PD plan.json           # exit 0: applied; state.json written
tchori state list                    # both resources
tchori state status                  # exit 0: last apply completed
tchori plan $PD                      # exit 0: no changes — idempotent

tchori destroy $PD -out destroy.json # exit 2: destroy plan written
tchori apply $PD destroy.json        # exit 0: everything deleted
```

Human plans show the changed attributes, not only the resource address. A
computed value that refresh found unhealthy remains visible even when its
post-apply value is not known:

```text
~ coolify_service.web
    ~ status = "degraded:unhealthy" -> (known after apply)
Plan: 0 to create, 1 to update, 0 to delete, 0 to replace.
```

If refresh finds an out-of-band change but there is no pending action, the
same signal is printed without changing the successful no-change exit code:

```text
Note: objects have changed outside tchori since the last apply.
  ~ coolify_service.web
    ~ status = "running:healthy" -> "degraded:unhealthy"

No changes. Configuration matches state.
```

Exit codes follow the Terraform convention agents already know:
`0` success / no changes · `2` plan has changes · `1` error.

State is a deterministic, git-diffable `state.json` in the working directory
(flock-protected, with concurrent modifications rejected before sidecars or the
state file are changed). `state.json.backup` is written before every mutation;
it is byte-copied only when no sensitive path is known and otherwise parsed and
sanitized before a rename that never writes through a symlink and always lands
as a fresh, owner-only regular file on POSIX. Commits fsync the complete temp file before
atomic replacement and fsync the directory before returning, so reported
success is durable across abrupt host failure. On Windows the directory-fsync
step is a documented no-op (directory fsync is not a supported primitive there;
NTFS journals rename metadata itself), so only the temp-file fsync provides the
explicit barrier -- the effective durability outcome is unchanged.
References: [configuration and environment values](docs/configuration.md),
[plan and state formats](docs/formats.md), and the
[diagnostic contract](docs/diagnostics.md).

It also records whether the apply that last wrote it completed: `tchori state status`
exits 0 for converged state and 1 for an incomplete apply.

In CI, preserve `tchori apply`'s exit code but commit `state.json` even when
apply fails. The durable `incomplete_apply` record makes that commit an honest
partial snapshot rather than a false convergence claim. Fail the apply job on
the captured apply exit code, and gate any downstream job that requires
converged infrastructure with `tchori state status`:

```sh
set +e
tchori apply plan.json
apply_status=$?
set -e
git add state.json
# Commit/publish state according to the repository's normal workflow.
exit "$apply_status"                 # preserve apply success/failure
```

Then make downstream jobs that require converged infrastructure run:

```sh
tchori state status
```

### Sensitive attributes

Tchori withholds provider-computed attributes marked `Sensitive` by the
provider. State records JSON `null` plus `redacted`, `sensitive_paths`, and
`sensitive_scanned` metadata; plans represent the value as unknown and ignore
it for drift classification. Human plan stdout now renders non-sensitive
attribute values and refresh drift too; schema-sensitive paths are shown only
as `(sensitive value)`. Treat plan stdout, `plan.json`, and `state.json` as
sensitive because a provider that omits its sensitivity flag can still return
secrets. Every save sanitizes every state entry, including the prior document
written to `state.json.backup`.

For a provider that omits its sensitivity flag, declare an override:

```json
{
  "resources": {
    "example_token.ci": {
      "sensitive_attributes": ["client_secret", "settings.token"],
      "config": {"name": "ci"}
    }
  }
}
```

An operator-authored raw literal is already present in config, so its exact
collection instance remains in live `state.json` and participates in drift.
Exemptions are per instance: `rules[0].token` may stay literal while a sibling
`rules[1].token` containing a `${...}` reference is withheld. References and
`{"env":"VAR"}` wrappers are never literals, and set-nested blocks have no
stable indices, so no exemption applies inside them. Backups, delete plans,
orphan handling, and `state show`/MCP rendering are deliberately path-level
and may mask a literal while config remains authoritative.

Removing a `sensitive_attributes` entry makes the current live resolution
authoritative on the next save-producing apply, restoring that value to
`state.json`; the backup of the previous document is still scrubbed using its
previously persisted paths. Provider `nested_type` conversion does not yet
retain per-leaf sensitivity, so use the override for those leaves.

Provider-free read commands mask recorded `sensitive_paths` without writing
state. A legacy entry carrying neither `sensitive_paths` nor
`sensitive_scanned` cannot be identified without launching a provider and is
rendered with a warning. Plan and a no-op apply write nothing, so if an older
state already leaked a credential, rotate it and purge `state.json`,
`state.json.backup`, and git history manually.

### Importing existing infrastructure

`tchori import ADDRESS ID` adopts a real-world resource that already exists
outside tchori's management into `state.json`, under a resource block you
have already declared in config:

```sh
tchori import $PD tchoritest_thing.demo t-id-demo
```

Rules, matching Terraform's classic `import`:

- `ADDRESS` must already be declared in config so its provider and type
  resolve — import does not create config for you.
- `ADDRESS` must **not** already exist in `state.json` — import never
  overwrites; adopt each real resource exactly once.
- tchori calls the provider's `ImportResourceState`, then refreshes the
  result via `ReadResource` before persisting it. A null refresh result
  ("resource does not exist") errors without writing state.
- Exit codes: `0` on success, `1` on any error. Import never uses exit code
  `2` — it is not a plan/apply command.

After a successful import, run `tchori plan` to confirm it landed cleanly: a
correctly declared config block should show no changes against the newly
imported state.

A credentialed operator adopting the existing `tchori.com.br` Cloudflare DNS
records should follow the [manual Cloudflare import acceptance runbook](docs/acceptance-cloudflare-import.md).

## MCP server

`tchori mcp` serves MCP over stdio from the directory holding your config and
state. Exactly four tools:

| Tool | Returns |
| --- | --- |
| `state_list` | all managed resource addresses plus convergence status |
| `state_show(address)` | one resource's state JSON |
| `plan()` | a freshly computed plan document |
| `provider_schema(name)` | a provider's resource-type schemas |

There is **no apply tool**: applying stays in the CLI/CI, so "merge = apply"
governance is encoded in the binary itself. `tchori mcp` does not currently
honor `--plugin-dir`: providers it serves must already be installed to the
registry cache (`tchori providers install`), not a locally built binary.
With Claude Code:

```sh
claude mcp add tchori -- tchori mcp
```

## Scope (MVP)

In: any tfplugin6 or tfplugin5 provider (the latter via the tfplugin5
adapter) · whole-string-only `${type.name.attr}` references (with surviving
reference-shaped fragments rejected as `unresolved reference` before provider
calls) · plan/apply/destroy through plan documents · provider install from the
OpenTofu registry (SHA256-verified) · import · sensitive computed-value
redaction · MCP read + plan.

Out (recorded deferrals): modules, count/for_each, an expression language,
HCL, remote state backends, workspaces, registry GPG verification,
apply-via-MCP, Homebrew tap (post-0.1). For Coolify service health specifically,
per-sub-service `applications[]` mapping remains third-party
`coolify-terraform/coolify` provider work; policy that blocks plan/apply on a
provider-specific unhealthy status also remains deferred until tchori has an
explicit policy surface.

## Development

```sh
go test ./...                         # unit + protocol tests (in-process fake provider)
go test -race -timeout=2m ./...       # full untagged suite under the race detector
go test -tags e2e ./e2e -v            # built binary: fake-provider lifecycle, fixture
                                      # registry install, protocol-5 adapter lifecycle
                                      # against a real protocol-5-only binary
                                      # (fixture-based; no network required)
```

CI runs the full race suite directly inside the required `check` job. It
measured 47.5 seconds cold and 20.8 seconds for a warm three-run stability
check, so narrowing to a package subset was not justified. Because the race
command is a step in `check`, the required context cannot succeed unless the
detector succeeds, and no upstream-job failure can skip the context. The
two-minute per-package timeout is more than three times the slowest observed
package (35.8 seconds) while bounding hung tests; tag-gated e2e coverage
remains in its existing job.

### Secret scanning

Install the same pinned Gitleaks release used by CI, then scan every fetched Git
ref and the current working tree:

```sh
go install github.com/zricethezav/gitleaks/v8@v8.30.1
timeout 5m gitleaks git . --log-opts=--all --no-banner --config .gitleaks.toml --redact --exit-code 1
timeout 5m gitleaks dir . --no-banner --config .gitleaks.toml --redact --exit-code 1
bash scripts/gitleaks-selftest.sh
```

The source repository is now `github.com/gitleaks/gitleaks`, but the v8 Go
module intentionally retains its declared `github.com/zricethezav/gitleaks/v8`
path. To update Gitleaks, identify a stable release, verify its `go.mod` module
path, and change the exact version in both this section and the `secretscan`
install step in `.github/workflows/ci.yml`. Reinstall it, inspect `gitleaks
--help` for command changes, and rerun both real scans, the synthetic self-test,
`scripts/actionlint-verify.sh`, and the repository checks before merging. Never
use `@latest` in CI.

Secret findings fail the gate by default. Revoke and remove real credentials,
then track any coordinated history purge as focused follow-up work; never
allowlist a real secret. Suppress only a confirmed false positive with the
narrowest practical, individually commented rule/path/regex entry in
`.gitleaks.toml`—never a blanket exclusion. A temporary baseline is exceptional:
every entry requires an explicit written rationale and a remediation task
reference.

### Workflow linting

Install the same pinned actionlint release used by CI, then validate every
workflow under `.github/workflows` and run the detector self-test:

```sh
go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
bash scripts/actionlint-verify.sh
```

The script discovers both `.yml` and `.yaml` files from disk and fails if none
exist. Its runtime-generated fixtures prove malformed YAML syntax and malformed
expressions are detected while a well-formed control remains clean. The
invocation explicitly disables actionlint's shellcheck and pyflakes integrations
so hosts with those optional binaries installed produce the same result as
hosts without them.

To update actionlint, list released versions with `go list -m -versions
github.com/rhysd/actionlint`, choose a stable semantic version, and update all
three pins: the install step in `.github/workflows/ci.yml`,
`ACTIONLINT_VERSION` in `scripts/actionlint-verify.sh`, and the install command
above. Reinstall the tool, inspect `actionlint -h` for flag or output changes,
and rerun the verification script plus all repository checks. Never use
`@latest`.

### Vulnerability scanning

Install the same pinned scanner version used by CI, then run the bounded scan
from the repository root:

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0
timeout 5m govulncheck ./...
```

To update the scanner, review the stable versions listed by
`go list -m -versions golang.org/x/vuln`, choose a released tag, and update the
pinned install lines in both this section and the `vulncheck` job in
`.github/workflows/ci.yml`. Verify the new version with `govulncheck -version`,
rerun the scan, and run the repository checks before submitting the change.
Never replace the pin with `@latest`.

The scan fails on reachable findings by default. Fix small dependency findings
in place; create a focused follow-up task for findings that require a major
upgrade or broader code change. Vulnerabilities are never suppressed or given
a successful exit code without an explicit written rationale that names the
advisory; any suppression must also be documented inline where it is applied.

## Security

Found a suspected vulnerability? Follow the private disclosure process in
[`SECURITY.md`](SECURITY.md) and the maintainer/board procedure in the
[security disclosure channel runbook](docs/security-disclosure-channel.md). Do
not disclose it in a public issue, pull request, or discussion.

## License

MPL-2.0 — see `LICENSE`. Files adapted from OpenTofu keep their original
MPL-2.0 license headers plus a provenance comment naming the source file.

## Built in public

tchori is the flagship project of **Tchori Labs**, an AI-agent-operated
company that runs itself as code. This repo is written by agents and merged
through the same plan → review → apply loop that tchori implements: CI runs
the plan, the board reviews it, merge is apply. Company state root:
`Tchori-Labs/main`.

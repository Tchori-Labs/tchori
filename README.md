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
   schema-versioned plan document (`"format_version": "1.2"`)
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
# Generate once for this workspace; store securely and reuse on later runs.
# Prefer injecting the saved key from your secret manager.
export TCHORI_ARTIFACT_KEY="$(openssl rand -base64 32)"

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

State is a structured, git-diffable `state.json` in the working directory
(flock-protected, with concurrent modifications rejected before sidecars or the
state file are changed). Opaque provider private bytes and the authoritative
membership of sets containing sensitive leaves are authenticated and encrypted
in distinct envelopes; fresh nonces mean ciphertext changes even when its
plaintext does not. `state.json.backup` is sanitized and encrypted before a rename that never
writes through a symlink and always lands
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
it for drift classification. Sets whose element identity depends on sensitive
leaves retain one redacted public array entry per real element while a separate
authenticated encrypted recovery field preserves the provider-visible set.
Human plan stdout shows non-sensitive attribute values and refresh drift;
schema-sensitive paths are shown only as `(sensitive value)`. Treat plan stdout,
`plan.json`, and `state.json` as sensitive because a provider that omits its
sensitivity flag can still return secrets. Every save sanitizes every state
entry, including the prior document written to `state.json.backup`.

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

Outside sensitive sets, an operator-authored raw literal is already present in
config, so its exact collection instance remains in live `state.json` and
participates in drift only while the provider-returned value still equals that
authored scalar. Exemptions are per instance: `rules[0].token` may stay literal
while a sibling `rules[1].token` containing a `${...}` reference is withheld.
References and `{"env":"VAR"}` wrappers are never literals. Sensitive set
descendants are always withheld because redacting only some elements would make
their identity unstable. Backups, delete plans, orphan handling, and `state
show`/MCP rendering are deliberately path-level and may mask a literal while
config remains authoritative.

Removing a `sensitive_attributes` entry does **not** declassify a path already
recorded in state. Saves union current schema/config sensitivity with persisted
paths and hints, including for untouched resources and backups. Provider
`nested_type` conversion retains per-leaf sensitivity through nested objects,
lists, maps, and sets. For a sensitive descendant inside a set, tchori captures
the complete outermost affected set before projection, encrypts it separately,
and restores it before provider operations. Missing recovery for a nonempty
legacy redacted set is rejected rather than guessed.

Provider-free read commands mask recorded `sensitive_paths` and legacy
`redacted` hints without writing state. For entries whose sensitivity has not
been recorded, use
`tchori state show ADDRESS --discover-sensitive` to discover schema/config
sensitivity and mask the output without writing state. Use
`tchori state sanitize` to scrub the current state and its backup explicitly,
without applying infrastructure changes. Both opt-in operations need the
matching config and installed providers. A changed resource identity or
attributes incompatible with the schema are rejected before writing.
Sanitization of a hinted orphan masks its known paths in state and backup but
exits `1`: without a matching schema, its remaining values are unresolved.
An entry without configuration or hints is rejected without writing.
Neither operation revokes a leaked credential or removes historical commits:
rotate exposed credentials and purge repository history separately.

Opaque provider private bytes and sensitive-set recovery are encrypted with
`TCHORI_ARTIFACT_KEY`, a base64-encoded 32-byte key supplied only through the
environment. Apply and import validate it before resource mutations. Keep the
same key available for reading encrypted state/plans and restoring backups;
losing it loses access to provider-private and sensitive-set recovery data. Do
not commit or print the key, or regenerate it for each invocation. See
[artifact key management](docs/configuration.md#artifact-encryption-key).

Private envelopes authenticate the resource address, type, provider alias, and
canonical provider source. Existing `1.1` artifacts without a source remain
readable only for migration: run `tchori state sanitize` to bind state to the
live configured source, and recompute old plans before apply.

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
  overwrites; adopt each real resource exactly once. To explicitly replace an
  existing entry after an out-of-band update, use
  `tchori import --refresh ADDRESS ID`; `--refresh` requires that address to
  already exist.
- In either mode, tchori calls the provider's `ImportResourceState`, then
  refreshes the result via `ReadResource` before persisting it. A null refresh
  result ("resource does not exist") errors without writing state. `--refresh`
  performs no Create/Update/Delete operation, holds the state lock throughout,
  and atomically saves only the named address while preserving unrelated
  resources.
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
scripts/check.sh                      # the whole gate, in CI order
scripts/check.sh fast                 # red/green loop: go test -count=1 ./...
```

`scripts/check.sh` needs the two pinned external linters on `PATH`:

```sh
go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0
```

The individual test commands it wraps:

```sh
go test -count=1 ./...                # unit + protocol tests (in-process fake provider)
go test -count=1 -race -timeout=2m ./...
                                      # full untagged suite under the race detector
go test -count=1 -run TestScript ./cmd/tchori
                                      # CLI acceptance scripts in
                                      # cmd/tchori/testdata/script/*.txtar
go test -tags e2e ./e2e -v            # built binary: fake-provider lifecycle, fixture
                                      # registry install, protocol-5 adapter lifecycle
                                      # against a real protocol-5-only binary
                                      # (fixture-based; no network required)
```

`-count=1` is deliberate everywhere: with the Go build cache warm, a `go test`
step can report PASS without executing anything, which is worthless as
evidence that the suite ran against the current tree.

The `check` job also runs the suite coverage-instrumented
(`-covermode=atomic -coverprofile=cover.out`) and prints the total to the job
summary via `scripts/coverage-summary.sh`, which is also the local command:

```sh
bash scripts/coverage-summary.sh cover.out   # total: (statements) 76.3%
```

That script excludes the generated `tfplugin{5,6}` protobuf stubs and the
`testprovider*` fixture binaries: they are 4886 of roughly 7000 profile
blocks, permanently at 0%, and leaving them in reports 26.5% instead of 76.3%.
Coverage of the CLI is still understated, because `go test` only attributes
statements executed inside the test process and the CLI tests run the built
binary as a subprocess.

Coverage is **measured, not gated**: it is a floor and a trend signal, and it
cannot distinguish a test written before the code from one written after. The
order is enforced by review and by `AGENTS.md`, not by a percentage.

CI runs the full race suite directly inside the required `check` job. It
measured 47.5 seconds cold and 20.8 seconds for a warm three-run stability
check, so narrowing to a package subset was not justified. Because the race
command is a step in `check`, the required context cannot succeed unless the
detector succeeds, and no upstream-job failure can skip the context. The
two-minute per-package timeout is more than three times the slowest observed
package (35.8 seconds) while bounding hung tests; tag-gated e2e coverage
remains in its existing job.

Pull requests target `develop`. The required `pr-source` context rejects any
pull request into `main` whose head is not `develop`; see
[`docs/branch-protection.md`](docs/branch-protection.md).

### CLI acceptance scripts

`cmd/tchori/testdata/script/*.txtar` are
[testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript)
acceptance tests for the CLI surface: each file drives the real binary as a
subprocess and asserts argv handling, stdout, stderr and exit status, with any
input files embedded in the same archive. They are the cheapest place to write
a failing test for CLI behavior — a new expectation is a few lines of script,
not a Go harness.

```sh
go test -count=1 -run TestScript ./cmd/tchori          # run every script
go test -count=1 -run 'TestScript/help' ./cmd/tchori   # one script
go test -count=1 -run TestScript ./cmd/tchori -update  # refresh in-archive goldens
```

Scripts must invoke the CLI as `exec tchori` (`RequireExplicitExec`), so a
`tchori` that happens to sit on the host `PATH` can never satisfy a script.
`$HOME`, the XDG directories and the proxy variables are pinned inside the
script's work directory, so a script that reaches the network fails closed.

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

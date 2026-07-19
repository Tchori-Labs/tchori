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
   `"${type.name.attr}"` string values and define the dependency graph.
   Secrets come from the environment via `{"env": "VAR_NAME"}` wrappers —
   they never live in config files.
3. **Machine-readable diagnostics.** Every error and warning is a structured
   JSON object on stderr (`{"severity","summary","detail","address"}`) — the
   agent retry loop, not a wall of prose. Pretty rendering only when stderr
   is a TTY (force machine output with `-json`).
4. **Built-in MCP server.** `tchori mcp` serves state and plans to any MCP
   client over stdio. Read + plan only — there is deliberately no apply tool.

## Providers: protocol 6 only (MVP)

tchori speaks plugin protocol **6** (tfplugin6) — the protocol every
provider built on terraform-plugin-framework speaks. The classic hashicorp
utility providers (`null`, `random`, `time`, `local`) publish
protocol-5-only binaries: `tchori providers install` still downloads them,
PGP-verifies the registry's signed `SHA256SUMS`, and SHA256-verifies the
archive, but launching one fails fast with a structured diagnostic naming the
protocol mismatch (`provider protocol unsupported`). A tfplugin5 adapter is a
recorded post-MVP roadmap item.

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

No credential-free protocol-6 provider exists on the public registry yet, so
the quickstart uses tchori's in-repo test provider via `--plugin-dir`:

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
tchori plan $PD                      # exit 0: no changes — idempotent

tchori destroy $PD -out destroy.json # exit 2: destroy plan written
tchori apply $PD destroy.json        # exit 0: everything deleted
```

Exit codes follow the Terraform convention agents already know:
`0` success / no changes · `2` plan has changes · `1` error.

State is a deterministic, git-diffable `state.json` in the working directory
(flock-protected, with concurrent modifications rejected before
`state.json.backup` and the state file are changed). On permission-supporting
platforms, each backup is forced to owner read/write mode (`0600`). Commits
fsync the complete temp file before atomic replacement and fsync the directory
before returning, so reported success is durable across abrupt host failure.
Format reference: [docs/formats.md](docs/formats.md).

Provider responses are stored verbatim in `state.json` and `plan.json`, so
values a provider *derives* from env-sourced secrets can end up recorded
there too — treat both files as sensitive (redaction is a recorded post-MVP
item, not yet implemented).

## MCP server

`tchori mcp` serves MCP over stdio from the directory holding your config and
state. Exactly four tools:

| Tool | Returns |
| --- | --- |
| `state_list` | all managed resource addresses |
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

In: any tfplugin6 provider · `${type.name.attr}` references · plan/apply/
destroy through plan documents · provider install from the OpenTofu registry
(SHA256-verified) · MCP read + plan.

Out (recorded deferrals): tfplugin5 adapter (the classic null/random/time/
local providers), modules, count/for_each, an expression language, HCL,
remote state backends, workspaces, import, registry GPG verification,
apply-via-MCP, Homebrew tap (post-0.1).

## Development

```sh
go test ./...                         # unit + protocol tests (in-process fake provider)
go test -race -timeout=2m ./...       # full untagged suite under the race detector
go test -tags e2e ./e2e -v            # built binary: fake-provider lifecycle, real
                                      # registry install, protocol-5 graceful failure
                                      # (network required)
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
and `actionlint .github/workflows/ci.yml` before merging. Never use `@latest` in
CI.

Secret findings fail the gate by default. Revoke and remove real credentials,
then track any coordinated history purge as focused follow-up work; never
allowlist a real secret. Suppress only a confirmed false positive with the
narrowest practical, individually commented rule/path/regex entry in
`.gitleaks.toml`—never a blanket exclusion. A temporary baseline is exceptional:
every entry requires an explicit written rationale and a remediation task
reference.

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

Provider acceptance: `docs/acceptance-admanager.md` is the manual checklist
for running `terraform-provider-admanager` under tchori against a Google Ad
Manager test network.

## License

MPL-2.0 — see `LICENSE`. Files adapted from OpenTofu keep their original
MPL-2.0 license headers plus a provenance comment naming the source file.

## Built in public

tchori is the flagship project of **Tchori Labs**, an AI-agent-operated
company that runs itself as code. This repo is written by agents and merged
through the same plan → review → apply loop that tchori implements: CI runs
the plan, the board reviews it, merge is apply. Company state root:
`Tchori-Labs/main`.

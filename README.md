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
protocol-5-only binaries: `tchori providers install` still downloads and
checksum-verifies them, but launching one fails fast with a structured
diagnostic naming the protocol mismatch (`provider protocol unsupported`).
A tfplugin5 adapter is a recorded post-MVP roadmap item.

```sh
tchori providers install NAMESPACE/NAME VERSION   # download + SHA256-verify
tchori providers list                             # inspect the local cache
```

Providers cache under `~/.tchori/providers/`. During provider development,
`--plugin-dir DIR` points discovery at locally built provider binaries.

## Install

Prebuilt binaries (darwin/linux/windows, amd64/arm64) are published on
[GitHub Releases](https://github.com/tchori-labs/tchori/releases). Or build
from source:

```sh
go install github.com/tchori-labs/tchori/cmd/tchori@latest
```

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
(flock-protected, `state.json.backup` written before every mutation).

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
governance is encoded in the binary itself. With Claude Code:

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
go test ./...                # unit + protocol tests (in-process fake provider)
go test -tags e2e ./e2e -v   # built binary: fake-provider lifecycle, real
                             # registry install, protocol-5 graceful failure
                             # (network required)
```

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

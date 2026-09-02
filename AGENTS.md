# tchori — Agent Rulebook

This is the `tchori` engine repo — the product, not the company state root
(that's `Tchori-Labs/main`). Read this before opening a PR here.

## Identity

- All agent work is authored by the **Tchorizo** GitHub account.
- Always self-identify as an automated agent in any external interaction.

## Layout

| Path | What it is |
| --- | --- |
| `cmd/tchori` | CLI entrypoint |
| `internal/` | Implementation packages (plan, apply, state, provider, registry, mcpserv, config, runtime, diag, version) |
| `docs/formats.md` | `plan.json` / `state.json` format reference — update it in the same PR as any format change |
| `e2e/` | End-to-end tests against the built CLI |

## Commits

- Conventional commits only: `feat:`, `fix:`, `test:`, `chore:`, `docs:`,
  `refactor:`. One logical change per commit.
- Never merge or approve your own PRs — CODEOWNERS review is the apply gate,
  same as `main`.

## Scope discipline

- Change only what your task requires. Every line in the diff must be
  justifiable as "this task needs it."
- Don't refactor code you didn't have to touch — even if it's bad. Note
  unrelated problems in the PR description as follow-ups; don't fix them in
  the same PR.
- Traps to avoid: "while I'm here" cleanups, speculative "for future
  flexibility" abstractions, drive-by modernization of working code.

## Tests are required

- Every behavior change adds or extends tests; `go test -count=1 ./...` must
  exercise the new behavior. Bug fixes start from a failing test that
  reproduces the bug: run it, observe the failure, then fix. `-count=1` is not
  optional — a cached PASS is not evidence that the suite ran.
- Tests are deterministic and offline — no network, no wall-clock or locale
  dependence.
- Prefer package-level tests for logic. CLI behavior belongs in
  `cmd/tchori/testdata/script/*.txtar` (testscript: argv, stdout, stderr, exit
  code); `e2e/` covers the built-binary lifecycle.
- The red/green loop is `scripts/check.sh fast` (tests only). Coverage is
  measured by the full gate and reported in the CI job summary; it is not a
  merge gate, and coverage is never a substitute for having watched the test
  fail first.

## Releases

- **No releases without board sign-off.** Tagging a version and letting
  goreleaser publish to GitHub Releases only happens after the board (a
  human) has explicitly approved it (recorded as a decision in `main`).
  Agents may prepare a release PR; they do not tag or push it themselves.

## Before opening a PR

Run the whole gate from the repo root with one command. Install the pinned
actionlint and golangci-lint versions documented in `README.md` first:
```bash
scripts/check.sh
```
It runs, in CI order: `gofmt -l .`, `go vet ./...`,
`GOOS=windows go vet ./...`, `golangci-lint run`,
`bash scripts/actionlint-verify.sh`,
`go test -count=1 -covermode=atomic -coverprofile=cover.out ./...`, and
`go test -count=1 -race -timeout=2m ./...`.

CI's `check` job (`.github/workflows/ci.yml`) re-runs the same checks,
validates GitHub Actions workflows, and runs the bounded full-suite race
detector. It is a required status check and must be green before merge.

Pull requests into `main` must come from `develop`; the required `pr-source`
context rejects any other head ref. Target `develop` unless the task is the
`develop` → `main` integration itself.

## Branch protection

See [`docs/branch-protection.md`](docs/branch-protection.md) for the importable
`main` ruleset and the repository-admin apply, audit, and verification runbook.

## Security

- Never commit secrets, tokens, or credentials — not in code, tests,
  fixtures, or docs. Provider and API auth is environment-only.
- Never print credential values in output, logs, or diagnostics; redact.

## License

MPL-2.0. Files adapted from OpenTofu keep their original MPL header plus a
provenance comment naming the source file; original tchori files need no
header.

## Cross-repo protocol

Tchori Labs is spread across these repos (all under `github.com/Tchori-Labs`):

| Repo | What lives there |
| --- | --- |
| `main` | Company state root — goals, budget, org, ideas, ledger, decisions |
| `tchori` | This repo — the engine/product |
| `site` | Public website (tchori.com.br) — Astro, renders `main`'s public state |
| `design` | Design system — DESIGN.md, tokens, brand assets |
| `infra` | Infrastructure as code — OCI box, Cloudflare DNS, Coolify/Fusion deploy |

When a task needs a change in a repo other than this one:

1. Don't make the change yourself, and don't clone the other repo into your
   worktree.
2. File an issue there: `gh issue create --repo Tchori-Labs/<repo>` describing
   what you need, why, and your task ID.
3. Alert the human: add a comment on your task with the issue URL and call it
   out in your final summary. Then continue with what you can do locally, or
   mark the task blocked on that issue.

Never push branches, open pull requests, or comment on repos outside
`github.com/Tchori-Labs` — including upstreams of forked or vendored code
(e.g. OpenTofu) — without explicit human approval recorded on the task first.

## Related

Parent company repo: `Tchori-Labs/main` (state root — company/, org/,
policies/, decisions/). This repo is the product itself.

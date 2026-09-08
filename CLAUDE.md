# tchori — Agent Rulebook

This is the `tchori` engine rulebook, not the company state root
(`Tchori-Labs/main`). `AGENTS.md` points here; keep instructions canonical here.
Check the actual Git remote before changing repository state: the public
product is `Tchori-Labs/tchori`; `Tchori-Labs/tchori-internal` preserves the
private history and issue backlog. Directory names are not repository identity.

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
- Never approve your own PRs or submit a CODEOWNER review using a human's
  account. Human review and execution of a merge are separate actions.
- An agent may execute a merge, including for an agent-authored PR, only when
  the human CODEOWNER explicitly authorizes that PR and its exact current
  head SHA. Authorization may be given in the task conversation or on GitHub;
  record its source, authorizing CODEOWNER, and head SHA in a PR comment,
  identifying the executor as Tchorizo, before merging.
- Delegated execution does not replace review: the required human CODEOWNER
  approval must already be recorded on GitHub for the current head, required
  checks must pass, and all branch-protection and review requirements must
  remain satisfied. New commits invalidate the merge authorization.
- Execute an authorized merge as Tchorizo with an expected-head SHA check.
  Never use a human's credentials, administrator bypass, force push, or a
  protection change to complete it. Preserve permanent branches such as
  `develop`; if any gate is unmet, stop and report the blocker.
- This delegation applies only to PR merges, not release tags, workflow
  dispatches, deployment approvals, publication, or administrative changes.
  It becomes available only after independent human review and merge of this
  policy; it cannot authorize its own adoption or override higher-priority
  session instructions.

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
  The human may expressly delegate release-command execution to Tchorizo
  under the authorization rules below, including publication dispatch.
- `.github/workflows/release.yml` starts automatically on a pushed `v*` tag
  in the **public** `Tchori-Labs/tchori` repository. Forks and the internal
  archive do not run its release jobs. Changes prepared internally must be
  reviewed and transferred to the public repository before publication.
- Use a semantic version such as `v0.1.0` or `v0.1.0-rc.1`. The existing tag
  must identify the checked-out commit and belong to reviewed `origin/main`
  history. The workflow rejects tags outside that history.
- **The authorized human user is the decision gate; the agent may be the
  executor.** Once this policy has been human-reviewed and merged into public
  `main`, an explicit instruction in the task conversation or on GitHub may
  authorize Tchorizo to create/push the release tag and execute
  `.github/workflows/release.yml` through `workflow_dispatch`, including
  `mode: publish`. No new policy PR or version-specific exception is needed
  for each release.
- Before execution, record the authorizing human, source of authorization,
  repository, tag, exact target commit, and authorized operations/modes in
  the release decision issue in `Tchori-Labs/main`. A human acting as the
  board/CODEOWNER may approve the release and delegate execution in the same
  instruction; record that decision without presenting it as an agent
  approval. An instruction such as "publish this release" is sufficient when
  the preceding conversation unambiguously identifies that scope.
- Authorization may cover the complete stated sequence (tag, dry-run,
  verification, publish) in one decision. Do not repeatedly request consent
  for already authorized operations. A different tag, target commit, mode
  outside that sequence, or materially changed workflow requires new consent;
  revocation stops execution. PR approval or issue closure alone is not
  release authorization without an explicit release decision.
- Execute as Tchorizo using Tchorizo's credentials, distinct from the required
  Environment reviewer. Check reviewed `origin/main` ancestry, passing
  required CI, the approved tag target, and the live release Environment
  before execution. Never move/delete a release tag: if it already resolves
  to the approved commit, reuse it; if it differs, stop.
- Dispatch from the reviewed `main` ref with the explicit approved tag and
  mode. `dry-run` remains the default; `publish` must be expressly authorized
  and must follow a successful, human-approved dry-run with verified
  artifacts for that tag/commit. Confirm the selected workflow still matches
  the reviewed execution path, and record each resulting run URL.
- Delegation permits initiating publication through the protected workflow;
  it does not permit bypassing it with direct GitHub Release/asset mutations.
  It never permits the agent to approve a CODEOWNER review or Environment
  deployment, impersonate the human, disable `prevent_self_review`, change
  protections/credentials, force-push, or use administrator bypass. The human
  reviewer still approves each protected deployment in GitHub. If an
  enforced gate is unmet, report the concrete blocker rather than claiming
  that an expressly authorized dispatch is categorically forbidden.
- Do not cancel/retry an existing run or replace an incomplete draft unless
  that operation is also expressly authorized. After publication, verify the
  public release, assets, checksums, signature, and provenance before
  reporting completion. Existing security-remediation gates remain in force.
- Historical decisions remain evidence, not standing authority for unrelated
  releases: [ADR-0012](https://github.com/Tchori-Labs/main/blob/main/decisions/0012-tchori-v0.1.0-release.md)
  authorized the first `v0.1.0` tag; the
  [v0.1.1 board approval](https://github.com/Tchori-Labs/main/issues/163#issuecomment-5578320537)
  identifies `aeeb8f0dae1195a78cfd66058d0a00223c3306ad`.
  Neither tag may be retargeted to a later policy commit. This policy does
  not authorize its own adoption or override higher-priority instructions.
- GoReleaser builds Linux/macOS/Windows for amd64/arm64, with six archives,
  six SPDX SBOMs, SHA-256 checksums, a Cosign signature/certificate, and
  GitHub provenance. Publish stages a draft, verifies the complete artifact
  matrix and checksums, verifies the Cosign identity/issuer and signature,
  attests the outputs, and only then makes it public.
- Before a release, run `scripts/check.sh`, the offline `e2e` suite,
  `govulncheck`, redacted Gitleaks scans, and a local GoReleaser snapshot.
  A local snapshot is not proof of live OIDC signing or publication.
- Read [docs/releasing.md](docs/releasing.md) for approval, dry-run,
  publication, retry, and consumer verification. Verify live protection with
  `scripts/verify-release-environment.sh`; committed YAML alone is not proof.

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
| `tchori` | Public engine/product and release destination |
| `tchori-internal` | Private engine history and internal issue backlog |
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

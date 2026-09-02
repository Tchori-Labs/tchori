# Code scanning

This repository runs CodeQL for Go from [`.github/workflows/codeql.yml`](../.github/workflows/codeql.yml). The workflow runs for pull requests, pushes to `main`, and once each week. It initializes the default CodeQL query suite plus `security-extended`, uses CodeQL autobuild for the Go database, and fails when analysis fails.

## Capability gate

Code scanning requires GitHub Advanced Security, which is free for public
repositories. `Tchori-Labs/tchori` is public, so
`repos/Tchori-Labs/tchori.security_and_analysis` reports code scanning as
available, and the workflow's unconditional `capability` step — which asks
`GET /repos/{owner}/{repo}/code-scanning/alerts` — resolves `available=true`
on every run: Go setup, init, autobuild, and analyze all execute, and CodeQL
performs real analysis on every pull request, push to `main`, and weekly run.

| Probe result | Outcome |
| --- | --- |
| `200`, or `404` (enabled, nothing analysed yet) | `available=true`; Go setup, init, autobuild, and analyze run unchanged. |
| `403` mentioning `Advanced Security` | `available=false`; the CodeQL steps are skipped and the job summary states that no analysis ran. |
| any other status, or a `403` for another reason | the job fails with the payload. |

This reports a missing capability; it never swallows a CodeQL failure. There is
no `continue-on-error` anywhere in the workflow, and
`internal/ci.CodeQLAnalysisIsCapabilityGated` fails the required `check` job if
the probe is removed, given a condition, or if any CodeQL step loses the gate or
gains `continue-on-error`.

The `available=false` branch remains only as a safeguard: it is not expected
to trigger while this repository stays public, but it protects against a
future state where Advanced Security becomes unavailable here (for example if
the repository were ever made private again), producing a clear job-summary
message instead of a red, unexplained `Analyze Go` check.

## Required repository setting

A repository maintainer must select **Advanced** setup under **Settings → Code security → Code scanning**. Do not enable GitHub's **Default** CodeQL setup: that managed setup can conflict with or suppress results from this workflow-based Advanced analysis. Only a human or the board can change this repository setting; committing the workflow does not change it.

If CodeQL must block merges at the branch-protection level, a maintainer must also add the CodeQL pull-request check (the `CodeQL / Analyze Go` job) as a required status check after it has run at least once. This is a repository/board action, not an automated-agent action.

## Permissions and fork pull requests

The workflow's global `GITHUB_TOKEN` permission is only `contents: read`. The single `analyze` job additionally requests `actions: read` and `security-events: write`; no write permission is granted globally.

The workflow deliberately uses `pull_request`, never `pull_request_target`. On pull requests from forks, GitHub provides a read-only `GITHUB_TOKEN` and does not expose repository secrets. CodeQL findings are surfaced through the pull-request check without giving the fork write access. Checkout also sets `persist-credentials: false`, so credentials are not retained in the local Git configuration used by later steps.

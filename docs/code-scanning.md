# Code scanning

This repository runs CodeQL for Go from [`.github/workflows/codeql.yml`](../.github/workflows/codeql.yml). The workflow runs for pull requests, pushes to `main`, and once each week. It initializes the default CodeQL query suite plus `security-extended`, uses CodeQL autobuild for the Go database, and fails when analysis fails.

## Required repository setting

A repository maintainer must select **Advanced** setup under **Settings → Code security → Code scanning**. Do not enable GitHub's **Default** CodeQL setup: that managed setup can conflict with or suppress results from this workflow-based Advanced analysis. Only a human or the board can change this repository setting; committing the workflow does not change it.

If CodeQL must block merges at the branch-protection level, a maintainer must also add the CodeQL pull-request check (the `CodeQL / Analyze Go` job) as a required status check after it has run at least once. This is a repository/board action, not an automated-agent action.

## Permissions and fork pull requests

The workflow's global `GITHUB_TOKEN` permission is only `contents: read`. The single `analyze` job additionally requests `actions: read` and `security-events: write`; no write permission is granted globally.

The workflow deliberately uses `pull_request`, never `pull_request_target`. On pull requests from forks, GitHub provides a read-only `GITHUB_TOKEN` and does not expose repository secrets. CodeQL findings are surfaced through the pull-request check without giving the fork write access. Checkout also sets `persist-credentials: false`, so credentials are not retained in the local Git configuration used by later steps.

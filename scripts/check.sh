#!/usr/bin/env bash
# Single entrypoint for the local gate documented in AGENTS.md. It runs the
# same checks the CI `check` job runs, in the same order, so a green run here
# is evidence the required status context will be green.
#
# Usage:
#   scripts/check.sh            # full gate (lint + workflows + tests + race)
#   scripts/check.sh fast       # red/green loop: tests only, no lint, no race
set -euo pipefail

mode=${1:-full}
case "$mode" in
  full | fast) ;;
  *)
    printf 'usage: %s [full|fast]\n' "$0" >&2
    exit 2
    ;;
esac

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

step() {
  printf '\n==> %s\n' "$1"
}

# -count=1 on every test invocation: a cached PASS is not evidence that the
# suite ran against the current tree, which is exactly the evidence the
# red/green loop depends on.
if [ "$mode" = fast ]; then
  step 'go test -count=1 ./...'
  go test -count=1 ./...
  printf '\nRESULT: PASS (fast loop; run "%s full" before opening a pull request)\n' "$0"
  exit 0
fi

step 'gofmt -l .'
fmt_out=$(gofmt -l .)
if [ -n "$fmt_out" ]; then
  printf 'gofmt needs to be run on:\n%s\n' "$fmt_out" >&2
  exit 1
fi

step 'go vet ./...'
go vet ./...

step 'GOOS=windows go vet ./...'
GOOS=windows go vet ./...

step 'golangci-lint run'
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run
else
  printf 'golangci-lint is not installed; install the version pinned in .github/workflows/ci.yml\n' >&2
  exit 1
fi

step 'bash scripts/actionlint-verify.sh'
bash scripts/actionlint-verify.sh

step 'go test -count=1 -covermode=atomic -coverprofile=cover.out ./...'
go test -count=1 -covermode=atomic -coverprofile=cover.out ./...
bash "$repo_root/scripts/coverage-summary.sh" cover.out

step 'go test -count=1 -race -timeout=2m ./...'
go test -count=1 -race -timeout=2m ./...

printf '\nRESULT: PASS (local gate matches the CI check job)\n'

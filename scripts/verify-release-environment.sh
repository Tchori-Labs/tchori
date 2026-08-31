#!/usr/bin/env bash
set -euo pipefail

repo=${GH_REPO:-Tchori-Labs/tchori}

not_auditable() {
  printf 'NOT APPLIED / NOT AUDITABLE: %s\n' "$1" >&2
  exit 1
}

command -v gh >/dev/null 2>&1 || not_auditable "gh is required to read the live release Environment"
command -v go >/dev/null 2>&1 || not_auditable "the Go toolchain pinned by go.mod is required to validate live responses"
gh auth status >/dev/null 2>&1 || not_auditable "gh is not authenticated; authenticate and request repository read access"

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "${script_dir}/.." && pwd)
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

if ! gh api "repos/${repo}/environments" >"$tmpdir/environments.json" 2>"$tmpdir/environments.err"; then
  cat "$tmpdir/environments.err" >&2
  not_auditable "cannot list repository Environments"
fi
printf 'PASS: read repository Environments endpoint\n'

fetch_optional_environment_endpoint() {
  local endpoint=$1
  local output=$2
  local error=$3
  local label=$4
  if gh api "$endpoint" >"$output" 2>"$error"; then
    printf 'PASS: read %s endpoint\n' "$label"
    return
  fi
  if grep -q 'HTTP 404' "$error"; then
    : >"$output"
    printf 'FAIL: %s endpoint returned HTTP 404 (release Environment is not applied)\n' "$label"
    return
  fi
  cat "$error" >&2
  not_auditable "cannot read ${label}; repository access or administrator-visible audit data may be required"
}

fetch_optional_environment_endpoint \
  "repos/${repo}/environments/release" \
  "$tmpdir/environment.json" "$tmpdir/environment.err" \
  "release Environment detail"
fetch_optional_environment_endpoint \
  "repos/${repo}/environments/release/deployment-branch-policies" \
  "$tmpdir/branch-policies.json" "$tmpdir/branch-policies.err" \
  "release deployment-branch-policies"

if ! gh api "repos/${repo}" --jq '.permissions.admin' >"$tmpdir/is-admin" 2>"$tmpdir/repository.err"; then
  cat "$tmpdir/repository.err" >&2
  not_auditable "cannot read repository permissions"
fi
if [ "$(cat "$tmpdir/is-admin")" = true ]; then
  printf 'PASS: current token has repository-admin visibility for the audit\n'
else
  printf 'NOTICE: current token is not a repository admin; no live Environment mutation is possible\n'
fi

printf 'Auditing required_reviewers, prevent_self_review, deployment_branch_policy, branch main, and tag v*\n'
export RELEASE_ENVIRONMENTS_JSON="$tmpdir/environments.json"
export RELEASE_ENVIRONMENT_JSON="$tmpdir/environment.json"
export RELEASE_BRANCH_POLICIES_JSON="$tmpdir/branch-policies.json"
if ! (cd "$repo_root" && go test -count=1 -v -run '^TestLiveReleaseEnvironmentMatchesPolicy$' ./internal/ci); then
  printf 'RESULT: FAIL — apply .github/environments/release.json and the declared deployment policies as a repository administrator, then rerun this verifier\n' >&2
  exit 1
fi

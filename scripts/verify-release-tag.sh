#!/usr/bin/env bash
set -euo pipefail

tag=${1:?usage: verify-release-tag.sh TAG}
if [ "${GITHUB_EVENT_NAME:-}" = workflow_dispatch ] && [ "${GITHUB_REF:-}" != refs/heads/main ]; then
  printf 'Manual releases must be dispatched from main\n' >&2
  exit 1
fi
number='(0|[1-9][0-9]*)'
identifier='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
pattern="^v${number}\\.${number}\\.${number}(-${identifier}(\\.${identifier})*)?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$"
if [[ ! $tag =~ $pattern ]]; then
  printf 'Release tag must be a v-prefixed semantic version\n' >&2
  exit 1
fi
git show-ref --verify --quiet "refs/tags/$tag"
commit=$(git rev-parse --verify "refs/tags/$tag^{commit}")
test "$(git rev-parse HEAD)" = "$commit"
if ! git merge-base --is-ancestor "$commit" refs/remotes/origin/main; then
  printf 'Release tag must point to a commit in reviewed origin/main history\n' >&2
  exit 1
fi
printf 'PASS: checked-out release tag belongs to main\n'

#!/usr/bin/env bash
set -euo pipefail

ACTIONLINT_VERSION=v1.7.12

repo_root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
workflows_dir="$repo_root/.github/workflows"
tmp_dir=$(TMPDIR=/tmp mktemp -d "actionlint-verify.XXXXXX")

cleanup() {
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT

fail() {
  printf 'Actionlint verification: FAIL: %s\n' "$*" >&2
  exit 1
}

command -v actionlint >/dev/null 2>&1 || fail "actionlint is not on PATH; install $ACTIONLINT_VERSION"
installed_version=$(actionlint -version | head -n 1)
[[ "$installed_version" == "$ACTIONLINT_VERSION" ]] || \
  fail "actionlint version is $installed_version, want $ACTIONLINT_VERSION"

# Enumerate exactly .github/workflows from disk. Files elsewhere are not linted,
# so detector-efficacy proofs must place workflow defects in this directory.
mapfile -d '' workflow_files < <(
  find "$workflows_dir" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 | sort -z
)
((${#workflow_files[@]} > 0)) || fail "no .yml or .yaml workflows found under $workflows_dir"

printf 'Actionlint verification: linting %d workflow file(s):\n' "${#workflow_files[@]}"
for workflow_file in "${workflow_files[@]}"; do
  printf '  %s\n' "${workflow_file#"$repo_root/"}"
done

# Empty values explicitly disable host-dependent shellcheck and pyflakes
# integrations. JSON Kind values below are actionlint's observed stable class
# identifiers: syntax-check for YAML parsing and expression for expressions.
actionlint -shellcheck= -pyflakes= "${workflow_files[@]}"

cat >"$tmp_dir/malformed-syntax.yml" <<'EOF'
name: Malformed syntax
on: push
jobs:
  broken:
    runs-on: ubuntu-latest
    steps
      - run: echo broken
EOF

cat >"$tmp_dir/malformed-expression.yml" <<'EOF'
name: Malformed expression
on: pull_request
jobs:
  broken:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ github.nonexistent_property }}"
EOF

cat >"$tmp_dir/well-formed-control.yml" <<'EOF'
name: Well-formed control
on: push
jobs:
  control:
    runs-on: ubuntu-latest
    steps:
      - run: echo valid
EOF

run_fixture() {
  local fixture=$1
  local output_path=$2
  set +e
  actionlint -shellcheck= -pyflakes= -format '{{json .}}' "$fixture" >"$output_path" 2>&1
  fixture_status=$?
  set -e
}

run_fixture "$tmp_dir/malformed-syntax.yml" "$tmp_dir/malformed-syntax.json"
[[ $fixture_status -ne 0 ]] || fail "malformed syntax fixture unexpectedly passed"
grep -q '"kind":"syntax-check"' "$tmp_dir/malformed-syntax.json" || \
  fail "malformed syntax fixture produced no syntax-check finding"

run_fixture "$tmp_dir/malformed-expression.yml" "$tmp_dir/malformed-expression.json"
[[ $fixture_status -ne 0 ]] || fail "malformed expression fixture unexpectedly passed"
grep -q '"kind":"expression"' "$tmp_dir/malformed-expression.json" || \
  fail "malformed expression fixture produced no expression finding"

run_fixture "$tmp_dir/well-formed-control.yml" "$tmp_dir/well-formed-control.json"
[[ $fixture_status -eq 0 ]] || fail "well-formed control fixture produced findings"
[[ $(<"$tmp_dir/well-formed-control.json") == '[]' ]] || \
  fail "well-formed control fixture output was not an empty finding list"

printf 'Actionlint verification: PASS (%d workflow files linted; malformed syntax and expression detected; well-formed control clean)\n' \
  "${#workflow_files[@]}"

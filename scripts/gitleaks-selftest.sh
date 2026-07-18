#!/usr/bin/env bash
set -euo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
config_path="$repo_root/.gitleaks.toml"
tmp_dir=$(TMPDIR=/tmp mktemp -d "gitleaks-selftest.XXXXXX")
fixtures_dir="$tmp_dir/fixtures"
report_path="$tmp_dir/report.json"

cleanup() {
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT

fail() {
  printf 'Gitleaks self-test: FAIL: %s\n' "$*" >&2
  exit 1
}

command -v gitleaks >/dev/null 2>&1 || fail "gitleaks is not on PATH"
[[ -f "$config_path" ]] || fail "missing config at $config_path"
mkdir -p -- "$fixtures_dir"

# Build fabricated, non-functional examples only at runtime. Splitting the
# identifying strings keeps secret-like fixtures out of the repository itself.
printf 'aws_access_key_id = "%s%s"\n' \
  'AK' 'IA7FQ5W2M4R6TY3UPZ' >"$fixtures_dir/aws.txt"
printf 'api_token = "%s%s"\n' \
  'mN7qV2xK9pR4tY8wC3dF6hJ1' 'sL5zB0uE7iO2aG9k' >"$fixtures_dir/token.txt"
printf '%s%s\n%s\n%s%s\n' \
  '-----BEGIN PRI' 'VATE KEY-----' \
  'c3ludGhldGljLW5vbi1mdW5jdGlvbmFsLWtleS1tYXRlcmlhbA==' \
  '-----END PRI' 'VATE KEY-----' >"$fixtures_dir/private-key.pem"

set +e
gitleaks dir "$fixtures_dir" \
  --no-banner \
  --config "$config_path" \
  --redact \
  --report-format json \
  --report-path "$report_path" \
  --exit-code 1
scan_status=$?
set -e

[[ $scan_status -eq 1 ]] || fail "expected leak exit code 1, got $scan_status"
[[ -s "$report_path" ]] || fail "scanner did not produce a JSON report"

grep -q '"RuleID": "aws-access-token"' "$report_path" || \
  fail "AWS-style access key was not detected"
grep -q '"RuleID": "generic-api-key"' "$report_path" || \
  fail "generic API token was not detected"
grep -q '"RuleID": "private-key"' "$report_path" || \
  fail "private-key material was not detected"

printf 'Gitleaks self-test: PASS (AWS access key, generic token, private key detected)\n'

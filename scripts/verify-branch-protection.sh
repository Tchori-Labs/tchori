#!/usr/bin/env bash
set -euo pipefail

repo=${GH_REPO:-Tchori-Labs/tchori}
expected_name=${RULESET_NAME:-main-protection}

api_failure() {
  printf 'NOT APPLIED / NOT AUDITABLE: %s\n' "$1" >&2
  exit 1
}

command -v gh >/dev/null 2>&1 || api_failure "gh is required to read the live repository ruleset"
command -v python3 >/dev/null 2>&1 || api_failure "python3 is required to validate the live repository ruleset"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

if ! gh api --paginate "repos/${repo}/rulesets?per_page=100" \
  --jq '.[].id' >"$tmpdir/ids" 2>"$tmpdir/rulesets.err"; then
  cat "$tmpdir/rulesets.err" >&2
  api_failure "cannot list live repository rulesets; authenticate with gh and request repository access"
fi

while IFS= read -r ruleset_id; do
  [ -n "$ruleset_id" ] || continue
  if ! gh api "repos/${repo}/rulesets/${ruleset_id}" >"$tmpdir/ruleset-${ruleset_id}.json" 2>"$tmpdir/ruleset-${ruleset_id}.err"; then
    cat "$tmpdir/ruleset-${ruleset_id}.err" >&2
    api_failure "cannot read live ruleset ${ruleset_id}; repository-admin audit access may be required"
  fi
done <"$tmpdir/ids"

if ! gh api "repos/${repo}" >"$tmpdir/repository.json" 2>"$tmpdir/repository.err"; then
  cat "$tmpdir/repository.err" >&2
  api_failure "cannot read repository permissions"
fi

python3 - "$tmpdir" "$expected_name" <<'PY'
import glob
import json
import os
import sys

root, expected_name = sys.argv[1:]
failures = 0


def report(ok, criterion, detail):
    global failures
    status = "PASS" if ok else "FAIL"
    print(f"{status}: {criterion} — {detail}")
    if not ok:
        failures += 1


def load(path):
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


details = [load(path) for path in glob.glob(os.path.join(root, "ruleset-*.json"))]
preferred = [item for item in details if item.get("name") == expected_name]
main_targeting = [
    item
    for item in details
    if "refs/heads/main"
    in item.get("conditions", {}).get("ref_name", {}).get("include", [])
]

if len(preferred) == 1:
    ruleset = preferred[0]
elif len(preferred) > 1:
    print(f"NOT APPLIED / APPLIED BUT AMBIGUOUS: multiple rulesets are named {expected_name!r}")
    sys.exit(1)
elif len(main_targeting) == 1:
    ruleset = main_targeting[0]
    print(
        "NOTICE: intended ruleset "
        f"{expected_name!r} is not applied; auditing the sole ruleset targeting main, "
        f"{ruleset.get('name')!r} (id {ruleset.get('id')})"
    )
elif not main_targeting:
    print(
        "NOT APPLIED: no live ruleset named "
        f"{expected_name!r} or live ruleset explicitly targeting refs/heads/main"
    )
    sys.exit(1)
else:
    names = ", ".join(repr(item.get("name")) for item in main_targeting)
    print(
        "NOT APPLIED / APPLIED BUT AMBIGUOUS: intended ruleset "
        f"{expected_name!r} is absent and multiple rulesets target main: {names}"
    )
    sys.exit(1)

print(f"Auditing live ruleset {ruleset.get('name')!r} (id {ruleset.get('id')})")

report(
    ruleset.get("enforcement") == "active",
    "active enforcement",
    f"enforcement={ruleset.get('enforcement')!r}",
)
report(
    ruleset.get("target") == "branch",
    "branch ruleset target",
    f"target={ruleset.get('target')!r}",
)

ref_condition = ruleset.get("conditions", {}).get("ref_name", {})
includes = ref_condition.get("include", [])
excludes = ref_condition.get("exclude", [])
report(
    includes == ["refs/heads/main"] and excludes == [],
    "main-only target",
    f"include={includes!r}, exclude={excludes!r}",
)

rules = ruleset.get("rules", [])
by_type = {}
for rule in rules:
    by_type.setdefault(rule.get("type"), []).append(rule)

pull_rules = by_type.get("pull_request", [])
report(len(pull_rules) == 1, "pull request required", f"rule count={len(pull_rules)}")
pull = pull_rules[0].get("parameters", {}) if len(pull_rules) == 1 else {}

review_count = pull.get("required_approving_review_count")
report(
    isinstance(review_count, int) and not isinstance(review_count, bool) and review_count >= 1,
    "approving review",
    f"required_approving_review_count={review_count!r}",
)
for key, label in (
    ("require_code_owner_review", "CODEOWNER review"),
    ("dismiss_stale_reviews_on_push", "stale review dismissal"),
    ("require_last_push_approval", "last-push approval"),
    ("required_review_thread_resolution", "review thread resolution"),
):
    value = pull.get(key)
    report(value is True, label, f"{key}={value!r}")

status_rules = by_type.get("required_status_checks", [])
report(len(status_rules) == 1, "required status checks rule", f"rule count={len(status_rules)}")
status = status_rules[0].get("parameters", {}) if len(status_rules) == 1 else {}
contexts = [
    item.get("context")
    for item in status.get("required_status_checks", [])
    if isinstance(item, dict)
]
report(contexts == ["check"], "only required check context", f"contexts={contexts!r}")
strict = status.get("strict_required_status_checks_policy")
report(strict is True, "up-to-date branch", f"strict_required_status_checks_policy={strict!r}")

report(len(by_type.get("non_fast_forward", [])) == 1, "force-push block", "non_fast_forward rule present")
report(len(by_type.get("deletion", [])) == 1, "deletion block", "deletion rule present")

permissions = load(os.path.join(root, "repository.json")).get("permissions", {})
is_admin = permissions.get("admin") is True
if not is_admin:
    report(
        False,
        "bypass audit",
        "current token is not a repository admin; GitHub omits bypass_actors from the detailed ruleset response",
    )
    report(
        False,
        "Tchorizo isolation audit",
        "admin-visible bypass state and a rejected direct-push observation are still required",
    )
else:
    bypass = ruleset.get("bypass_actors")
    report(
        isinstance(bypass, list) and len(bypass) == 0,
        "no standing bypass",
        f"bypass_actors={bypass!r}; no App, bot, team, role, deploy key, or Tchorizo bypass is allowed",
    )
    prohibited = []
    for actor in bypass if isinstance(bypass, list) else []:
        text = json.dumps(actor, sort_keys=True).lower()
        if actor.get("actor_type") in {"Integration", "DeployKey"} or "bot" in text or "tchorizo" in text:
            prohibited.append(actor)
    report(
        isinstance(bypass, list) and not prohibited,
        "no bot/App/Tchorizo bypass",
        f"prohibited entries={prohibited!r}",
    )

if failures:
    print(f"RESULT: FAIL ({failures} criterion/criteria failed); live protection is absent, incomplete, or not fully auditable")
    sys.exit(1)

print("RESULT: PASS (all machine-auditable main-protection criteria match)")
PY

#!/usr/bin/env bash
set -euo pipefail

repo=${GH_REPO:-Tchori-Labs/tchori}
state_repo=${STATE_REPO:-Tchori-Labs/main}
repo_root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
security_md=${SECURITY_MD:-$repo_root/SECURITY.md}

fail_count=0
unauditable_count=0

# Unlike verify-branch-protection.sh, API failures are deferred: offline C2
# must be reported even when every live lookup is unavailable.
record_pass() { printf 'PASS: %s — %s\n' "$1" "$2"; }
record_not_applicable() { printf 'PASS: %s — not applicable: %s\n' "$1" "$2"; }
record_fail() { printf 'FAIL: %s — %s\n' "$1" "$2"; fail_count=$((fail_count + 1)); }
record_unauditable() { printf 'NOT APPLIED / NOT AUDITABLE: %s — %s\n' "$1" "$2" >&2; unauditable_count=$((unauditable_count + 1)); }
fatal() { printf 'NOT APPLIED / NOT AUDITABLE: C0 — %s\n' "$1" >&2; exit 1; }
exit_with_status() { [ "$fail_count" -eq 0 ] && [ "$unauditable_count" -eq 0 ]; }

[ -r "$security_md" ] || fatal "policy document is missing or unreadable: $security_md"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# Narrow detector: C1 and published bodies ask whether a reporter can act.
# The wider detector below serves pending/outside-region prohibitions. Neither
# exempts angle-bracket placeholders; that exemption exists only in the
# documentation contract-example verification and must not be copied here.
contact_affordance_present() {
  local f=$1
  LC_ALL=C grep -qiE 'mailto:[^[:space:]]|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$f" && return 0
  LC_ALL=C grep -qE '^[[:space:]]*[*_>#[:space:]-]*Private security contact:[[:space:]]*[^[:space:]]' "$f"
}

contact_shaped_value_present() {
  local f=$1
  contact_affordance_present "$f" && return 0
  # Split the armor header so this detector's own source is self-non-matching.
  LC_ALL=C grep -qE 'BEGIN ''PGP|[0-9A-Fa-f]{40}|([0-9A-Fa-f]{4}[[:space:]-]){9}[0-9A-Fa-f]{4}|0x[0-9A-Fa-f]{16,40}' "$f"
}

marker_count() {
  local token=$1
  { LC_ALL=C grep -oF "$token" "$security_md" || true; } | wc -l | tr -d '[:space:]'
}

n_pending=$(marker_count '<!-- security-channel: pending -->')
n_pvr=$(marker_count '<!-- security-channel: github-pvr -->')
n_fallback=$(marker_count '<!-- security-channel: fallback-contact')
n_region_start=$(marker_count '<!-- security-contact:published:start -->')
n_region_end=$(marker_count '<!-- security-contact:published:end -->')

fallback_ref=''
if [ "$n_fallback" -eq 1 ]; then
  fallback_ref=$( { LC_ALL=C grep -oE '<!-- security-channel: fallback-contact decision=[^[:space:]]+ -->' "$security_md" || true; } \
    | sed -E 's/^.* decision=([^ ]+) -->$/\1/' | awk 'NR==1' )
fi

decision_ref_valid() {
  local ref=$1 rest segment
  [ -n "$ref" ] || return 1
  [[ "$ref" =~ ^Tchori-Labs/main:decisions/([A-Za-z0-9][A-Za-z0-9._-]*/)*[A-Za-z0-9][A-Za-z0-9._-]*\.md$ ]] || return 1
  [[ "$ref" != *'@'* && "$ref" != *'\'* && "$ref" != *'%'* && "$ref" != *[[:space:]]* ]] || return 1
  [ "${ref//:/}" != "$ref" ] || return 1
  [ "$(printf '%s' "$ref" | tr -cd ':' | wc -c | tr -d '[:space:]')" -eq 1 ] || return 1
  rest=${ref#Tchori-Labs/main:}
  [[ "$rest" == decisions/* && "$rest" != */../* && "$rest" != */./* && "$rest" != *//* ]] || return 1
  segment=${rest##*/}
  [[ "$segment" != EXAMPLE-* ]]
}

fallback_ref_valid=false
if [ "$n_fallback" -eq 1 ] && decision_ref_valid "$fallback_ref"; then fallback_ref_valid=true; fi

region_valid=false
region_has_affordance=false
outside_has_contact=false
if [ "$n_region_start" -eq 1 ] && [ "$n_region_end" -eq 1 ]; then
  start_ln=$( { LC_ALL=C grep -nF '<!-- security-contact:published:start -->' "$security_md" || true; } | awk 'NR==1' | cut -d: -f1 )
  end_ln=$( { LC_ALL=C grep -nF '<!-- security-contact:published:end -->' "$security_md" || true; } | awk 'NR==1' | cut -d: -f1 )
  if [ -n "$start_ln" ] && [ -n "$end_ln" ] && [ "$end_ln" -gt "$start_ln" ]; then
    LC_ALL=C awk -v s="$start_ln" -v e="$end_ln" 'NR>s && NR<e' "$security_md" >"$work/region"
    LC_ALL=C awk -v s="$start_ln" -v e="$end_ln" 'NR<=s || NR>=e' "$security_md" >"$work/outside"
    total=$(LC_ALL=C awk 'END{print NR+0}' "$work/region")
    blank=$(LC_ALL=C grep -cE '^[[:space:]]*$' "$work/region" || true)
    bad=0
    while IFS= read -r line || [ -n "$line" ]; do
      printf '%s\n' "$line" >"$work/line"
      if ! contact_affordance_present "$work/line"; then bad=$((bad + 1)); fi
    done <"$work/region"
    if contact_affordance_present "$work/region"; then region_has_affordance=true; fi
    if contact_shaped_value_present "$work/outside"; then outside_has_contact=true; fi
    if [ "$total" -ge 1 ] && [ "$total" -le 3 ] && [ "$blank" -eq 0 ] && [ "$bad" -eq 0 ]; then region_valid=true; fi
  fi
fi

fallback_qualifies=false
if [ "$fallback_ref_valid" = true ] && [ "$region_valid" = true ] && [ "$region_has_affordance" = true ] && [ "$outside_has_contact" = false ]; then
  fallback_qualifies=true
fi

# One live PVR read is shared by C1 and C3.
pvr_state=unreadable
if command -v gh >/dev/null 2>&1; then
  if gh api "repos/${repo}/private-vulnerability-reporting" >"$work/pvr" 2>"$work/pvr.err"; then
    if LC_ALL=C grep -qE '"enabled"[[:space:]]*:[[:space:]]*true' "$work/pvr"; then pvr_state=enabled
    elif LC_ALL=C grep -qE '"enabled"[[:space:]]*:[[:space:]]*false' "$work/pvr"; then pvr_state=disabled
    fi
  fi
fi

# C1 — at least one usable channel.
if [ "$pvr_state" = enabled ] || [ "$fallback_qualifies" = true ]; then
  record_pass C1 "an operative reporter-actionable private channel is declared"
elif [ "$pvr_state" = disabled ]; then
  record_fail C1 "no operative reporter-actionable private channel is available"
else
  record_unauditable C1 "live PVR state is unreadable and no qualifying fallback is declared"
fi

# C2 — offline document consistency. Placeholder domains are allowed here only
# because the stub harness uses them. That is unrelated to angle-bracket
# placeholder values, which both detectors intentionally reject in pending.
notice=false; runbook=false; pvr_described=false; fallback_described=false
if LC_ALL=C grep -qF 'Maintainer action required' "$security_md"; then notice=true; fi
if LC_ALL=C grep -qF 'docs/security-disclosure-channel.md' "$security_md"; then runbook=true; fi
if LC_ALL=C grep -qiE 'private vulnerability reporting[^\n]*(enabled|operative)|enabled[^\n]*private vulnerability reporting' "$security_md"; then pvr_described=true; fi
if LC_ALL=C grep -qiE 'board-approved[^\n]*(fallback|private security contact)|fallback[^\n]*board-approved' "$security_md"; then fallback_described=true; fi

c2_ok=false
if [ "$n_pending" -eq 1 ] && [ "$n_pvr" -eq 0 ] && [ "$n_fallback" -eq 0 ]; then
  shaped=false; if contact_shaped_value_present "$security_md"; then shaped=true; fi
  if [ "$notice" = true ] && [ "$runbook" = true ] && [ "$shaped" = false ] && [ "$n_region_start" -eq 0 ] && [ "$n_region_end" -eq 0 ]; then c2_ok=true; fi
elif [ "$n_pending" -eq 0 ] && [ "$n_pvr" -le 1 ] && [ "$n_fallback" -le 1 ] && [ $((n_pvr + n_fallback)) -ge 1 ]; then
  channels_described=true
  if [ "$notice" = true ]; then channels_described=false; fi
  if [ "$n_pvr" -eq 1 ] && [ "$pvr_described" != true ]; then channels_described=false; fi
  if [ "$n_fallback" -eq 1 ] && { [ "$fallback_described" != true ] || [ "$fallback_ref_valid" != true ] || [ "$region_valid" != true ] || [ "$region_has_affordance" != true ] || [ "$outside_has_contact" != false ]; }; then channels_described=false; fi
  if [ "$n_fallback" -eq 0 ] && { [ "$n_region_start" -ne 0 ] || [ "$n_region_end" -ne 0 ]; }; then channels_described=false; fi
  if [ "$channels_described" = true ]; then c2_ok=true; fi
fi
if [ "$c2_ok" = true ]; then record_pass C2 "committed policy state, notice, markers, and published region are consistent"
else record_fail C2 "committed policy violates the marker, notice, contact-value, decision-reference, or published-region contract"; fi

# C3 — Option A marker agrees with live state.
if [ "$pvr_state" = unreadable ]; then record_unauditable C3 "live PVR state cannot be read"
elif { [ "$pvr_state" = enabled ] && [ "$n_pvr" -ge 1 ]; } || { [ "$pvr_state" = disabled ] && [ "$n_pvr" -eq 0 ]; }; then record_pass C3 "github-pvr marker agrees with live PVR state"
else record_fail C3 "github-pvr marker drifts from live PVR state"; fi

# C4 — valid declared fallback path existence only; never request its body.
if [ "$n_fallback" -ne 1 ] || [ "$fallback_ref_valid" != true ]; then
  record_not_applicable C4 "no valid fallback-contact marker declared"
else
  decision_path=${fallback_ref#Tchori-Labs/main:}
  if ! command -v gh >/dev/null 2>&1; then
    record_unauditable C4 "state repository unreadable from this identity"
  elif ! gh api "repos/${state_repo}" --jq .full_name >"$work/state" 2>"$work/state.err"; then
    record_unauditable C4 "state repository unreadable from this identity"
  elif gh api "repos/${state_repo}/contents/${decision_path}" --jq .path >"$work/decision" 2>"$work/decision.err"; then
    record_pass C4 "declared fallback decision path resolves"
  elif LC_ALL=C grep -qi 'HTTP 404' "$work/decision.err"; then
    record_fail C4 "declared fallback decision file is absent from the readable state repository"
  else
    record_unauditable C4 "declared fallback decision path cannot be audited"
  fi
fi

exit_with_status

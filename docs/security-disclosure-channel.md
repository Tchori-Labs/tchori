# Private security disclosure channel

This runbook is the repository-admin and board procedure for making
[`SECURITY.md`](../SECURITY.md) an operative private disclosure policy. Committing
this runbook changes no GitHub setting and designates no contact. The human gates
in [`AGENTS.md`](../AGENTS.md) remain authoritative.

## Current state (2026-09-02)

A read-only audit observed:

- `gh api repos/Tchori-Labs/tchori/private-vulnerability-reporting` returned
  `{"enabled":true}`: GitHub private vulnerability reporting (PVR) is now
  enabled.
- `gh api repos/Tchori-Labs/main --jq '[.full_name, .default_branch] | @tsv'`
  proved the state repository readable. One Git Trees request for
  `main:decisions?recursive=1` returned `truncated:false`, but local `jq` was
  unavailable. The single snapshot therefore could not be parsed without a
  forbidden second request. The fallback verdict remains **NOT AUDITABLE
  (`listing-tool-unavailable`)**, verdict rule V1; no Option B candidate was
  classified.

Option A is now proven operative: live PVR agreement (C3) passes against the
`{"enabled":true}` readback. Option B remains unaudited rather than proven or
disproven; an unverified option never downgrades an independently proven
option, and V3 (`APPROVED`) wins once at least one option is operative.
`SECURITY.md` still carries whichever marker state was current when it was
last edited; re-run [`verify-security-contact.sh`](../scripts/verify-security-contact.sh)
and update the document's marker and published region (C1/C2) before treating
the channel as fully closed out end to end.

## Option A: enable GitHub PVR (repository admin only)

In GitHub, open **Settings → Code security → Private vulnerability reporting**
and choose **Enable**. The API equivalent is:

```sh
gh api repos/Tchori-Labs/tchori --jq .permissions
gh api --method PUT repos/Tchori-Labs/tchori/private-vulnerability-reporting
gh api repos/Tchori-Labs/tchori/private-vulnerability-reporting
```

The first command must show `admin:true`. An agent identity without that
permission must stop and must not claim the channel is active. Preserve the API
readback and run [`verify-security-contact.sh`](../scripts/verify-security-contact.sh)
after the setting changes.

## Option B: approve and publish a fallback contact

The board must approve a durable maintainer-controlled private channel, scope it
to this repository, and explicitly authorize publication in `SECURITY.md`. The
board records that decision under `decisions/` in the company state root
`Tchori-Labs/main`. No address, handle, key, or other value may be invented,
guessed, or published before that record exists. The marker contains only the
non-secret decision path.

A designation without publication authorization, without an extractable
payload, or without either is not operative. Keep the notice and omit the
fallback marker. The board can authorize publication and re-record the decision
with a payload; alternatively a maintainer can perform the policy edit by hand,
or an admin can enable Option A.

### Make the decision machine-checkable

Each line-leading field must occur exactly once and have exactly the required
value. Optional Markdown list, quote, or emphasis syntax around a field is
accepted; narrative prose is not.

| Field | Required value |
| --- | --- |
| `Status:` | `Approved` |
| `Scope:` (or `Applies to:` / `Repository:`) | `Tchori-Labs/tchori` |
| `Designation:` | `private-security-contact` |
| `Publish:` | `Approved` |
| `Approved:` | one ISO `YYYY-MM-DD` date and nothing else |

The date has the same exactly-once/exact-value contract. Two `Approved:` fields,
even with the same date, a date in prose, a date range, or a non-ISO date yields
`NOT AUDITABLE(date-ambiguous)`. Prose can innocently mention this repository and
a security contact in an unrelated decision, so it can never establish scope or
designation. A topical record without the exact fields yields `NOT AUDITABLE
(scope-not-machine-readable)`: re-record it with the fields or ask a maintainer
to publish by hand. Automated publication does not occur.

The following block is a **non-real contract illustration**. Its placeholder and
all paths are examples, not a designated contact or a recorded decision.

<!-- contract-examples:start -->
```md
Status: Approved
Scope: Tchori-Labs/tchori
Designation: private-security-contact
Publish: Approved
Approved: YYYY-MM-DD

<!-- publish:security-contact:start -->
Private security contact: <value>
<!-- publish:security-contact:end -->

<!-- security-channel: pending -->
<!-- security-channel: github-pvr -->
<!-- security-channel: fallback-contact decision=Tchori-Labs/main:decisions/EXAMPLE-security-contact.md -->

<!-- security-contact:published:start -->
Private security contact: <value>
<!-- security-contact:published:end -->
```

Rejected decision-path illustrations include `decisions/../README.md`,
`decisions/./x.md`, `decisions//x.md`, `/decisions/x.md`, and
`decisions/%2e%2e/x.md`.
<!-- contract-examples:end -->

A publication block must have exactly one occurrence of each sentinel token in
the entire decision, on separate lines in start-then-end order. Its body is the
1–3 lines strictly between them. It has no empty, blank, whitespace-only, or CR
line (including a trailing blank line); every line must be a contact affordance;
and no body line may contain decision metadata or an HTML comment marker. Save
it with LF endings. The block lets an agent splice authorized text without
reading, printing, or transcribing it. A malformed or absent block does not
undo board authority, but requires human publication.

### How a decision is classified

Candidate classification is first-match-wins:

| Rule | First matching condition | Class |
| --- | --- | --- |
| R1 | decoding gate fails | `AMBIGUOUS(reason)` |
| R2 | no designation field and no topical narrative hint | `OUT-OF-SCOPE` |
| R3 | exactly one recognized non-approved status | `OUT-OF-SCOPE` |
| R4 | status is not exactly one exact approval | `AMBIGUOUS(status-ambiguous)` |
| R5 | exact scope or designation is missing | `AMBIGUOUS(scope-not-machine-readable)` |
| R6 | exact approval date is missing or duplicated | `AMBIGUOUS(date-ambiguous)` |
| R7 | publication fields duplicate or approval contradicts denial | `AMBIGUOUS(publication-ambiguous)` |
| R8 | supersession language is unresolved | `AMBIGUOUS(superseded-unresolved)` |
| R9 | publication is unauthorized or payload is not extractable | `APPROVED-NOT-PUBLISHABLE(reasons)` |
| R10 | all checks pass | `OPERATIVE-FALLBACK` |

R7 deliberately precedes R9: duplicate or contradictory publication fields are
ambiguous, not merely unpublishable. R9 reasons use fixed order:
`publication-not-authorized+payload-not-extractable` when both apply.

The aggregate verdict is also first-match-wins:

| Rule | First matching condition | Verdict |
| --- | --- | --- |
| V1 | probe, enumeration, or complete classification is inconclusive | `NOT AUDITABLE(stage-token)` |
| V2 | any candidate is ambiguous | `NOT AUDITABLE(first-candidate-token)` |
| V3 | at least one operative candidate | `APPROVED`, latest date wins; a latest-date tie is `multiple-operative-decisions` |
| V4 | at least one approved-but-unpublishable candidate | latest date and fixed-order union of reasons |
| V5 | all candidates are conclusively out of scope, or none exist | `NONE` |

V2 precedes V3 because an ambiguous record could be an approval or rescission.
No Markdown decision is skipped due to its filename: every file under
`decisions/` is decoded and classified. A positive `NONE` cannot rest on a
human-chosen title.

### Enumeration, decoding, and payload validation

Enumeration uses one Git Trees response from
`git/trees/<default-branch>:decisions?recursive=1`. Both `.truncated` and all
candidate paths are read from that same saved snapshot. Two requests would
certify one snapshot while classifying another. The Contents directory API is
not used: GitHub silently caps it at 1,000 entries and supplies no pagination or
truncation signal. Only `truncated:false`, or a `decisions/` 404 after a
successful repository probe, can support `NONE`. Unreadable, truncated,
missing/unparseable-flag, or no-`jq` snapshots yield `listing-unreadable`,
`listing-truncated`, `listing-completeness-unverifiable`, or
`listing-tool-unavailable`. If Trees itself exceeds its limits, redesign the
audit rather than assuming cleanliness.

Every Markdown blob enters an encoding gate before content inspection. The gate
checks metadata readability, `type=file`, `encoding=base64`, size at most
1,048,576 bytes, raw fetch success, exact byte count, no NUL, no Git LFS pointer,
and UTF-8 (or ASCII when `iconv` is unavailable). Failure tokens are
`metadata-unreadable`, `not-a-file`, `encoding-not-base64`, `oversize`,
`fetch-failed`, `size-mismatch`, `contains-nul`, `lfs-pointer`, `not-utf8`, and
`decode-tool-unavailable`. Every failure is ambiguous, never absent.

The body is held only in a mode-0600 file in a trap-cleaned `mktemp -d` outside
the worktree. Shell variables cannot hold NUL. Payload checks scan the original
sentinel-bounded line range; command substitution removes trailing newlines and
could hide a forbidden trailing blank line. The validator runs with errexit,
nounset, and pipefail, guards every expected no-match, uses `awk 'NR==1'` rather
than `head`, and is self-tested before real content is read. A no-sentinel file
returns `false no-block` rather than aborting.

## Marker and published-region contract

Let `n_pending`, `n_pvr`, and `n_fallback` count marker-comment occurrences.
Exactly one state is valid:

- pending: `n_pending == 1`, `n_pvr == 0`, `n_fallback == 0`;
- operative: `n_pending == 0`, `n_pvr <= 1`, `n_fallback <= 1`, and
  `n_pvr + n_fallback >= 1`.

One PVR and one fallback marker together is valid. Duplicate same-kind markers,
pending mixed with operative, and marker-free documents fail. Pending requires
the maintainer notice, the runbook link, no published-region sentinel, and no
contact-shaped value. Operative requires no notice and prose describing every
declared channel.

A fallback marker requires exactly one balanced published-contact region. Its
body is 1–3 nonblank lines, every line is an affordance, and no contact-shaped
value may occur outside it. With no fallback marker, neither region sentinel may
occur. This bounded home makes verification branch-aware: it strips only this
region and separately verifies it, rather than exempting the whole policy.

The decision reference must match the `Tchori-Labs/main:decisions/` namespace,
end in `.md`, and consist only of nonempty segments beginning alphanumerically.
Dot, empty, leading-dot, leading/trailing-slash, percent-encoded, whitespace,
backslash, extra-colon, `@`, and mail-scheme forms are rejected. A lexical
`path.Clean` containment check provides a second guard. A final segment with the
reserved `EXAMPLE-` prefix is illustrative only and is rejected in live policy.
A fallback marker without both a valid reference and a reporter-usable
affordance is a hard failure.

### Two deliberately separate detectors

A **contact affordance** asks whether a reporter can act: (1) a mail URI with a
nonempty address; (2) an RFC-5322-shaped bare address; or (3) a line which,
after optional Markdown list/emphasis syntax, begins with the literal private
contact label, a colon, and a nonempty same-line value. It alone drives C1,
payload validation, and published-region body validation. It must not be
widened to key material.

A **contact-shaped value** includes every affordance plus an ASCII-armored PGP
header, a bare 40-hex fingerprint, ten groups of four hex characters separated
by spaces or hyphens, or a `0x`-prefixed 16–40-hex key ID. It drives pending and
outside-region prohibitions. Every affordance is contact-shaped, but key
material alone is not an affordance.

Neither detector exempts angle-bracket placeholders. The only exemption is the
documentation-example check described below; it must never enter the script or
Go detectors.

## Contract examples and synthetic fixtures

The `contract-examples` sentinels may occur only in this file, immediately after
text identifying non-real illustrations. Their blocks may teach marker, field,
payload, region, placeholder-path, and rejected-path grammar. They contain no
contact-shaped value except the angle-bracket-placeholder label needed to show
the payload skeleton. Step 6 strips these blocks before the strict docs scan,
then scans their bodies with a dedicated placeholder-exempt
`label_re_doc_examples` pattern. The fingerprint syntax and every other detector
remain active inside the blocks.

Synthetic addresses using the reserved `.invalid` TLD and synthetic PGP-shaped
blocks containing the token `NOT-A-REAL-KEY` are allowed only under
`internal/security/` and in never-committed validator temp fixtures.
Fingerprint fixtures are built at runtime in Go, never committed as literal hex
runs. `CheckNoSyntheticContact` rejects placeholder domains in the live policy.
The shell auditor intentionally omits that domain guard because its stubbed-`gh`
harness uses synthetic fixtures. Placeholder domains and angle-bracket
placeholder values are different: only the former is tolerated by fixture-level
shell checks.

## Closing the notice

After re-audit proves Option A and/or Option B:

1. replace the pending marker with markers for proven options only;
2. for Option B, splice the exact authorized payload into the published region
   and cite the real decision path in the same edit;
3. remove the maintainer-action notice and describe every operative channel;
4. rerun the shell audit and Go policy tests.

If fields, decoding, payload, or safe non-transcribing publication fail, retain
the pending state. The board must re-record the decision or a maintainer must
perform the bounded policy edit by hand.

## Audit

Run:

```sh
scripts/verify-security-contact.sh
```

Overrides are `GH_REPO`, `STATE_REPO`, and `SECURITY_MD`. Every non-fatal run
prints exactly one status for C1 through C4: operative channel, document
consistency, live PVR agreement, and fallback-reference existence. Statuses are
`PASS`, `FAIL`, and `NOT APPLIED / NOT AUDITABLE`. C4 prints a successful
`PASS … not applicable` when no valid fallback marker exists and makes no API
call. Exit is zero only with no failure or unauditable criterion.

Unlike the branch-protection verifier, API failures are aggregated rather than
exiting immediately: offline C2 and every other evaluable criterion still
report. Only a missing or unreadable policy is fatal C0. C4 first probes state
repository readability, then checks path existence with metadata only, so a
repository 404 is not misreported as a missing decision. Neither this auditor
nor its Go/stub harness reads or prints a decision body. The Go tests enforce
policy/readme consistency offline and exercise API/marker combinations; a real
run checks live PVR and the cited path.

# Main branch protection

`main` is the repository's apply boundary: changes reach it only through a
pull request, the required CI gate, and CODEOWNER approval. The importable
source of truth is
[`.github/rulesets/main-protection.json`](../.github/rulesets/main-protection.json).
The live GitHub repository setting remains the enforcement mechanism; keeping
the JSON in Git does not apply it by itself.

## What the ruleset enforces

| Requirement | Ruleset definition |
| --- | --- |
| Changes use a pull request with at least one approval from the owner selected by [`.github/CODEOWNERS`](../.github/CODEOWNERS). | `pull_request.required_approving_review_count` is `1` and `require_code_owner_review` is `true`. The current CODEOWNER is `@VictorCano`; change CODEOWNERS rather than duplicating the identity in the ruleset. |
| New pushes invalidate stale approval, the last pusher cannot supply the final approval, and review conversations are resolved. | `dismiss_stale_reviews_on_push`, `require_last_push_approval`, and `required_review_thread_resolution` are `true`. |
| CI succeeds on the current base branch before merge. | `required_status_checks` contains only the `check` context and `strict_required_status_checks_policy` is `true`. The `check` job directly runs the repository's formatting, vet, lint, and test gates. |
| Force pushes and deletion of `main` are blocked. | `non_fast_forward` and `deletion` rules are present. |
| No bot, App, deploy key, role, team, or agent has standing bypass. | `bypass_actors` is empty. This is stricter than a repository-wide Admin-role exception and prevents a present or future admin bot from inheriting bypass. |
| Tchorizo cannot push directly to `main`. | The pull-request rule is active and Tchorizo has no bypass. The operator proof below must also record an actually rejected direct-push attempt; the committed payload alone is not evidence of live enforcement. |

There is no standing break-glass bypass. In an emergency, a repository admin
must obtain the same recorded human authorization used for other apply
changes, preserve the pre-change ruleset response, make the smallest temporary
ruleset edit, record the actor and reason, and restore and re-verify this
payload immediately afterward. Giving the entire Admin repository role a
bypass is prohibited because automated identities may acquire that role.

## Apply or update (repository admin only)

The following commands mutate repository settings. They are an explicit
**human repository-admin/operator step**; an agent without `admin: true` in
repository permissions must stop after preparing the payload and must not
claim that protection is active.

Confirm the target and permissions first:

```sh
git remote -v
gh auth status
gh api repos/Tchori-Labs/tchori --jq .permissions
```

For a repository with no existing main ruleset, create it from the repository
root:

```sh
gh api --method POST repos/Tchori-Labs/tchori/rulesets \
  --input .github/rulesets/main-protection.json
```

If a main ruleset already exists, update that ruleset rather than creating an
overlapping rule. Obtain its ID from the audit command below, preserve its
current response as evidence, and run:

```sh
RULESET_ID=<existing-main-ruleset-id>
gh api --method PUT "repos/Tchori-Labs/tchori/rulesets/${RULESET_ID}" \
  --input .github/rulesets/main-protection.json
```

GitHub may reject unsupported fields or a token without repository
administration instead of partially applying the request. Retain the complete
API response, then read the rule back and run the verifier before considering
the operation successful.

## Audit and verify

List rulesets, then read the selected rule in full:

```sh
gh api repos/Tchori-Labs/tchori/rulesets | python3 -m json.tool
RULESET_ID=<main-protection-ruleset-id>
gh api "repos/Tchori-Labs/tchori/rulesets/${RULESET_ID}" | python3 -m json.tool
```

Run the automated live-state audit from the repository root:

```sh
scripts/verify-branch-protection.sh
```

The verifier resolves `main-protection`, with a diagnostic fallback to the
single existing ruleset that explicitly targets `refs/heads/main`. It prints a
PASS or FAIL for each machine-auditable criterion and exits non-zero when the
rule is absent, wrong, ambiguous, or cannot be fully audited. GitHub hides
bypass actor details from non-admin tokens, so a non-admin run fails those
audit criteria instead of reporting a false PASS.

A PASS verifies live ruleset structure. It does not replace the behavioral
observations below.

## Non-destructive behavioral verification

A repository admin performs this procedure after the API readback and verifier
both match the committed payload. Do not merge the test pull request, do not
approve it with the author identity, and do not weaken the ruleset.

1. Record the current `main` SHA and create an empty commit on a uniquely named
   throwaway branch:

   ```sh
   baseline=$(gh api repos/Tchori-Labs/tchori/git/ref/heads/main --jq .object.sha)
   branch="verify/tc-013-$(date +%Y%m%d%H%M%S)"
   git switch --create "$branch" "origin/main"
   git commit --allow-empty -m "test(TC-013): exercise main protection"
   git push --set-upstream origin "$branch"
   ```

2. Open a draft-free test PR without merging it. Record its URL and API state:

   ```sh
   pr_url=$(gh pr create --base main --head "$branch" \
     --title "test(TC-013): verify main protection" \
     --body "Automated-agent test only. Do not approve or merge.")
   gh pr view "$pr_url" --json url,reviewDecision,mergeStateStatus,statusCheckRollup
   ```

3. Wait for `check` to finish. Confirm the PR remains blocked without a
   CODEOWNER approval. The live ruleset response must show that `check` is
   required and strict; retain both the PR JSON and ruleset readback as
   evidence. Do not ask the authoring Tchorizo identity to approve.
4. Reconfirm that `main` still equals `$baseline`, then attempt the test
   branch's commit as a normal direct update to `main`. This attempt is made
   only after the all-PASS admin audit proves the PR rule is active, and its
   expected result is a GitHub ruleset rejection:

   ```sh
   test "$(gh api repos/Tchori-Labs/tchori/git/ref/heads/main --jq .object.sha)" = "$baseline"
   if git push origin "${branch}:refs/heads/main"; then
     echo "FAIL: direct push unexpectedly succeeded; stop and escalate" >&2
     exit 1
   fi
   test "$(gh api repos/Tchori-Labs/tchori/git/ref/heads/main --jq .object.sha)" = "$baseline"
   ```

   Capture the remote rejection text. If the push unexpectedly succeeds,
   stop: protection is not proven and the repository admin must remediate the
   unexpected `main` change through the normal reviewed process.
5. Close the unmerged PR, delete the remote branch, and return to the prior
   local branch:

   ```sh
   gh pr close "$pr_url"
   git push origin --delete "$branch"
   git switch -
   git branch -D "$branch"
   ```

Evidence is complete only when it contains the admin-visible API readback, an
all-PASS verifier run, the unapproved PR's blocked state with `check`, the
rejected direct-push output, the unchanged `main` SHA, and cleanup of the PR
and branch.

## Current application status

Repository admin `@VictorCano` applied the TC-013 controls to live ruleset
`19127009` (`protect-main-releases-only`) on 2026-07-19. The admin handoff and
application readback are recorded on
[issue #21](https://github.com/Tchori-Labs/tchori/issues/21#issuecomment-5017039145).
The live API response shows active enforcement on `refs/heads/main`, all pull
request review controls, strict required `check`, deletion and non-fast-forward
rules, and `current_user_can_bypass: "never"`; the administrator recorded no
bypass actors.

The non-destructive behavioral check used unmerged
[PR #42](https://github.com/Tchori-Labs/tchori/pull/42). After `check` passed,
the PR remained `BLOCKED` with `reviewDecision: REVIEW_REQUIRED`. A direct push
by Tchorizo was rejected with `GH013`, the `main` SHA remained
`e408ca5a98ce721b43943304c583145072d39964`, and the PR and throwaway branch
were closed and deleted without merging. These observations prove the gate for
the Tchorizo identity; future audits should still run the verifier and repeat
the procedure after material ruleset changes.

## Preserved human gates

This ruleset technically reinforces, and does not replace, the repository
rulebook in [`AGENTS.md`](../AGENTS.md):

- CODEOWNERS approval is the apply gate for `main`; the current owner comes
  from `.github/CODEOWNERS` (`@VictorCano`).
- Agents never merge or approve their own pull requests.
- Releases still require board sign-off recorded in `Tchori-Labs/main`.
  Agents may prepare a release PR; they never tag or push a release.

The release and board-sign-off rules are unchanged by this ruleset.
`AGENTS.md` remains the authoritative human-gate rulebook. TC-007's
complementary triage and merge ownership documentation can be cross-linked
here after `docs/task-ownership.md` lands.

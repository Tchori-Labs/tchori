# Task ownership and gated release path

This page assigns responsibility for moving approval-gated work from planning
to execution and for handling an approved pull request. It supplements the
repository rules in [`AGENTS.md`](../AGENTS.md); it does not replace or weaken
them.

## Owners

| Responsibility | Owner | Scope |
| --- | --- | --- |
| Triage release and replan decisions | **Triage Lead** (`triage` role) | Verify the required human decision, release approved `awaiting-approval` tasks, and return `needs-replan` tasks through planning. This is a non-authoring role. |
| Merge handling | **Merge Steward** (`merger` role) | Handle merge mechanics only after required checks and human CODEOWNERS approval. This is a non-authoring, non-reviewing role. |

These are dedicated roles, separate from the code-authoring DevOps Automator,
Fullstack Engineer, and SRE agents and from the Reviewer and Cloud Security
Architect reviewers. That separation makes authors, reviewers, and the merge
owner disjoint.

The live control-plane assignment is tracked by Fusion task `TC-027`. TC-027
is the board-actionable assignment record because the fn agent control-plane
calls made during TC-007 could not start their TaskStore. Until `fn_agent_show`
and `fn_agent_org_chart` visibly confirm both named owners and roles, the
roles are **pending**, not silently or implicitly delegated to an authoring
agent. A human operator retains the decisions below while the assignment is
pending.

## Release path from triage

The triage column is an approval boundary, not an execution queue. A task may
leave it only through the following path.

### `awaiting-approval`

1. The board records an explicit approval for that task. Approval of another
   task, a complete specification, or an agent recommendation does not count.
2. Triage Lead verifies that approval, dependencies, and the task's workflow
   are current. The owner records the decision on the task for auditability.
3. Triage Lead releases the task's manual hold with the dashboard's promote
   action or `fn_task_promote(task_id="TC-NNN")`. The workflow routes the task
   to its configured execution destination (normally `todo`); the owner does
   not edit its `PROMPT.md` as part of release.
4. An executor may claim the task only after that transition is visible on
   the board.

### `needs-replan`

1. Triage Lead records the review findings that require replanning and resumes
   the workflow's replan path rather than releasing the old plan to an
   executor.
2. The planning and plan-review nodes produce and review a corrected plan.
   Triage Lead verifies that the blocking findings are resolved and that any
   required board approval applies to the corrected scope.
3. Only then does Triage Lead release the resulting manual hold with the
   dashboard promote action or `fn_task_promote`. A task still marked
   `needs-replan` never goes directly to execution.

If approval evidence is absent or ambiguous, the task stays in triage. Agents
must not use promote, retry, workflow changes, or a replacement task to route
around the hold.

## Pull-request and merge governance

Gated pull requests use the **`builtin:pr-workflow` PR lifecycle** for merge
handling. Its `await-review` hold reconciles GitHub review state and reaches
the merge gate only from an `approved` outcome. For this repository, an
approved outcome means GitHub records approval from the CODEOWNER declared in
[`.github/CODEOWNERS`](../.github/CODEOWNERS):

```text
* @VictorCano
```

The repository rules remain authoritative:

- Never merge or approve your own PRs. CODEOWNERS review is the apply gate,
  same as `main`.
- The Merge Steward does not author implementation changes, approve PRs, or
  merge a PR it authored.
- Required checks and `@VictorCano` approval must both be present before the
  Merge Steward handles a merge. Force-merge and auto-merge without that
  approval are prohibited for gated tasks, even if a workflow exposes such a
  transition.
- No releases happen without board sign-off. That human decision must be
  recorded in `Tchori-Labs/main`. Agents may prepare a release PR; they do not
  tag a version or push a release.

The merge owner performs mechanics after the human gate; ownership never
turns an agent into an approver and never substitutes fn review for GitHub
CODEOWNERS approval.

## Operator verification

After any ownership change, verify the control plane rather than relying on
this document alone:

1. `fn_agent_show` reports Triage Lead with role `triage` and Merge Steward
   with role `merger`.
2. `fn_agent_org_chart` shows exactly one owner for each role.
3. The author, reviewer, and merger sets remain disjoint.
4. A released task's board log includes its approval evidence and triage
   transition; a merged PR includes required checks and `@VictorCano`
   approval.

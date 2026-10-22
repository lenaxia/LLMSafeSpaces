# Worklog: #1847 PR3 — board_reclaim + escalation reasons + decommission note (spec items 5-7)

**Date:** 2026-10-22
**Session:** lane-1847 (platform-derived lease liveness), PR3 of the slicing.
**Status:** Complete — stacked on PR2 (feat/1847-liveness-core)

---

## Objective

Issue #1847 spec items 5-7: the companion primitives around
platform-derived liveness — board_reclaim for prior claimants (with the
attempts-unchanged rule), escalation reasons that distinguish
death/no-show from treadmill artifacts, and the decommission note for
the lane's lease-keeper cron.

## Work Completed

### Claim history (no migration)

The spawn-stamp (engine.go spawnSync) now runs its guarded UPDATE and a
`claim_assign` board_event in ONE transaction (id
`engine:assign:<task>:<token>` — deterministic per episode, replay-safe
by the (project_id, id) PK): {task_id, session, token}. This is the
assign history — every claim episode that stamped a session is
recoverable by charter token. All spawn paths (initial claim,
bounce/drift/exit-pending re-claims via spawnFor) ride the same
spawnSync, so coverage is total.

### board_reclaim (spec item 5)

- `Service.ReclaimTask` (pkg/board/reclaim.go): identity follows the
  PRIOR claim token from the charter envelope — the claim-history
  check resolves the context-holding session; NO LLM-declared session
  id exists on the path (the #1847 trust rule extended to identity).
- Guards: token ∈ this task's assign history (else 409 claim_token);
  no live claimant (backlog, or in_progress with lapsed lease — an
  unexpired lease refuses); pending_input is the human gate
  (manager-surface guard — reclaim cannot bypass the question);
  done/in_review refuse (RC5 / the bounce owns re-entry); effective
  pause checked in-tx (the bounce's belt); the mint re-checks
  stage+lapse+NOT paused server-side.
- The mint: fresh token+lease, the HISTORICAL session re-stamped as
  assignee, attempts UNCHANGED with claim_incremented_attempts=false
  (a 429 on this claim must not decrement what was never incremented),
  cooldown/appeal/lapse columns cleared, the #1813 overlay cleared
  (the recovery repaired the lapse's damage — no context loss ever
  happened), backlog→in_progress transition, `reclaim` audit event
  {by: prior_claimant, attempts_unchanged: true}, and a fresh
  claim_assign so a SECOND lapse reclaims the same way. No spawn —
  the claimant is alive (it just called); if it lies, the lease
  lapses again and the platform path heals.
- Surface: `POST /internal/v1/automation/board/tasks/reclaim`
  (board_automation.go), agentd MCP tool `board_reclaim`
  (priorClaimToken required-arg guard; description pins the
  409→read→reclaim-or-leave flow), loopback client method, appended to
  the unreleased `WorkerSeamWireV2`; the parity matrix gains the
  worker-only `reclaim_task` row (absent on REST/external MCP by
  design — external actors hold no claim tokens); the appeal directive
  text now names board_reclaim.

### Escalation reasons (spec item 6)

Both escalation sites (engine claimOnce + TaskStart) read the row's
`lapse_reason` (RC1's classification) and carry it in the escalate
event payload (`reason`; `attempts_exhausted` fallback). With
`lapse_reason` on the task read (PR2), pending_input rows are
manager-serviceable BY CAUSE: no-show classes (no_assignee/
session_unknown) vs treadmill artifacts (silent_beyond_grace/
stale_liveness/appeal_ignored).

### Decommission note (spec item 7)

CHANGELOG [Unreleased] documents that trigger `4c0629c5` on workspace
`b150f355` (the lease-keeper cron whose updated_at-near-spawn check
renewed stillborn claims — the corpse-renewal hole in the evidence
table) is unnecessary under platform liveness and deletable at the
owner's convenience. Nothing deleted from the repo (the trigger lives
in workspace state the platform does not modify — the charter's "do
NOT delete anything you can't see" rule).

## Red-first arc

Red commit (before the implementation): 6 of 7 reclaim/escalation pins
failing (claim_assign: 0 events; reclaim refused uniformly by the
stub; escalate event carried no reason). Two TEST-side bugs found
during the arc and fixed in the green commit's message: the
transition-count arithmetic, and a MoveStage arg-order slip that put
the evidence URL in requestID (the done_guard caught it — the guard
works).

## Verification

- Board integration suite (reclaim + escalation + full) green; the
  seven PR3 pins pass:
  PriorClaimantReclaimsWithContext (attempts unchanged, same session,
  fresh token), LiveClaimantRefused, LapsedInProgressReclaims,
  PendingInputRefused, TerminalRefused, PausedRefused,
  EscalationRecordsReason.
- Handlers suite green (parity matrix + compat replay incl. the new
  wire request), agentd suite green (tool registry + guidance pins),
  opencode wire pin green, repolint green.

## Acceptance mapping

- "A lapsed-then-alive claimant reclaims with context; attempts
  unchanged" → TestKill_ReclaimTask_PriorClaimantReclaimsWithContext
  (post-reclaim) + _LapsedInProgressReclaims (pre-reclaim) — plus
  PR2's appeal-window re-arm covers the never-repooled case.

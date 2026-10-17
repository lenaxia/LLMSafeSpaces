# Worklog (NNNN — final number assigned at merge) — #1696 Task Board design (w32)

**Date:** 2026-10-15
**Session:** w32 design lane, chartered by #1696. Worktree `/workspace/wt-1696`, branch `design/1696-task-board` off `origin/main` @ a22dd37e.
**Status:** Design doc delivered — `design/0067_2026-10-15_task-board.md`, PR to main.

## Objective

Produce the design doc for #1696 (Task Board: projects/epics/stories/tasks, auto-dispatch to workers, self-healing engine). The issue carries the complete owner-settled sketch; the design elaborates HOW, never re-litigates WHETHER. Design-only, no implementation.

## Study trail (all file:line claims verified in-tree @ a22dd37e)

- **Doc pattern**: design 0064 (options + phased plan + owner menu), 0065 (ruling record), 0066-in-flight (PR #1693: settled-rulings transcription + HOW elaboration — the closest template since #1696's sketch is likewise owner-settled).
- **#1691/PR #1693 (design 0066)**: short-ID mint (prefix + 8 chars `[0-9a-z]`, per-scope unique composite index, check-and-retry in Go), did-you-mean 404+suggestions (`session_suggest.go:59,112-118,125` — bounded Levenshtein, suggest-only, 404-class-only). Board reuses this machinery directly (prefixes prj-/epc-/sty-/tsk-); implementation sequences behind PR #1693.
- **Outbox/idempotency**: `outbox.go:430-470` Accept — clientMessageID, marker-after-persist (SetNX), race loser LRem + `Duplicate{AcceptedID}` → 200-not-error. Board events adopt this discipline verbatim (requestId = board_events.id, UNIQUE).
- **Epic 71 / #1643 frame**: single-owner stages, pending-input leases (`sessionstate/lease.go`), D6 evidence gating (`proxy_lifecycle.go:512-542`); #1685 owner ruling: liveness = evidence, not elapsed time — adopted as the heartbeat semantics (watermark-moved extends lease, silent heartbeat does not; pending-input pauses the lease clock).
- **Atomic-claim precedent**: D8 `uq_workflow_run_single_inflight` partial index (`000016:270-279`) — INSERT-as-gate; board claim is the UPDATE…WHERE-unclaimed-RETURNING form of the same class.
- **Parity tier template**: Epic 64 trigger/workflow — dual-scope handlers (`triggers.go:4-8,82-89`), me/org routes (`router.go:2076-2121`), external MCP (`pkg/mcp/server.go:19-48`, `workflow_tools.go:23-96`), agent-internal seam (`mcp_tools.go:1274-1347` mcpAutomation → `/internal/v1/automation/*` with pod SA token, `router.go:1302,1311`).
- **Auth**: `org_memberships` admin|member (`000001:230-236`), `workspaces.org_id` nullable, `owner_type` user|org CHECK (`000016:49`, `pkg/types/workflows.go:21,175`).
- **SSE**: persist-then-publish (`workspace.alert` + RecordAlert, `proxy_lifecycle.go:175,594-600`; `pkg/types/session.go:86`); #1646/#1687 hardening (reconnect reseed + periodic reconcile) applied to the board feed.
- **Worker execution precedent**: `engine.go:116-148` (sessionId ⟺ session exists invariants); abort rides the Act InterruptAction seam (worklog 0984 D1).

## Decisions made in the design (HOW-level, not re-litigation)

- Migration slot 000036 (+ helm mirror); four tables + board_events; display inheritance as materialized columns with one-UPDATE-per-table renames.
- BoardService = the only home of guards/validation/idempotency/audit; four thin projections; parity as a table-driven contract-test matrix generated from action×surface (missing rows fail the build).
- Dispatch = single claim UPDATE with `FOR UPDATE SKIP LOCKED` over the ready-set ORDER BY (priority, epic.vft, created_at); DRR counters are columns (no in-memory scheduler); evidence-bearing leases; throttle requeue rolls back the attempt increment; attempts incremented at claim time.
- Reconciler = R1 lease reclaims, R2 PR drift (conservative explicit anchors), R3 criteria re-eval (hourly + on-demand, evidence stored in blocked_eval), R4 epic completion, R5 cancelled immutability (trigger + pin). Deterministic event IDs for replay convergence.
- Authz: worker = pod SA token → owning user → §4.7 matrix row; claim token narrows in-flight writes; org-owner/admin full; member matrix carried as OPEN RULING R1 per the issue.

## Open rulings left to the owner (§8)

R1 member role matrix (with proposal), R2 org-workspace targeting breadth v1, R3 attempts-cap N (default 3), R4 criteria evaluator model + cadence, R5 PR-anchor syntax.

## Numbering notes

- Design 0067 taken: 0066 exists only in open PR #1693; taking 0067 avoids the in-flight collision (the design dir has tolerated in-flight duplicates historically — 0047/0052/0060 pairs — this avoids one).
- Worklog uses the NNNN_ sentinel (post-merge bot assigns the final number). Migration 000036 named for the implementation lane (last is 000035; repolint sequential rules honored at implementation time).

## Deliverable

`design/0067_2026-10-15_task-board.md` via PR `--base main` (explicit). Never self-merge; iterate review to APPROVED; comment on #1696 with the PR link at open, and "w32: PR #N APPROVED" at approval.

## 2026-10-16 addendum — owner rulings conformed

Two rulings arrived mid-review (relayed by the orchestrator):
1. **Blocking model** (#1696 comment 6204924480): blocked is purely structural; depends_on[] only; criteria/evaluator machinery deleted; external conditions become explicit tasks.
2. **Consolidated R1-R5 set** (#1696 comment 6204962615): R1 entities = epic+task only (stories CUT — schema, actions, routes, identifiers, parity rows all reworked); R2 exact field model (epic.outcome, derived-not-stored progress, acceptance_criteria jsonb, assignee_session, last_progress_at, board_transitions for evidence-per-transition + cycle-time actuals, NO story points); R3 structural blocking; R4 depends_on semantics (N-ary, ALL-Done unblock, cycle rejection 409+did-you-mean, ready-set without topo sort, cancelled-dep → pending_input "dependency abandoned", within-project/cross-epic/never-cross-project); R5 no DAG entities/milestones/phases.

Doc changes: §2b transcribes both rulings verbatim; reconciler invariant labels renamed R1-R5 → RC1-RC5 and open rulings → OR1-OR4 to avoid collision with the owner's ruling numbers; the criteria-evaluator open ruling is deleted (mechanism removed). Receipt confirmed on the issue thread per the orchestrator's ask.

Also: main moved twice during review (worklog renumbering + #1690) — merge conflict in COORDINATE.md resolved (both rows kept); one dropped-synchronize event recovered by re-pushing.

## 2026-10-16/17 review iterations (r2-r19 — APPROVED)

r1 (15 findings): attempts single-owner, DRR mechanics + columns, worker_slots in the claim CTE, schema guards (tautology/polymorphic FK/priority), ready-set index, lease re-arm, escalation exit, system actor, slot-cooldown home, parity reverse asymmetry, task_start/claimed_by, requestId contract, OR2 trim. r2-r3: residue passes (sty- route, off-by-one, exit wording, cancel cell, stage-machine enumeration, caller-workspace binding, total lease lifecycle). Mid-review: both owner rulings conformed (see addendum above). r4: reconciler core (RC2 done-guard + drift_review, drift re-claims, exit actor, cancelled-row stability, mechanized cross-project DRR, per-entity tags, archive actions, delete out of v1). r5: candidate-less-project EXISTS, fourth origin conformed, task_start scoped, RC4 done-trigger, cancel-epic pauses, no token-less in_progress. r6: candidate-anchored ORDER BY key, two-level rollover, assignee cleared on reclaim/cancel, activate-time RC4 re-check. r7: abandonment-exit provenance scoped (live-claim keeps the assignee, takes re-claim), re-open exempt from the re-check, biconditional scoped, epic-priority role stated (r7 correction: that edit silently missed its target row and actually landed in r8, with the worklog claim ahead of the doc — fixed). r8: throttle stage guard (no pending_input self-exit), assignee retirement on throttle, abandoned-from-in_review provenance, :314 gloss, epic-priority role landed for real (r7 claim corrected above). r9: attempts rollback made provenance-conditional (claim_incremented_attempts marker; re-claim-class 429s requeue without decrement; non-negative CHECK belt) + bounce-throttle pin; by-provenance pin exhaustive. r10: flag writers at every mint site (claim/task_start true; bounce/RC2/exit re-claim false; no-mint exits stated); worker-origin manager exit; RC1 full write list; paren balance. r11: resolution resume declared a no-mint preserve-case (flag untouched, both token provenances); RC2 reference re-pointed; normative reclaim UPDATE carries the flag clear (the r11 pin-count edit introduced a wrong count, corrected in r13). r12: zero substantive findings (first in 12 rounds); residues deferred to r13. r13: owner rulings round 2 folded (§2c transcribed verbatim — R6 platform knobs, R7 user-level-only boards, R8 single target confirmed, R9 always-fresh workers + forward-compat ownership/access note; §4.7 replaced, §4.4 worker model rewritten as claim-then-spawn, §8 OR1-OR3 settled); r12 minors fixed (pin arithmetic six/four, throttle column list, :215 duplication + stranded relative, hygiene pin auto-stage, COORDINATE author range). r14: the R9 claim-then-spawn rewrite conformed doc-wide (engine-side claim with assignee-NULL interim state + spawn-stamped identity + lease re-arm at stamp; every in-flight delivery is a spawn — the re-claim branch re-spawns, no dead-letter nudges; task_start owner-surface only; claim_next engine-internal everywhere; §4.7 matrix cells made deliverable; dispatch drive = event + sweep backstop; §4.3b/§5 conformed; interim-state/bounce-latency/dispatch-drive pins). r15: the two r14 edits that had silently missed their targets landed for real (engine workspace binding from the project row; §5 spawn wording) — the r14 message had claimed both; caller-identity vestiges removed (task_start param, schema gloss, assignee hints); the exit re-spawn aborts the superseded session + clears identity at mint (interim shape — the auto-stage scan can't revert the exit; scan keys on non-NULL assignee); the spawn-stamp guarded (stage+token; spawn-race losers aborted); glosses + slots enumeration conformed. r16: the two r15 replaces that silently missed their targets landed for real (:250 assignee hints — the r15 message and this log overclaimed it; :330 recorded-assignee gloss), and the COORDINATE author note corrected (r13-r15 commits carried a rotated pod-default label w36 — worktree user re-set to w32). r17: :330 timing precision (cleared at mint, stamped by the guarded spawn-stamp); author-note r12 boundary corrected (r12's minors live in the w36-labelled r13 commit). r18: the attempts-classification clause says not-incremented (the exit's reset-to-0 is the column's one mover); COORDINATE status current per-round. r19: APPROVED at aab25d73 (both r18 one-liners verified fixed; zero new findings from the primary sweep and the skeptical second pass; 'the normative machinery has now survived three consecutive adversarial rounds with no substantive findings'). Approval posted to #1696; awaiting orchestrator merge. The pre-ruling body above is superseded by §2b/§2c of the doc.

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

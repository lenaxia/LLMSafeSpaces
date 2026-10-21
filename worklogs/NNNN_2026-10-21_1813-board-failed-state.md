# NNNN — 2026-10-21 — #1813: board failed-state attribute overlay

## Context

Post-#1763, a task whose worker attempt failed (crash loop, lease
lapse, attempts-cap exhaustion) had no first-class visible state: it
read as ordinary backlog and was silently redispatched. Issue #1813
asked whether "failed" should be a sixth stage; the dispatch charter
adopted the issue's suggestion — an ATTRIBUTE OVERLAY — which
preserves the just-landed five-stage contract.

## What landed

- Migration 000040 (api + helm mirrors): `tasks.last_attempt_failed`,
  `failure_count`, `last_failure_at`.
- Engine sets the overlay at the two failure points:
  - `rc1Reclaims` (lease lapse): `failure_count+1`, `last_failure_at`,
    `cooldown_until = now() + base << (n-1)` (doubling, cap 1h) —
    auto-return-to-ready rides the EXISTING cooldown candidacy gate.
  - Claim-time escalation (attempts >= cap): the pending_input row
    carries the overlay (`failure_count >= attempts`) so the human
    sees WHY.
- Reset on success: in_review/done entry (`reset_overlay` clause) and
  both pending_input human exits (attempts-reset statements also clear
  the overlay + count).
- Knob: `EngineConfig.FailureBackoff` (default 60s) —
  `board.failureBackoffSeconds` (helm values + config default +
  `LLMSAFESPACES_BOARD_FAILUREBACKOFFSECONDS` env).
- Reads: `ListTasks(…, stage, failed)` tri-state; REST
  `GET /me/projects/{id}/tasks?failed=true|false` (400 otherwise);
  MCP `board_read {failed: true}`; openapi BoardTask fields + query
  param; UI `FAILED ×n` badge on the task card (never a column move);
  reclaim/escalate audit events carry the overlay fields.

## Red-first pins

- `pkg/board/1813_kill_test.go` (integration, Postgres-gated):
  1813-a reclaim marks overlay + backoff; 1813-b backoff delays then
  permits redispatch; 1813-c consecutive doubling + success reset;
  1813-d escalation marks the pending_input row; 1813-e the human exit
  clears the overlay with the attempts reset.
- Unit tiers (runnable locally, green): REST tri-state filter pin
  (board_rest_test), MCP `failed` arg threading pin
  (board_tools_test), UI badge pin (BoardPage.test — FAILED ×2 on a
  backlog card, data-stage stays backlog).

## r6-r9 (review rounds)

- r6: claim-path escalation overlay (the dominant automatic site) +
  helm env threading + MCP explicit false + all-four-SDK sync +
  MoveStage doc restore + coverage (drift-done regression pin 1813-f,
  ListTasks DB pin 1813-g, backoff boundary unit pins, REST semantic
  filter pin, worker-semantics seam e2e arm).
- r7: the read-path pin seeded FIFO-wrong (clean first) — fixed.
- r8: Go SDK variadic options (3-arg call sites keep compiling — the
  migration path), UI FAILED filter toggle, TaskStart-at-cap pin
  (1813-h), exitPendingReclaim reset pin (1813-i), Python/Java edge
  arms.
- r9 (truthful semantics, review-5): escalation sets the marker ONLY
  when failures exist (`last_attempt_failed = failure_count > 0`) —
  all-success exhaustion escalates WITHOUT a failure claim (no FAILED
  ×0; 1813-d/h flipped to the truthful assertions; new contrast pin
  1813-j keeps the crash-loop marker). Every reset site clears
  last_failure_at (migration contract). Escalate events carry
  exhausted=true (symmetric); the reclaim event carries the timestamp.
  boardApi failed-branch pins; helm chart knob pins (defaults +
  overrides, the seventh knob).

## Verification

Local: build + vet (unit + integration tags) green; pkg/board,
pkg/mcp, handlers unit suites green; frontend tsc + 13/13 BoardPage
tests. Integration kill pins run in CI (no local PG in the sandbox —
stated, not claimed). Adversarial subagent verify before push per the
standing gate.

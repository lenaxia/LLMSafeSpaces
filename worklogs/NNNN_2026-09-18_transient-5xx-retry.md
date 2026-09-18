# Worklog: #1441 — bounded retry on transient upstream 5xx for routine agent calls

**Date:** 2026-09-18
**Session:** The exposed result column (v0.34.2, #1446) caught the intermittent routine failure live: {"code":"script_failed","error":"opencode returned 500"}. Direct experiments ruled out memory mechanics and opencode concurrency (sequential 12/12, cross-session concurrency 6/6); the surviving cause is transient model-provider blips surfacing as opencode 500s. A single blip failed a fire and burned auto-disable budget.
**Status:** Complete

---

## Objective
A provider blip must not fail a fire or auto-disable a healthy trigger.

## Work Completed
- executeWithRetry: bounded (3 attempts, linear backoff 2s/4s, ctx-cancellable) around the routine agent-node AgentdClient.Execute.
- retryableAgentdFailure: retries ONLY the transient shapes — agentd transport "returned 500/502/503" and agentd script_failed wrapping "opencode returned 500/502/503" (provider blips). TIMEOUTS are explicitly OUT of the class (review r1): fresh-session retries of a timed-out turn risk double execution, and a 10m-timeout retry would triple the scheduler's worst-case per-fire latency. Deterministic failures never retried.

## Key Decisions
- Engine-side (not agentd-side): one retry policy for every executor implementation; agentd keeps faithful single-shot reporting.
- The exhausted path surfaces the SAME error as before — behavior unchanged beyond the retry window.

## Blockers
None.

## Tests Run
- Unit matrix: transient-recovers (exactly one retry), exhausted (bounded 3, failure surfaces), deterministic-no-retry, transport shapes (502 in; 404 out), timeout-not-retrried (504 transport, agentd script_timeout, opencode 504 wrap — each attempted once).
- WIRING (review r2): TestScheduler_RoutineFireRetriesTransient5xx — a pending webhook routine fire whose executor answers 500-then-success DELIVERS with exactly one retry; reverting the executeWithRetry call site leaves it red (0 retries, fire failed). Along the way the mock's ClaimDueCronTriggers gained store-parity due-gating (it returned every row, double-firing pending-drained webhook triggers).
- Full workflows suite green; e2e leg T7 records the wiring pin (a live provider-blip assertion would be flake-shaped by definition).

## Scope
The failure half of #1441 only: the session-listing symptom is #1452, the prompt-growth design decision is #1453.

## Next Steps
PR → review → release; post-deploy, the hunt harness should show delivered fires across provider blips.

## Files Modified
- api/internal/workflows/engine.go (+engine_test.go)

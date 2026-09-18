# Worklog: #1441 — bounded retry on transient upstream 5xx for routine agent calls

**Date:** 2026-09-18
**Session:** The exposed result column (v0.34.2, #1446) caught the intermittent routine failure live: {"code":"script_failed","error":"opencode returned 500"}. Direct experiments ruled out memory mechanics and opencode concurrency (sequential 12/12, cross-session concurrency 6/6); the surviving cause is transient model-provider blips surfacing as opencode 500s. A single blip failed a fire and burned auto-disable budget.
**Status:** Complete

---

## Objective
A provider blip must not fail a fire or auto-disable a healthy trigger.

## Work Completed
- executeWithRetry: bounded (3 attempts, linear backoff 2s/4s, ctx-cancellable) around the routine agent-node AgentdClient.Execute.
- retryableAgentdFailure: retries ONLY the transient shapes — agentd transport "returned 5xx" and agentd script_failed wrapping "opencode returned 5xx". Deterministic failures (invalid_node_data, unsupported language, 4xx) are never retried.

## Key Decisions
- Engine-side (not agentd-side): one retry policy for every executor implementation; agentd keeps faithful single-shot reporting.
- The exhausted path surfaces the SAME error as before — behavior unchanged beyond the retry window.

## Blockers
None.

## Tests Run
Transient-recovers (exactly one retry), exhausted (bounded 3, failure surfaces), deterministic-no-retry (invalid_node_data once), transport shapes (5xx retried, 404 not). Full workflows suite green.

## Next Steps
PR → review → release; post-deploy, the hunt harness should show delivered fires across provider blips.

## Files Modified
- api/internal/workflows/engine.go (+engine_test.go)

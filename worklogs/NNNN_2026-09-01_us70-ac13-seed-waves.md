# Worklog: US-70.0 pool run 6 — AC-13 create-side seeding in waves

**Date:** 2026-09-01
**Session:** Pool run 6 (33468410395): AC-1 PASS (first-spawn env + anchored spawnedRev=1:…) and AC-2 PASS — the ≥1h #1087 gate held at 12.7s (budget 90s). AC-13's provisioning wedged: workspace #4 never Active inside its whole 240s budget while the controller log showed an optimistic-concurrency storm ("object has been modified") across the fleet — 100 pods creating at once on one kind node, with status writes racing (controller phase writes vs API activity writes).
**Status:** Complete — harness fix; product finding recorded for the operator.

## Work Completed

- AC-13 provisioning seeds + binds + waits in waves of `SEED_WAVE` (default 20) with a 600s per-workspace budget; the RESUME burst stays ${SCALE}-wide — that concurrency is what AC-13 actually measures.
- Pin: `TestUS70Harness_AC13SeedsInWaves`.

## Key Decisions

1. **Create-burst ≠ the AC's subject.** AC-13 measures concurrent resumes; a simultaneous 100-pod create herd on one kind node tests the controller's conflict backoff, not delivery. Waves keep the measurement honest.
2. **Product finding, not fixed here:** the reconcile conflict storm (two status writers on the Workspace CR at burst scale — controller phase/status vs the API's batched lastActivityAt flush) deserves its own investigation (/status subresource partitioning or write backoff). Filed to the operator with the run-6 log as evidence.

## Tests Run

`go test -timeout 60s ./local/` ok; bash -n clean.

## Files Modified

- `local/us-70-secret-delivery-e2e.sh`, `local/us70_harness_script_test.go`

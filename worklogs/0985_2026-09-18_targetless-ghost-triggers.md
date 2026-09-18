# Worklog: #1440 — targetless triggers ticked silently (FK SET NULL zombie)

**Date:** 2026-09-18
**Session:** Live verification of missing-workflow auto-disable contradicted the green in-repo tests: a workflow-targeted cron trigger whose workflow was deleted ticked forever with ZERO fire rows and no failure count (prod v0.33.2 AND v0.34.0). Root cause found by live discrimination (a LIVE target fires fine; only deleted targets go silent) + migration reading.
**Status:** Complete

---

## Objective
Make every targetless trigger loud and self-disarming, whatever route made it targetless.

## Root cause
Migration 000020: triggers.workflow_id REFERENCES workflows(id) ON DELETE SET NULL. Deleting the workflow nulls the trigger's target in place; fireTrigger then routes to fireRoutineTarget, whose nil-workspace guard logged and returned BEFORE creating any fire row. The #1412 loud missing-workflow path in fireWorkflowTarget is unreachable for real deletes (the scheduler never sees a non-nil ghost WorkflowID). The nightly R4 row inserts its ghost UUID directly into the DB, bypassing the FK — why tests stayed green. #1442's create-response anomaly (rollout window) lands in the same targetless state by a different, non-reproducible route.

## Work Completed
- fireRoutineTarget's nil-target guard now records a FAILED fire (reason trigger_has_no_target + hint), increments consecutiveFailures, and honors autoDisableAfter — identical accounting to the missing-workflow path.
- Engine tests: targetless trigger records exactly one failed fire with the named payload + count (no run); auto-disable at threshold.
- Store integration test pins the FK semantics the fix relies on: delete succeeds, trigger SURVIVES, workflow_id NULL (never CASCADE, never RESTRICT).
- Nightly R4d added (create-then-delete through the user API): asserts the targetless trigger records trigger_has_no_target failed fires and auto-disables — the delete route now has its own row, distinct from R4's never-existed-UUID route (which rides the unchanged GetWorkflow-miss path).
- A store-level trg_target_present CHECK was considered and REJECTED (coordination with the #1442 author): with ON DELETE SET NULL, the CHECK would fail the workflow-delete transaction itself — deletion breaks. Engine-side loudness is the sound invariant for the SET NULL route.

## Key Decisions
- Fix at the ROUTE-AGNOSTIC point (the guard): whether the target was lost to FK SET NULL, a create anomaly (#1442), or authoring without workspace_id, the zombie behaves identically.

## Blockers
None.

## Tests Run
- TestScheduler_TargetlessTriggerFailsLoudly / _AutoDisables (engine) — PASS.
- TestWorkflowDeleteNullsTriggerTarget (store integration; Postgres suite in CI) — compiles, pins migration 000020.
- Full workflows suite green.
- Live discriminator preserved in the issue threads: live target fires (fires=1, run created) vs deleted target silent.

## Next Steps
PR → review → release → the live repro trigger (verify-ghost2-cron, left ticking) auto-disarms and shows failed fires.

## Files Modified
- api/internal/workflows/engine.go (+engine_test.go)
- pkg/workflows/store_integration_test.go

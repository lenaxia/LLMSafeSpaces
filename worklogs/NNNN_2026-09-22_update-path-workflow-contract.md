# Worklog: Fix #1519 — update-path workflow-existence contract (create parity)

**Date:** 2026-09-22
**Session:** Small product lane on branch `fix/1519-update-path-workflow-contract` (wt-1453).
**Status:** Complete

## Objective

Mirror create's workflow-existence contract on the triggers update path: PATCH retargeting to a nonexistent/foreign workflowId must answer the named 400 (create's "target workflow not found"), not the opaque 500 (store FK) / silent persist (cross-owner) of the current asymmetry.

## Work Completed

TDD (7-case matrix, RED-first on the two asymmetric arms):
- PATCH to nonexistent workflow → 400 "target workflow not found" (create parity)
- PATCH to foreign-owner workflow → 400 (owner-scoped GetWorkflow returns NotFound)
- PATCH to existing own workflow → 200
- PATCH NOT touching workflowId → no check (unrelated fields pass; #1440's drain-time guard owns the stored-target-deleted case)

Fix: one guard in the update handler, ordered after the #1442 targetless check and before the store update. The guard fires only when `req.WorkflowID != nil && *req.WorkflowID != ""` — patches that don't touch the target field skip it (incident-population editability preserved per the #1442 round-2 tests).

Two pre-existing tests updated to seed their referenced workflows in the mock store (the repair and swap tests previously used unseeded workflow IDs — valid before the check, invalid after; the real-world repair/swap targets exist by definition).

## Key Decisions

1. **Placement after #1442, not inside the V-matrix**: the V-matrix owns opted-in validation's specific errors; this guard owns the existence contract for every retarget regardless of opted-in status.
2. **SUPERSEDED by r1** ~~Not checking the merged view's stored workflow~~: a trigger whose stored target was deleted between create and PATCH is #1440's drain-time case — PATCH-time rejection would strand the incident population (the same reasoning as the #1442 round-2 tests).

## Tests Run

- 4-case matrix: RED on nonexistent + foreign arms; all 4 GREEN post-fix.
- Full handlers suite (94s) — ok, incl. the two updated tests.
- go vet, gofmt, golangci-lint (new-from-rev) — clean.

## Blockers

None.

## Next Steps

- Review loop to APPROVED; orchestrator merges.

## Files Modified

- `api/internal/handlers/triggers.go` — the update-path guard
- `api/internal/handlers/triggers_test.go` — 4-case matrix + 2 seeded workflows
- `worklogs/NNNN_2026-09-22_update-path-workflow-contract.md`

## Review Round 1 (the contract was too narrow — the ruling's merged-view scope adopted)

The reviewer caught that my guard fired only on `req.WorkflowID != nil` — narrower than the issue's ruling, which places the check INSIDE the `touchesMapping` block on the POST-PATCH MERGED `mergedWorkflowID`. The divergence: a de-opt patch (`{"input": null}`) on a stored-ghost row (legacy cross-owner wiring) would silently persist — the exact half-(b) harm. Fixed: the check now uses the merged view inside the mapping block; a de-opt on a ghost 400s; a non-mapping patch on a ghost stays editable.

Also this round: the stale V3 comment corrected ("only the UPDATE view persists onward" was false post-fix — both callers now close the hole); the test matrix expanded from 4 to 6 cases with persistence assertions on every arm (nothing-persisted on rejects, persisted on accepts); the R1d e2e row added to the nightly harness beside R1c; the dead `seedRoutineTriggerRowAt` scaffolding removed.

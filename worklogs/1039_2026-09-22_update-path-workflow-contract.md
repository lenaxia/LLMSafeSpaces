# Worklog: Fix #1519 — update-path workflow-existence contract (create parity)

**Date:** 2026-09-22
**Session:** Small product lane on branch `fix/1519-update-path-workflow-contract` (wt-1453).
**Status:** Complete

## Objective

Mirror create's workflow-existence contract on the triggers update path: PATCH retargeting to a nonexistent/foreign workflowId must answer the named 400 (create's "target workflow not found"), not the opaque 500 (store FK) / silent persist (cross-owner) of the current asymmetry.

## Work Completed

TDD (6-case matrix, RED-first on the asymmetric arms):
- PATCH to nonexistent workflow → 400 "target workflow not found", nothing persisted (create parity)
- PATCH to foreign-owner workflow → 400, nothing persisted (owner-scoped GetWorkflow returns NotFound)
- PATCH to existing own workflow → 200, persisted
- De-opt patch on a stored ghost → 400 (the merged view surfaces the ghost)
- Org-scope retarget to a ghost (OrgUpdate, shared handler) → 400, nothing persisted
- Non-mapping patch on a stored ghost → 200 (incident-population editability preserved)

Fix: one guard inside the `touchesMapping` block in the update handler, on the POST-PATCH MERGED `mergedWorkflowID`, before the #1442 targetless check and the store update. Non-mapping patches skip it; every mapping-touching patch is checked (the ruling's scope — a de-opt on a stored ghost 400s too).

Two pre-existing tests updated to seed their referenced workflows in the mock store (the repair and swap tests previously used unseeded workflow IDs — valid before the check, invalid after; the real-world repair/swap targets exist by definition).

## Key Decisions

1. **SUPERSEDED by r1** ~~Placement after #1442, not inside the V-matrix; fires only when `req.WorkflowID != nil`~~: the ruling places the check INSIDE `touchesMapping` on the merged view — narrower placement let a de-opt on a stored ghost silently persist (half (b)'s exact harm).
2. **SUPERSEDED by r1** ~~Not checking the merged view's stored workflow~~: the ruling's contract DOES check the merged view; non-mapping patches skip (the #1442 population stays editable), but every mapping-touching patch surfaces stored ghosts.
3. **Guard inside `touchesMapping` on the merged view**: the V-matrix owns opted-in validation's specific errors; this guard owns the existence contract for every mapping-touching patch regardless of opted-in status, ordered before #1442 so the more specific contract 400 answers first.
4. **The store FK stays the integrity anchor** (ON DELETE SET NULL, #1440): the handler check is the contract surface, not a data-integrity mechanism.

## Tests Run

- 6-case matrix: rejection arms RED pre-fix; all 6 GREEN post-fix.
- PG-gated store-integration FK-face tests added (`pkg/workflows/store_integration_test.go`): nonexistent target rejected at the FK (the opaque 500's source), foreign-owner target persists at the store (why ownership lives at the handler). Compile-verified; skip without a live PG per the suite's design.
- Ledger pins: R1d + R1d-happy needles in `local/issue_1410_automation_e2e_script_test.go`; R1d-happy e2e row's specYaml escaping fixed (single-escape form).
- Full handlers suite — ok, incl. the two updated tests and the store-integration compile.
- go vet, gofmt, golangci-lint (new-from-rev) — clean.

## Blockers

None.

## Next Steps

- Review loop to APPROVED; orchestrator merges.

## Files Modified

- `api/internal/handlers/triggers.go` — the update-path guard + V3/V6 doc-bullet rewrites + the update arm's fall-through note
- `api/internal/handlers/triggers_test.go` — 6-case matrix, nothing-persisted assertions on all reject arms, seeded workflows with #1517 OWNER-OVERRIDE notes
- `pkg/workflows/store_integration_test.go` — the two ruled PG-gated FK-face tests
- `local/issue-1410-1412-automation-e2e.sh` — R1d-happy e2e row (escaping fixed)
- `local/issue_1410_automation_e2e_script_test.go` — R1d/R1d-happy ledger needles
- `design/0059_2026-09-17_trigger-input-mapping.md` — §8 OQ2 amended RESOLVED (both halves)
- `CHANGELOG.md` — [Unreleased] entry
- `worklogs/1039_2026-09-22_update-path-workflow-contract.md`

## Review Round 1 (the contract was too narrow — the ruling's merged-view scope adopted)

The reviewer caught that my guard fired only on `req.WorkflowID != nil` — narrower than the issue's ruling, which places the check INSIDE the `touchesMapping` block on the POST-PATCH MERGED `mergedWorkflowID`. The divergence: a de-opt patch (`{"input": null}`) on a stored-ghost row (legacy cross-owner wiring) would silently persist — the exact half-(b) harm. Fixed: the check now uses the merged view inside the mapping block; a de-opt on a ghost 400s; a non-mapping patch on a ghost stays editable.

Also this round: the stale V3 comment corrected ("only the UPDATE view persists onward" was false post-fix — both callers now close the hole); the test matrix expanded from 4 to 6 cases with persistence assertions on every arm (nothing-persisted on rejects, persisted on accepts); the R1d e2e row added to the nightly harness beside R1c; the dead `seedRoutineTriggerRowAt` scaffolding removed.

## Review Round 2–3 (record-vs-tree corrections + the ruling's ancillary deliverables)

Round 2 added the org-scope pin, the R1d e2e row, and the CHANGELOG entry — but the round-2 commit claimed ledger needles that had not landed (the pin-file edit's replacement target never matched; caught by grep in round 3) and the R1d-happy row's specYaml was double-escaped (unconditional 400 at setup). Round 3 landed the needles (grep-verified), fixed the escaping to the working single-escape form, added the two ruled PG-gated FK-face tests, rewrote the false V3/V6 doc bullets, completed the nothing-persisted assertions, added the #1517 OWNER-OVERRIDE seed notes, amended design/0059 §8 OQ2, and corrected this record to match the tree (6-case count, superseded Decision 1, Files Modified, Tests Run). Lesson recorded: verify a claimed edit landed in the tree before claiming it — grep, don't trust the tool's success reply alone.

## Review Rounds 4–5 (live-PG verification via the Secrets Integration suite)

The CI "Secrets Integration" workflow runs the PG-gated suite against a live PostgreSQL — the environment my sandbox lacks. It executed both FK-face tests and caught two seed defects my compile-verify could not: r4 — the nonexistent-target test asserted `ErrorIs(ErrNotFound)` where the ruling pins the opposite (inverted to `NotErrorIs`); r5 — both seeds omitted `AutoDisableAfter`, violating the triggers CHECK constraint (added, matching the suite's established seed shape). Lesson: a PG-gated test is unverifiable by compile alone; where a live DB exists in CI, treat its first run as the real RED/GREEN.

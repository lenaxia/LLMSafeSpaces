# Worklog: Fix #1473 — close the routine-drain lifecycle gaps (targetless accounting, transient-fetch split, fireWorkflowTarget dedup)

**Date:** 2026-09-19
**Session:** #1473 lane on branch `fix/drain-accounting-fetch-split` (worktree wt-1453; follows #1453/PR #1462, #1467/PR #1468, #1454/PR #1472 — all merged). Issue filed by this session per orchestrator routing.
**Status:** Complete

---

## Objective

Land the three follow-ups #1454 deliberately preserved and pinned: (1) account + auto-disable drain-targetless fires with the unified `trigger_has_no_target` payload (the drain twin of #1440's cron guard); (2) split the drain's trigger-fetch error handling (transient → leave pending for re-drive; deleted → best-effort loud failure); (3) mechanical: `fireWorkflowTarget`'s two remaining inline accounting blocks → `accountTriggerFailure`.

---

## Work Completed

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| 1 | Gap 2's not-found arm can mirror fireWorkflowTarget INCLUDING accounting | **DISPROVED — recorded as a finding on #1473.** `trigger_fires.trigger_id` is `ON DELETE CASCADE` (migration 000016:349): a deleted trigger's fires vanish with it, so `ErrNotFound` on a pending fire is only the mid-tick ListPendingRoutineFires→fetch race; and `IncrementTriggerFailures` on a missing row returns no row (store.go:871-878) — no counter exists to hold. Corrected design: not-found arm fails the fire best-effort, NO accounting |
| 2 | The transient arm is the production-relevant fix | Webhook receiver 202s before the tick drains; a pool blip previously permanently failed an acknowledged fire — the exact class #1412 fixed for fireWorkflowTarget (engine.go:606-614) |
| 3 | The real store maps misses to `wf.ErrNotFound` | store.go:396-398; the engine MOCK did not — parity fixed (miss → `wf.ErrNotFound`, plus `getTriggerByIDErr` transient-injection field) |
| 4 | Unifying the drain payload to the cron guard's is safe | Both routes' consumers are the fire-audit surface (`GET /triggers/:id/fires`) + the nightly e2e (R4/R4d pin `trigger_has_no_target` on the CRON route only — drain pins were none); grep confirms no reference to the old `has no workspace_id` string anywhere |

### TDD — pins flipped RED first

`engine_fire_lifecycle_test.go` rewritten from #1454's characterization pins to the fixed expectations; RED confirmed pre-implementation (8 failing assertion groups across 6 subtests):

- `TestProcessPendingRoutineFire_TargetlessDrain_AccountsAndDisables` — nil/empty workspace + at-threshold variants: failed, byte-exact `trigger_has_no_target` payload, updated-not-minted, exactly one increment, threshold → disabled.
- `TestProcessPendingRoutineFire_TransientFetchError_LeavesPending` — direct + tick-drain drivers: NOTHING written (no status, no result), zero accounting, never disabled.
- `TestProcessPendingRoutineFire_TriggerDeleted_FailsLoudUnaccounted` — `ErrNotFound`: failed + byte-exact `{"error":"trigger not found"}` + zero accounting (guard for the split's not-found arm; coincides with pre-fix behavior for that arm).
- `TestScheduler_PendingDrainTick_TargetlessTrigger_Accounts` — tick-level wiring: pending webhook fire + workspaceless trigger drains through `tick` → failed + accounted + disabled at threshold.
- Retained from #1454: `TestExecuteRoutine_AccountingCallCounts` (5 subtests) and `TestFireRoutineTarget_Targetless_ExactPayloadAndCounts`.

### Implementation (engine.go)

- `processPendingRoutineFire`: transient (`!goerrors.Is(err, wf.ErrNotFound)`) → log + return (fire stays pending, next tick re-drives); not-found → log + best-effort failed write with cause payload, no accounting; targetless → unified payload + `accountTriggerFailure` (persistence stays update-vs-create per route).
- `fireWorkflowTarget`: both inline accounting blocks → `accountTriggerFailure` (gap 3, mechanical).
- No other behavior touched; #1470/#1471's zone (agent-call block, their helper anchor) untouched.

---

## Key Decisions

1. **Not-found arm takes no accounting** — schema-excluded (assumption 1 finding); documented in code and on #1473.
2. **Payload unification** — one cause name (`trigger_has_no_target` + hint) across both doors; the drain's old `trigger has no workspace_id` string is dead.
3. **Transient semantics mirror #1412 exactly** — platform's problem ≠ fire's problem; pending re-drive, never counted.

---

## Blockers

None.

---

## Tests Run

- RED-first: 6 subtests failing pre-implementation (targetless ×3, transient ×2, tick-level ×1).
- Mutation: revert only `engine.go` → 8 failing assertion groups; restored.
- `go test -timeout 600s -count=1 ./api/internal/workflows/` — ok (42.8s full package).
- `go test -race -count=1` (same package) — ok (44.0s); `go vet` + scoped build ok (disk-constrained).
- `gofmt`/`goimports` clean; `golangci-lint --new-from-rev=9c6624ed` — 0 issues.

---

## Next Steps

- Review loop to APPROVED; orchestrator merges (may carry "Fixes #1473" — all three gaps land here).

---

## Files Modified

- `api/internal/workflows/engine.go` — processPendingRoutineFire fetch-split + targetless accounting/payload; fireWorkflowTarget ×2 → accountTriggerFailure
- `api/internal/workflows/engine_fire_lifecycle_test.go` — pins flipped to fixed expectations + tick-level test
- `api/internal/workflows/engine_test.go` — mock parity: `wf.ErrNotFound` on miss, `getTriggerByIDErr` injection
- `worklogs/NNNN_2026-09-19_drain-accounting-fetch-split.md` — this worklog

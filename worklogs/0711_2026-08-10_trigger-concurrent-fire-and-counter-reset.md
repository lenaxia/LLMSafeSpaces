# Worklog: Trigger Concurrent-Fire Race and Counter-Reset Fix

**Date:** 2026-08-10
**Session:** Fix two cron-trigger scheduler bugs that produced the 15/10 circuit-breaker overshoot on production trigger Test1we.
**Status:** Complete

---

## Objective

Production cron-routine trigger `Test1we` (`97feb914-…`) auto-disabled at 15 failures despite `auto_disable_after=10`. After #696 fixed the underlying workspace-activation failure, the trigger's failure counter was stuck at 15 and would immediately re-disable on any future failed fire. Investigation surfaced two independent scheduler bugs: (1) a concurrent-fire race that inflated the failure counter past the threshold, and (2) a missing counter reset on re-enable that made recovery impossible.

---

## Root Cause

### Bug 1 — Concurrent-fire race (the 15/10 overshoot)

`ListDueCronTriggers` (`pkg/workflows/store.go`) used a plain `SELECT` with no row locking. The API server is horizontally scalable (README §architecture — "Stateless API server, horizontally scalable, no sticky sessions required"), so every replica's 30s scheduler tick selected the same due trigger and fired it concurrently. Each failed fire incremented `consecutive_failures`; the first replica to reach `n >= auto_disable_after` disabled the trigger, but in-flight fires on sibling replicas pushed the count past the threshold before they observed the disable — landing at 15 for a threshold of 10.

The existing `ClaimQueuedRuns` (workflow reconciler, `store.go:608`) already used `FOR UPDATE SKIP LOCKED` for exactly this multi-replica safety. The cron scheduler path predated that pattern.

### Bug 2 — Counter not reset on re-enable

`UpdateTrigger` (`pkg/workflows/store.go`) updated `enabled` via `COALESCE($6, enabled)` but did not touch `consecutive_failures`. A trigger disabled at 15 failures, when re-enabled via the UI, retained `consecutive_failures=15`. The first failed fire after re-enable hit `IncrementTriggerFailures` → 16 → `16 >= 10` → instant re-disable. Recovery was impossible without a manual SQL `UPDATE`.

---

## Work Completed

### Fix 1 — `ClaimDueCronTriggers` replaces `ListDueCronTriggers`

`pkg/workflows/store.go` — new `ClaimDueCronTriggers(ctx, now, limit, nextFireFn)`:
1. Opens a transaction
2. `SELECT ... FOR UPDATE SKIP LOCKED` on due+enabled cron triggers (sibling replicas skip locked rows)
3. For each claimed trigger, calls `nextFireFn(t)` to compute the next cron slot, then `UPDATE last_fired_at + next_fire_at` within the same tx (trigger is no longer "due" for any replica)
4. Commits

The `nextFireFn func(*TriggerRow) time.Time` callback lets the engine compute the next fire time via `computeNextFire` (which parses the cron expression) without coupling the data-access layer to `robfig/cron` or `pkg/types.CronSourceConfig`. This mirrors the `filepath.Walk(root, walkFn)` convention and inverts the dependency per SOLID's dependency inversion principle.

`api/internal/workflows/engine.go` — `tick` now calls `ClaimDueCronTriggers` instead of `ListDueCronTriggers`. `fireTrigger` no longer calls `UpdateTriggerFireTimestamps` (the claim advances timestamps atomically). The `SchedulerStore` interface, mock (`mockSchedulerStore`), and dead store method `UpdateTriggerFireTimestamps` were all updated/removed.

### Fix 2 — `UpdateTrigger` CASE clause resets counter on re-enable

`pkg/workflows/store.go` — added a `CASE` clause to `UpdateTrigger`'s `SET`:
```sql
consecutive_failures = CASE
    WHEN COALESCE($6, enabled) = true AND enabled = false THEN 0
    ELSE consecutive_failures
END
```
This resets to 0 only on the `enabled = false → true` transition. SET expressions reference OLD column values, so `enabled` on the right side is the pre-update value. All five transition cases verified correct (see Tests).

### Tests added

`pkg/workflows/store_integration_test.go` (against real PostgreSQL):
- `TestClaimDueCronTriggers` — claim returns only due+enabled, advances timestamps atomically, second claim returns empty
- `TestClaimDueCronTriggers_SkipLocked` — concurrent transaction cannot claim a row locked by another (regression for Bug 1)
- `TestClaimDueCronTriggers_ConcurrentMethodCalls` — two goroutines calling `ClaimDueCronTriggers` concurrently get disjoint results (direct guard on the implementation, not raw SQL)
- `TestUpdateTrigger_ReEnableResetsFailures` — re-enable resets counter to 0 (regression for Bug 2)
- `TestUpdateTrigger_EnableStaysTrue_DoesNotReset` — updates to already-enabled triggers preserve the counter
- `TestUpdateTrigger_DisableViaUpdate_PreservesFailures` — explicit disable via `Enabled=&false` preserves the counter

`api/internal/workflows/engine_test.go` — `mockSchedulerStore` updated to implement `ClaimDueCronTriggers` (records `nextFires` via callback). All existing scheduler tests pass unchanged.

---

## Key Decisions

1. **Transaction-spanning claim over a unique constraint.** A unique constraint on `(trigger_id, fire_slot)` would also prevent duplicate fires but requires a schema migration. The `FOR UPDATE SKIP LOCKED` + atomic timestamp advance needs no migration and reuses the established `ClaimQueuedRuns` pattern. The trade-off: a process crash between claim and fire loses one slot (the next cron tick); acceptable for a scheduler.
2. **Callback for cron parsing, not store-layer import.** Moving cron parsing into `pkg/workflows/store.go` would couple the data-access layer to `robfig/cron` + `pkg/types.CronSourceConfig`. The `nextFireFn` callback inverts the dependency — idiomatic Go, no new imports in the store.
3. **CASE clause over a separate reset call.** A separate `ResetTriggerFailures` call (already exists in the store) would require the handler to call it conditionally before `UpdateTrigger` — two round trips and a race window. The CASE clause is atomic and requires zero handler changes.
4. **`enabled` CASE references OLD value.** PostgreSQL `SET` expressions see the pre-update column values. Verified against all five transition cases in integration tests.

---

## Assumptions (validated)

1. **The API runs multiple replicas.** Validated: README §architecture states "Stateless API server — horizontally scalable, no sticky sessions required." The `ClaimQueuedRuns` pattern exists for exactly this reason.
2. **`FOR UPDATE SKIP LOCKED` is the correct multi-replica claim primitive.** Validated: `ClaimQueuedRuns` (`store.go:608`) uses it and its doc comment explains the rationale.
3. **`computeNextFire` is safe to call inside an open transaction.** Validated: pure function, cron parsing only, microsecond-level, no I/O.
4. **`next_fire_at` advance makes the trigger invisible to other replicas.** Validated: `ListDueCronTriggers`/`ClaimDueCronTriggers` WHERE clause is `next_fire_at <= $1`; advancing it past `now` excludes it.

---

## Blockers

None.

---

## Tests Run

- `go vet ./api/internal/workflows/... ./pkg/workflows/...` — clean
- `go build ./api/internal/workflows/... ./pkg/workflows/...` — clean
- `go test -timeout 120s ./api/internal/workflows/...` — PASS
- `go test -timeout 120s ./pkg/workflows/...` — PASS (incl. integration tests against real PostgreSQL 16)
- `go test -timeout 120s ./api/internal/handlers/...` — PASS
- Regression tests (`TestClaimDueCronTriggers_SkipLocked`, `TestUpdateTrigger_ReEnableResetsFailures`) verified to fail against pre-fix code
- CI: Lint, Gitleaks, Trivy, govulncheck, Build Frontend, review — ALL PASS

---

## Next Steps

1. Merge + deploy; verify `Test1we` recovers (re-enable resets counter; next cron tick fires successfully).
2. Consider adding `consecutive_failures` to the `TriggerResponse` DTO so the frontend can show the current count proactively (currently only visible after auto-disable).

---

## Files Modified

- `pkg/workflows/store.go` — `ClaimDueCronTriggers` (replaces `ListDueCronTriggers`); `UpdateTrigger` CASE clause; removed `UpdateTriggerFireTimestamps`
- `api/internal/workflows/engine.go` — `tick` uses `ClaimDueCronTriggers`; `fireTrigger` no longer advances timestamps; interface updated
- `api/internal/workflows/engine_test.go` — `mockSchedulerStore` updated to new interface
- `pkg/workflows/store_integration_test.go` — 6 new integration tests

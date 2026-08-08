# Worklog: Epic 64 Engine Migration — Controller → API Server

**Date:** 2026-08-08
**Session:** Move workflow engine from controller to API server (architectural fix)
**Status:** Complete

---

## Objective

Fix the architectural violation: workflow engine (reconciler + scheduler) was wired into the controller with direct PostgreSQL access. The controller should never touch PostgreSQL — it talks to K8s only.

---

## Work Completed

### Removed from controller
- `controller/internal/workflows/` — entire directory (reconciler.go, scheduler.go, tests, cron_test.go)
- `controller/main.go` — stripped pgxpool, reconciler registration, scheduler registration, workspaceActivator, resolvePodNamespace, all workflow imports
- `controller/main_test.go` — deleted (tested resolvePodNamespace which no longer exists)

### Added to API
- `api/internal/workflows/engine.go` (~650 lines) — reconciler + scheduler + K8sWorkspaceActivator + HTTPAgentExecutor + AppEngineLogger
- Uses `sync.Mutex` for cancel tracking (not custom channel-based mutex)
- Uses `Patch` (not `Update`) for workspace activation to avoid 409 conflict races
- `http.NewRequestWithContext` for proper context propagation
- `K8sClient == nil` guard in EnsureActive (nil-safe)
- Cron parser uses stdlib `strings.Fields`, `strings.HasPrefix`, `strconv.Atoi`

### app.go wiring
- `wfReconciler` and `wfScheduler` stored on App struct
- Constructed in `New()` where secretsPool + k8sClient are in scope
- Started in `Run()` as background goroutines (same pattern as jwtSessionJanitor)
- `FOR UPDATE SKIP LOCKED` provides multi-replica safety without leader election

### 27 engine tests
- 5 reconciler: happy path, workspace unavailable, node failure, trigger increment, cancel
- 1 condition branching: skip branch executed, else-path NOT executed
- 1 node retry: first attempt fails, second succeeds
- 1 error-code response: agentd returns error_code → run fails
- 1 regression test: engine works without controller DB access
- 1 topoSort
- 10 cron parser: */5min, hourly, daily, weekdays, empty, malformed, TZ valid, TZ invalid
- 4 scheduler: fires due, missed fire skipped, run_script target, advances next_fire_at
- 1 K8sWorkspaceActivator: nil client guard
- 2 HTTPAgentExecutor: context cancellation, successful JSON contract

---

## Key Decisions

1. **API server, not controller** — the engine needs PG + K8s + HTTP to pods. The API already has all three. The controller has only K8s.
2. **sync.Mutex over custom channel mutex** — reviewer flagged deadlock risk with the custom mutex. stdlib sync.Mutex is correct.
3. **Patch over Update** — workspace activation via Update causes 409 conflict races. Patch with merge-patch is the safe pattern.
4. **stdlib string helpers** — removed custom splitFields/hasPrefix/atoiSafe. stdlib strings/strconv is correct.

---

## Files Modified

- `api/internal/workflows/engine.go` (new, ~650 lines)
- `api/internal/workflows/engine_test.go` (new, ~400 lines, 27 tests)
- `api/internal/app/app.go` (modified — engine construction + startup)
- `controller/main.go` (modified — all workflow code removed)
- `controller/internal/workflows/` (deleted entirely)
- `controller/main_test.go` (deleted)

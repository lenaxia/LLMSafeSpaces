# Worklog: #1452 — preserved routine sessions indexed at fire completion

**Date:** 2026-09-18
**Session:** Closed the platform-side half of the routine-session invisibility gap localized by the live-pod experiment: preserved routine sessions (row + transcript pod-side, listed by opencode's `GET /session`) never appeared in the sidebar because no routine-fire path wrote a `session_index` row.
**Status:** Complete

---

## Objective

A routine session with `preserveSession=always` (and a PreserveOnFailure session whose delete failed) must be visible on `GET /workspaces/:id/sessions` deterministically — not only when an interactive session happened to hold the usage-stream gate open within 30s of the fire.

---

## Work Completed

- **Seam:** new caller-shaped `SessionIndexWriter` interface in `api/internal/workflows/engine.go` (`RecordMessage` + `UpsertTitle` — the only two operations the scheduler needs; interface segregation, satisfied implicitly by `*sessionindex.Service`). Added `Scheduler.SessionIndex` field alongside `PasswordProvider`.
- **Write site:** `executeRoutine` now calls `indexRoutineSession` inside the existing `if sessionID != "" && !sessionDeleted` block, i.e. exactly where and when `RecordSessionOrigin` runs (engine.go ~:845). The helper nil-guards the seam (loud error, mirroring the PasswordProvider-nil precedent in `deleteRoutineSessionAuthorized`), does a synchronous best-effort `UpsertTitle` (trigger name — failure logged, fire still delivered), and stamps `last_message_at` via the service's non-blocking `RecordMessage` (empty title param — verified the drainer drops it, `service.go` drain() passes only ws/session/at).
- **Wiring:** `app.go` Scheduler construction passes `SessionIndex: sessionIndexSvc` (already in scope, constructed at app.go:265).
- **Pre-existing bug fixed en route:** `sessionindex.Service.Start/Stop` dereferenced `s.logger` unconditionally while `RecordMessage`/`drain` nil-guard it — `sessionindex.New(db, nil)` + `Start()` segfaulted. Both lifecycle methods now guard like the rest of the file.

## Key Decisions

- **Index at fire time (Option A from the #1452 analysis), not read-time merge (Option B).** Follows the codebase's own index-at-write precedent (the 2026-08-29 "agent listed 3, API served 2" fix in `proxy_handlers.go`): the platform owns session CRUD and writes the index where the write happens. B would add a pod round-trip per sidebar render and reintroduce the exact race this fix removes.
- **Same condition as `RecordSessionOrigin`** (`sessionID != "" && !sessionDeleted`): PreserveNever sessions don't exist (agentd deletes the ephemeral and returns `""`), PreserveOnFailure-success sessions are deleted — neither is indexed, matching what `RecordSessionOrigin` already does. Deleted sessions get index rows deleted nowhere because they are never created.
- **Upsert idempotency over a re-drive guard:** `processPendingRoutineFire` re-drives pending fires through `executeRoutine`; both `UpsertSessionTitle` and `UpsertSessionMessage` are `ON CONFLICT DO UPDATE` (database.go:1129-1148), so a re-drive refreshes the row (title same, last_message_at bumped, message_count +1 — the re-drive genuinely executes another turn in the same session, so the count bump is truthful). No new dedupe machinery for a self-converging write.
- **Failed fires are not indexed** (the write sits in the delivered-path block, as the origin write always did): a failed fire has no completed preserved session by contract; PreserveAlways retries create fresh sessions per fire.
- **Nil seam logs an error rather than silently skipping:** mirrors `deleteRoutineSessionAuthorized`; a silently-absent index would silently reintroduce this bug for any future Scheduler construction site.

## Blockers

None.

## Tests Run

- TDD red 1 (compile): `go test ./api/internal/workflows/ -run '...SessionIndex...'` — undefined `SessionIndexWriter`/`SessionIndex` field.
- TDD red 2 (behavioral): seam added, logic absent → `TestExecuteRoutine_PreserveAlways_IndexesSession`, `TestExecuteRoutine_IndexTitleError_NonFatal`, `TestExecuteRoutine_Redrive_RefreshesIndex`, `TestExecuteRoutine_PreserveAlways_RealSessionIndexServiceWiring` FAIL (0 index writes observed); negative tests pass trivially.
- Green: all 8 new tests in `engine_session_index_test.go` PASS — happy path (title = trigger name, last_message_at stamped in a bounded window, origin unchanged), PreserveOnFailure-delete-success, PreserveNever, agent-failure, title-error non-fatal (ordering write still attempted), nil-seam no-panic, re-drive refresh, and the real-wiring test (Scheduler → real `sessionindex.Service` → `MockDatabaseService`, including the async drainer reaching the DB via `assert.Eventually`).
- Full suites: `go test ./api/internal/workflows/ ./api/internal/services/sessionindex/ ./api/internal/app/` — all ok (existing ~30 Scheduler constructions unaffected by the nil-guard).
- `go build ./api/...`, `go vet` on the three packages, `gofmt -l` clean, `golangci-lint v2 run` on the three packages — 0 issues.

## Next Steps

PR → review → merge. Post-deploy verification: fire a `preserveSession=always` routine trigger with zero interactive traffic for >30s and confirm the session appears in the sidebar (the previously-racy case). The product call flagged in the analysis — whether PreserveNever/Ephemeral routine turns should ALSO be surfaced (they are currently invisible by design, same as before this change) — remains open for @lenaxia.

---

## Files Modified

- `api/internal/workflows/engine.go` — `SessionIndexWriter` interface, `Scheduler.SessionIndex` field, `indexRoutineSession` helper, write call in `executeRoutine`
- `api/internal/workflows/engine_session_index_test.go` — new test file (8 tests)
- `api/internal/app/app.go` — Scheduler wiring (`SessionIndex: sessionIndexSvc`)
- `api/internal/services/sessionindex/service.go` — nil-logger guards in `Start`/`Stop`
- `CHANGELOG.md` — Unreleased entry

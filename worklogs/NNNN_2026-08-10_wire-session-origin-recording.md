# Worklog: Wire Session Origin Recording for Routine Triggers

**Date:** 2026-08-10
**Session:** Connect the session_origins feature — built during Epic 64 but never wired into the engine.
**Status:** Complete

---

## Objective

Production trigger `Test1we` (97feb914-…) fired successfully, but the session was invisible in the sidebar and the chat URL returned 404. Root cause: the `session_origins` table, `RecordSessionOrigin` store method, and sidebar badge UI were all built during Epic 64 but never connected. The engine's `executeRoutine` never called `RecordSessionOrigin`, so routine-created sessions appeared as plain manual sessions with no linkage to their trigger.

---

## Root Cause

The session origin tracking feature has four components:

| Component | Status before this PR |
|---|---|
| `session_origins` table (migration 022) | ✅ Deployed, 0 rows in prod |
| `RecordSessionOrigin` store method (`store.go:1146`) | ✅ Implemented |
| `ListSessionOrigins` API endpoint + handler | ✅ Working |
| Sidebar badge UI (`Sidebar.tsx:889`) | ✅ Renders purple "routine" pill |
| **Engine calling `RecordSessionOrigin`** | ❌ **Never called** |

The engine extracted `session_id` from the agent output only inside the `PreserveOnFailure` branch — and only to delete the session on success. For `PreserveAlways`, the session_id was available in `agentResp.Output` but never used. For `PreserveNever`, agentd returned `session_id=""` after deleting the ephemeral session.

---

## Work Completed

### Engine changes (`api/internal/workflows/engine.go`)

1. **Added `RecordSessionOrigin` to the `SchedulerStore` interface** — the store method existed but the engine's interface didn't declare it.

2. **Restructured the `executeRoutine` success path** (lines 639-683):
   - Extract `session_id` from `agentResp.Output` unconditionally (not just inside `PreserveOnFailure`)
   - `PreserveOnFailure` success: attempt DELETE, set `sessionDeleted = true` only if the HTTP call succeeds AND returns status < 400
   - Log DELETE errors instead of swallowing them
   - Call `RecordSessionOrigin` when `sessionID != "" && !sessionDeleted`
   - Log `RecordSessionOrigin` errors instead of swallowing them

3. **Robustness fix:** the previous code set `sessionDeleted = true` unconditionally, even if the DELETE HTTP call failed. This meant a failed DELETE would suppress the origin recording, leaving the session invisible. Now `sessionDeleted` only flips when the DELETE actually succeeds.

### Test changes

**Unit tests (`api/internal/workflows/engine_test.go`):**
- Added `sessionOrigins` field + `RecordSessionOrigin` method to `mockSchedulerStore`
- Updated `TestExecuteRoutine_SuccessDelivered` to include `session_id` in mock output
- `TestExecuteRoutine_PreserveAlways_RecordsSessionOrigin` — verifies origin recorded with correct trigger/fire/title linkage
- `TestExecuteRoutine_PreserveNever_DoesNotRecordOrigin` — verifies no origin for empty session_id
- `TestExecuteRoutine_PreserveOnFailure_DeleteFails_RecordsOrigin` — documents that when DELETE fails, the session still exists, so origin IS recorded

**Integration tests (`pkg/workflows/store_integration_test.go`):**
- Added `session_origins` to `SetupTest` TRUNCATE (was missing)
- `TestRecordSessionOrigin_Insert` — basic insert with trigger + fire linkage
- `TestRecordSessionOrigin_Upsert` — ON CONFLICT DO UPDATE (same session_id, different trigger/title)
- `TestRecordSessionOrigin_WorkspaceFK` — FK constraint rejects non-existent workspace_id
- `TestRecordSessionOrigin_TriggerFK` — trigger deletion SET NULLs trigger_id (not cascade)

---

## Key Decisions

1. **`sessionDeleted` only on successful DELETE.** The reviewer correctly flagged that setting `sessionDeleted = true` unconditionally was a bug — if the DELETE HTTP call failed, the session still existed but the origin wouldn't be recorded, making it invisible. Now `sessionDeleted` flips only when DELETE returns < 400.

2. **Log errors instead of swallowing.** Both the DELETE failure and the `RecordSessionOrigin` failure now log via `logger.Error()`. These are non-fatal (the fire still succeeds), but the user needs visibility when session linkage breaks.

3. **No schema change needed.** The `session_origins` table already has `fire_id` and `trigger_id` columns (migration 022). The earlier analysis recommended adding `session_id` to `trigger_fires`, but that was wrong — the existing `session_origins` table is the designed linkage point.

4. **`on_failure` failure path is a known gap.** When the agent fails, agentd's `writeWorkflowError` doesn't include `session_id` in the response. So the engine can't record the origin even though the session exists (preserved by `on_failure` policy). This requires an agentd-side fix to include `session_id` in error responses — out of scope for this PR.

---

## Assumptions (validated)

1. **agentd always includes `session_id` in success responses.** Validated: `workflow_execute.go:374` puts `"session_id": sessionID` in the result map. For `PreserveNever`, it's set to `""` after deletion (line 391).
2. **`session_id` survives the wire to `agentResp.Output`.** Validated: `HTTPAgentExecutor.Execute` (`engine.go:862`) deserializes the full agentd response body to `NodeExecResponse.Output` (a `json.RawMessage`). The `session_id` field is in that JSON.
3. **`session_origins` trigger_id uses ON DELETE SET NULL.** Validated: migration 022 (`REFERENCES triggers(id) ON DELETE SET NULL`). Integration test confirms.

---

## Blockers

None.

---

## Tests Run

- `go vet ./api/internal/workflows/... ./pkg/workflows/...` — clean
- `go build ./api/internal/workflows/... ./pkg/workflows/...` — clean
- `go test ./api/internal/workflows/...` — PASS (10 tests)
- `go test ./pkg/workflows/...` — PASS (incl. 4 new integration tests against real PostgreSQL 16)

---

## Files Modified

- `api/internal/workflows/engine.go` — `RecordSessionOrigin` in interface; success path restructured
- `api/internal/workflows/engine_test.go` — mock updated; 3 new unit tests
- `pkg/workflows/store_integration_test.go` — `session_origins` in TRUNCATE; 4 new integration tests

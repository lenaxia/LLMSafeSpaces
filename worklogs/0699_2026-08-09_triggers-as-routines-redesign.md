# Worklog: Triggers as Routines Redesign (v0.10.0)

**Date:** 2026-08-09
**Session:** Redesign triggers to embody agent routines directly. Breaking API change.
**Status:** Complete

---

## Objective

The trigger model shipped in v0.9.x was limited to firing DAG workflows or scripts. The user's actual automation needs (5 real flows) revealed that 4 of 5 are single agent turns on a schedule — not DAGs. The redesign makes triggers directly embody agent routines (scheduled agent turns with memory, capture, and session lifecycle).

---

## Work Completed

### Schema migrations (000020 + 000021)

- Dropped `target_type` + `target_config` from triggers. Added explicit routine columns: `workspace_id`, `workflow_id`, `prompt`, `agent`, `script_path/args/env`, `memory_mode`, `memory_max_runs`, `capture_mode`, `preserve_session`.
- Backfilled existing data from target_type/target_config with idempotent DO blocks.
- Added `trigger_fires.result` + `result_captured_at` for memory + observability.
- Relaxed action_type CHECK constraint for 'routine'.
- session_origins table deferred (no implementation yet — removed from migration).

### Type system redesign

- Dropped `TriggerTargetRunWorkflow`/`TriggerTargetRunScript` constants + `ValidTriggerTargetType`.
- Added `MemoryNone`/`MemoryLastResult`, `CaptureErrorsOnly`/`CaptureFull`, `PreserveNever`/`PreserveAlways`/`PreserveOnFailure` enums + validators.
- Updated all transfer objects (`TriggerResponse`, `CreateTriggerRequest`, `UpdateTriggerRequest`).

### Engine: routine executor

- `fireRoutineTarget` creates a fire row, then calls `executeRoutine`.
- `executeRoutine`: activates workspace → renders prompt with `{{.prevResult}}` memory → executes optional script → sends prompt to opencode agent → captures result per capture policy → deletes/preserves session per preserve policy → records fire result.
- `processPendingRoutineFire`: picks up webhook-triggered routine fires on the next scheduler tick via `ListPendingRoutineFires`.
- `buildRoutineAgentSpec`: respects `PreserveSession` (never→ephemeral, always/on_failure→new).
- `buildRoutineScriptSpec`: generates Python subprocess handler from trigger's script config.
- Deleted `BuildRunScriptSpec`, `GetOrCreateScriptWorkflow`, `goQuoteSlice`, old `fireScriptTarget`.

### Reviewer fixes (3 iterations)

1. Fixed `ListDueCronTriggers` — was selecting dropped columns.
2. Fixed webhook routine fires — were silently dropped. Added `ListPendingRoutineFires` + `processPendingRoutineFire`.
3. Fixed `PreserveSession` — was always ephemeral. Now respects trigger config.
4. Fixed success status — now transitions to "delivered" (was stuck at "fired").
5. Fixed `GetLastRoutineResult` — was querying status='fired' instead of 'delivered'.
6. Fixed script-failure auto-disable bypass — `return` was skipping `IncrementTriggerFailures`.
7. Fixed `PreserveOnFailure` — engine now sends session delete on success when on_failure.
8. Added memory+capture conflict validation (`last_result` requires `full` capture).
9. Added mutual exclusion validation (`workflowId` and `workspaceId` cannot both be set).
10. Implemented `MemoryMaxRuns` — engine now fetches N previous results via `GetRecentRoutineResults`.
11. Added 9 unit tests covering: success/failure status, script-failure auto-disable, activation failure, capture modes, preserve_session spec generation.

### Frontend

- Trigger create form rebuilt: routine/workflow mode toggle, workspace picker, prompt editor, memory/capture/preserve controls, optional script fields.
- API client updated for new field names.

### SDKs (all 4)

- Go, TypeScript, Python, Java SDK trigger create/update methods updated for new fields.

### MCP tools

- `trigger_create` tool signature updated. APIClient interface + HTTPClient + mock updated.

---

## Key Decisions

1. **Triggers absorb routines.** No separate entity. The presence/absence of `workflow_id` determines mode.
2. **Scheduler executes routines directly.** Holds `WorkspaceActivator` + `AgentdExecutor`. Routine execution is synchronous within the tick goroutine.
3. **Memory via `last_result`.** The scheduler queries the last successful fire's result and injects it into the prompt template. No session continuity needed for the common case.
4. **Session lifecycle by policy.** `never` = ephemeral (delete after capture). `always` = persist (shows in sidebar). `on_failure` = persist on error, delete on success.

---

## Tests Run

- `go test -race` engine (25+ tests including 9 new routine executor tests) — PASS
- `go test -race` types — PASS
- `go test -race` store — PASS
- `go test -race` handlers (trigger/webhook/workflow) — PASS
- `go test -race` MCP — PASS
- `npx tsc --noEmit` frontend — PASS
- `npx vitest run` frontend (18 tests) — PASS
- CI: migration idempotency, round-trip, FK cascade, schema invariants, lint, full test suite — ALL PASS

---

## Next Steps

1. E2E integration test (cron trigger → routine execution → result capture)
2. Session origin tracking (session_origins table + sidebar enrichment) — deferred until implementation
3. agentd built-in MCP server for in-workspace agent tools
4. robfig/cron parser for full cron expression support

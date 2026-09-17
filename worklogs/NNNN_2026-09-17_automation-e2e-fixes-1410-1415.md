# Worklog: automation e2e fixes — trigger scheduling, DAG linkage, and validation (#1410–#1415)

**Date:** 2026-09-17
**Session:** A live end-to-end exercise of the trigger/automation surface (create/update/delete/fires, cron + webhook sources, workflow integration via the agentd MCP tools) surfaced six defects, opened as issues #1410–#1415. This session fixes all six in one pass, then iterates on the PR #1420 AI review round (case-variant stamp bypass, no-op-enable race, partial-issue items, missing integration/e2e coverage, worklog).
**Status:** Complete

---

## Objective

Make the automation surface behave as its contracts promise: schedules that take effect when written (not at stale slots), DAG triggers creatable from the pod surface and loud when their workflow disappears, run inputs that obey declared schemas, script failures that name their cause, and tool descriptions that stop inventing vocabulary.

## Work Completed
- Shared cron logic: new `pkg/workflows/schedule.go` — `ValidateCronSourceConfig` / `NextCronFire` / `NextCronFireFromConfig`, single source of truth for the write path (handlers) and read path (engine).
- #1411: create validates expr (five-field) + IANA tz (400 on bad) and initializes `next_fire_at` to the first real occurrence instead of `now` (no more immediate fire on create; no more hourly retry loops for garbage exprs). Engine keeps legacy fallbacks (UTC for unloadable tz, +1h for unparseable expr) for pre-validation rows.
- #1410: `TriggerUpdate.NextFireAt` + `next_fire_at` SET clause in `UpdateTrigger`; the update handler recomputes the slot when `sourceConfig` changes and refreshes a stale slot on disabled→enabled re-enable — guarded to that transition (review r2: a no-op `enabled:true` on an already-enabled trigger must never push an imminent-but-unclaimed fire).
- #1412: `scopeTriggerCreateBody` replaces `forceTriggerWorkspace` — normalizes the `workflow_id` alias (contradictory duplicates 400), strips every case-variant workspace key (`strings.EqualFold`; review r2 validated the exact-spelling strip was bypassable via `workspaceid` since the decoder binds case-insensitively and map keys marshal sorted), and passes workflow-targeted bodies without the routine stamp after the pod handler verifies the workflow exists, belongs to the resolved owner, and targets THIS workspace (403/404). `fireWorkflowTarget` records a failed fire + failure increment + auto-disable when the workflow is gone (was a silent tick).
- #1413: `pkg/workflows/input_schema.go` (existing direct dep `santhosh-tekuri/jsonschema/v6`) — manual runs validate input against `inputSchema` before queueing; schemas compile-checked at workflow create/update. Trigger-fired runs deliberately bypass (system envelope can never satisfy user schemas) — documented at the site, design options tracked in #1425.
- #1414: script language validated at spec time (`python|node`); `execScriptNode` keeps `err.Error()` for pre-execution failures (`exit -1: ` with empty stderr was swallowing "unsupported language: bash").
- #1415: `trigger_create` / `workflow_create` / `workflow_update` MCP descriptions match the validated contracts (workflowId camelCase, four-node vocabulary, script `handler(input) -> dict` contract, targetWorkspaceId requirement, no-immediate-fire semantics, UpdateWorkflowRequest patch shape).
- E2e: `local/issue-1410-1412-automation-e2e.sh` (R1–R5, API-only rows — no LLM, no workspace pods; ghost-workflow targets so no DAG executes) + structural pins in `local/issue_1410_automation_e2e_script_test.go` + nightly registration (e2e-nightly.yml, port 18086).
- Integration: real-PG `TestUpdateTrigger_NextFireAt_PersistsAndNilPreserves` (writes through, nil preserves, RETURNING reflects).

## Key Decisions
- Validation at the write boundary, defensive fallbacks at the read boundary: legacy rows with bad exprs/tzs must not pause or spin — they keep the historical engine behavior while new writes are clean.
- The alias normalization (`workflow_id`→`workflowId`) lives in the pod seam, not the DTO: the platform's wire contract is camelCase; forgiving the snake_case spelling at the seam is where the silent-drop class was born, so that's where it dies.
- Re-enable slot refresh is transition-guarded (`!existing.Enabled`) rather than state-blind: an update that doesn't change the trigger's life state must never interact with the scheduler's claim window.
- E2e rows are deliberately LLM-free: the defects under test are all control-plane (create/patch/fire-audit/run-gating), and ghost-workflow targets keep the scheduler paths exercised without executing any DAG.

## Blockers
None. Follow-up design decision tracked in #1425 (trigger-carried static input vs create-time wiring checks for schema-bearing DAGs).

## Tests Run
- Unit: schedule validation matrix (8 cases), next-fire timezone math + strictly-after boundary, input-schema table (7 cases), scoping-body tests incl. case-variant bypass + contradictory aliases + non-string workflowId, schedule-change recompute, no-op-enable slot stability, stale re-enable, workflow-target scoping (201/403/404), engine failed-fire + auto-disable, script detail preservation (sentinel vs process-exit paths, python3 skip inlined per file convention).
- Handler/agentd/MCP-description suites green; `go build ./...` clean; `golangci-lint` 0 issues; gofmt/goimports clean; pre-commit (repolint incl. migrations/CRD/agent-boundary checks) passed.
- Structural pins for the e2e script (bash -n, row assertions, nightly registration, no calendar-date rot).

## Next Steps
- PR #1420 review round 2.
- Nightly e2e exercises R1–R5 on the next kind run; #1425 design decision unblocks schema-bearing DAG triggers.

## Files Modified
- pkg/workflows/schedule.go (+_test), input_schema.go (+_test), store.go, dag.go (+_test)
- api/internal/handlers/triggers.go (+_test), pod_automation.go (+_test), workflows.go (+_test)
- api/internal/workflows/engine.go (+_test)
- cmd/workspace-agentd/mcp_server.go, workflow_execute.go (+_test)
- local/issue-1410-1412-automation-e2e.sh (+ structural test), .github/workflows/e2e-nightly.yml
- pkg/workflows/store_integration_test.go (next_fire_at persistence row)

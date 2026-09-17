# Worklog: #1410–#1413 — trigger scheduling + DAG-trigger + input-contract fixes

**Date:** 2026-09-17
**Session:** API-side batch from the live automation verification: reschedule keeps the old fire slot (#1410), create fires immediately and accepts junk schedules (#1411), workflow-firing triggers are uncreatable and their failures silent (#1412), declared input schemas are never enforced (#1413).
**Status:** Complete

---

## Objective

Make the trigger/workflow API behave as its own contracts claim: next_fire_at follows the schedule, schedules validate at write time, DAG triggers are creatable end-to-end with pod-scoping enforced, and run inputs satisfy declared schemas.

## Work Completed
- **#1410**: `TriggerUpdate.NextFireAt` + SQL column; update recomputes on reschedule (NEW schedule from now) and on re-enable (stale past slot would misfire). Legacy malformed configs (empty/unparseable) proceed untouched instead of blocking unrelated updates; NEW configs must validate (400).
- **#1411**: exported strict `NextCronFire(expr, tz, now)` (validates 5-field cron + IANA tz, returns next slot UTC); create validates and anchors to the schedule's next real slot — a monthly trigger created at 15:46 no longer fires at 15:46. `computeNextFire` stays LENIENT for scheduler fallback (invalid expr → +1h retry; unknown tz → UTC) — pinned by the pre-existing TestComputeNextFire semantics.
- **#1412**: scheduler `fireWorkflowTarget` records a FAILED fire (workflow_not_found) + increments failures + honors auto-disable (was: silent no-op). Pod-automation create: `workflowId` path skips the routine workspace stamp (the user API rejects both), validates the workflow exists + is owned by the resolved owner + targets THIS pod's workspace (403 otherwise); snake_case `workflow_id` alias normalized; the resolver-spelling `workspaceID` is stripped from the delegated body (Go's case-insensitive decode folded it into the DTO's `workspaceId`, colliding with `workflowId`).
- **#1413**: `runWorkflow` validates input against the declared `inputSchema` (santhosh-tekuri/jsonschema v6) — 400 naming each violation, no run queued; empty input counts as {}; unparseable schema refuses the run naming the compile error.

## Key Decisions
- Strict-at-the-edge, lenient-in-the-scheduler: write paths reject bad schedules; the tick loop degrades gracefully for pre-validation rows.
- Pod scoping for DAG triggers enforced on the workflow's target workspace — the same "this pod schedules only its own workspace" rule routines get by forcing.
- inputSchema enforced on MANUAL runs only this round; the webhook path feeds the fire envelope by design (#1419 tracks the mapping decision).

## Blockers
None.

## Tests Run
- Create: invalid expr / unknown tz → 400; monthly anchors to next slot (day/hour asserted).
- Update: reschedule recomputes to the NEW schedule; invalid new schedule → 400; re-enable re-anchors; legacy empty-config rows still updatable (regression vs MCP-router fixture, fixed).
- Automation create: workflow-path happy (linked, no workspace stamp), snake alias, wrong-workspace 403, unknown workflow 400; routine path unchanged (forced stamp).
- Scheduler: missing workflow → 1 failed fire + failure count; at threshold auto-disables.
- inputSchema: missing required field / wrong type → 400 (named, no run); valid → 202; schema-less unaffected.
- Full api/... + pkg/workflows suites green (one pre-existing flaky uploads fixture noted, passes in isolation).

## Next Steps
- PR → review; agentd batch next (#1414/#1415/#1417); then #1416 image CA; #1418 YAML.

## Files Modified
- api/internal/handlers/triggers.go (+tests) — create validation, update recompute
- api/internal/workflows/engine.go (+tests) — NextCronFire, lenient fallback, loud missing-workflow
- api/internal/handlers/pod_automation.go (+tests) — workflow-target path + normalization
- api/internal/handlers/workflows.go (+tests) — inputSchema enforcement
- pkg/workflows/store.go — TriggerUpdate.NextFireAt + SQL
- api/internal/app/app.go (+wiring test) — wfStore wiring

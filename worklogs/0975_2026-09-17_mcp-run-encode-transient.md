# Worklog: MCP workflow_run double-encode + scheduler transient≠missing (post-#1420 salvage)

**Date:** 2026-09-17
**Session:** #1420 landed a parallel #1410-#1415 batch mid-review of my four PRs. Two defects survive on main: the SDK workflow_run still double-encodes input while main now ENFORCES inputSchema (conforming MCP runs 400 — user-visible), and fireWorkflowTarget counts transient store errors as missing-workflow failures toward auto-disable. This branch salvages exactly those.
**Status:** Complete

---

## Objective
Make the platform MCP surface able to start schema-valid runs at all, and stop outages from auto-disabling healthy triggers.

## Work Completed
- pkg/mcp: workflow_run tool declares an OBJECT input; the handler marshals the map; RunWorkflow carries json.RawMessage and embeds a raw JSON object on the wire (the string wrap double-encoded it). Mock + interface follow.
- engine: fireWorkflowTarget branches on ErrNotFound — transient store errors log (sibling-path parity) and never record a fire or count; missing/deleted keeps main's loud failed-fire + auto-disable. Mock at store parity (ErrNotFound vs injectable transient) + the transient test.
- E2E through the production router + SDK client: conforming input ACCEPTED (red at main — the exact production break), violations rejected naming the field and the wanted type.

## Key Decisions
- Salvage-only scope: everything else my #1421 carried is on main via #1420 in equivalent or better shape.

## Blockers
None.

## Tests Run
Tool-handler units (raw-object passthrough, loud non-object, absent→{}), scheduler transient/missing/auto-disable trio, e2e happy+unhappy legs; pkg/mcp + api/... suites green.

## Next Steps
PR → review; ships with the rebated batch (#1428 YAML, #1423 CA, #1422-v2 templating).

## Files Modified
- pkg/mcp/client.go, workflow_tools.go (+tests)
- api/internal/workflows/engine.go (+tests)
- api/internal/server/mcp_router_integration_test.go

# Worklog: agentd automation tools — trigger_*/workflow_* over pod identity

**Date:** 2026-09-16
**Session:** PR #1402 — give the workspace agent CRUD + debugging access to its owner's automation (the iterate-on-automation loop for the broken workflows feature): 11 MCP tools on /v1/mcp, the /internal/v1/automation API surface, and the opencode seam. First review round caught three execution-verified defects in the body path (drained-body replay, query-clobber in resolve, un-unwrapped tool wrappers); this entry covers the build AND the fix round.
**Status:** Complete

---

## Objective

The agent cannot touch its own automation today: every iteration on triggers/workflows needs the human to drive the user API from outside the workspace, and "why isn't this trigger firing" has no in-workspace answer. Deliver: (1) an internal pod-identity automation surface delegating to the existing user handlers, (2) the opencode seam methods, (3) 11 MCP tools whose bodies pass through verbatim (schema-decoupled), (4) tests at every level INCLUDING delegated-handler integration.

## Work Completed

### Build (commit 97669c95)
- **API** `PodAutomationHandler` + 13 routes under `/internal/v1/automation/{triggers,workflows}[/:id][/fires|/runs]`: TokenReview → SA principal → namespace+workspace match → owner resolved server-side → `userID` injected → EXISTING TriggersHandler/WorkflowsHandler user methods. Trigger-create forces `workspaceId` (DTO spelling) to this pod's workspace. implOnlyAllowlist-ed; app.go wiring + SetLogger.
- **Seam** `pkg/agent/opencode`: `automationCall` transport (SA bearer, verbatim error text) + TriggerList/Create/Get/Update/Delete/Fires, WorkflowList/Create/Get/Update/Delete/Run/Runs. Creates stamp workspaceID after deleting caller scoping fields; IDs UUID-validated pre-dial; WorkflowRun wraps input as CreateWorkflowRunRequest{input}.
- **Tools** one `mcpAutomation` dispatcher + `automationDeps` (WORKSPACE_ID/LLMSAFESPACE_API_URL/bootstrap SA token; client targets the API URL, not opencode); 24 tools in tools/list; patch purity (id/workspaceID stripped from patches).

### Review round 1 — three confirmed body-path defects, all fixed
1. **Drained-body replay** (all 5 body routes dead): resolve's `ShouldBindJSON` consumed the body; the replay then read EOF → delegated bind 400. Fix: `captureBody` drains ONCE via io.ReadAll; resolve sniffs from the bytes; delegate replays bytes onto a fresh reader.
2. **Query-clobber**: `if workspaceID == ""` tested the never-assigned named return → body's (empty) value always overwrote the query → updates/runs 400 pre-delegation. Fix: guard on the query value (`resolved == ""`).
3. **Tool wrapper keys never unwrapped**: schema requires `{"trigger":{...}}` but the delegated handler binds FLAT `CreateTriggerRequest` → creates could never bind. Fix: `passThroughBody(body, key)` unwraps (flat tolerated); renamed from `mustMarshalBody` (naming gripe).

### Hardening from the same round
- Exact-key workspaceID sniff: Go's case-insensitive decode folds DTO `workspaceId` into the resolver's `workspaceID` struct field — a hostile DTO spelling could trip/satisfy identity. Now `map[string]json.RawMessage` + exact key.
- `forceTriggerWorkspace`: RawMessage-preserving merge (no float64 re-format of siblings) + explicit error (non-object body → clear 400, never a silent skip).
- `newReusableBody` → `bytes.NewReader` (double-copy); dead-code keepers removed from the seam test.
- App-level wiring guard `TestPodAutomationHandler_LoggerWired` (api/internal/app, #407 precedent shape).

## Key Decisions
- **Delegation, not duplication**: the internal surface adds auth + scoping ONLY; every domain rule change lands once, in the user handlers.
- **Scoping invariant**: a pod cannot schedule routines into other workspaces — enforced server-side (force on create), stamped at the seam, stripped from patches (id rides URL, workspaceID rides query).
- **Schema-decoupled pass-through**: bodies ride verbatim; agents learn shapes from live `*_list` output; platform schema evolution needs zero agentd changes. Verbatim error text is load-bearing.
- **Cross-layer testing under import boundaries**: agentd cannot import `api/internal/*` — so the delegated chain is pinned at the API layer against REAL handlers + mock stores (create/update/run), and the agentd L1 fake enforces the real bind contract (wrapper → 400, flat → created row).
- Trigger/Workflow Get exist at the seam but are not tools: list carries full entries; schema discovery is the point.

## Blockers
None.

## Tests Run
- Seam (16, pkg/agent/opencode): wire pins all methods, stamp+override, error surfacing, invalid-ID no-dial, run-input default.
- API integration (pod_automation_test.go): auth matrix (7 cases), body-vs-query workspaceID, **DelegatedTriggerCreate** (row owned by resolved owner, FORCED workspace — catches defects 1+3), **DelegatedTriggerUpdate** + **DelegatedWorkflowRun** (query-borne workspaceID through REAL handlers — catches defect 2), **LargeBodyReplay** (64KB, single-Read bug class), wrapper-rejected, forceTriggerWorkspace unit.
- Contract: 13 routes allowlisted + fixture.
- agentd: per-verb wire pins, patch purity, wrapper-unwrap assertion (top-level keys decoded), error passthrough, invalid-ID never-dials, missing deps, L1 full-stack with contract-enforcing fake; inventory (24 tools), per-tool auth gate, description-guidance pins (8 tools).
- app wiring guard. Full suites green: agentd, opencode, handlers, server, app.

## Next Steps
- PR #1402 review round 2; then 0.32.0 release + atomic talos-ops-prod bump; live `trigger_list` verification post-deploy.
- Follow-up candidates: L3 liveprobe legs for the automation tools; e2e leg in local/test.sh.

## Files Modified
- api/internal/handlers/pod_automation.go (+_test.go) — internal surface, capture/replay, forcing
- api/internal/server/router.go (+contract test) — 13 routes
- api/internal/app/app.go (+pod_automation_wiring_test.go) — wiring + SetLogger guard
- pkg/agent/opencode/loopback.go (+loopback_automation_test.go) — seam
- cmd/workspace-agentd/mcp_tools.go, mcp_server.go (+tests) — tools, unwrap, dispatcher

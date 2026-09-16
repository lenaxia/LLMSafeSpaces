# Agentd automation tools: trigger_* + workflow_* over pod identity

Date: 2026-09-16
Scope: cmd/workspace-agentd, pkg/agent/opencode, api (internal automation surface)

## Problem

The workspace agent could not touch its own automation (triggers + workflows).
Every change to the (currently broken) workflows feature needed the human to
drive the user-facing API from outside the workspace - no self-serve
iterate-on-automation loop, no in-workspace debugging of why a trigger is not
firing.

## Design

Delegation, not duplication. Three layers, all established patterns from the
rename_workspace / self-management trains:

1. API: `/internal/v1/automation/{triggers,workflows}[/:id][/fires|/runs]`
   (13 routes). PodAutomationHandler runs the same TokenReview chain as
   workspace-rename (SA principal -> namespace match -> workspaceID match,
   query on reads/deletes, body on creates), resolves the owner server-side,
   then injects `userID` into the gin context and calls the EXISTING
   TriggersHandler/WorkflowsHandler user methods. Every validation, quota,
   encryption, and audit row flows unchanged - the internal surface adds
   auth + scoping, nothing else. Trigger-create additionally forces
   `workspaceId` (DTO spelling) to this pod's workspace: a pod cannot
   schedule routines into other workspaces. Routes are implOnlyAllowlist-ed
   (contract test) - internal, not user API.

2. Seam: one transport (automationCall: SA-token bearer, JSON in, status+body
   out, platform error text surfaced verbatim) + typed methods
   (TriggerList/Create/Get/Update/Delete/Fires, WorkflowList/Create/Get/
   Update/Delete/Run/Runs) on *opencode.Client. Creates stamp workspaceID
   (resolver spelling) after deleting any caller-supplied scoping field;
   WorkflowRun wraps input as CreateWorkflowRunRequest{input} (nil -> {});
   IDs UUID-validated before any dial.

3. Tools: 11 MCP tools (trigger_list/create/update/delete/fires,
   workflow_list/create/update/delete/run/runs) via one mcpAutomation
   dispatcher (automationDeps: WORKSPACE_ID + LLMSAFESPACE_API_URL + bootstrap
   SA token; client targets the API URL, not the opencode agent). Bodies pass
   through verbatim - schema evolution needs zero agentd changes; agents learn
   shapes from live *_list output. Patches carry caller fields only
   (patchWithoutID strips id + both workspace spellings); ids ride URLs.

Trigger/Workflow Get methods exist at the seam for completeness but are not
exposed as tools - list output carries full entries (schema discovery is the
point of list here).

## Tests

- Seam (loopback_automation_test.go, 16): wire pins for all 12 methods,
  create-stamps-workspace + caller-override, error-body surfacing, invalid-ID
  no-dial, run-input defaulting.
- API (pod_automation_test.go): auth matrix (401/403 SA mismatch/403
  namespace/400 missing workspaceID/500 review/500 lookup/404 workspace),
  body-vs-query workspaceID, forceTriggerWorkspace, logger wiring.
- Contract: 13 routes allowlisted + fixture handler (reverse pin).
- agentd (mcp_tools_test.go): per-verb wire pins vs fake automation API
  (auth header, query, path, body), patch purity, run-input wrapping, error
  passthrough, invalid-ID never-dials, all three missing-dep failures,
  unknown-tool, L1 full-stack JSON-RPC pair (trigger_create ->
  trigger_fires); inventory tests now 24 tools; per-tool auth-gate probe;
  description-guidance pins for trigger_list/create/update/delete/fires +
  workflow_create/run/runs.

## Result

All suites green (agentd, opencode seam, handlers, server, app). Enables the
in-workspace automation iteration loop; no domain logic added anywhere.

## Notes

- Direct provider calls from agentd remain architecturally rejected
  (worklog 0924 addendum) - unrelated, unchanged.
- Tools use the SA token, NOT the workspace password: the automation surface
  authenticates pod identity, mirroring rename_workspace.

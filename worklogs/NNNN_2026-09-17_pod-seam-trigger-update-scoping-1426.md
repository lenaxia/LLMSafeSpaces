# Worklog: pod-seam trigger_update scoping (#1426) + v0.33.0 release + prod bump

**Date:** 2026-09-17
**Session:** PR #1420's review flagged the pod automation scoping rule as create-only — `trigger_update` replayed caller `workspaceId`/`workflowId` verbatim, so a pod could retarget its owner's trigger to another workspace or a cross-workspace DAG. This session closes #1426, then cuts the release carrying #1410–#1415 + #1426 and bumps the prod environment.
**Status:** Complete

---

## Objective

Make the pod seam's invariant — a pod schedules work only in ITS workspace — hold on the update path, with the same alias/case-variant hardening create received in #1420.

## Work Completed
- Extracted the shared body normalization (`normalizeTriggerTargetKeys`): non-object/`null` guard, snake_case alias fold with contradictory-duplicate 400, non-string workflowId 400, case-insensitive workspace-key strip (reports whether a workspace key was present so update can distinguish retarget attempts from unrelated patches).
- `scopeTriggerUpdateBody`: non-empty workflowId passes through for gating; workspace retargets forced to THIS pod's workspace; clearing a DAG target (`workflowId:""`) without naming a workspace stamps this workspace (routine HERE, never a targetless zombie); patches touching neither key pass through verbatim.
- `gateWorkflowTarget` helper — the create gate (exists / owner / targets-this-workspace → 404/403) reused by `TriggerUpdate`.
- Tests: `TestScopeTriggerUpdateBody` matrix (verbatim passthrough, case-variant retarget forced, gated DAG retarget, clear-to-routine-here, null/non-string guards) + full delegated-chain rows: workspace retarget silently forced back, workflow-target gating 403/404/200 through the REAL update handler, clear lands the routine in this workspace.

## Key Decisions
- Update scoping mirrors create instead of rejecting target-key patches outright: agents legitimately retarget to another of THIS workspace's workflows or disable triggers; only the cross-workspace escapes are closed.
- Clearing a DAG target stamps the routine workspace rather than erroring: the alternative (a trigger with neither target) fails at fire time with "trigger has no workspace" — a zombie the owner never intended.
- No e2e row: the surface under test requires the pod SA identity inside a workspace pod; the delegated-chain tests exercise the REAL handlers over the REAL delegation path, which is the deepest seam reachable without a live pod.

## Blockers
None.

## Tests Run
- Targeted: scoping matrix + delegated-chain retarget/gating rows (all red against pre-fix code by construction — `scopeTriggerUpdateBody` and the update-path gate did not exist).
- `go build ./...` clean; handlers suite green; gofmt/golangci-lint clean.

## Next Steps
- Release + prod bump (same session).

## Files Modified
- api/internal/handlers/pod_automation.go (+_test.go)

# Worklog: #1449 — org-scope trigger/workflow CRUD param shadowing

**Date:** 2026-09-18
**Session:** #1446 review surfaced that every org-scope trigger GET/PUT/DELETE/fires/rotate 404s: on /api/v1/orgs/:id/triggers/:triggerId the delegated helpers read c.Param("id") — the ORG id — as the trigger id, so the owner-scoped lookup never matches. Found and fixed the identical twin in org-scope workflow routes (:workflowId vs the same helpers).
**Status:** Complete

---

## Objective
Make org-scope automation CRUD reachable at all, without weakening the fail-closed owner scoping.

## Work Completed
- Delegated helpers now take the resource id as a PARAMETER (the issue's preferred direction — kills the fallback convention and the dead c.Param("triggerId") branches): triggers get/update/del/listFires/rotateWebhookSecret and workflows get/update/del/runWorkflow/listRuns.
- User wrappers pass c.Param("id"); org wrappers pass the segment their route carries (:triggerId / :workflowId).
- Regression tests mount the PRODUCTION org route shapes and assert GET/PUT/fires(+runs)/DELETE resolve the actual resource, plus cross-org 404 fail-closed.

## Key Decisions
- Parameter injection over param-context rewriting: the shadowing was a naming-convention accident; explicit arguments make the route-to-handler binding visible and unshadowable.

## Tests Run
- TestOrgTriggerRoutes_ResolveTriggerID, TestOrgWorkflowRoutes_ResolveWorkflowID (red under the old convention by construction — the routes 404'd); full handlers + api tree green; golangci-lint 0 issues.

## Files Modified
- api/internal/handlers/triggers.go (+_test.go), workflows.go (+_test.go)

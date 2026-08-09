# Worklog: Workflow Activator Must Refresh last-activity-at

**Date:** 2026-08-09
**Session:** Fix workspace immediate re-suspend after workflow/trigger-driven activation.
**Status:** Complete

---

## Objective

Workspaces activated by the workflow engine (routine triggers, workflow runs) were being immediately re-suspended by the controller's idle-timeout check on the next reconcile pass. The user-scope cron trigger `Test1we` could never complete a fire because its target workspace was Suspended → briefly Creating → Suspended again within the same activation window, producing `workspace activation timed out after 2m0s (phase: Creating)` on every fire.

---

## Root Cause (Proven)

Two activation paths exist in the codebase, with an asymmetry:

| Path | File:line | Sets `spec.suspend=false` | Refreshes `last-activity-at` |
|---|---|---|---|
| API `POST /workspaces/:id/activate` → `ActivateWorkspace` | `workspace_service.go:1671-1675` | Yes | **Yes** |
| Workflow engine `K8sWorkspaceActivator.EnsureActive` | `engine.go:144-147` | Yes | **No** |

The controller's idle check at `controller/internal/workspace/phase_active.go:235` compares `time.Since(lastActivity) > idleTimeoutSeconds` on every reconcile of an Active workspace. When the workflow engine activated a workspace whose `lastActivity` was older than `idleTimeoutSeconds` (default 8h, this workspace's was 167h stale), the controller suspended it on the very next reconcile — faster than the activator's 2s poll could observe the Active phase.

### Evidence chain (live cluster)

- `kubectl patch workspace ... -p '{"spec":{"suspend":false}}'` → phase Suspended → Creating → pod created
- Pod logs: `opencode started pid 42`, `opencode server listening on http://0.0.0.0:4096` (pod was healthy)
- Controller log (same second): `Workspace idle timeout exceeded; suspending lastActivity: 2026-08-02 18:53:10 idle: 167h10m timeout: 8h0m0s`
- `status.startTime == status.suspendedAt == 18:04:51Z` (same reconcile pass activates then suspends)
- Working workspace `a2703d3d` has the same 8h autoSuspend config but `lastActivity = 2026-08-09T18:11:27Z` (recent) → stays Active

The image pull delay (3m10s for 1.27GB) and the transient 503 startup-probe errors were symptoms, not causes — even with the image cached and the pod healthy, the idle-suspend check killed the workspace before activation could complete.

### Why earlier diagnoses were wrong

| Claimed | Reality |
|---|---|
| "stuck in Creating" | Creating is transient; resting state is Suspended |
| "controller not reconciling" | Controller IS reconciling — it suspends as fast as it activates |
| "runtime env status empty = cause" | Irrelevant; runtime env resolves fine |

---

## Work Completed

### Fix (`api/internal/workflows/engine.go:143-151`)

`K8sWorkspaceActivator.EnsureActive` now includes `metadata.annotations["llmsafespaces.dev/last-activity-at"]` set to `time.Now().UTC()` in the same MergePatch that sets `spec.suspend=false`. This mirrors the API service's `ActivateWorkspace` (`workspace_service.go:1675`) and ensures the controller's idle check sees a current timestamp on the next reconcile.

The patch only fires when phase is Suspended or Failed (once per activation); subsequent poll iterations see Creating/Active and skip the patch, so the timestamp is written exactly once per activation — matching the API path's behavior.

### Regression test (`api/internal/workflows/engine_test.go`)

`TestK8sWorkspaceActivator_PatchRefreshesLastActivity` uses the existing `mockkubernetes` fakes to capture the Patch call's data argument and asserts:
1. The body contains a `metadata` key
2. The `metadata.annotations` map includes `llmsafespaces.dev/last-activity-at`
3. The timestamp parses as RFC3339
4. The timestamp is recent (< 1 minute old)
5. The timestamp differs from the stale fixture value

Verified the test fails against the pre-fix code (`regression: patch body missing metadata key (body={"spec":{"suspend":false}})`) and passes after the fix (body now `{"metadata":{"annotations":{"llmsafespaces.dev/last-activity-at":"2026-08-09T18:25:58Z"}},"spec":{"suspend":false}}`).

---

## Key Decisions

1. **Mirror the API path, don't centralize.** Both activation paths now perform the same annotation refresh, but I did not refactor them to share code. The two paths have different constraints: the API path uses `wsClient.Update` inside `RetryOnConflict` (full object write), the workflow path uses a targeted MergePatch (atomic, smaller blast radius). Forcing them into a shared helper would either weaken the workflow path's atomicity or complicate the API path. The asymmetry was the bug; the fix restores symmetry at the contract level (both refresh lastActivity on activate) without forcing implementation uniformity.
2. **Refresh once per activation, not per poll.** The activator polls every 2s; refreshing on every iteration would advance lastActivity continuously and defeat the idle-timeout feature. Refreshing only when transitioning from Suspended/Failed (the patch branch) matches the API path's single-write semantics.
3. **Use `time.Now().UTC().Format(time.RFC3339)` directly rather than `v1.SetLastActivityAtAnnotation`.** The helper operates on a `map[string]string`, but the MergePatch body needs `map[string]any` for JSON marshaling. Inlining the format avoids a type conversion and keeps the patch body construction in one place.

---

## Tests Run

- `go vet ./api/internal/workflows/...` — clean
- `go test -timeout 120s ./api/internal/workflows/...` — PASS (including new regression test)
- Regression test verified to fail against buggy code, pass against fix

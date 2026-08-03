# Worklog: Image Factory launch integration

**Date:** 2026-08-03
**Session:** Implemented workspace launch integration for image-factory configs — users can now select a Ready config when creating a workspace.
**Status:** Complete

---

## Objective

Close the launch-integration gap (design/0048 item #2): allow users to select an image-factory config in the new-workspace dialog and launch a workspace against its pre-built image.

---

## Work Completed

### Architecture decision — Option A-prime

Investigated three approaches (auto-create RuntimeEnvironment, new WorkspaceSpec field, image-ref-in-spec.runtime). Chose A-prime: set `spec.runtime` to the image ref directly, leveraging the controller's existing `/`-passthrough (`runtime_resolver.go:35-37`). Zero controller change, zero CRD change, zero RuntimeEnvironment change.

### Backend

- `pkg/types/workspace.go` — `CreateWorkspaceRequest` gains `ImageConfigHash`
- `database/imagefactory.go` — `GetLaunchableConfigByHash`: single-query JOIN of config+build, qualified columns, pq.Array scan, returns `(Config, imageRef, error)`
- `workspace/workspace_service.go` — `LaunchableConfigResolver` interface + `SetImageFactoryStore` setter; `resolveImageFactoryConfig` tries member→org→platform, breaks on real errors
- `app/app.go` — wires `dbSvc` as the resolver
- Tests: 5 service-level unit tests, 3 sqlmock DB tests, 1 integration test

### Frontend

- `NewWorkspaceDialog.tsx` — config picker with status pills (Ready=selectable, Building/Rejected=disabled)
- `workspaces.ts` — `create` accepts `imageConfigHash`
- `Sidebar.tsx` — `+` button opens the dialog
- Tests: 3 new vitest tests

### Review fixes

- Fixed ambiguous JOIN columns (qualified with c./b.)
- Fixed selection scan (pq.Array, not JSON)
- Fixed loop-break logic (break on non-ErrNotFound errors)
- Added sqlmock + integration tests

---

## Key Decisions

1. **A-prime over auto-create RuntimeEnvironment** — RTE is cluster-scoped with no owner fields; auto-creating one per user config would produce unbounded CRs with no GC story.
2. **Member→org→platform scope waterfall** — mirrors `ListVisibleConfigs` visibility model; a config the user can see is launchable.
3. **`published_only` policy concerns listing, not launching** — documented in `resolveImageFactoryConfig`'s godoc.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — pass
- `go vet` (workspace, database, app) — pass
- `go test ./internal/services/workspace/` — pass (5 new + all existing)
- `go test ./internal/services/database/ -run GetLaunchableConfigByHash` — pass (3 sqlmock)
- `go test ./internal/handlers/ -run 'IF_|ImageFactory'` — pass (no regressions)
- `npx tsc --noEmit` — pass

---

## Next Steps

1. Merge PR #641.
2. Cut release v0.8.5.
3. Bump talos-ops-prod Helm release.
4. Test end-to-end: create a workspace with the `test2` config from the UI.

---

## Files Modified

- `pkg/types/workspace.go`
- `api/internal/services/database/imagefactory.go`
- `api/internal/services/database/imagefactory_test.go`
- `api/internal/services/database/imagefactory_integration_test.go`
- `api/internal/services/workspace/workspace_service.go`
- `api/internal/services/workspace/image_factory_launch_test.go` (new)
- `api/internal/app/app.go`
- `frontend/src/api/workspaces.ts`
- `frontend/src/components/workspace/NewWorkspaceDialog.tsx`
- `frontend/src/components/workspace/NewWorkspaceDialog.test.tsx`
- `frontend/src/components/layout/Sidebar.tsx`

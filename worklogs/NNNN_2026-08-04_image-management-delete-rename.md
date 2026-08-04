# Worklog: Image management — delete + rename config

**Date:** 2026-08-04
**Session:** PR2 features: delete/rename image-factory config API + frontend wiring.
**Status:** Complete

---

## Objective

Wire up the "Rename" and "Delete" buttons that were placeholders in the Workspace Images tab. Add the backend endpoints, store methods, and frontend integration.

---

## Work Completed

### Backend

- `database/imagefactory.go`: Added `DeleteConfig` and `RenameConfig` methods + `ErrConflict` sentinel. Delete checks RowsAffected → ErrNotFound. Rename maps pq unique violation (23505) → ErrConflict.
- `handlers/imagefactory_manage.go`: `DeleteConfig` handler (member-scope only, rejects building status), `RenameConfig` handler (member-scope only, validates name), shared `resolveConfigByHash` helper.
- `handlers/imagefactory.go`: Added `DeleteConfig` + `RenameConfig` to the handler-local `imageFactoryStore` interface.
- `server/router.go`: Added `DELETE /configs/:hash` + `PATCH /configs/:hash` routes.
- 5 handler tests: delete success (member), delete conflict (building), delete forbidden (platform), rename success, rename empty-name validation.

### Frontend

- `api/imageFactory.ts`: Added `deleteConfig` + `renameConfig` methods.
- `api/client.ts`: Added `patch` method.
- `WorkspaceImagesTab.tsx`: Wired rename (inline edit + save/cancel) and delete (confirm dialog) buttons.

---

## Key Decisions

1. **Member-scope only for delete/rename** — org and platform configs require admin elevation (deferred to the admin portal management section).
2. **Reject deletion while building** — prevents orphaned GH Actions builds whose callback would reference a deleted config.
3. **FK cascade** — `image_factory_builds.config_id` has no `ON DELETE CASCADE`. The store's `DeleteConfig` cascades in a single transaction: deletes builds first, then the config, with rollback on failure.
4. **Name collision** — `RenameConfig` maps postgres unique violation to `ErrConflict` → handler returns 409.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — pass
- `go test ./internal/handlers/ -run 'DeleteConfig|RenameConfig'` — pass (5 tests)
- `npx tsc --noEmit` — pass
- `make repolint` — pass

---

## Next Steps

1. Merge PR.
2. Admin/org portal management section (separate PR).

---

## Files Modified

- `api/internal/services/database/imagefactory.go` — DeleteConfig + RenameConfig + ErrConflict
- `api/internal/handlers/imagefactory.go` — interface additions
- `api/internal/handlers/imagefactory_manage.go` (new) — handlers
- `api/internal/handlers/imagefactory_manage_test.go` (new) — tests
- `api/internal/handlers/imagefactory_test.go` — fake additions
- `api/internal/handlers/imagefactory_e2e_test.go` — fake additions
- `api/internal/server/router.go` — routes
- `frontend/src/api/client.ts` — patch method
- `frontend/src/api/imageFactory.ts` — deleteConfig + renameConfig
- `frontend/src/components/settings/WorkspaceImagesTab.tsx` — wired buttons

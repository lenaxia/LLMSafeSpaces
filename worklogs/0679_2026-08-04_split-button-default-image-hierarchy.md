# Worklog: Split-button launch + default-image hierarchy

**Date:** 2026-08-04
**Session:** Replaced the full NewWorkspaceDialog with a segmented control (split button) and implemented the default-image hierarchy (user → org → platform → base).
**Status:** Complete

---

## Objective

Implement the user's requested UI: `[+]` launches the default image in one click, `[▼]` opens a popup of available images. Build the full default-image hierarchy: platform default → org default → user override.

---

## Work Completed

### Backend

- Migration `000015`: `org_settings` table (generic key-value, FK to `organizations(id)` ON DELETE CASCADE)
- `pkg/settings/schema.go`: `preferredRuntime` Tier 3 user setting
- `database/settings.go`: `OrgSettingsStore` interface + CRUD + `GetOrgSettingString`
- `workspace_service.go`: `resolveDefaultRuntime` walks user → org → platform → base; `UserDefaultReader` + `OrgDefaultReader` deps
- `handlers/org_settings.go`: `GET /orgs/:id/settings` (member) + `PUT /orgs/:id/settings/default-image` (admin)
- `app.go`: wires `userSettings` + `dbSvc` as hierarchy sources
- 5 hierarchy tests (user wins, org when no user, fallback to base, not-launchable falls through, DB error falls through)

### Frontend

- `NewWorkspaceSplitButton.tsx`: segmented control (+ default, ▼ popup)
- `Sidebar.tsx`: replaces dialog with split-button
- `workspaces.ts`: stops hardcoding `"base"`, lets backend resolve hierarchy
- `orgs.ts`: `getSettings` + `setDefaultImage` API methods
- Deleted dead `NewWorkspaceDialog` + its tests

### Review fixes (first round)

- Fixed migration FK: `orgs(id)` → `organizations(id)` (table name was wrong)
- Synced migration to Helm chart (`make chart-sync-migrations`)
- Added DB-error logging before fall-through in `resolveDefaultRuntime`
- Deleted dead `NewWorkspaceDialog` component

---

## Key Decisions

1. **Split-button over dialog** — the user explicitly wanted `[+]` as one-click default launch + `[▼]` as a skinnier attached popup, not a full modal dialog.
2. **Generic `org_settings` table** — the codebase deliberately avoids a generic org-settings table (org config lives in dedicated normalized tables), but a generic key-value store is the right fit for extensible org defaults. Modeled after `user_settings`.
3. **Each tier stores a config hash** — resolved via image factory. Platform tier stores a direct image ref (existing behavior). This unifies the default concept around image-factory configs.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — pass
- `go vet` (changed packages) — pass
- `go test ./internal/services/workspace/` — pass (all existing + 5 new hierarchy tests)
- `npx tsc --noEmit` — pass
- `make repolint` — pass (chart-migrations synced)

---

## Next Steps

1. Merge PR #642 after review approval.
2. Release + deploy.
3. Org admin UI for setting the default image (API exists, UI deferred).
4. Clean up `NewWorkspaceDialog` import references if any remain.

---

## Files Modified

- `api/migrations/000015_org_settings.up.sql` (new)
- `api/migrations/000015_org_settings.down.sql` (new)
- `helm/migrations/000015_org_settings.up.sql` (new, chart sync)
- `helm/migrations/000015_org_settings.down.sql` (new, chart sync)
- `pkg/settings/schema.go`
- `api/internal/services/database/settings.go`
- `api/internal/services/workspace/workspace_service.go`
- `api/internal/services/workspace/workspace_defaults_test.go`
- `api/internal/services/workspace/default_runtime_hierarchy_test.go` (new)
- `api/internal/handlers/org_settings.go` (new)
- `api/internal/server/router.go`
- `api/internal/app/app.go`
- `frontend/src/api/workspaces.ts`
- `frontend/src/api/orgs.ts`
- `frontend/src/components/workspace/NewWorkspaceSplitButton.tsx` (new)
- `frontend/src/components/layout/Sidebar.tsx`
- `frontend/src/components/workspace/NewWorkspaceDialog.tsx` (deleted)
- `frontend/src/components/workspace/NewWorkspaceDialog.test.tsx` (deleted)

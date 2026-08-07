# Worklog NNNN — Image factory org/platform-scoped config creation (frontend)

**Session:** 2026-08-07
**Status:** Ready for merge
**Scope:** Frontend PR #2 of design/0047 — org admin Images tab + platform
admin Image Factory tab.

## Objective

The backend (PR #664) added org/platform-scoped config creation routes.
This PR adds the frontend to use them: an Images tab in the org admin
portal and an Image Factory tab in the platform admin portal.

## Key Decisions

1. **Reuse WorkspaceImagesTab with a scope prop** (like McpServersTab).
   The component accepts `scope: "user" | "org" | "platform"` which
   controls which create endpoint is called, which configs are editable,
   and how configs are grouped into sections.
2. **Member configs in a separate section** (Q3 decision). When scope is
   org or platform, the tab shows "Org & Platform Images" / "All Images"
   first, then a "Member Images" section. For user scope, all configs
   render in one flat list ("My Workspace Images") — unchanged from before.
3. **listConfigs returns bare array** — unwrapped the `{configs: [...]}`
   envelope in the API client so the type is honest (mirrors the MCP
   servers envelope fix from #650). All callers updated.

## Work Completed

- `frontend/src/api/imageFactory.ts` — added `createOrgConfig`,
  `createPlatformConfig`; `listConfigs` now unwraps the envelope
- `frontend/src/components/settings/WorkspaceImagesTab.tsx` — refactored
  to accept `scope` prop; section grouping, scope-aware create, edit
  permissions
- `frontend/src/router.tsx` — org route `/orgs/:id/images`, admin route
  `/admin/image-factory`
- `frontend/src/components/org-admin/OrgAdminLayout.tsx` — "Images" nav
  item
- `frontend/src/components/platform-admin/PlatformAdminLayout.tsx` —
  "Image Factory" nav item
- Updated all callers of `listConfigs` (SettingsForm,
  NewWorkspaceSplitButton) + their tests to use bare array

## Tests Run

- `npx tsc --noEmit` — clean
- `npx vitest run` — 1536 pass (144 files)
- `npm run build` — succeeds

## Blockers

None.

## Next Steps

- E2e: extend org-admin.spec.ts + add platform-admin image factory specs
- D3: `allowed_image_configs` restriction policy

## Files Modified

- `frontend/src/api/imageFactory.ts`
- `frontend/src/components/settings/WorkspaceImagesTab.tsx`
- `frontend/src/router.tsx`
- `frontend/src/components/org-admin/OrgAdminLayout.tsx`
- `frontend/src/components/platform-admin/PlatformAdminLayout.tsx`
- `frontend/src/components/settings/SettingsForm.tsx`
- `frontend/src/components/workspace/NewWorkspaceSplitButton.tsx`
- `frontend/src/components/settings/SettingsForm.test.tsx`
- `frontend/src/components/workspace/NewWorkspaceSplitButton.test.tsx`
- `frontend/src/components/settings/WorkspaceImagesTab.test.tsx`
- `frontend/src/components/settings/WorkspaceImagesTab.grouping.test.tsx`

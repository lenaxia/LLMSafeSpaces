# Worklog NNNN — Org admin Settings tab: surface 6 backend-only org policies

**Date:** 2026-08-05
**Scope:** New org-admin Settings tab exposing the 6 org policies that have
been backend-only since Epic 43 (US-43.7/43.8).

## Objective

The org admin portal had 9 backend org policies but only 3 had any UI
(`sys_prompt_org`, `allow_user_prompt` in Agent Config; `allow_user_mcp_servers`
added in #650). The remaining 6 — workspace quotas, model/provider
restrictions, MCP per-workspace cap, and default workspace image — were
backend-only with no way for org admins to manage them. This was an original
gap from Epic 43, not a regression: no org Settings tab ever existed.

## Key Decisions

1. **Separate Settings tab, not cram into Agent Config.** The Agent Config
   tab already held non-agent policies (MCP governance). Adding 6 more would
   overload it. A dedicated Settings tab mirrors the platform admin's split
   (Settings vs Agent Config) and gives org admins one place for operational
   policies.
2. **Toggle + textarea for model/provider restrictions.** Empty array =
   unrestricted; non-empty = restricted. The toggle visualizes the state; the
   textarea accepts one model/provider per line. Matches the backend's
   `[]string` validation in `policies.go:134-136`.
3. **0 = unlimited convention for numeric quotas.** Consistent with the
   backend validation (`n >= 0` in `policies.go:137-142`).
4. **Component named `OrgAdminSettingsTab` (not `OrgSettingsTab`)** to avoid
   collision with the platform-admin's `OrgSettingsTab` in
   `components/settings/`.

## Summary of changes

- **`frontend/src/components/org-admin/OrgSettingsTab.tsx`** (new) — exports
  `OrgAdminSettingsTab`. Three cards:
  1. Workspace Limits: `max_workspaces_per_member`,
     `max_active_workspaces_per_member`
  2. Model & Provider Restrictions: `allowed_models`, `allowed_providers`
     (toggle + textarea)
  3. MCP & Image Defaults: `max_mcp_servers_per_workspace`,
     `default_runtime`
- **`frontend/src/router.tsx`** — lazy import + route at `/orgs/:id/settings`
- **`frontend/src/components/org-admin/OrgAdminLayout.tsx`** — nav item
  "Settings" after "Agent Config"
- **`frontend/src/components/org-admin/OrgSettingsTab.test.tsx`** (new) — 9
  unit tests

## Tests Run

- `npx tsc --noEmit` — clean
- `npx vitest run src/components/org-admin/OrgSettingsTab.test.tsx` — 9 pass
- `npx vitest run` — 1532 pass (143 files)
- `npm run build` — succeeds

## Blockers

None.

## Next Steps

- Integration/e2e coverage for `/orgs/:id/settings` — codebase-wide gap (no
  e2e exists for any org-admin portal tab); tracked separately, not blocking.
- Consider auto-populating the `default_runtime` dropdown from the image-factory
  configs API instead of a free-text input.

## Files Modified

- `frontend/src/components/org-admin/OrgSettingsTab.tsx` (new)
- `frontend/src/components/org-admin/OrgSettingsTab.test.tsx` (new)
- `frontend/src/router.tsx` (lazy import + route)
- `frontend/src/components/org-admin/OrgAdminLayout.tsx` (nav item)

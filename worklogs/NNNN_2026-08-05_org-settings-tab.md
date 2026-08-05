# Worklog NNNN — Org admin Settings tab: surface 6 backend-only policies

**Date:** 2026-08-05
**Scope:** Add an org-level Settings tab that exposes the 6 org policies
that have been backend-only since Epic 43 (US-43.7/43.8).

## Summary

The org admin portal had 9 backend policies but only 3 had UI (`sys_prompt_org`,
`allow_user_prompt` in Agent Config; `allow_user_mcp_servers` added in #650).
The other 6 — workspace quotas, model/provider restrictions, MCP caps,
default workspace image — were backend-only with no way for org admins to
manage them. This was an original gap from Epic 43, not a regression: no
org Settings tab ever existed.

## Changes

- **`OrgSettingsTab.tsx`** (new) — three cards managing all 6 policies:
  1. **Workspace Limits**: `max_workspaces_per_member`,
     `max_active_workspaces_per_member` (number inputs, 0 = unlimited)
  2. **Model & Provider Restrictions**: `allowed_models`,
     `allowed_providers` (toggle + textarea tag input; disabled = empty
     array = unrestricted)
  3. **MCP & Image Defaults**: `max_mcp_servers_per_workspace`,
     `default_runtime` (number input + text input)
- **`router.tsx`** — lazy import `OrgAdminSettingsTab`, route at
  `/orgs/:id/settings`
- **`OrgAdminLayout.tsx`** — nav item "Settings" after "Agent Config"
- **`OrgSettingsTab.test.tsx`** (new) — 7 tests: renders 3 cards, loads
  existing values, saves workspace limits, saves model restrictions
  (enable + disable), saves MCP/runtime together, error toast on failure

The component reuses the `policiesApi` (GET/PUT `/orgs/:id/policies`)
introduced in PR #650.

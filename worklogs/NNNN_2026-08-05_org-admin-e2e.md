# Worklog NNNN — E2e coverage for org-admin portal

**Session:** 2026-08-05
**Status:** Ready for merge

**Scope:** Add Playwright e2e specs for the org-admin portal — the
codebase-wide gap flagged by every PR review of org-admin tabs (#650, #653).

## Objective

No e2e coverage existed for any org-admin portal tab (Overview, Members,
Credentials, MCP Servers, Workspaces, Audit, Billing, SSO, Agent Config,
Settings). The bot review of #653 (org Settings tab) flagged this as a
mandatory gate that was consistently deferred as a "codebase-wide item."
This PR closes the gap.

## Key Decisions

1. **One spec file covering the portal broadly**, not per-tab. The portal
   shares one auth/org mock setup; splitting into per-tab files would
   duplicate the mock boilerplate. The spec covers deep-linking, sidebar
   navigation, and role-gated visibility.
2. **Mocked backend (consistent with all existing specs).** Every existing
   e2e spec (`settings.spec.ts`, `relay-admin.spec.ts`, etc.) uses
   `page.route()` to stub API responses — no real backend needed. This spec
   follows the same pattern.
3. **Settings tab gets the deepest coverage** (load + save PUT assertion)
   because it's the newest and most interactive. The save test uses
   `expect.poll()` to assert the PUT body reaches the mocked endpoint.

## Work Completed

- **`frontend/tests/e2e/org-admin.spec.ts`** (new) — 7 tests:
  1. Renders the portal with all admin nav items visible
  2. Deep-links to `/orgs/:id/settings` and renders all 3 policy cards
     with loaded values
  3. Navigates from overview to settings via sidebar link
  4. Deep-links to `/orgs/:id/agent-config` and renders Member Customization
  5. Deep-links to `/orgs/:id/mcp-servers` and renders the tab
  6. Saving workspace limits PUTs the policy (asserts request body)
  7. Non-admin member does not see admin-only tabs (Members, Settings,
     Credentials hidden; Overview, Workspaces visible)

## Tests Run

- `npx tsc --noEmit` — clean
- Playwright e2e cannot run in this sandbox (browser launch restriction);
  CI validates via `npx playwright test` with mocked backend.

## Blockers

None.

## Next Steps

- Add interaction tests for the MCP Servers tab (create/delete flows)
  once the org-scope create endpoint stabilizes.
- Add save-failure path test for the Settings tab (mock PUT rejection).

## Files Modified

- `frontend/tests/e2e/org-admin.spec.ts` (new)

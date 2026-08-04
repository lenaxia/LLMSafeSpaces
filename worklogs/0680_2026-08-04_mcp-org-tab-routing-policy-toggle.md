# Worklog 0680 — MCP org tab: routing, list-envelope crash, and member-policy toggle

**Date:** 2026-08-04
**Scope:** Three org-admin MCP bugs reported in production after v0.7.x.

## Summary

1. **Org-level add rejected with "org admin has disabled member MCP servers".**
   Root cause: `router.tsx` mounted `<OrgMcpServersTab scope="org" />` at
   `/orgs/:id/mcp-servers` without passing `orgId`. The component's API-call
   guard (`scope === "org" && orgId`) was always false → every org-scope
   request fell through to the **user** endpoint (`POST /me/mcp-servers`),
   which hit the user-scope policy gate and returned the 403 meant for
   members. The same root-cause class as the v0.7.1 production 500 (org tab
   posting to `/me/`).

2. **`n.map is not a function` crash after a successful add.** Root cause:
   the backend `list` handler returns `gin.H{"servers": out}` (an envelope),
   but the API client typed the response as `McpServerResponse[]` (a bare
   array). `setServers(data)` stored the envelope object; `servers.map`
   threw on the next render.

3. **No UI for org admins to manage member MCP servers.** The backend
   `allow_user_mcp_servers` policy was fully wired (`policies.go` validates
   it, `mcp_servers.go:228` enforces it), but the frontend never exposed a
   toggle — so the policy defaulted locked with no way for an org admin to
   unlock it. The mirrored `allow_user_prompt` had a toggle in
   `OrgAgentConfigTab`; MCP never got the equivalent.

## Fixes

- **Routing (frontend/src/components/settings/McpServersTab.tsx):** the
  component now reads `orgId` from the `OrgAdminLayout` outlet context
  (`useOutletContext<{org}>()`) when `scope === "org"`, mirroring
  `OrgAgentConfigTab`. The `orgId` prop was removed from the public props
  interface (it was never passed by the router).
- **List envelope (frontend/src/api/mcpServers.ts):** the three `list`
  functions unwrap `{servers: [...]}` into a bare array, so the type
  annotation is honest and `setServers(data)` receives an array. Defensive
  against both shapes (`Array.isArray(r) ? r : r.servers ?? []`).
- **Member-policy toggle (frontend/src/components/org-admin/` +
  "OrgAgentConfigTab.tsx):** added a second `Toggle` in the "Member
  Customization" card, backed by a new `policiesApi`
  (`frontend/src/api/policies.ts`) that GETs `/orgs/:id/policies` and PUTs
  `/orgs/:id/policies/allow_user_mcp_servers` with a raw `true`/`false`
  body.
- **Client PUT falsy-body bug (frontend/src/api/client.ts):** the `put`
  helper used `body ? JSON.stringify(body) : undefined`, which silently
  dropped falsy bodies (`false`, `0`). Setting the policy to `false` would
  have sent no body → 400. Changed to `body !== undefined`.

## Validation

- `npx tsc --noEmit` — clean.
- `npx vitest run` — 1496 tests pass (138 files), including 2 new tests
  for the MCP toggle (`reflects the allow_user_mcp_servers policy`,
  `shows the locked caption when absent`).
- `npm run build` — succeeds.

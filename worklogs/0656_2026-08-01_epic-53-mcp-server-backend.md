# Worklog 0656 — Epic 53: MCP Server Integration (Backend)

**Date:** 2026-08-01
**Epic:** 53 — MCP Server Integration
**Scope:** Backend implementation (US-53.1 through US-53.8, US-53.11, US-53.11b). Frontend (US-53.9/53.10/53.10b) and e2e (US-53.12) pending.

## Summary

Implemented the complete backend wired path for external MCP server management: platform/org/user-scoped registration, 3-tier encrypted storage, injection into workspace pods via secrets.json, and pod-side materialization into opencode's `mcp` config section. The org-admin control model was revised from the original Epic 53 design (D11/D12 feature-flag model) to a per-org `allow_user_mcp_servers` policy mirroring the proven `allow_user_prompt` pattern, plus a plan-tier registration quota for solo users.

## Assumptions (stated + validated)

1. **A6 (opencode mcp support):** Validated from `cmd/workspace-agentd/testdata/opencode-config.schema.json:1014,550-673`. Top-level `mcp` key with `local`/`remote` per-server shapes. Contract in `MATERIALIZE-CONTRACT.md`.
2. **Migration numbering:** Design doc cited migrations up to `000041`; actual snapshot has `000001`–`000011`. New migration is `000012`. Verified via `ls api/migrations/`.
3. **`users.plan_id` exists:** At `000001:286` (not `000026:17` as the design claims). Reuses `OrgPlan` constants (`free/team/business/enterprise`) — no `basic` tier.
4. **`GetUserOrgID` exists:** At `pg_org_store.go:866` (not `:801` as the design claims). Used by the user-scope gate.
5. **`RootKeyProvider` interface:** `Encrypt`/`Decrypt` at `root_key.go:35-38`. Admin/org MCP servers reuse `"provider-credentials"` and `"org-credentials"` purposes (D3).

## Design deviation: control model

The original Epic 53 design (D11/D12) used a hard org-membership disqualification + boolean `PersonalMcpServers` feature flag. The user requested an org-admin toggle (mirroring `allow_user_prompt`). Revised:
- **Org members:** gated by `allow_user_mcp_servers` org policy (default locked). Org admin toggles.
- **Solo (no-org) users:** gated by plan-tier quota `MaxPersonalMcpServers` (free=5, team/business/enterprise=unlimited). Flag enabled on all tiers (no 402 for free).

## What was built

### US-53.1 — Materialize contract
- `design/stories/epic-53-mcp-server-integration/MATERIALIZE-CONTRACT.md` — the opencode mcp config contract (A6/A7/A17 validated from pinned schema).

### US-53.2 — Migration + types
- `api/migrations/000012_mcp_servers.up.sql` / `.down.sql` (+ helm mirror) — three tables: `mcp_servers`, `mcp_server_bindings`, `mcp_server_auto_apply`. Owner-type CHECK includes `'user'` from day one. Transport CHECK includes http/sse/stdio.
- `pkg/types/mcp.go` — `MCPServer`, `MCPServerResponse` (secrets `json:"-"`), `CreateMCPServerRequest`, `UpdateMCPServerRequest`, validators, `OpencodeMCPType()` mapper.

### US-53.3 — Store layer
- `pkg/secrets/mcp_store.go` — `PgSecretStore` methods: CRUD (Create/List/Get/Update/Delete), `GetWorkspaceMCPServers` (injection query), `SeedWorkspaceMCPServers` (mirrors `SeedWorkspaceCredentials` with D11 enforcement), `BindMCPServerToWorkspace`, `BackfillMCPServerAutoApply`, auto-apply CRUD, `CountMCPServersByOwner`, `CountWorkspaceMCPServers`.

### US-53.4/53.5/53.5b — Handlers + routes
- `api/internal/handlers/mcp_servers.go` — `MCPServersHandler` covering all three scopes. Admin/org use `RootKeyProvider`; user-scope returns 501 for encryption (DEK session wiring pending). Validation: transport, URL, SSRF (loopback/link-local/metadata blocked), name regex.
- Routes in `api/internal/server/router.go`: admin `/api/v1/admin/mcp-servers` (AdminGuard), org `/api/v1/orgs/:id/mcp-servers` (OrgAdminGuard), user `/api/v1/me/mcp-servers` (AuthMiddleware + org-policy gate + plan-quota gate).
- Handler construction in `api/internal/app/app.go`.

### US-53.6 — Binding lifecycle
- `CredentialProvisioner` interface extended with `SeedWorkspaceMCPServers`. Called alongside `SeedWorkspaceCredentials` in `workspace_service.go:389`.

### US-53.7 — Injection pipeline
- `pkg/secrets/types.go` — `SecretTypeMcpServer = "mcp-server"` added to enum + `ValidSecretTypes`.
- `pkg/secrets/injection.go` — `loadMCPServers()` added to both `InjectSecrets` and `InjectSessionlessSecrets`. Decrypt matrix: admin via `adminProvider`, org via `orgProvider`, user via session DEK (skipped-with-audit when no session, D13).

### US-53.8 — Pod-side materialization
- `pkg/agentd/secrets/secrets.go` — `StagedMCPServer` type + `applyMCPServer` applier (stages env/headers for rendering). Added to `applyOne` dispatch + `reset()`.
- `cmd/workspace-agentd/agent_config_writer.go` — `mcpServerEntry` field + `SetMCPServers()` setter + `buildOpencodeMCPServerEntry()` renderer + mcp section in `rebuild()`.
- `cmd/workspace-agentd/secrets.go` — `applyMCPServersToConfig()` (boot path) + reload-path `SetMCPServers` wiring.

### US-53.11 — Governance
- Org policies: `allow_user_mcp_servers` (bool, default locked) + `max_mcp_servers_per_workspace` (int, default 5) added to `pkg/types/orgs_policy.go`, policy service (`applyPolicyValue` + `intersect`), handler validation (`isValidKey`/`isValidValue`), migration CHECK.
- Instance setting: `mcp.allowOrgAdminServers` (bool, default true) in `pkg/settings/schema.go` (SchemaVersion bumped to 8).

### US-53.11b — Plan-tier quota
- `pkg/billing/plan_tiers.go` — `MaxPersonalMcpServers int` added to `PlanFeatures`: free=5, team/business/enterprise=-1. `IsFeatureAllowed("personal_mcp_servers")` returns true when `!= 0`.

## Tests
- `pkg/types/mcp_test.go` — name/transport validators, payload round-trip, response secret-leak assertion, org-policy accessors.
- `pkg/billing/plan_tiers_test.go` — per-tier quota, IsFeatureAllowed, unknown-feature fail-open.
- `api/internal/handlers/mcp_servers_test.go` — validation (8 cases), admin list, user org-member-blocked (403), policy-allowed (passes gate), quota-exceeded (409), unlimited-plan (passes quota).

### US-53.9/53.10/53.10b — Frontend tabs
- `frontend/src/api/mcpServerTypes.ts` — shared TypeScript types matching Go DTOs.
- `frontend/src/api/mcpServers.ts` — three API clients (admin/org/user) with CRUD + bindings + auto-apply.
- `frontend/src/components/settings/McpServersTab.tsx` — shared component parameterized by scope. Transport selector, conditional fields (url for remote, command+args for stdio), secret env/header editors (password type), auto-apply toggle, enable/disable, delete.
- Router wiring: admin `/admin/mcp-servers`, org `/orgs/:id/mcp-servers`, user `/settings/mcp-servers`.
- Nav items added to PlatformAdminLayout, OrgAdminLayout, SettingsPage.

## Pending
- US-53.12: E2E integration tests against a live kind cluster (12 scenarios).
- Comprehensive store-layer tests with go-sqlmock (integration tests written but gated).

## Session 3: Gap resolution

All gaps identified in the adversarial self-review have been resolved:

1. **userPlan resolution (HIGH):** `User` struct has no `PlanID` field. Added `GetUserPlan(userID)` store method on `PgOrgStore` that reads `users.plan_id` directly. The handler now resolves the plan from the store (not auth context). Returns `'free'` on error (fail-safe).

2. **BackfillMCPServerAutoApply SQL (HIGH):** Simplified the org-workspace match from a convoluted organizations subquery to a direct `workspaces.org_id::text` comparison. Also added `w.deleted_at IS NULL` filter.

3. **Reload fan-out after bind (HIGH):** Added `mcpSecretPusher` func type + `SetSecretPusher` setter. The `Bind` handler now calls the pusher after a successful bind, triggering `POST /v1/reload-secrets` on the workspace's running pod via the shared `agentpush.Service`. A wrapper adapter in `app.go` adapts `agentpush.Service.Push` → `mcpSecretPusher`.

4. **Quota enforcement at Bind (MEDIUM):** `Bind` handler now checks `CountWorkspaceMCPServers` against the org policy quota before binding. Over-quota returns 409. Platform-admin servers are exempt (platform policy, not org-controlled).

5. **mcp.allowOrgAdminServers kill-switch (HIGH):** Added `mcpSettingsReader` interface + `SetSettings` setter. `OrgCreate` checks the instance setting; returns 403 when disabled.

6. **Audit logging (MEDIUM):** Added `mcpAuditLogger` interface + `SetAudit` setter. `create` and `del` now emit `LogAuditEvent` with domain/actor/action/target/metadata. Best-effort (failures logged at Warn, response unaffected).

7. **Prometheus metrics (MEDIUM):** Added 3 metrics to `metrics.go`: `mcp_servers_total{scope,action}`, `mcp_bindings_total{source_type}`, `mcp_user_scope_gate_total{result}`. Wired into create/delete/bind paths.

8. **OpenAPI spec (LOW):** Added all MCP paths (`/admin/mcp-servers`, `/orgs/{id}/mcp-servers`, `/me/mcp-servers`) with full CRUD + bindings + auto-apply. Added `McpServer`, `CreateMcpServerRequest`, `UpdateMcpServerRequest` schemas. Added `mcp` tag.

9. **Validator whitelist (LOW):** Added all MCP paths to `sdks/validate/main.go` expectedPaths.

# Epic 53: MCP Server Integration

**Status:** In implementation (backend + frontend, all stories). Originally "Planning."
**Created:** 2026-06-21
**Last Revised:** 2026-08-01 — reconciled against the actual codebase snapshot (see "Snapshot Reconciliation" below).
**Priority:** Medium-High (unblocks enterprise "bring your own tooling" workflows; security-sensitive)
**Depends On:** Epic 30 (Unified Credential Model — injection pipeline), Epic 11/43 (organizations, org admin guard). Soft-depends on Epic 50 (master KEK hardening) for clean crypto, but does NOT block on it.
**Authoritative for:** How the platform lets admins register, secure, bind, and inject **external** MCP servers so that agents running inside workspaces gain their tools.

---

## Snapshot Reconciliation (2026-08-01)

This document was originally written against a newer codebase projection. Implementation against the actual snapshot (`/workspace/LLMSafespaces`, latest migration `000011`) required the following corrections:

1. **Migration numbers corrected.** The original cited `000015/000026/000030/000033/000041-043`; none exist here. The actual new migration is **`000012_mcp_servers`**. Features the doc attributed to those future migrations already exist in `000001` (e.g. `users.plan_id` at `000001:286`; `org_policies` at `000001:243`).
2. **US-53.1 complete.** A6/A7/A17 are validated by the pinned opencode schema (`cmd/workspace-agentd/testdata/opencode-config.schema.json:1014, 550-673`) — no live spike required. Contract captured in `MATERIALIZE-CONTRACT.md`.
3. **D11 + D12 + US-53.11b revised** (see revised decisions below). The org-membership gate is retained (D11), but the solo-user gate is now a **plan-tier registration quota** (`MaxPersonalMcpServers`), not a boolean flag. An org-admin policy (`allow_user_mcp_servers`) gates org members. The `UserFeatureGuard` returns 402 only when the quota is 0 (disabled).
4. **`GetUserPlan` does not exist** and is added by this epic (single-column read of `users.plan_id`). `OrgPlan` constants (`free/team/business/enterprise`) are reused for user-scope — there is no separate `UserPlan` type and no `basic` tier.

---

## Problem Statement

Today every workspace runs an AI agent (`opencode serve`) whose tool surface is whatever opencode ships with plus the platform's own tools. There is **no supported way** for an operator to give a fleet of agents access to third-party tool servers — a GitHub MCP server, a Slack MCP server, an internal database MCP server, a custom company MCP server — without each end user manually editing their own agent config and pasting secrets into a sandbox.

Concretely, a code audit (2026-06-21) confirmed:

- The codebase has **zero** concept of an "external MCP server", "MCP client", "tool registry", or "external tool". `grep` for `tool_registry`, `ExternalTool`, `mcp_client`, `McpServer`, `external_mcp` across all `*.go` files returns no production hits.
- The platform's **own** MCP server (`pkg/mcp/server.go`) is the inverse of this feature: it exposes *the platform's* tools to external MCP clients (Claude, Cursor). Epic 53 is the reverse direction — the platform manages *external* MCP servers that *its agents* connect to.
- `opencode` (the agent) does support configuring external MCP servers in its config, but the platform **never writes that section**. `pkg/agent/opencode/format.go:45` only emits `provider` and `model`; there is no `mcp`/`mcpServers` emission anywhere in `runtimes/`, `pkg/`, or `cmd/workspace-agentd/`.
- The frontend already contains a placeholder: `frontend/src/components/settings/ComingSoonTab.test.tsx:13` renders `<ComingSoonTab name="MCP Servers" />` — a hint that this was always planned, never built.

**The ask:** Org admins and platform admins must be able to add MCP servers *for their users* — register a server once (with its transport, endpoint, and auth secrets), have it securely bound to the right workspaces, and have it automatically show up as available tools inside those workspaces' agents. Users should never handle the secrets.

This is the same problem class Epic 30 solved for LLM provider credentials, applied to MCP tool servers. The injection pipeline Epic 30 built is the foundation Epic 53 extends.

### Relationship to Epic 4 (do not confuse the two)

| | Epic 4 (shipped) | Epic 53 (this epic) |
|---|---|---|
| Direction | Platform **is** an MCP server | Platform **manages** external MCP servers |
| Who connects | External MCP clients connect *to* LLMS | Agents in workspaces connect *out* to MCP servers |
| Tool flow | LLMS exposes tools (workspace_create, etc.) | External servers expose tools *to* LLMS agents |
| Code location | `pkg/mcp/server.go`, `cmd/mcp/main.go` | New: `pkg/mcpservers/`, new handlers, new materializer applier |
| Config written | None (platform serves) | opencode `mcp` section in `agent-config.json` |

---

## Scope

### In scope

- **Actors who can add MCP servers:** platform admins (`AdminGuard`, platform-wide scope), org admins (`OrgAdminGuard`, per-org scope), and **individual users not in any org** whose `personal_mcp_servers` feature flag is enabled (`UserFeatureGuard`, own-workspace scope). Org members are deliberately excluded from user-scope (see D11).
- **Transports:** remote HTTP/SSE MCP servers (the enterprise norm: a company-hosted or vendor-hosted server). Local `stdio` MCP servers (a command run inside the workspace pod) are supported in the data model with documented runtime constraints.
- **Lifecycle:** register, list, update (rotate secrets, change endpoint), disable/enable, delete; auto-apply to all platform workspaces / all org workspaces / all of a user's own workspaces; explicit per-workspace bind/unbind; propagate changes to live workspaces via the existing reload path.
- **Security:** secret portions (env vars, auth headers) encrypted at rest — admin/org via the master-KEK-derived crypto, user-scope via the user's password-derived DEK (zero-knowledge); never returned by the API; injected into workspace pods through the existing `secrets.json` → `/sandbox-cfg` tmpfs channel; audit-logged.
- **Governance:** an org policy quota bounding the number of MCP servers per workspace (protects agent startup time and blast radius); a feature flag gating user-scope behind an enabled capability (D12). The flag layer is built here; how a user obtains the capability (billing or otherwise) is a separate concern.

### Out of scope (with rationale — see "Out of Scope" section)

A user-facing tool-allowlist policy, MCP-server marketplace/discovery, running MCP servers *as* platform-managed processes, and SAML/SCIM-style provisioning of MCP access. (User-scope itself is now **in** scope — see D9/D11/D12.)

---

## Actors & Roles

| Actor | Auth guard | Can do |
|---|---|---|
| Platform admin (`users.role='admin'`) | `AdminGuard` (`api/internal/middleware/admin_guard.go:14`) | Register/list/update/delete platform-wide MCP servers (auto-apply to **all** workspaces) |
| Org admin (`org_memberships.role='admin'`) | `OrgAdminGuard` (`api/internal/middleware/org_guard.go:54`) | Register/list/update/delete org-scoped MCP servers (auto-apply to **that org's** workspaces) |
| Org member | `OrgMemberGuard` | Read-only list of MCP servers available in their org; **cannot** register user-scope MCP servers (D11) |
| Individual user (no org membership, feature enabled) | authenticated + `UserFeatureGuard` (D12) | Register/list/update/delete **personal** MCP servers (auto-apply to **their own** workspaces only) |
| Individual user (no org, feature disabled) | authenticated | **Cannot** register user-scope MCP servers — `402` (feature not enabled for current plan) |
| End user (any) | authenticated | Benefit only: their workspace agents gain the tools. Users never see admin/org secrets. |

**The org-membership disqualification rule (D11) in one line:** if `GetUserOrgID(userID)` (`pg_org_store.go:801`) returns a non-empty org_id, the user is in an org and user-scope MCP CRUD is refused with `403` — the org admin (not the member) owns the tool surface. Individual-only users fall through to the entitlement gate (D12).

---

## Use Cases

**UC-1 — Platform-wide tooling (platform admin).** A platform operator wants every agent in the deployment to be able to search the company wiki. They register one "Company Wiki MCP" server (HTTP transport, URL + bearer header) once. Every workspace — existing and future — automatically gains the wiki tools. No user action required.

**UC-2 — Org-scoped tooling (org admin).** The "Payments" org admin registers a "Stripe MCP" server scoped to their org. Only workspaces in the Payments org gain Stripe tools. Other orgs are unaffected. Secrets never leave the Payments org's encrypted store.

**UC-3 — Rotating a compromised token.** A vendor discloses a token leak. The admin opens the MCP server record, pastes a new token, saves. The platform re-encrypts, re-injects into the K8s Secret, and triggers a session-aware reload on every affected live workspace. Agents reconnect with the new token within seconds. The old token is gone from disk (tmpfs) and from the K8s Secret.

**UC-4 — Emergency kill-switch.** An MCP server is found malicious. The admin disables (or deletes) it. The platform unbinds it and reloads affected workspaces; the tools vanish from the agents. Audit log shows who disabled what and when.

**UC-5 — Selective enablement.** An org admin wants a sensitive "Production DB MCP" available only on one specific "ops" workspace, not org-wide. They create the server with auto-apply **off** and explicitly bind it to that one workspace.

**UC-6 — Workspace boot.** A new workspace starts. At creation the controller/API seeds the workspace's MCP server bindings from the applicable auto-apply rules (platform + org). On pod boot the `materialize` subcommand renders all bound MCP servers into the opencode `mcp` section of `agent-config.json`. The agent connects to each at startup and the user sees the combined tool set.

**UC-7 — Agent uses the tools.** Inside a running workspace the agent invokes a tool exposed by an external MCP server (e.g. `github.create_issue`). opencode handles the MCP client connection; the platform's job ended at correct config injection. Tool results flow back into the agent's context.

**UC-8 — Org governance.** An org admin wants to cap tool sprawl. They set `max_mcp_servers_per_workspace = 5`. Binding attempts beyond 5 are rejected with a clear error.

**UC-9 — Individual power user (no org, feature enabled).** A solo developer not in any org wants their workspaces' agents to use a personal GitHub MCP server and a Notion MCP server. Their `personal_mcp_servers` feature flag is enabled (by plan tier, admin grant, or — when billing is wired — a purchase), so the "MCP Servers" tab is visible in their personal settings. They register both servers (secrets encrypted with their password-derived DEK — zero-knowledge, the platform cannot read them without their session). The servers auto-apply to all of their own workspaces. No other user is affected.

**UC-10 — Org member attempts personal MCP server (blocked).** A member of the "Payments" org tries to register a personal MCP server. The platform refuses: they are in an org, so the org admin — not the member — owns the tool surface (D11). The "MCP Servers" tab is hidden in their personal settings. If they attempt the API directly, they get `403`.

**UC-11 — Feature-disabled individual attempts MCP servers (capability-denied).** A solo developer whose `personal_mcp_servers` flag is not enabled opens their personal settings. The "MCP Servers" tab shows a "not available on your plan" prompt instead of the management UI (the frontend may render an upgrade path if billing context is available, but that is a frontend concern, not a feature-flag concern). Direct API attempts return `402` with `{feature, planId, hint}`.

---

## Workflows

### WF-1 — Register a platform MCP server (auto-applies to all workspaces)

```
Platform admin                API (stateless)                 PostgreSQL / K8s
     │                              │                                │
     │ 1. POST /admin/mcp-servers   │                                │
     │  {name, transport:http,      │                                │
     │   url, headers:{Auth:...},   │                                │
     │   autoApply:"all"}           │                                │
     │ ────────────────────────────►│                                │
     │                              │ 2. validate (transport, url,   │
     │                              │    SSRF, bounds) — US-53.4     │
     │                              │ 3. encrypt headers+env with    │
     │                              │    admin KEK ("provider-       │
     │                              │    credentials" purpose)       │
     │                              │ 4. INSERT mcp_servers          │
     │                              │    (owner_type='admin')        │ ──►
     │                              │ 5. INSERT mcp_server_auto_apply│ ──►
     │                              │    (target_type='all')         │
     │                              │ 6. backfill: bind to all       │
     │                              │    existing workspaces         │ ──►
     │                              │ 7. for each live workspace:    │
     │                              │    rewrite K8s Secret + queue  │
     │                              │    agent reload (US-53.6)      │
     │ 8. 201 {id, name, ...}       │                                │
     │  (no secrets in response)    │                                │
     │ ◄────────────────────────────│                                │
```

### WF-2 — Register an org MCP server (auto-applies to org workspaces)

Identical to WF-1 but: route `POST /orgs/:id/mcp-servers` behind `OrgAdminGuard`; `owner_type='org'`, encrypted with the org KEK (`"org-credentials"` purpose); auto-apply `target_type='org'`; backfill scoped to that org's workspaces.

### WF-3 — Workspace boot materializes MCP servers (the critical integration path)

```
API (workspace create)      controller (reconcile)        workspace pod
       │                            │                            │
       │ SeedWorkspaceMCPServers    │                            │
       │  → mcp_server_bindings     │                            │
       │  (auto + explicit) ──────► PostgreSQL                   │
       │                            │                            │
       │ PrepareSecretsForInjection │                            │
       │  merges LLM creds + MCP    │                            │
       │  servers into secrets.json │                            │
       │  (new SecretType "mcp-     │                            │
       │   server", US-53.7)        │                            │
       │  → K8s Secret              │                            │
       │    workspace-secrets-<id>  │ ──────────────────────────► │
       │                            │                            │
       │                            │   init: credential-setup    │
       │                            │   cp secrets.json →         │
       │                            │   /sandbox-cfg (tmpfs)  ──► │
       │                            │                            │
       │                            │   materialize subcommand:   │
       │                            │   read secrets.json ──────► │
       │                            │   applyMCPServer stages     │
       │                            │   env/headers (US-53.8)     │
       │                            │   AgentConfigWriter.rebuild │
       │                            │   merges 4 sources:         │
       │                            │   providers+model+relay+mcp │
       │                            │   → /tmp/agent-config.json  │
       │                            │                            │
       │                            │   opencode serve starts,    │
       │                            │   reads agent-config.json,  │
       │                            │   connects to each MCP ───► │ external
       │                            │   server, registers tools   │  MCP servers
```

### WF-4 — Update (rotate token / change endpoint)

```
admin → PUT /admin/mcp-servers/:id (or /orgs/:id/mcp-servers/:id)
  → re-encrypt changed secret portions (empty field = keep existing, per OrgSSOTab pattern)
  → UPDATE mcp_servers
  → for each bound live workspace:
       rewrite secrets.json in K8s Secret
       POST /v1/reload-secrets on agentd (session-aware restart, cmd/workspace-agentd/secrets.go:557-709)
  → opencode restarts, re-reads agent-config.json, reconnects with new config
```

### WF-5 — Disable / delete (kill-switch)

```
admin → DELETE /admin/mcp-servers/:id  (or PATCH {enabled:false})
  → delete mcp_server_bindings rows (or skip disabled)
  → delete mcp_servers row (CASCADE removes auto_apply + bindings)
  → for each formerly-bound live workspace: rewrite Secret + reload
  → agent restarts; tools vanish
  → audit_log row: {actor, action:'mcp_server.delete', target_id, ts}
```

### WF-6 — Explicit per-workspace bind (selective enablement)

```
admin → POST /orgs/:id/mcp-servers/:serverId/bindings  {workspaceId}
  → enforce quota (org policy max_mcp_servers_per_workspace) — US-53.11
  → INSERT mcp_server_bindings (source_type='explicit')
  → rewrite Secret + reload that one workspace
```

### WF-7 — Governance quota check

At every bind path (seed-at-create, explicit bind, backfill), count the workspace's resulting MCP servers and reject with `409` if it would exceed `max_mcp_servers_per_workspace` for the owning org (platform-admin servers are exempt from org quotas — they are platform policy, set globally if needed).

---

## Validated Assumptions

Per README-LLM.md Rule 7, every assumption is listed with its validation status. Items marked **⚠ VALIDATION REQUIRED** are blocking unknowns that US-53.1 (the spike) must close before dependent stories start.

| # | Assumption | Verified At | Result |
|---|---|---|---|
| A1 | The platform has no existing external-MCP-server / tool-registry concept | grep `*.go` for `tool_registry\|ExternalTool\|mcp_client\|McpServer\|external_mcp` | **Confirmed** — zero production hits |
| A2 | `pkg/mcp/server.go` is the platform's *own* MCP server (LLMS-as-server), not a client | read of `pkg/mcp/server.go:18-45` | **Confirmed** — inverse direction; Epic 53 is greenfield |
| A3 | The existing injection pipeline (Epic 30) is the correct extension point | `pkg/secrets/injection.go:44-138` (`PrepareSecretsForInjection`), `pkg/secrets/types.go:24-37` (`SecretType`) | **Confirmed** — pipeline already dispatches by `SecretType`; adding `mcp-server` is the natural seam |
| A4 | `AgentConfigWriter` is the sole in-process writer of `agent-config.json`, holding 3 sources (providers, model, relay) | `cmd/workspace-agentd/agent_config_writer.go:56-62,152-200`; README-LLM.md §"Relay Config Subsystem" | **Confirmed** — MCP becomes a 4th source; no parallel write path |
| A5 | opencode reads `agent-config.json` only at startup (no hot-reload); config changes require a session-aware restart | README-LLM.md:429; `cmd/workspace-agentd/secrets.go:557-709` (`/v1/reload-secrets`) | **Confirmed** — reload path already exists and is reused |
| A6 | `opencode` supports configuring external MCP servers in its config (an `mcp`/`mcpServers` section) | grep of repo: no `mcp` section emitted anywhere; `opencode.json` has none | **⚠ VALIDATION REQUIRED (US-53.1)** — must be confirmed against the **pinned runtime opencode version** and the exact section key/shape captured. This is the single highest-risk unknown. |
| A7 | The opencode config supports `{env:VAR}` interpolation (so secret env can be referenced rather than inlined) | `opencode.json:11` (`"baseURL": "{env:OPENAI_API_BASE}"`) | **Confirmed** for provider options; **⚠ confirm for the mcp section** in US-53.1 |
| A8 | Admin/org credential encryption uses master-KEK-derived keys with purpose strings `"provider-credentials"` (admin) and `"org-credentials"` (org) | `design/stories/epic-50-master-kek-hardening/README.md:28`; `pkg/secrets/injection.go:154-186` | **Confirmed** — MCP server tokens are the same threat class; reuse these purposes (see D3) |
| A9 | `provider_credentials` stores LLM-specific shapes (`LLMProviderData`); overloading it for MCP servers would violate type safety | `pkg/secrets/types.go:212-219`; `pkg/secrets/injection.go:154-192` (`decryptBinding` switch on owner type) | **Confirmed** — a dedicated table is correct (D1) |
| A10 | The existing `credential_auto_apply` table FKs to `provider_credentials`; it cannot host MCP rules without a polymorphic FK | `api/migrations/000015_unified_credential_model.up.sql:49-55` | **Confirmed** — dedicated `mcp_server_auto_apply` is required (D2) |
| A11 | There is no generic `org_settings` table by design; each org concern gets a dedicated normalized table | README-LLM.md:1499; `org_policies`, `org_sso_configs` | **Confirmed** — MCP servers follow the same dedicated-table convention |
| A12 | The platform returns 404 (not 403) from `AdminGuard` to hide route existence | `api/internal/middleware/admin_guard.go:14-23` | **Confirmed** — admin MCP routes inherit this behaviour |
| A13 | `/sandbox-cfg` is a memory-backed tmpfs; plaintext secrets never touch node disk | `controller/internal/workspace/pod_builder.go:152-155` | **Confirmed** — MCP secret env/headers land here, same as LLM keys |
| A14 | `AdminProviderCredentialsTab.tsx` (~729 lines) and `OrgCredentialsTab.tsx` (~421 lines) are the right UI templates (write-only secret fields, eye-toggle, `hasSecret` boolean, partial-update send) | frontend explore report | **Confirmed** — clone-and-adapt pattern |
| A15 | The latest migration is `000041`; new migrations start at `000042` | `api/migrations/` listing | **Confirmed** |
| A16 | Remote MCP servers require network egress from workspace pods; the platform's network policy design governs this | `design/archive/v1/0020_2025-03-05_network.md`; chart NetPols | **Confirmed** — egress allow-listing is an operator concern; documented in US-53.4 validation |
| A17 | The `redact` binary (16-rule pipeline) may not cover arbitrary MCP-server token formats; agent output could echo a custom token | README-LLM.md tech-stack row "Secret redaction" | **⚠ VALIDATION REQUIRED (US-53.1)** — determine whether MCP env-var values can leak via tool results, and whether additional redaction rules or env-var-hiding are needed |
| A18 | User-scope credentials are decrypted via the user's session DEK (`decryptBinding` `case "user"`, `s.keys.GetDEK(ctx, sessionID)`); the injection pipeline already supports this for LLM credentials | `pkg/secrets/injection.go:157-165`; Epic 50 risk-matrix test `TestPrepareSecretsForInjection_AdminOnly_NoUserSession` | **Confirmed** — user-scope MCP servers inherit the same constraint (injectable only with an active session); no new mechanism |
| A19 | `users.plan_id` exists (`TEXT NOT NULL DEFAULT 'free'`) and carries the individual user's plan tier; org plans are a separate column on `organizations` | `api/migrations/000026_usage_limits.up.sql:17`; `api/migrations/000030_organizations_status.up.sql:28` | **Confirmed** — user-scope entitlement reads `users.plan_id`, not the org plan |
| A20 | A feature-flag layer exists (`PlanFeatures` struct + `IsFeatureAllowed(plan, feature)`) that maps plan tier → boolean capability flags; it is org-scoped today (reads `org.PlanID` via `:id` path param) and contains zero Stripe/payment code (that lives in the separate `stripe_provider.go`/`webhook.go`/`org_billing.go`) | `api/internal/middleware/feature_guard.go:36-64`; `pkg/billing/plan_tiers.go:9-71` (flag layer); `pkg/billing/stripe_provider.go`, `api/internal/handlers/org_billing.go` (billing layer — not touched) | **Confirmed** — D12 adds a `PersonalMcpServers` flag + a parallel `UserFeatureGuard` using the flag layer; billing is a separate concern with a clean integration point at `users.plan_id` |
| A21 | `GetUserOrgID(userID)` returns the user's org_id or `""` when the user is in no org — the exact "is this user in an org" check D11 needs | `api/internal/services/database/pg_org_store.go:801-814` | **Confirmed** — single indexed query; the disqualification gate is one cheap call |

---

## Design Decisions

### D1 — Dedicated `mcp_servers` table, NOT an overload of `provider_credentials`

MCP server config is a genuinely different domain entity from an LLM provider credential: it carries transport (`stdio`/`http`/`sse`), endpoint (`url` or `command`+`args`), a map of env vars, and a map of headers — none of which fit `LLMProviderData` (`{provider, apiKey, baseURL, models, default, smallModel}`, `pkg/secrets/types.go:212-219`). Overloading `provider_credentials.ciphertext` with a discriminator would force every consumer (`decryptBinding` switch at `injection.go:154-192`, the materializer, the formatter) to branch on an untyped tag — a Rule 1 (type safety) and Rule 4 (right-sized complexity) violation. A dedicated table mirrors the codebase's own convention (`org_sso_configs` is dedicated, not crammed into `provider_credentials`) and keeps `decryptBinding` clean. The *patterns* (owner_type discriminator, encrypted ciphertext, bindings, auto-apply) are reused; the *storage* is separate.

### D2 — Dedicated binding + auto-apply tables; no polymorphic FKs

`workspace_credential_bindings` and `credential_auto_apply` FK to `provider_credentials(id)`. Reusing them for MCP servers would require either a polymorphic FK (no real referential integrity) or a `credential_kind` discriminator column (complex, fragile). Three cohesive tables in one migration (`000042`): `mcp_servers`, `mcp_server_bindings`, `mcp_server_auto_apply`. This is table-count honest with the domain (three distinct concerns) and matches the proven Epic 30 shape.

### D3 — Reuse the existing admin/org encryption purposes; do NOT introduce parallel crypto

MCP server tokens (env vars, auth headers) are the same threat class as LLM provider API keys: operator-supplied secrets that the platform must decrypt at injection time. They reuse the existing master-KEK-derived purposes — `"provider-credentials"` for `owner_type='admin'`, `"org-credentials"` for `owner_type='org'` — via the same `RootKeyProvider` (post-Epic-50) / `AdminKeyDeriver` (pre-Epic-50) path. Introducing a parallel `"mcp-credentials"` purpose would recreate the exact two-layer problem Epic 50 exists to eliminate. **If Epic 50 ships first**, MCP servers benefit from its unification + audit logging automatically with zero extra work. **If Epic 53 ships first**, it uses the current `deriveServerKey` mechanism and Epic 50's US-50.2 sweeps it into the unified interface. Either ordering is safe.

### D4 — MCP servers compose additively; bindings carry NO precedence

Unlike LLM providers (two Anthropic keys compete; `within_priority` + `source_type='explicit'` decide the winner), multiple MCP servers are **all** injected — an agent can use GitHub tools *and* Slack tools simultaneously. Therefore `mcp_server_bindings` is a pure join table `(workspace_id, server_id, source_type)` with **no** `within_priority` column. This is simpler than the credential bindings and correct for the additive composition model. Disabled servers (`enabled=false`) are skipped at injection, not deleted.

### D5 — Plaintext columns for display fields; ciphertext for secrets only

Listing MCP servers in the admin UI must not require decryption (listing N servers should be O(1) decrypts, not O(N)). So display/structural fields are plaintext columns: `name`, `transport`, `url`, `command`, `args` (JSONB), `enabled`, `owner_type`, `owner_id`, timestamps. Only the genuinely secret portions — the `env` map and the `headers` map — go in the encrypted `ciphertext` blob. This mirrors how `provider_credentials` keeps `name`/`provider`/`model_allowlist` plaintext and only `ciphertext` is encrypted, and it makes the UI fast and the secrets minimal.

### D6 — Remote transports first (HTTP/SSE); stdio supported in the model with a runtime caveat

The enterprise norm is a *remote* MCP server (company-hosted or vendor-hosted URL). Remote servers need only egress allow-listing (an operator concern, A16) — no runtime-image change. `stdio` servers (a command spawned inside the workspace pod, e.g. `npx -y @modelcontextprotocol/server-github`) are powerful but require the binary/runtime to be present in the workspace image and obey the pod's egress policy. The **data model and API support all three transports** (`http`, `sse`, `stdio`) so the abstraction is faithful to opencode, but US-53.12 (e2e) validates with a **remote** server, and the stdio path is documented as "requires runtime coordination" rather than built out with image changes in this epic. This avoids over-building while not artificially capping the abstraction.

### D7 — The spike (US-53.1) closes the opencode assumption BEFORE any dependent code

A6 ("opencode supports an mcp config section") is the load-bearing assumption of the entire epic. If it is false, or the section shape differs from expectations, the materializer contract (US-53.8) changes. Per Rule 7, an unvalidated assumption must not be built on. US-53.1 produces a validated contract (exact JSON shape, interpolation behaviour, secret-handling) as its deliverable; stories US-53.7/53.8 consume that contract. The spike also closes A7 (env interpolation in the mcp section) and A17 (redaction of MCP tokens). No code that renders opencode config merges before US-53.1 is accepted.

### D8 — Reload propagation reuses the existing session-aware restart path

Changing an MCP server (token rotation, disable) must reach live workspaces. The platform already has this machinery for credentials: rewrite the K8s Secret, then `POST /v1/reload-secrets` to agentd (`cmd/workspace-agentd/secrets.go:557-709`), which re-materializes and triggers a session-aware opencode restart. MCP servers use the **same** path — US-53.6 wires MCP server mutations into the existing reload trigger, not a new one. One reload mechanism, one restart semantics, one set of failure modes.

### D9 — User-scope is IN scope, with two gates: org-membership disqualifies, and a feature flag must be enabled

User-scope MCP servers are a first-class part of this epic — not deferred. Two product rules govern who qualifies (the rules are the design; the mechanisms are D11 and D12):

1. **Org members cannot** register user-scope servers (D11). Once you join an org, the org admin owns your tool surface.
2. **Individual users must have the feature flag enabled** to register user-scope servers (D12). The flag is a capability gate, distinct from how the user obtained it (billing or otherwise).

The technical feasibility hinges on three pre-existing capabilities, all validated during planning (A18–A20): the user-DEK encryption path, the `users.plan_id` column, and the existing feature-flag layer (`PlanFeatures`/`IsFeatureAllowed`). None of these needed to be invented — user-scope is an additive extension of the same patterns, not a new subsystem. The schema's `owner_type` CHECK includes `'user'` from day one (no forward-compat-only migration).

### D10 — No user-facing tool allowlist in this epic; quota is the governance control

An org policy `allowed_mcp_servers` allowlist is a plausible governance feature but presumes a UX for selecting servers and a policy-evaluation point in the injection path. It is deferred (see Out of Scope). The shipped governance controls are: the org-membership disqualification (D11), the feature-flag gate (D12), and a single org policy `max_mcp_servers_per_workspace` bounding agent-startup cost and blast radius. Right-sized: clear rules, enforced at distinct gates, no new evaluation engine.

### D11 — Org membership disqualifies user-scope MCP servers (governance)

The asymmetry is deliberate and threat-model-driven. Org members *can* manage personal **LLM provider credentials** (which model answers their prompts — preference/cost, low exfiltration risk) but *cannot* manage personal **MCP servers** (which external systems their agent can call — high data-exfiltration risk: a malicious MCP server receives whatever the agent sends it, including code, in-context secrets, and workspace files). An org admin is accountable for the org's data-egress surface; letting members add arbitrary MCP servers would let them bypass org egress controls and audit. So org membership transfers ownership of the *tool* surface to the admin while leaving the *model-preference* surface with the member.

**Revised gate (2026-08-01):** the org-membership check is retained, but it now consults an **org-admin-controlled policy** rather than being an unconditional hard lockout. The user-scope CRUD handler calls `GetUserOrgID(userID)` (`pg_org_store.go:866`); a non-empty result → consult the org's `allow_user_mcp_servers` policy. When the policy is unset or false (the default — locked), the request is refused with `403`. When the org admin sets it to true, members CAN register personal MCP servers. This mirrors the proven `allow_user_prompt` pattern exactly (same policy table, same default-locked semantics, same resolution path).

### D12 — Solo-user user-scope is bounded by a plan-tier registration quota

**Revised (2026-08-01):** the original boolean `PersonalMcpServers` flag is replaced by an integer quota field `MaxPersonalMcpServers` on `PlanFeatures` (`pkg/billing/plan_tiers.go`). The semantics:

| Value | Meaning |
|---|---|
| `0`  | Disabled — solo users on this plan cannot register personal MCP servers (`402`). |
| `-1` | Unlimited. |
| `N>0` | At most N personal MCP servers may be registered. |

Tier mapping (the four existing `OrgPlan` constants; there is no `basic` tier):

| Plan | `MaxPersonalMcpServers` |
|---|---|
| `free`       | `5`  |
| `team`       | `-1` (unlimited) |
| `business`   | `-1` (unlimited) |
| `enterprise` | `-1` (unlimited) |

The feature-flag layer (`PlanFeatures`/`GetPlanFeatures`) remains the authoritative "is the capability enabled, and to what ceiling?" source — it is plan-driven and contains no billing code. The separation of concerns from the original D12 rationale is preserved: billing (Stripe, Epic 12) is a *writer* of `users.plan_id`; the flag layer *reads* it. A deployment with no Stripe configured can still gate the feature by setting `users.plan_id` directly.

`IsFeatureAllowed(plan, "personal_mcp_servers")` returns `true` when `MaxPersonalMcpServers != 0` (so the `UserFeatureGuard` 402 path fires only for fully-disabled plans). The quota ceiling is enforced separately at Create time by counting the caller's existing user-scope servers.

### D13 — User-scope encryption uses the user password-derived DEK (zero-knowledge), requiring an active session

User-scope MCP server secrets are the same threat class as user LLM credentials: user-owned, must be unreadable by the platform absent the user. They reuse the **exact** user-DEK path the credential pipeline already implements — `decryptBinding` `case "user"` (`injection.go:157-165`) obtains the session DEK via `s.keys.GetDEK(ctx, sessionID)` and decrypts with `DecryptSecret(dek, ct)`. Consequences, all inherited from the existing user-credential behaviour (no new constraint):

- User-scope MCP servers can only be **injected** when the user has an active session (the `sessionID` is required to derive the DEK). A controller-initiated workspace boot with no session injects admin + org servers only — identical to how user LLM credentials behave today (validated by Epic 50's `TestPrepareSecretsForInjection_AdminOnly_NoUserSession` risk-matrix test).
- User-scope ciphertext is unrecoverable if the user forgets their password (no server-side key) — same as user secrets and user LLM keys. Documented, not a new risk.
- The store layer's user-scope CRUD takes a `sessionID` and obtains the DEK before encrypt/decrypt — mirroring the user-credential store path.

---

## Data Model (preview — final shapes land in US-53.2)

```
mcp_servers
  id              UUID PK
  owner_type      TEXT CHECK IN ('admin','org','user')     -- D9: all three in scope
  owner_id        TEXT                                     -- '_platform' | org UUID | user UUID
  name            TEXT NOT NULL
  transport       TEXT CHECK IN ('http','sse','stdio')    -- D6
  url             TEXT                                     -- remote transports
  command         TEXT                                     -- stdio transport
  args            JSONB                                    -- stdio args array
  ciphertext      BYTEA NOT NULL                           -- encrypted {env, headers}
                                                          --  admin/org: master-KEK purpose key (D3)
                                                          --  user: user password-derived DEK (D13)
  key_version     INTEGER NOT NULL DEFAULT 1               -- Epic-50-compatible (admin/org); user rows stay at 1
  enabled         BOOLEAN NOT NULL DEFAULT true
  created_at, updated_at TIMESTAMPTZ
  UNIQUE(owner_type, owner_id, name)

mcp_server_bindings                       -- D4: pure join, no priority
  workspace_id   UUID FK→workspaces(id) ON DELETE CASCADE
  server_id      UUID FK→mcp_servers(id) ON DELETE CASCADE
  source_type    TEXT CHECK IN ('explicit','auto')
  PRIMARY KEY(workspace_id, server_id)

mcp_server_auto_apply                     -- mirrors credential_auto_apply
  server_id      UUID FK→mcp_servers(id) ON DELETE CASCADE
  target_type    TEXT CHECK IN ('all','org','user')   -- 'all'=platform, 'org'=org, 'user'=individual
  target_id      TEXT                            -- NULL for 'all', org UUID for 'org', user UUID for 'user'
  PRIMARY KEY(server_id, target_type, target_id)
```

Go domain types land in `pkg/types/mcp.go` (DTOs) and reuse `pkg/secrets` crypto. **No CRD is introduced** — MCP servers are API-owned relational data, exactly like provider credentials and SSO configs (they are not Kubernetes resources; they do not need reconciliation).

---

## Non-Functional Requirements

### Security (the dominant concern)

| Threat | Control |
|---|---|
| Secret exfiltration via the API | Secret portions (env/headers) are `json:"-"` on response DTOs; only a `hasSecret: boolean` surfaces (mirror `OrgSSOConfigResponse`, `pkg/types/orgs.go:189`). List endpoints never decrypt. |
| Secret at rest | Encrypted with master-KEK-derived key (D3); `ciphertext BYTEA`; `key_version` for future rotation (Epic 50). |
| Secret on node disk | Never — injected into `/sandbox-cfg` tmpfs (`pod_builder.go:152-155`); K8s Secret `workspace-secrets-<id>` is the only durable channel. |
| Secret in logs | Structured-logger sensitive-data filtering (`go.uber.org/zap`); ciphertext blob and env/header maps never logged. Audit log stores actor + action + target id — never secret bytes. |
| Cross-org access | Org MCP servers scoped by `owner_id = org_id`; `OrgAdminGuard` enforces; queries always filter by org. A Payments org admin cannot see/read/inject a Billing org server. |
| Org member bypassing the tool-surface rule | User-scope CRUD handler calls `GetUserOrgID(userID)` (`pg_org_store.go:801`); non-empty org_id → `403` before any mutation (D11). |
| Feature-flag bypass (user-scope disabled) | `UserFeatureGuard` reads the caller's plan and checks the `PersonalMcpServers` flag via `IsFeatureAllowed`; flag off → `402` (D12). Gate is on every mutating user-scope route. The flag is the source of truth — no billing dependency in the gate path. |
| User-scope secret readable by platform | User-scope ciphertext encrypted with the user password-derived DEK (D13); platform has no key absent the user's session. Same zero-knowledge property as user secrets / user LLM credentials. |
| Supply-chain (malicious admin-registered server) | Admin trust boundary — admins are privileged. Mitigations: full audit log of every CRUD + inject; `enabled` kill-switch (WF-5); operator can set platform policy to disable org-admin MCP registration entirely (config flag, US-53.11). |
| Prompt injection via tool results | Inherent MCP risk, not platform-caused. Documented as operator advisory. The agent's existing permission model (`opencode.json:20-32`) governs tool execution. |
| SSRF via remote URL | URL validation in US-53.4: scheme allow-list (`https` default, `http` opt-in), block link-local/loopback/metadata endpoints at registration (defense-in-depth; pod egress policy is the primary control). |
| stdio command injection | `command`/`args` stored as data, rendered into opencode config opaquely (never shell-interpolated by the platform). The platform does not execute the command — opencode does, as the sandbox user. Documented threat. |
| Token leak via agent output | A17 — spike determines whether the `redact` pipeline covers MCP token formats; if not, add targeted rules or env-var-hiding before GA. |

### Scalability & Performance

| Concern | Approach |
|---|---|
| Listing MCP servers | Indexed on `(owner_type, owner_id)`; paginated; no decryption on list. |
| N MCP servers per workspace | Additive injection into one `secrets.json` and one `agent-config.json`. Bounded by org quota (`max_mcp_servers_per_workspace`, default 5). Each adds an opencode startup connection — the quota is the performance guardrail. |
| Reload fan-out on platform-wide change | A platform server update affects all workspaces. US-53.6 uses the existing bounded-parallelism bulk-reload pattern (`agent_reload.go:431-454`, `maxParallel=5`) — no unbounded goroutine fan-out. |
| Backfill on auto-apply create/change | Bounded, resumable, in batches (mirror `credential_backfill_jobs`, migration `000015:71-79`). |
| Hot-path cost | Injection (`PrepareSecretsForInjection`) gains one extra store query (MCP bindings) per workspace boot — O(number of bound servers), indexed. Negligible vs the existing credential decrypt cost. |

### Robustness

| Concern | Approach |
|---|---|
| One MCP server unreachable at boot | opencode handles per-server failure gracefully (marks failed, continues). The platform must NOT fail workspace boot if one server is down — injection writes config regardless of reachability; connection is the agent's concern. |
| Partial reload failure | One workspace failing reload must not block others; per-workspace error isolation, logged + metric'd, existing pattern. |
| Concurrent admin edits | Row-level: `UPDATE ... SET ... WHERE id=$ AND updated_at=$` optimistic-concurrency on update (mirror provider-credential update path), returning fresh row; 409 on conflict. |
| K8s Secret rewrite race | Existing `MergeSecretsManifest`/`EnsureSecretsManifest` (`workspace_service.go:1455,1516`) already handle this; MCP joins the same write path. |
| Orphaned bindings | `ON DELETE CASCADE` on `mcp_server_bindings.server_id`; deleting a server cleans bindings; reload un-injects. |

---

## Stories

| Story | Title | Effort | Depends On |
|---|---|---|---|
| US-53.1 | Validation spike: opencode external-MCP config contract | 1d | None (BLOCKING — closes A6/A7/A17) |
| US-53.2 | Data model: migrations + Go domain/transfer types (all three owner_types) | 0.5d | US-53.1 (for the materialization contract) |
| US-53.3 | Storage layer: MCP server store (CRUD + crypto for admin/org/user + bindings + auto-apply seeding) | 2d | US-53.2 |
| US-53.4 | Admin (platform) CRUD handler + routes + validation | 1d | US-53.3 |
| US-53.5 | Org admin CRUD handler + routes + validation | 1d | US-53.3 |
| US-53.5b | User-scope CRUD handler + routes + org-disqualification gate + feature-flag gate | 1.5d | US-53.3, US-53.11b |
| US-53.6 | Binding lifecycle: seed-at-create (admin/org/user), explicit bind/unbind, backfill, reload fan-out | 1.5d | US-53.3 |
| US-53.7 | Injection (API side): new `SecretType "mcp-server"` in `secrets.json` (admin/org/user decrypt paths) | 1d | US-53.1, US-53.3 |
| US-53.8 | Materialization (pod side): applier + `AgentConfigWriter` mcp source + reload integration | 1.5d | US-53.1, US-53.7 |
| US-53.9 | Frontend: platform-admin MCP servers tab | 1.5d | US-53.4 |
| US-53.10 | Frontend: org-admin MCP servers tab | 1.5d | US-53.5 |
| US-53.10b | Frontend: user MCP servers tab (visibility-gated: no-org + feature-enabled; capability-denied state when disabled) | 1.5d | US-53.5b |
| US-53.11 | Governance + observability: org quota policy, audit logging, metrics | 1d | US-53.3, US-53.6 |
| US-53.11b | Feature flag: `PersonalMcpServers` capability flag + `UserFeatureGuard` middleware | 0.5d | None (foundational for US-53.5b) |
| US-53.12 | E2E integration: full wired path across all three scopes (admin/org/user), gates, and the kill-switch | 2d | US-53.6, US-53.8, US-53.9, US-53.10, US-53.10b |

Total estimated effort: ~19.5 days.

---

## Dependency Graph

```
US-53.1 (spike: opencode mcp contract)   ─── BLOCKING, can start immediately
US-53.11b (feature flag + UserFeatureGuard) ─── can start immediately (foundational for user-scope)
   │
   ├──> US-53.2 (data model)
   │       │
   │       └──> US-53.3 (store + crypto + seeding, all owner types)
   │               ├──> US-53.4 (admin CRUD)  ──> US-53.9 (platform UI)
   │               ├──> US-53.5 (org CRUD)    ──> US-53.10 (org UI)
   │               ├──> US-53.5b (user CRUD + gates) ──> US-53.10b (user UI)
   │               ├──> US-53.6 (binding lifecycle + reload fan-out)
   │               └──> US-53.11 (quota + audit + metrics)
   │
   ├──> US-53.7 (injection: API side)  ─── also depends on US-53.3
   │       │
   │       └──> US-53.8 (materialization: pod side)  ─── also depends on US-53.1
   │
   └──> US-53.12 (e2e) ─── depends on US-53.6, US-53.8, US-53.9, US-53.10, US-53.10b
```

Two stories have no dependencies and can start on day 1: US-53.1 (the blocking spike) and US-53.11b (the feature flag, foundational for the entire user-scope track). US-53.2 cannot start until the materialization contract is known.

---

## Execution Strategy

**Phase 0 — De-risk (day 1):** US-53.1 spike (closes A6/A7/A17) **and** US-53.11b (feature flag + `UserFeatureGuard`) in parallel — both are dependency-free. The spike de-risks the opencode contract; the flag story de-risks the capability dimension and unblocks the user-scope track. **No other story merges before US-53.1 is accepted.**

**Phase 1 — Foundation (days 2–4):** US-53.2 (data model, all three owner_types) → US-53.3 (store, with the user-DEK path). Backend-only, fully tested, no UI.

**Phase 2 — Surfaces (days 4–8):** US-53.4 (admin CRUD), US-53.5 (org CRUD), US-53.5b (user CRUD + both gates), US-53.6 (binding lifecycle), US-53.11 (governance + audit + metrics) in parallel after US-53.3. End of Phase 2: all three actor scopes are fully manageable via the API; workspaces get seeded across all scopes.

**Phase 3 — The wire (days 8–11):** US-53.7 (injection, all three decrypt paths) → US-53.8 (materialization). End of Phase 3: a registered MCP server in any scope actually appears as tools inside a running agent.

**Phase 4 — UX + closure (days 11–15):** US-53.9 (platform UI), US-53.10 (org UI), US-53.10b (user UI with visibility-gating + capability-denied state) in parallel; US-53.12 (e2e across all scopes + both gates) consumes all of it. End of Phase 4: the full human-workable, end-to-end-verified feature.

Each phase ends with `make test && make build && make lint` green and a worklog entry. No phase skips the validator loop (README-LLM.md Multi-Agent Workflow).

---

## Per-Story Detail

### US-53.1: Validation spike — opencode external-MCP config contract (BLOCKING)

**Goal:** Convert assumptions A6, A7, A17 from "believed" to "validated with evidence," and produce the exact materialization contract that US-53.7/53.8 implement against. This is a research + contract artifact, not production code.

**Deliverables (written, in this epic's folder):**
1. `MATERIALIZE-CONTRACT.md` — the exact JSON shape opencode expects for external MCP servers in its config, validated **against the pinned runtime opencode version** in `runtimes/`. For each transport (`http`, `sse`, `stdio`): the section key, the per-server object fields, and a known-good minimal example captured from a real opencode run.
2. Confirmation of `{env:VAR}` interpolation behaviour inside the mcp section (A7) — does opencode interpolate env refs in `url`/`headers`/`command`/`args`, and if so how are secrets best supplied (inline vs env-var reference)?
3. A redaction assessment (A17): run a representative MCP server whose tool result echoes a secret env var; determine whether agent output can leak it; recommend whether new `redact` rules or env-var-hiding are required.
4. The `SecretType "mcp-server"` payload schema for `secrets.json` — the exact `{type, name, metadata, plaintext}` shape the materializer will consume (metadata = transport/url/command/args; plaintext = the env+headers JSON). This becomes the contract US-53.7 emits and US-53.8 parses.

**Validation evidence required:** captured `agent-config.json` snippet from a live opencode run that actually connected to an external MCP server and exposed its tools. "It should work" is not acceptable (Rule 7).

**Acceptance criteria:**
- A live workspace, given a hand-written `mcp` section, connects to a real external MCP server (HTTP transport — e.g. a local test MCP server the spike stands up) and the server's tools appear in the agent.
- `MATERIALIZE-CONTRACT.md` exists, is concrete (real JSON, not "TBD"), and is reviewed by a second reader.
- A17 finding documented with a recommendation (redact rule additions or env-hiding) — even if the finding is "no action needed," the evidence is recorded.

**Tests (TDD):** N/A (spike). The live-connection demonstration IS the test.

### US-53.2: Data model — migrations + Go domain/transfer types (all three owner_types)

**Goal:** Land the schema and types. No behaviour yet.

**Files:**
- `api/migrations/000042_mcp_servers.up.sql` / `.down.sql` — three tables per the Data Model preview; `owner_type` CHECK includes `'user'` (D9) from day one; `mcp_server_auto_apply.target_type` CHECK includes `'user'`; indexes on `(owner_type, owner_id)` and `mcp_server_bindings(workspace_id)`; partial unique index on `mcp_server_auto_apply` for NULL `target_id` (same pattern as `credential_auto_apply`, migration `000015:58-68`).
- `pkg/types/mcp.go` (new) — `MCPServer` (DB shape, `Env`/`Headers` inside `Ciphertext []byte`, `json:"-"`), `MCPServerResponse` (API shape, `HasSecret bool`), `CreateMCPServerRequest`, `UpdateMCPServerRequest` (secret fields omitempty for partial update), `MCPServerTransport` constants, `MCPServerOwnerType` constants (`"admin"|"org"|"user"`), `MCPServerBinding`, `MCPServerAutoApplyRule`.
- Mirror the field discipline of `pkg/types/orgs.go:174-215` (SSO DTOs) — that is the closest existing analog.

**Acceptance criteria:**
- Migration applies idempotently and rolls back cleanly on a kind cluster.
- Down migration drops all three tables with no orphaned FKs.
- Types compile; `Env`/`Headers`/`Ciphertext` are `json:"-"` on response shapes.
- The CHECK constraints accept all three owner_types and all three auto-apply target_types.

**Tests (TDD):** migration idempotency + rollback test (existing pattern); struct JSON-tag test asserting secrets never serialize; constraint tests inserting one row per owner_type.

### US-53.3: Storage layer — MCP server store (CRUD + crypto for admin/org/user + bindings + auto-apply seeding)

**Goal:** The data-access layer the handlers and injection pipeline call. Handles all three owner types with their distinct encryption paths.

**Files:**
- `pkg/secrets/mcp_store.go` (new) — `McpServerStore` interface + `PgMcpServerStore`: `Create/Get/List/Update/Delete`, `BindToWorkspace/UnbindFromWorkspace`, `CreateAutoApply/ListAutoApply/DeleteAutoApply`, `GetWorkspaceMCPServers(workspaceID)` (returns bound + enabled servers, ordered for deterministic injection), `SeedWorkspaceMCPServers(workspaceID, userID, orgID)` (mirrors `SeedWorkspaceCredentials`, `pg_credential_store.go:84-141`), `BackfillAutoApply(serverID)`.
- **Crypto, three paths (D3 + D13):**
  - `owner_type='admin'` → encrypt with the `"provider-credentials"` purpose key.
  - `owner_type='org'` → encrypt with the `"org-credentials"` purpose key.
  - `owner_type='user'` → encrypt with the user's session DEK (`keys.GetDEK(ctx, sessionID)`); the user-scope CRUD methods take a `sessionID` parameter, exactly as the user-credential store does.
  - Reuse `EncryptSecret`/`DecryptSecret` (pre-Epic-50) or `RootKeyProvider` (post-Epic-50) for admin/org; reuse the user-DEK path for user. Write `key_version` on admin/org encrypt (user rows stay at 1 — no server KEK involvement).
- Wire into `pkg/secrets/injection.go` consumers as needed (the injection call site is US-53.7).

**Acceptance criteria:**
- Round-trip per owner type: Create with secret env → Get returns display fields, NOT the secret; a `GetForInjection` path returns decrypted env+headers (admin/org always; user only with a valid `sessionID`).
- `SeedWorkspaceMCPServers` correctly resolves `target_type='all'` + `target_type='org'` + `target_type='user'` rules + explicit bindings.
- **D11 enforcement at seed time:** when `orgID != ""`, `target_type='user'` rules are **skipped** — a user who joins an org after creating personal MCP servers must not have those servers injected into org workspaces (org admin owns the tool surface). One conditional at seed time closes the gap that the CRUD-only gate leaves open.
- User-scope Get without a session → error (cannot derive DEK), mirroring user-credential behaviour.
- Delete cascades to bindings + auto-apply.
- All store tests pass with `go-sqlmock` (DB) and `-race`.

**Tests (TDD):** table-driven CRUD for each owner type; cross-org isolation; cross-user isolation (user A cannot Get user B's server); user-scope-without-session error; seeding precedence (explicit + auto all three scopes bind, no dedup conflict); **D11 seed-time skip: user-scope rule exists but orgID is non-empty → not seeded**; backfill idempotency; concurrent seed + delete.

### US-53.4: Admin (platform) CRUD handler + routes + validation

**Goal:** Platform-admin MCP server management API.

**Routes** (registered behind `AdminGuard`, `api/internal/server/router.go` admin group):
```
GET    /api/v1/admin/mcp-servers
POST   /api/v1/admin/mcp-servers
GET    /api/v1/admin/mcp-servers/:id
PUT    /api/v1/admin/mcp-servers/:id
DELETE /api/v1/admin/mcp-servers/:id
POST   /api/v1/admin/mcp-servers/:id/bindings      {workspaceId}
DELETE /api/v1/admin/mcp-servers/:id/bindings/:workspaceId
POST   /api/v1/admin/mcp-servers/:id/auto-apply
GET    /api/v1/admin/mcp-servers/:id/auto-apply
DELETE /api/v1/admin/mcp-servers/:id/auto-apply/:targetType/:targetId?
```

**Files:**
- `api/internal/handlers/mcp_servers.go` (new) — `AdminMCPServersHandler`. Clone the structure of `admin_provider_credentials.go` (handler shape, `buildCredentialResponse` analog that strips secrets, auto-apply sub-CRUD).
- Validation: transport in `{http,sse,stdio}`; `url` present for remote transports; `command` present for stdio; URL scheme allow-list + SSRF block-list (loopback, link-local, cloud metadata `169.254.169.254`); name length/bounds; body size limit (existing `http.MaxBytesReader`).

**Acceptance criteria:**
- Non-admin → 404 (route hidden, per `AdminGuard` A12).
- Create returns 201 with no secret fields; List returns display fields only.
- Update with empty secret fields preserves existing ciphertext (partial update, mirror `OrgSSOTab`/sso service pattern).
- Invalid transport/URL → 400 with generic message.
- Auto-apply CRUD works; `target_type='all'` only for admin scope.

**Tests (TDD):** happy + unhappy (every validation rule); secrets-stripped assertions; 404 for non-admin; partial-update-preserves-secret.

### US-53.5: Org admin CRUD handler + routes + validation

**Goal:** Per-org MCP server management. Identical surface to US-53.4 but org-scoped.

**Routes** (behind `OrgAdminGuard`, under `registerOrgRoutes`, `router.go:1164-1173` pattern):
```
GET    /api/v1/orgs/:id/mcp-servers
POST   /api/v1/orgs/:id/mcp-servers
GET    /api/v1/orgs/:id/mcp-servers/:serverId
PUT    /api/v1/orgs/:id/mcp-servers/:serverId
DELETE /api/v1/orgs/:id/mcp-servers/:serverId
POST   /api/v1/orgs/:id/mcp-servers/:serverId/bindings      {workspaceId}
DELETE /api/v1/orgs/:id/mcp-servers/:serverId/bindings/:workspaceId
POST   /api/v1/orgs/:id/mcp-servers/:serverId/auto-apply    (target_type hardcoded 'org')
```

**Files:** `api/internal/handlers/org_mcp_servers.go` (new), cloning `org_credentials.go` structure. Org auto-apply is `target_type='org'`, `target_id=orgID` (mirror `org_credentials.go:320`).

**Acceptance criteria:**
- Org admin of org A cannot read/modify org B's servers (cross-org isolation enforced in queries by `owner_id`).
- Org member (non-admin) → 403 from `OrgAdminGuard`.
- All US-53.4 validation rules apply.

**Tests (TDD):** cross-org isolation; `OrgAdminGuard` enforcement; partial update; auto-apply hardcoded to org scope.

### US-53.5b: User-scope CRUD handler + routes + org-disqualification gate + feature-flag gate

**Goal:** Individual-user MCP server management with two gates: org-membership disqualifies (D11), and the `personal_mcp_servers` feature flag must be enabled (D12).

**Routes** (behind authentication, no org path — these are personal-scope):
```
GET    /api/v1/me/mcp-servers
POST   /api/v1/me/mcp-servers
GET    /api/v1/me/mcp-servers/:id
PUT    /api/v1/me/mcp-servers/:id
DELETE /api/v1/me/mcp-servers/:id
POST   /api/v1/me/mcp-servers/:id/bindings      {workspaceId}
DELETE /api/v1/me/mcp-servers/:id/bindings/:workspaceId
POST   /api/v1/me/mcp-servers/:id/auto-apply    (target_type hardcoded 'user', target_id = caller)
```

**Files:**
- `api/internal/handlers/user_mcp_servers.go` (new) — `UserMCPServersHandler`. The `me` path resolves the caller's `userID` from auth context (no `:id` path param). Two gates run on **all** routes (including GET/List — an org member or feature-disabled user should not discover personal MCP servers exist):
  1. **Org-disqualification (D11):** `orgStore.GetUserOrgID(ctx, userID)`; non-empty → `403` ("you are a member of an organization; ask your org admin to add MCP servers").
  2. **Feature-flag gate (D12):** `UserFeatureGuard` from US-53.11b; flag disabled for the caller's plan → `402` with `{feature, planId, hint}`.
- User-scope crypto requires the session DEK: the handler threads the `sessionID` from auth context into the store calls (US-53.3 user path). Read-only List/Get do NOT require a session (display fields are plaintext); only the secret-bearing paths (Create/Update inject, Get-for-injection) need it.
- Auto-apply is `target_type='user'`, `target_id=callerUserID` (the user auto-applies to their own workspaces).
- Validation: identical transport/URL/SSRF/name rules as US-53.4.

**Acceptance criteria:**
- User in an org → `403` on every mutating route (UC-10); List/Get also hidden (the UI hides the tab, but the API enforces it too).
- User not in an org, flag disabled → `402` with `{feature, planId, hint}` on mutating routes (UC-11).
- User not in an org, flag enabled → full CRUD works; secrets encrypted with the user DEK; auto-apply to own workspaces.
- Cross-user isolation: user A cannot read/modify user B's servers (`owner_id = caller` enforced in every query).
- Session required for Create/Update; absent session → `401`/`403` (cannot derive DEK).

**Tests (TDD):** org-member-blocked (`403`); feature-disabled-blocked (`402` shape); feature-enabled-happy-path; cross-user isolation; session-required-on-secret-paths; partial-update-preserves-secret; auto-apply-hardcoded-to-user.

### US-53.6: Binding lifecycle — seed-at-create (admin/org/user), explicit bind/unbind, backfill, reload fan-out

**Goal:** The machinery that connects MCP server records to actual workspaces and propagates changes to live pods.

**Files:**
- `api/internal/services/workspace/workspace_service.go` — `CreateWorkspace` calls `mcpStore.SeedWorkspaceMCPServers(ctx, meta.ID, userID, meta.OrgID)` alongside the existing `SeedWorkspaceCredentials` (~line 337).
- Bind/unbind handlers (in US-53.4/53.5 handlers) call `BindToWorkspace`/`UnbindFromWorkspace`, enforce quota (US-53.11), then trigger reload.
- Backfill: on auto-apply create/change, `BackfillAutoApply(serverID)` binds to existing matching workspaces in bounded batches (mirror `BackfillFreeTierBindings`, `pg_credential_store.go:163-180`).
- Reload fan-out: on any MCP server create/update/delete/disable, rewrite the K8s Secret for each bound workspace and queue an agent reload via the **existing** bounded-parallelism bulk path (`agent_reload.go:431-454`, `maxParallel=5`). No new fan-out mechanism.

**Acceptance criteria:**
- New workspace auto-receives platform + org + user-scoped MCP servers per auto-apply rules (user-scope only when the owner has an active session at create time — otherwise seeded on first interactive session, per D13).
- Explicit bind adds a server to one workspace; unbind removes it; both trigger reload.
- Backfill is resumable and idempotent; running twice is a no-op.
- Reload fan-out never exceeds the existing concurrency cap; a failed reload on one workspace does not abort the batch.
- Quota violation at bind → 409 with `Retry-After`-style guidance (mirror the 409 pattern in `proxy_handlers.go:71-87`).

**Tests (TDD):** seed-at-create (platform + org + explicit mix); backfill idempotency; reload fan-out concurrency cap; partial-reload failure isolation; quota rejection.

### US-53.7: Injection (API side) — new `SecretType "mcp-server"` in `secrets.json`

**Goal:** Extend the existing injection pipeline so MCP servers travel the same durable channel as LLM credentials.

**Files:**
- `pkg/secrets/types.go:24-37` — add `SecretTypeMcpServer SecretType = "mcp-server"`, **and** register it in the `ValidSecretTypes` map (`types.go:49-56`) — a type absent from this map is rejected by the validation gate.
- `pkg/secrets/injection.go` — extend `PrepareSecretsForInjection`. The LLM-provider path ends in `decryptBinding` (`injection.go:154-192`) which unmarshals specifically into `LLMProviderData` (line 188); MCP servers have a different shape and therefore decrypt in the store (`GetWorkspaceMCPServers` returns already-decrypted MCP configs), feeding a parallel append next to `buildNonLLMSecrets` (`injection.go:194`). Per injected server: `InjectedSecret{Type: "mcp-server", Name: server.Name, Metadata: {transport,url,command,args}, Plaintext: json(env+headers)}` per US-53.1's contract.
- No new K8s Secret; MCP servers ride in the existing `workspace-secrets-<id>` `secrets.json` key.

**Acceptance criteria:**
- A workspace with bound MCP servers produces a `secrets.json` containing both `llm-provider` and `mcp-server` entries.
- Disabled servers are skipped.
- A workspace with zero MCP servers produces byte-identical `secrets.json` to today (no regression).
- Decrypt failures for one server do not abort the whole injection (log + skip + metric).

**Tests (TDD):** injection-with-and-without-MCP; disabled-server-skipped; mixed admin+org decrypt; **user-scope decrypt with session + skip-without-session** (D13); partial-failure isolation; secrets.json regression (byte equality for the no-MCP case).

### US-53.8: Materialization (pod side) — applier + `AgentConfigWriter` mcp source + reload integration

**Goal:** Turn injected MCP server entries into opencode config so the agent connects at startup.

**Files:**
- `pkg/agentd/secrets/secrets.go` — add `applyMCPServer` alongside `applyLLMProvider` (~line 586). Stages MCP server configs into `m.stagedMCPServers` (mirror the `stagedProviders` pattern, lines 586-599). Dispatch added to the `applyOne` switch (~line 433).
- A formatter (new, in `pkg/agent/opencode/` or `pkg/agentd/secrets/`) renders staged MCP servers into the opencode `mcp` section per US-53.1's `MATERIALIZE-CONTRACT.md`. Pure function, no side effects (mirror `FormatOpenCodeConfig` purity, `format.go:40`).
- `cmd/workspace-agentd/agent_config_writer.go` — add a **fourth source**, `mcpServers`, alongside providers/model/relay (lines 56-62). `setMCPServers(...)`, and `rebuild()` (lines 152-200) merges the `mcp` section into the written `agent-config.json`. Atomic write unchanged.
- `cmd/workspace-agentd/secrets.go` `materialize` subcommand (boot) and `/v1/reload-secrets` handler (reload) both stage MCP servers and trigger `rebuild()`. Reload triggers the existing session-aware opencode restart (lines 557-709).

**Acceptance criteria:**
- On boot, `agent-config.json` contains the `mcp` section with all bound + enabled servers.
- On reload (token rotation, disable, bind change), the section is rewritten and opencode restarts session-aware.
- Empty MCP-server set produces an `agent-config.json` with no `mcp` key (or empty) — byte-equivalent provider/model/relay sections (no regression to the relay subsystem).
- `AgentConfigWriter` remains the **sole** in-process writer (no parallel write path introduced — Rule 5 / README-LLM relay subsystem).

**Tests (TDD):** materialize-then-format golden-file test (known input → expected `mcp` JSON per contract); rebuild-merges-all-four-sources; reload-rewrites-and-restarts; empty-set-regression; concurrent-rebuild `-race`.

### US-53.9: Frontend — platform-admin MCP servers tab

**Goal:** Admin UI for platform-wide MCP servers.

**Files:**
- `frontend/src/api/mcpServerTypes.ts` (new) — shared types; response type has `hasSecret: boolean`, NO secret fields (mirror `providerCredentialTypes.ts:15-24`).
- `frontend/src/api/mcpServers.ts` (new) — `adminMcpServersApi` (CRUD + bindings + auto-apply), cloning `providerCredentials.ts:58-79`.
- `frontend/src/components/settings/AdminMcpServersTab.tsx` (new) — clone `AdminProviderCredentialsTab.tsx`. Secret fields use Pattern C (`hasSecret` + `type="password"` + `••••••••` placeholder + `autoComplete="new-password"` + omit-on-empty partial update). Transport selector (http/sse/stdio); conditional fields (url for remote, command+args for stdio); env-var + header key/value editors.
- `frontend/src/pages/SettingsPage.tsx:16-28` — add `{ id: "platform-mcp", label: "MCP Servers", adminOnly: true }` to `allTabs`; add the render branch. Replaces the `ComingSoonTab` placeholder (the test at `ComingSoonTab.test.tsx:13` is updated or removed).

**Acceptance criteria:**
- Admin can create/list/update/delete platform MCP servers; secrets never displayed; partial update works.
- Auto-apply rule sub-CRUD (target `all`).
- Non-admin sees no tab (filtered by `isAdmin`).
- Existing `ComingSoonTab` "MCP Servers" placeholder is replaced, not duplicated.

**Tests (TDD):** component tests mirror `AdminProviderCredentialsTab` coverage; secret-stripping assertion on rendered output; transport-conditional-fields; partial-update-omits-empty-secret.

### US-53.10: Frontend — org-admin MCP servers tab

**Goal:** Org-admin UI, scoped to one org.

**Files:**
- `frontend/src/api/mcpServers.ts` — add a `orgMcpServersApi` block nested in `orgsApi` (clone the credentials block at `orgs.ts:130-159`).
- `frontend/src/components/org-admin/OrgMcpServersTab.tsx` (new) — clone `OrgCredentialsTab.tsx`. Consumes `useOutletContext<{org, isAdmin}>()`.
- `frontend/src/components/org-admin/OrgAdminLayout.tsx:51-61` — add `{ to: "mcp-servers", label: "MCP Servers", adminOnly: true }` to `navItems`.
- `frontend/src/router.tsx:56-65` — add `{ path: "mcp-servers", element: <OrgMcpServersTab /> }` to the `/orgs/:id` children.

**Acceptance criteria:** Org admin can fully manage org-scoped MCP servers; org members don't see the tab; cross-org navigation impossible (the `:id` is bound to membership).

**Tests (TDD):** mirror `OrgCredentialsTab.test.tsx` coverage; adminOnly filtering; secret masking.

### US-53.10b: Frontend — user MCP servers tab (visibility-gated: no-org + feature-enabled; capability-denied state when disabled)

**Goal:** Personal MCP server management for individual (non-org) users with the feature flag enabled; hidden for org members; capability-denied state for individuals without the flag.

**Files:**
- `frontend/src/api/mcpServers.ts` — add `userMcpServersApi` (CRUD + bindings + auto-apply on `/me/mcp-servers`).
- `frontend/src/api/me.ts` (or extend an existing user-context API) — a `getMcpServersEligibility()` call returning `{ eligible: bool, reason: "org_member"|"feature_disabled"|"eligible" }` so the frontend can pick the right render state without guessing. Backed by the same gates as the CRUD routes.
- `frontend/src/components/settings/UserMcpServersTab.tsx` (new) — three render states driven by eligibility:
  - `eligible` → full management UI (clone `AdminProviderCredentialsTab` shape, scoped to the user's own servers).
  - `org_member` → hidden entirely (the tab is not registered; org members never see a personal MCP tab — UC-10).
  - `feature_disabled` → a "not available on your plan" card. The card MAY render an upgrade CTA if billing context (checkout URL, plan comparison) is available — this is a frontend presentation choice that reads billing state if present, but the feature-flag gate itself has no billing dependency (D12).
- `frontend/src/pages/SettingsPage.tsx:16-28` — add `{ id: "user-mcp", label: "MCP Servers", adminOnly: false }` to `allTabs` (visible to all authenticated users; the component itself renders the correct state). Render branch mounts `UserMcpServersTab`.

**Acceptance criteria:**
- Flag-enabled, no-org user → full CRUD; secrets never displayed.
- Org member → no "MCP Servers" entry in personal settings (the eligibility call returns `org_member`; the tab self-hides).
- Flag-disabled, no-org user → capability-denied card; no management UI.
- All three states covered by component tests.

**Tests (TDD):** eligible-renders-management; org_member-self-hides; feature_disabled-renders-denied-card; secret masking; partial update.

### US-53.11: Governance + observability — org quota policy, audit logging, metrics

**Goal:** Bound blast radius, make every action auditable, make the subsystem observable.

**Files:**
- Org policy: extend `pkg/types/orgs_policy.go` and `api/migrations/000033_org_policies.up.sql` CHECK (new migration `000043`) with `max_mcp_servers_per_workspace` (integer, default 5). Enforce at every bind path (US-53.6): count resulting servers for the workspace; reject >quota with 409.
- A platform config flag (instance setting) `mcp.allowOrgAdminServers bool` (default true) letting operators disable org-admin MCP registration entirely (defense for the supply-chain threat).
- Audit logging: every CRUD + bind/unbind + auto-apply writes to the org `audit_log` (org scope) or `secret_audit_log`-equivalent (platform scope) — mirror existing audit calls in `org_credentials.go` and `admin_provider_credentials.go`. Actor, action (`mcp_server.create/update/delete/bind/...`), target id, timestamp. No secret bytes.
- Metrics (Prometheus, `client_golang`): `mcp_servers_total{owner_type}` (cardinality 3: admin/org/user), `mcp_bindings_total{source_type}`, `mcp_reload_failures_total`, `mcp_injection_duration_seconds` (histogram), `mcp_user_scope_gate_total{result}` (`allowed`/`org_blocked`/`free_blocked` — UC-10/UC-11 observability). No workspace-id or user-id labels (mirror the `WorkspaceSafeModeActive` cardinality fix from Epic 24 US-24.11).

**Acceptance criteria:**
- Quota enforced at seed/bind/backfill; over-quota rejected with a clear 409.
- Every mutating API across all three scopes produces exactly one audit row with no secret data.
- `mcp.allowOrgAdminServers=false` makes org admin MCP routes return 403/404.
- Metrics registered and exercised in tests; no unbounded labels; the user-scope gate metric distinguishes the two denial reasons.

**Tests (TDD):** quota boundary (at-limit ok, over-limit 409); audit-row content (no secrets) for each scope; flag-off blocks org admin; metric registration.

### US-53.11b: Feature flag — `PersonalMcpServers` capability flag + `UserFeatureGuard` middleware

**Goal:** The feature-enablement foundation for user-scope MCP servers (D12). Builds the capability flag and the user-scoped guard; does **not** build any billing (Stripe checkout, webhooks, invoices — those are a separate concern). Foundational — US-53.5b and US-53.10b depend on it.

**Files:**
- `pkg/billing/plan_tiers.go:9-16` — add `PersonalMcpServers bool` to `PlanFeatures`; set `true` on `PlanTeam`, `PlanBusiness`, `PlanEnterprise`; `false` on `PlanFree`. (This file is the feature-flag layer — it maps plan tier → capability flags and contains no payment code, despite living in the `billing` package.)
- `pkg/billing/plan_tiers.go:57-71` — add `case "personal_mcp_servers": return f.PersonalMcpServers` to `IsFeatureAllowed`.
- `api/internal/middleware/feature_guard.go` — add `UserFeatureGuard(userPlanReader, feature string) gin.HandlerFunc` parallel to `FeatureGuard`. It reads the caller's `userID` from auth context, loads the caller's plan, and checks `IsFeatureAllowed`. On denial → `402` with the same body shape as `FeatureGuard` (`{error, feature, planId, hint}`). The `userPlanReader` interface is `GetUserPlan(ctx, userID) (types.OrgPlan, error)` — minimal, Interface Segregation, same discipline as the existing `orgPlanReader`.
- `api/internal/services/database/` — a `GetUserPlan` query (single-column read of `users.plan_id`); or extend the user store interface if a suitable method exists.

**Explicitly NOT in this story:** any Stripe integration, checkout-session creation, webhook handling, subscription lifecycle, or price/product configuration. The flag is set per plan tier in the static `PlanTiers` map — that is the feature-enablement mechanism. How a user ends up on a tier that has the flag is out of scope (see "Out of Scope — Billing for MCP servers").

**Acceptance criteria:**
- `IsFeatureAllowed(team, "personal_mcp_servers")` and `business`/`enterprise` return `true`; `free` returns `false`.
- `UserFeatureGuard` returns `402` for a user whose plan lacks the flag and `c.Next()` for a user whose plan has it.
- The existing org-scoped `FeatureGuard` is **untouched** (regression-free — existing org features keep working).
- No new dependency on `stripe_provider.go`, `webhook.go`, or `org_billing.go`.
- A deployment with no Stripe configured can still gate the feature by setting `users.plan_id` directly.

**Tests (TDD):** flag matrix per plan tier; `UserFeatureGuard` allows-when-enabled / denies-when-disabled (`402` body shape); org `FeatureGuard` regression (existing tests unchanged); `IsFeatureAllowed` unknown-feature fail-open preserved.

### US-53.12: E2E integration — full wired path

**Goal:** Prove the feature works end-to-end, per README-LLM.md Rule 0 (definition of done = demonstrated integration) and the E2E Wiring Verification standard. This is the gate.

**Scope (must all be demonstrated):**
1. Platform admin creates a remote HTTP MCP server via the API → it backfills to all workspaces.
2. A workspace boots → `agent-config.json` contains the `mcp` section → opencode connects → the server's tools are visible to the agent (verified via the agent's tool-list or a real tool call).
3. Org admin creates an org-scoped server → only that org's workspaces gain it; another org's workspace does not.
4. Token rotation: admin updates the server → live workspace reloads → reconnects with new token (old token unusable).
5. Kill-switch: admin disables the server → tools vanish from the live workspace after reload.
6. Explicit bind to a single workspace (auto-apply off).
7. Quota enforcement (bind beyond `max_mcp_servers_per_workspace` → 409).
8. Audit log contains an entry for each mutating action; no secret bytes in any row.
9. **User-scope happy path:** paid, no-org individual user creates a personal MCP server → it auto-applies to their own workspaces (and only theirs) → their agent gains the tools; secrets encrypted with the user DEK (zero-knowledge: platform cannot decrypt without the session).
10. **Org-member blocked (D11):** a user who is an org member attempts user-scope CRUD → `403`; their personal settings show no MCP tab.
11. **Feature-flag denied (D12):** a no-org individual whose `personal_mcp_servers` flag is disabled attempts user-scope CRUD → `402`; their personal settings show the capability-denied card.
12. **Frontend:** all three tabs (platform-admin, org-admin, user) render correctly; create+rotate+delete work; secrets never displayed; the user tab shows the right state per eligibility.

**Files:** `api/internal/tests/integration/` (new dir per Epic 16 US-16.13 gap) + `tests/` e2e harness + frontend component/e2e tests.

**Acceptance criteria:** all 12 scenarios pass on a kind cluster with `-race -count=1`; the test MCP server (HTTP) is stood up by the test harness; no manual verification steps remain.

**Tests (TDD):** the 12 scenarios above are the test list. Each is an integration/e2e test exercising the real wiring (router → middleware gates → service → store → K8s Secret → pod materializer → opencode config), not a unit test.

---

## Out of Scope (with rationale)

- **User-facing tool allowlist policy (`allowed_mcp_servers`).** Needs a selection UX and an injection-path evaluation point. Deferred; the org-membership disqualification (D11), feature-flag gate (D12), and quota (`max_mcp_servers_per_workspace`) are the shipped governance controls (D10).
- **Billing for MCP servers (Stripe checkout, webhooks, invoices, subscription lifecycle).** Billing and feature flagging are separate concerns (D12). This epic builds the feature-flag layer — the authoritative "is the capability enabled?" gate. Billing is a *writer* of the state the flag reads (`users.plan_id`); when wired (Epic 12 or a dedicated follow-up), a successful Stripe purchase flips the plan and the flag turns on automatically, with zero feature-flag code changes. No Stripe code is touched, depended on, or built in this epic. A deployment with no Stripe configured can still gate the feature by setting the plan directly.
- **MCP server marketplace / discovery / registry of public servers.** Out of scope — admins/users register specific servers they trust. A catalog is a separate product surface.
- **Running MCP servers as platform-managed processes (the platform hosting the server, not the agent connecting to one).** Different feature; the platform is the client here, not the host.
- **stdio transport full runtime support (image baking, package install, binary verification).** The data model + API support stdio (D6), but ensuring a given stdio command's binary exists in every runtime image is an operator/runtime concern documented, not built, in this epic. US-53.12 validates with a remote server.
- **Per-tool permission rules for MCP-exposed tools.** opencode's existing permission model (`opencode.json:20-32`) governs tool execution; the platform does not add a parallel permission layer for MCP tools in this epic.
- **MCP server health-checking / status dashboard.** A status dashboard (like the Relay Admin UX, Epic 48) is a natural follow-up but not required for the core flow. Metrics (US-53.11) provide observability in the meantime.
- **SAML/SCIM provisioning of MCP access.** Not applicable; MCP access is bound by workspace, not provisioned by IdP.
- **Usage-metered (per-call) metering for MCP servers.** Per-call usage metering via `usage_events` is Epic 12's open work. This epic ships only the boolean feature flag (on/off capability gate), not usage-based metering. The two are independent: the flag answers "enabled?"; metering answers "how much was consumed?".

---

## Open Questions

These are resolved during implementation (not blocking the epic definition), captured here so they are not lost:

1. **Should end users be able to explicitly bind an admin/org MCP server to their own workspace (opt-in to a server that exists but isn't auto-applied)?** Initial position: no in this epic — binding is admin/org-admin controlled. Revisit if UC-5 demand expands to users.
2. **Should a platform-wide MCP server override/be-hidden-by an org policy?** Initial position: platform servers are exempt from org quotas (they are platform policy); an org-level *hide* of a platform server is the deferred allowlist feature (D10).
3. **What is the right default for `max_mcp_servers_per_workspace`?** Proposed 5 (balances utility vs startup cost); validate against measured opencode startup-time impact during US-53.1.
4. **Do MCP tool results need to flow through the platform's `redact` pipeline, or does opencode's own output handling suffice?** A17 / US-53.1 answers this.
5. **Should MCP server config changes trigger immediate reload, or be batched (next pod restart)?** Proposed: immediate for disable/delete (kill-switch semantics), batched-for-next-restart for cosmetic changes. Confirm in US-53.6.
6. **Which plan tier(s) should have the `PersonalMcpServers` flag enabled by default?** D12 proposes `team`/`business`/`enterprise` (any non-`free`). Confirm the tier threshold during US-53.11b — the flag is a one-line per-tier change in the static `PlanTiers` map if the threshold moves. This is a feature-enablement decision (which tiers get the capability), not a billing decision (pricing/checkout).

---

## Definition of Done

This epic is complete when **all** of the following hold:

1. All 15 stories merged with passing tests (`make test && make build && make lint`), each with happy + unhappy + integration/e2e coverage per Rule 0.
2. **US-53.1's contract is validated with live evidence** — a real opencode run connecting to a real external MCP server, captured in `MATERIALIZE-CONTRACT.md`. A6/A7/A17 are no longer "assumptions."
3. The full wired path (US-53.12 scenarios 1–12) passes on a kind cluster with `-race -count=1`. A registered MCP server's tools (in any scope) are demonstrably usable inside a running agent.
4. No secret material (env vars, headers, ciphertext) appears in any API response, log line, audit row, or metric label — verifiable by test.
5. Cross-org isolation is proven: org A cannot read, modify, or inject org B's MCP servers. Cross-user isolation is proven: user A cannot read/modify user B's servers.
6. **The org-membership disqualification (D11) is enforced:** an org member's user-scope CRUD returns `403`; their personal MCP tab is hidden (UC-10).
7. **The feature-flag gate (D12) is enforced:** an individual whose `personal_mcp_servers` flag is disabled gets `402` on user-scope CRUD; their personal tab shows the capability-denied state (UC-11). Individuals with the flag enabled get full CRUD (UC-9). The flag layer has no billing dependency — no Stripe code is imported or required.
8. Reload propagation (rotation, kill-switch) reaches live workspaces within the existing bounded-parallelism cap; no unbounded fan-out.
9. `AgentConfigWriter` remains the sole in-process writer of `agent-config.json`; the relay subsystem is regression-free (byte-equivalent provider/model/relay sections when MCP set is empty).
10. Org quota `max_mcp_servers_per_workspace` enforced at every bind path; over-quota rejected with 409.
11. Every mutating admin/org/user MCP action produces exactly one audit row with no secret data; the `mcp.allowOrgAdminServers=false` flag disables org-admin registration.
12. All three frontend tabs ship (platform-admin + org-admin + user); the user tab correctly renders management/hidden/capability-denied per eligibility; the `ComingSoonTab` "MCP Servers" placeholder is replaced.
13. User-scope ciphertext is unrecoverable without the user's session (zero-knowledge verified: the platform cannot decrypt a user-scope secret with no active session).
14. Worklog entry created per the Worklog Requirements in README-LLM.md.
15. A README-LLM.md section ("MCP Server Integration") is added documenting the as-built system, mirroring §14 (Multi-Tenant OIDC SSO) — added when the epic ships, not before.

---

## File Reference (existing code to follow as templates)

| Concern | Template file:line |
|---|---|
| Unified credential table shape | `api/migrations/000015_unified_credential_model.up.sql:13-55` |
| Owner-type crypto switch | `pkg/secrets/injection.go:154-192` |
| Secret types | `pkg/secrets/types.go:24-37` |
| Seed-at-create | `pkg/secrets/pg_credential_store.go:84-141` |
| Backfill | `pkg/secrets/pg_credential_store.go:163-180` |
| Bounded reload fan-out | `api/internal/handlers/agent_reload.go:431-454` |
| 409 active-session guard | `api/internal/handlers/proxy_handlers.go:71-87` |
| Sole in-process config writer | `cmd/workspace-agentd/agent_config_writer.go:56-62,152-200` |
| Pod-side materializer | `pkg/agentd/secrets/secrets.go:420-650` |
| opencode config formatter (purity) | `pkg/agent/opencode/format.go:40-97` |
| AdminGuard (404 route-hide) | `api/internal/middleware/admin_guard.go:14-23` |
| OrgAdminGuard | `api/internal/middleware/org_guard.go:54-81` |
| Admin handler template | `api/internal/handlers/admin_provider_credentials.go` |
| Org handler template | `api/internal/handlers/org_credentials.go` |
| Org auto-apply (org-scoped) | `api/internal/handlers/org_credentials.go:320`, `pkg/secrets/org_credential_store.go:60-105` |
| Router: admin group | `api/internal/server/router.go:284-298` |
| Router: org group | `api/internal/server/router.go:1164-1173` |
| Org policy types | `pkg/types/orgs_policy.go:17-69` |
| Org policy table | `api/migrations/000033_org_policies.up.sql` |
| User plan column (`users.plan_id`) | `api/migrations/000026_usage_limits.up.sql:17` |
| Org-membership check (`GetUserOrgID`) | `api/internal/services/database/pg_org_store.go:801-814` |
| Feature-gate middleware (org-scoped, to mirror for user-scope) | `api/internal/middleware/feature_guard.go:36-64` |
| Plan features + `IsFeatureAllowed` | `pkg/billing/plan_tiers.go:9-71` |
| SSO DTO discipline (secrets `json:"-"`, `hasSecret`) | `pkg/types/orgs.go:174-215` |
| User-DEK decrypt path (`case "user"`) | `pkg/secrets/injection.go:157-165` |
| Frontend admin tab template | `frontend/src/components/settings/AdminProviderCredentialsTab.tsx` |
| Frontend org tab template | `frontend/src/components/org-admin/OrgCredentialsTab.tsx` |
| Frontend capability-denied UX template | `frontend/src/components/org-admin/OrgBillingTab.tsx` (layout pattern only; no billing logic imported) |
| Frontend API client template | `frontend/src/api/providerCredentials.ts:58-79` |
| Frontend shared-types template | `frontend/src/api/providerCredentialTypes.ts` |
| Frontend settings-tab registration | `frontend/src/pages/SettingsPage.tsx:16-35` |
| Frontend org-tab registration | `frontend/src/components/org-admin/OrgAdminLayout.tsx:51-61`, `frontend/src/router.tsx:56-65` |
| Frontend placeholder to replace | `frontend/src/components/settings/ComingSoonTab.test.tsx:13` |

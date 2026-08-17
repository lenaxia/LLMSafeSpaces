# Canary Test Plan

**Version:** 3.0  
**Date:** 2026-06-03  
**Scope:** SDK canaries (Go, Python, TypeScript), MCP server canary.  
**Goal:** Structured, scheduled verification that all major SDK and MCP server workflows continue to work against a live deployment, with clear separation between fast/frequent checks and deeper end-to-end flows.

---

## Table of Contents

1. [Design Principles](#1-design-principles)
2. [Shallow vs. Deep: The Two-Tier Model](#2-shallow-vs-deep-the-two-tier-model)
3. [Environment Variables](#3-environment-variables)
4. [Complete Scenario Index](#4-complete-scenario-index)
5. [Scenario Specifications — Shallow (Tier 1)](#5-scenario-specifications--shallow-tier-1)
6. [Scenario Specifications — Deep (Tier 2)](#6-scenario-specifications--deep-tier-2)
7. [Scenario Specifications — MCP Server](#7-scenario-specifications--mcp-server)
8. [Fission Deployment Schedule](#8-fission-deployment-schedule)
9. [CI Integration](#9-ci-integration)
10. [Test Accounts](#10-test-accounts)
11. [What Is Explicitly Out of Scope](#11-what-is-explicitly-out-of-scope)
12. [Changelog](#12-changelog)

---

## 1. Design Principles

- **Self-contained.** Every scenario creates its own resources and cleans them up in a `defer`/`finally`, even on failure. No scenario assumes state from a previous run.
- **Two failure modes matter.** A regression shows up either as an error response (wrong status code, missing field) or as a silent degradation (the call succeeds but the observable behavior changed). Both are first-class concerns.
- **Positive and negative.** Every scenario includes at least one happy-path check and at least one failure-path check (wrong credentials, nonexistent resource, malformed input).
- **SDK-faithful.** Canaries call the public SDK surface only. Raw HTTP is used only for MCP (JSON-RPC), health probes, and endpoints the SDKs don't yet expose.
- **Fission-compatible.** Each scenario compiles to a single binary/script that accepts an HTTP `GET` and returns a JSON `Result` object. The same binary runs from the command line with `os.Exit(1)` on failure, so CI uses it directly.
- **Assert, don't just record.** Where a comment in an earlier revision said "record actual behavior," this plan replaces that with a concrete assertion based on the documented API contract.

---

## 2. Shallow vs. Deep: The Two-Tier Model

The core insight: **not all canaries are equal in cost, risk of flakiness, and what they actually catch**.

### Tier 1 — Shallow (every 1 minute)

**What they test:** API availability, authentication, CRUD plumbing, error shapes, SDK parsing. These can run against any live API server. They create and immediately delete resources. They do **not** wait for Kubernetes to schedule a pod.

**Failure signature:** "The API itself is down or broken" — a 500, a missing field, an auth regression, a serialization bug.

**Target duration:** < 30 seconds. If a shallow canary takes longer, something is already badly wrong (slow DB, overloaded API pod).

**Acceptable flakiness:** Near-zero. These should be essentially deterministic against a healthy server.

**Examples:** `S-AUTH`, `S-APIKEY`, `S-WS-CRUD`, `S-SECRET-CRUD`, `S-ERROR-FORMAT`, `S-HEALTH`.

---

### Tier 2 — Deep (every 5–15 minutes)

**What they test:** Full user workflows that span multiple services: Kubernetes CRD reconciliation, agent startup, credential injection into a running pod, LLM round-trips, session persistence across suspend/resume. These require a workspace to reach `Active` phase.

**Failure signature:** "Something in the controller, agent, or their integration is broken" — a workspace that never becomes Active, a session that fails after resume, a credential that doesn't reach the agent.

**Target duration:** 3–10 minutes depending on workspace startup time.

**Acceptable flakiness:** Low but non-zero. Pod scheduling, image pulls, and LLM provider latency introduce noise. Alert only after 2+ consecutive failures.

**Examples:** `D-SESSION-MSG`, `D-CRED-MODEL-FLOW`, `D-SUSPEND-RESUME-SESSION`.

---

### Why this split matters

Running deep canaries every minute would:
1. Keep the test cluster perpetually littered with provisioning workspaces
2. Cause alert fatigue from legitimate scheduling noise
3. Mask real regressions in the constant churn

Running only shallow canaries would miss the most impactful class of failure: a workspace controller bug that makes every new workspace silently hang at `Pending`.

**The rule:** Shallow canaries tell you if the API is broken. Deep canaries tell you if the product is broken.

---

## 3. Environment Variables

| Variable | Required for | Default | Description |
|---|---|---|---|
| `LLMSAFESPACES_URL` | All | `http://localhost:8080` | API base URL |
| `LLMSAFESPACES_API_KEY` | All | — | Primary test user API key (`lsp_` prefix) |
| `LLMSAFESPACES_API_KEY_USER2` | S-OWNERSHIP | — | Second test user API key |
| `LLMSAFESPACES_EMAIL` | S-AUTH (JWT), S-LOGOUT, D-REVEAL-REAUTH | — | Primary test user email |
| `LLMSAFESPACES_PASSWORD` | S-AUTH, S-LOGOUT, S-SECRET-REVEAL, D-KEY-ROTATE, D-CHANGE-PASSWORD | — | Primary test user password |
| `LLMSAFESPACES_LLM_PROVIDER` | Deep agent scenarios | `anthropic` | LLM provider name |
| `LLMSAFESPACES_LLM_API_KEY` | Deep agent scenarios | — | Real LLM API key; deep message tests skip if absent |
| `LLMSAFESPACES_LLM_MODEL` | Deep agent scenarios | — | Model ID (e.g. `anthropic/claude-haiku-4-5`) |
| `LLMSAFESPACES_BAD_MODEL` | D-MODEL-SET | `invalid-provider/no-such-model` | Model expected to fail |

---

## 4. Complete Scenario Index

### Tier 1 — Shallow (< 30s, no pod wait)

| ID | Name | Schedule | `ci:fast` |
|---|---|---|---|
| S-HEALTH | API health endpoints | 1 min | ✓ |
| S-AUTH | Authentication flows | 1 min | ✓ |
| S-AUTH-CONFIG | Auth config / feature flags | 1 min | ✓ |
| S-LOGOUT | Logout and JWT revocation | 1 min | ✓ |
| S-APIKEY | API key lifecycle | 1 min | ✓ |
| S-USER-SETTINGS | User settings CRUD and schema | 1 min | ✓ |
| S-WS-CRUD | Workspace CRUD and storage validation | 1 min | ✓ |
| S-WS-STATUS | Workspace status response shape | 1 min | ✓ |
| S-WS-QUOTA | Workspace quota enforcement | 5 min | ✓ |
| S-SECRET-CRUD | Generic secret CRUD, update, and name validation | 1 min | ✓ |
| S-SECRET-REVEAL | Reveal with password reauth gate | 1 min | ✓ |
| S-SECRET-AUDIT | Secret audit log | 1 min | ✓ |
| S-SECRET-BINDINGS | Workspace secret bindings (idempotency) | 5 min | ✓ |
| S-ENV-VARS | Workspace env vars (API layer only) | 5 min | ✓ |
| S-CRED-CRUD | LLM credential CRUD | 1 min | ✓ |
| S-OWNERSHIP | Cross-user isolation | 5 min | ✓ |
| S-ERROR-FORMAT | Error response shape + proxy error shapes | 1 min | ✓ |
| S-RATE-LIMIT | Auth endpoint rate limiting | 5 min | ✓ |

### Tier 2 — Deep (3–15 min, requires Active workspace)

| ID | Name | Schedule | Requires LLM key |
|---|---|---|---|
| D-WS-LIFECYCLE | Full lifecycle: suspend/resume/restart + idempotency + status fields | 5 min | No |
| D-ACTIVATE-EVICTION | `activate` auto-evicts stalest workspace at cap | 10 min | No |
| D-SESSION-ENSURE | Session ensure (including auto-resume from Suspended), list, rename, abort, individual GET | 5 min | No |
| D-SESSION-MSG | Session message + verbose flag + status `lastActivityAt` | 5 min | Yes |
| D-SESSION-HISTORY | Session history after message | 5 min | Yes |
| D-SESSION-TITLE | Auto-generated session title backfill | 10 min | Yes |
| D-SESSION-LIMIT | Active session limit 429 + connection limit 429 | 10 min | No |
| D-SESSION-GET | Individual session GET endpoint | 5 min | No |
| D-PROMPT-ASYNC | `prompt_async` + SSE `session.idle` flow (MCP server code path) | 5 min | Yes |
| D-AGENT-INPUT | Question and permission input flows | 10 min | Yes |
| D-SESSION-SUBTASK | Subagent `parentId` backfill | 15 min | Yes |
| D-TERMINAL | Terminal ticket generation | 5 min | No |
| D-CRED-BIND | Credential bind + reload + unbind + reload-empty | 5 min | No |
| D-MODEL-LIST-ANNOTATED | Model list with `currentModel`, `selected`, `tier` fields | 5 min | Yes |
| D-MODEL-SET | Set model and verify selection | 5 min | Yes |
| D-CRED-MODEL-FLOW | Full: add cred → set model → call agent → reload session | 10 min | Yes |
| D-SUSPEND-RESUME-SESSION | Session history survives suspend/resume | 10 min | Yes |
| D-ENV-INJECTION | Env var reaches agent environment and clears on unbind | 10 min | Yes |
| D-SSE-EVENTS | SSE broker delivers phase + session events | 10 min | No |
| D-KEY-ROTATE | Encryption key rotation | 15 min | No |
| D-CHANGE-PASSWORD | Password change | 15 min | No |
| D-ACCOUNT-RECOVER | Account recovery with recovery key | 15 min | No |

### MCP Server

| ID | Name | Schedule | Tier |
|---|---|---|---|
| S-MCP-TOOLS | Tool registration completeness (named subset + min count) | 1 min | Shallow |
| S-MCP-AUTH-NEG | Invalid credentials → tool error (not JSON-RPC error) | 1 min | Shallow |
| S-MCP-CRED | Credential tools CRUD | 1 min | Shallow |
| S-MCP-INPUT-NEG | Input validation (missing args, oversized message, bad session ID) | 1 min | Shallow |
| D-MCP-WORKSPACE | Workspace lifecycle via MCP tools | 5 min | Deep |
| D-MCP-SESSION | Session + message via MCP tools | 5 min | Deep |
| D-MCP-PROMPT-ASYNC | MCP `session_message` uses prompt_async+SSE internally | 5 min | Deep |
| D-MCP-MODEL | Model list + set via MCP tools | 5 min | Deep |

---

## 5. Scenario Specifications — Shallow (Tier 1)

---

### S-HEALTH — API health endpoints

**Schedule:** 1 min | **Max duration:** 10s

| # | Check |
|---|---|
| P1 | `GET /livez` → 200, `{"status":"ok"}` |
| P2 | `GET /health` (alias) → 200, `{"status":"ok"}` |
| P3 | `GET /readyz` → 200, `{"status":"ready"}` |
| P4 | All three responses parse as valid JSON |
| N1 | `/readyz` and `/livez` return 200 even when called 10× in rapid succession (not rate-limited) |

---

### S-AUTH — Authentication flows

**Schedule:** 1 min | **Max duration:** 30s

| # | Check |
|---|---|
| P1 | Valid API key → `GET /auth/me` 200 with `id`, `email`, `role`, `username`, `active` |
| P2 | JWT login with valid credentials → token returned → subsequent `GET /auth/me` succeeds |
| P3 | `user.active` is `true` for the test account |
| N1 | Invalid API key (`lsp_invalid_canary_key`) → 401, `AuthError` |
| N2 | Empty auth header → 401 |
| N3 | Malformed bearer value (no `lsp_` prefix, not a JWT) → 401 |
| N4 | Wrong password for valid email → 401 (same error shape as N5; no enumeration) |
| N5 | Login with nonexistent email → same 401 shape as N4 |

---

### S-AUTH-CONFIG — Auth config endpoint

**Schedule:** 1 min | **Max duration:** 10s  
**Note:** This is the first call every frontend client makes. A regression breaks all UIs.

| # | Check |
|---|---|
| P1 | `GET /api/v1/auth/config` → 200 with no `Authorization` header (public endpoint) |
| P2 | Response has `registrationEnabled` (boolean) |
| P3 | Response has `oidcEnabled` (boolean) |
| P4 | Response has `instanceName` (non-empty string) |
| P5 | Response has `motd` (string, may be empty) |
| N1 | Response body has no `error` field |

---

### S-LOGOUT — Logout and JWT revocation

**Schedule:** 1 min | **Max duration:** 30s  
**Requires:** `LLMSAFESPACES_EMAIL`, `LLMSAFESPACES_PASSWORD`

Tests that logout actually invalidates the JWT in the revocation cache — not just clears the cookie.

| # | Check |
|---|---|
| P1 | Login via email/password → JWT token received |
| P2 | `GET /auth/me` with JWT before logout → 200 |
| P3 | `POST /auth/logout` with `Authorization: Bearer <jwt>` → 204 |
| P4 | `GET /auth/me` with the **same JWT** after logout → 401 (token revoked) |
| P5 | `POST /auth/logout` called a second time → 204 (idempotent) |
| N1 | `POST /auth/logout` with an API key → 204 (API keys not revoked by this path) |
| N2 | After logout, the API key is still valid for `GET /auth/me` |

---

### S-APIKEY — API key lifecycle

**Schedule:** 1 min | **Max duration:** 30s

| # | Check |
|---|---|
| P1 | Create API key → `id`, `key` starts with `lsp_`, `name`, `active=true` |
| P2 | `key` field present only on creation response (absent in list) |
| P3 | List API keys → created key appears |
| P4 | Delete API key → 204 |
| P5 | List after delete → key absent |
| P6 | Using the deleted key for `GET /auth/me` → 401 |
| N1 | Delete nonexistent key ID → error (not 204) |
| N2 | Create key with empty name → 400 |

---

### S-USER-SETTINGS — User settings CRUD and schema

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| P1 | `GET /users/me/settings` → 200 with `settings` object and `schemaVersion` integer |
| P2 | `GET /users/me/settings/schema` → 200 with `schemaVersion` integer and `settings` array |
| P3 | Schema `schemaVersion` equals the expected constant (currently `11`, `settings.SchemaVersion`); alert if it changes unexpectedly |
| P4 | Each setting in schema has `key`, `type`, `default`, `label` fields |
| P5 | `PUT /users/me/settings/theme` with `{"value": "dark"}` → 200 with `key` and `value` |
| P6 | `GET /users/me/settings` after PUT reflects `theme=dark` |
| P7 | Reset: `PUT /users/me/settings/theme` with `{"value": "system"}` → 200 |
| N1 | `GET /users/me/settings` without auth → 401 |
| N2 | `PUT /users/me/settings/theme` with missing `value` body → 400 |
| N3 | `PUT /users/me/settings/nonexistent.key` → 400 (schema validation rejects unknown keys) |

---

### S-WS-CRUD — Workspace CRUD and storage validation

**Schedule:** 1 min | **Max duration:** 30s

| # | Check |
|---|---|
| P1 | Create workspace → 201 with `id`, `name`, `runtime`, `storageSize`, `phase`, `createdAt`, `updatedAt` |
| P2 | `GET /workspaces/:id` → correct `id` |
| P3 | `GET /workspaces` → `items` array + `pagination` object; created workspace present |
| P4 | `pagination` has `total`, `limit`, `offset` fields |
| P5 | `?limit=1` → at most 1 item; `?limit=1&offset=1` → different item or empty |
| P6 | `PUT /workspaces/:id` (rename) → 204; GET shows new name |
| P7 | `DELETE /workspaces/:id` → 204 |
| P8 | After delete: GET returns 404 or terminal phase |
| N1 | GET nonexistent ID → 404 `NotFoundError` |
| N2 | DELETE nonexistent ID → error |
| N3 | Create with empty `runtime` → 400 |
| N4 | PUT with missing `name` body → 400 (must run BEFORE delete so workspace still exists) |
| N5 | Create with `storageSize` exceeding `workspace.maxStorageSize` (e.g. `9999Gi`) → 400 |
| N6 | Create with invalid `storageSize` format (e.g. `"invalid"`) → 400 |
| N7 | GET workspace owned by different user → **403 Forbidden** (workspace routes, not 404; see S-OWNERSHIP) |

---

### S-WS-STATUS — Workspace status response shape

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| P1 | `GET /workspaces/:id/status` immediately after create → 200 |
| P2 | `phase` is a string |
| P3 | `activeSessions` is an integer ≥ 0 |
| P4 | `credentialState.available` is a boolean |
| P5 | `agentHealth.status` is a string (`"Unknown"` expected pre-Active) |
| P6 | `agentHealth.providersConfigured` is an integer |
| P7 | `conditions` is an array (may be empty pre-Active) |
| P8 | Response does NOT have `error` field on success |
| N1 | Status of nonexistent workspace → 404 |
| N2 | Status of workspace owned by another user → 404 |

---

### S-WS-QUOTA — Workspace quota enforcement

**Schedule:** 5 min | **Max duration:** 60s  
**Note:** Auto-detects quota from `LLMSAFESPACES_MAX_WORKSPACES_PER_USER`. Skips gracefully if unlimited.

| # | Check |
|---|---|
| P1 | Can create up to the configured limit |
| N1 | Creating one beyond limit → 429 |
| N2 | 429 body has `error` and `limit` fields |

---

### S-SECRET-CRUD — Generic secret CRUD, update, and name validation

**Schedule:** 1 min | **Max duration:** 30s

| # | Check |
|---|---|
| P1 | Create `env-secret` → 201, `id`, `name`, `type` match request |
| P2 | `GET /secrets` → `{"secrets": [...]}` wrapper; created secret present |
| P3 | `GET /secrets/:id` → metadata (no plaintext value field) |
| P4 | `PUT /secrets/:id` (update value) → 204 |
| P5 | DELETE → 204; absent from list |
| P6 | Re-create with same name after delete → 201 (no lingering 409 from deleted record) |
| N1 | `GET /secrets/:id` nonexistent → 404 |
| N2 | Create with uppercase name (`My-Secret`) → 400 (name validation) |
| N3 | Create with empty name → 400 |
| N4 | Create second secret with **same name as existing** → 409 Conflict |
| N5 | DELETE nonexistent → error |

---

### S-SECRET-REVEAL — Reveal with password reauth gate

**Schedule:** 1 min | **Max duration:** 30s  
**Requires:** `LLMSAFESPACES_PASSWORD`

| # | Check |
|---|---|
| P1 | Create secret with known value |
| P2 | `POST /secrets/:id/reveal` with correct password → 200, `value` matches |
| P3 | `value` is NOT present in GET or list responses |
| N1 | Reveal without password body → 400 |
| N2 | Reveal with wrong password → 403 |
| N3 | Reveal nonexistent secret → error |

---

### S-SECRET-AUDIT — Secret audit log

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| P1 | `GET /secrets/audit` → 200 with `entries` array |
| P2 | After creating a secret, `entries` contains an entry with matching `secretId` |
| P3 | Each entry has `action`, `secretId`, `userId` fields |
| N1 | Without auth → 401 |

---

### S-SECRET-BINDINGS — Workspace secret bindings with idempotency

**Schedule:** 5 min | **Max duration:** 60s

| # | Check |
|---|---|
| P1 | `PUT /bindings` with 1 secret → 204 |
| P2 | `GET /bindings` → contains bound secret ID |
| P3 | `PUT /bindings` with same secret ID again → 204 (idempotent, no duplicate) |
| P4 | `GET /bindings` still contains exactly 1 entry |
| P5 | `PUT /bindings` with empty list → 204; `GET /bindings` → empty |
| P6 | `GET /secrets/:id/bindings` → `workspaces` array |
| N1 | Bind to nonexistent workspace → error |
| N2 | Get bindings for nonexistent workspace → error |
| N3 | Bind nonexistent secret ID → error |

---

### S-ENV-VARS — Workspace environment variables (API layer)

**Schedule:** 5 min | **Max duration:** 60s

| # | Check |
|---|---|
| P1 | `PUT /env` with `{"vars": {"CANARY_VAR": "hello"}}` → 204 |
| P2 | `GET /env` → `{"vars": [...]}` containing `CANARY_VAR` |
| P3 | Setting same var with new value → 204 (upsert semantics) |
| P4 | `DELETE /env/CANARY_VAR` → 204 |
| P5 | `GET /env` after delete → `CANARY_VAR` absent |
| N1 | `GET /env` on nonexistent workspace → error |
| N2 | `PUT /env` with missing `vars` body → 400 |
| N3 | `DELETE /env/:name` for nonexistent var → 404 |

---

### S-CRED-CRUD — LLM credential CRUD

**Schedule:** 1 min | **Max duration:** 30s

LLM provider credentials are stored as secrets of type `llm-provider`.

| # | Check |
|---|---|
| P1 | Create `llm-provider` secret → 201, `id`, `type=llm-provider` |
| P2 | List → secret present |
| P3 | DELETE → 204; absent from list |
| N1 | DELETE nonexistent → error |
| N2 | Create with malformed provider JSON → 400 |

---

### S-OWNERSHIP — Cross-user isolation

**Schedule:** 5 min | **Max duration:** 60s  
**Requires:** `LLMSAFESPACES_API_KEY_USER2`

**Implementation note (validated against source):** Workspace service routes (`GET`, `DELETE`, status) return **HTTP 403** (ForbiddenError) for cross-user access — not 404. The server intentionally does not hide workspace existence on these routes. The bindings route uses the secrets handler which maps `ErrWorkspaceNotOwned` to **HTTP 404** to prevent cross-user workspace enumeration via the bindings API.

| # | Check |
|---|---|
| P1 | User1 workspace W1: User1 can GET |
| P2 | User2 workspace W2: User2 can GET |
| P3 | User1 list → W1 present, W2 absent |
| P4 | User2 list → W2 present, W1 absent |
| N1 | User2 `GET /workspaces/{W1.id}` → **403 Forbidden** (workspace routes return 403 for ownership violation) |
| N2 | User2 `DELETE /workspaces/{W1.id}` → error (403) |
| N3 | User2 `GET /workspaces/{W1.id}/status` → error (403) |
| N4 | User2 `GET /secrets/{S1.id}` (S1 owned by User1) → error |
| N5 | User2 `POST /workspaces/{W1.id}/sessions/new` → error |
| N6 | User2 `GET /workspaces/{W1.id}/bindings` → **404** (secrets handler maps cross-user to 404) |

---

### S-ERROR-FORMAT — Error response shape and proxy error shapes

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| P1 | 401 `GET /auth/me` without auth → body has `error` string |
| P2 | 404 `GET /workspaces/nonexistent` → body has `error` string |
| P3 | 400 `POST /auth/register` with empty body → body has `error` string |
| P4 | 400 `PUT /workspaces/:id` with missing `name` → body has `error` string |
| P5 | All `error` values are strings (not null, object, or array) |
| P6 | No Go runtime strings (`panic:`, `runtime error:`) in any `error` field |
| P7 | Proxy `workspace not ready` 503: body has `error`, `phase`, and `retryAfter` fields (verified against a workspace in non-Active phase) |
| P8 | Session ID validation: `GET /sessions/../../etc/passwd` → 400 with `error` field (path traversal blocked at API layer) |
| P9 | Successful 2xx responses do NOT have an `error` field |

---

### S-RATE-LIMIT — Auth endpoint rate limiting

**Schedule:** 5 min | **Max duration:** 60s

| # | Check |
|---|---|
| P1 | First login attempt returns 200 or 401 (not 429) |
| P2 | Rapid burst of >5 login attempts with wrong password → at least one returns 429 |
| P3 | 429 body has `error` field |
| N1 | `/readyz` and `/livez` are NOT rate-limited (must remain reachable) |

---

## 6. Scenario Specifications — Deep (Tier 2)

---

### D-WS-LIFECYCLE — Full lifecycle with status field verification and idempotency

**Schedule:** 5 min | **Max duration:** 300s

| # | Check |
|---|---|
| P1 | Create workspace, wait for `Active` |
| P2 | Status fields on Active workspace: `imageTag` non-empty, `agentHealth.agentVersion` non-empty, `conditions` array non-empty |
| P3 | Status `diskTotalBytes` > 0 (PVC mounted) |
| P4 | Status `agentHealth.status` = `"Healthy"` |
| P5 | `conditions` array contains an entry with `type=CredentialsAvailable` |
| P6 | `POST /suspend` → 202; phase → `Suspended` within 60s |
| P7 | Suspend already-Suspended workspace → 409 Conflict (asserted, not just recorded) |
| P8 | `POST /activate` → 200; phase → `Active` within 90s |
| P9 | Activate already-Active workspace → 200 (idempotent) |
| P10 | `POST /restart` → 202; workspace transitions and returns to `Active` within 120s |
| N1 | Suspend nonexistent workspace → error |
| N2 | Activate nonexistent workspace → error |
| N3 | Restart Terminating workspace → 409 |

---

### D-ACTIVATE-EVICTION — `activate` auto-evicts stalest workspace at cap

**Schedule:** 10 min | **Max duration:** 480s  
**Note:** Tests `POST /activate` (which enforces credential injection before resuming).

| # | Check |
|---|---|
| P1 | Create N workspaces where N = `workspace.maxActiveWorkspacesPerUser` (default 3), wait for all Active |
| P2 | `POST /activate` on a Suspended workspace (N+1 activation attempt) → 200 |
| P3 | Response `resumed` field = activated workspace ID |
| P4 | Response `suspended` field = the stalest workspace ID (auto-evicted) |
| P5 | The evicted workspace reaches `Suspended` or `Suspending` phase |
| P6 | The activated workspace reaches `Active` phase |
| N1 | At-cap with all workspaces in transitional phases (Creating/Resuming, not Active) → 409 with descriptive error |

---

### D-SESSION-ENSURE — Session ensure with auto-resume, list, rename, abort, individual GET

**Schedule:** 5 min | **Max duration:** 300s

| # | Check |
|---|---|
| P1 | Wait for workspace `Active` |
| P2 | `POST /sessions/new` → `sessionId`, `workspaceId`, `resumed` (bool) |
| P3 | `resumed = false` on first ensure (workspace was already Active) |
| P4 | Suspend workspace; wait `Suspended` |
| P5 | `POST /sessions/new` on Suspended workspace → auto-resumes → `resumed = true`, `workspacePhase = "Active"` |
| P6 | `GET /sessions` → array of `SessionListItem` with `id`, `messageCount`, `status` |
| P7 | `GET /sessions/active` → `maxActive` > 0, `active` array |
| P8 | `PUT /sessions/:id/title` → 204; `GET /sessions` shows updated `title` |
| P9 | `GET /workspaces/:id/sessions/:sessionId` → individual session object with `id` and `title` |
| P10 | `POST /sessions/:id/abort` (idle session) → 202/200, no error |
| P11 | Second `POST /sessions/new` → succeeds (idempotent or new session) |
| N1 | `POST /sessions/new` on nonexistent workspace → error |
| N2 | `PUT /sessions/:id/title` with empty title → 400 |
| N3 | `POST /sessions/new` on workspace in `Failed` phase → error |

---

### D-SESSION-MSG — Session message, verbose flag, and `lastActivityAt`

**Schedule:** 5 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | Send `"Reply with exactly: PONG"` → 200 |
| P2 | Response `parts` array present |
| P3 | SDK-extracted `content` non-empty |
| P4 | `GET /workspaces/:id/status` after message: `lastActivityAt` is non-null and recent (within last 60s) |
| P5 | `GET /sessions/:id/message` (default, no verbose) → `patch` type parts absent |
| P6 | `GET /sessions/:id/message?verbose=true` → `patch` type parts present (if the model produced any) |
| N1 | Send to nonexistent session → error |
| N2 | Send to session of workspace owned by different user → 404 or 403 |

---

### D-SESSION-HISTORY — Session history

**Schedule:** 5 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | History before any messages → 200, empty array |
| P2 | After 2 messages → history has ≥ 2 entries |
| P3 | Entries have parseable `parts` structure |
| N1 | History for nonexistent session → error |

---

### D-SESSION-TITLE — Auto-generated session title backfill

**Schedule:** 10 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | Send a substantive message (not just "PONG") |
| P2 | Wait up to 20s; `GET /sessions` shows non-empty `title` for the session |
| P3 | `GET /workspaces/:id/sessions/:sessionId` shows non-empty `title` |
| N1 | Title never appears within timeout → failure (not a warning) |

---

### D-SESSION-LIMIT — Active session limit (429) and connection limit (429)

**Schedule:** 10 min | **Max duration:** 480s

Two distinct 429 paths:

**A) Active session limit** (per-workspace concurrent LLM calls):
| # | Check |
|---|---|
| P1 | Fill active session slots by sending concurrent messages |
| P2 | Next concurrent message → 429 with `maxActiveSessions` and `retryAfter` fields |
| P3 | After abort, new message succeeds |

**B) Connection limit** (per-workspace concurrent proxy connections, limit = 10):
| # | Check |
|---|---|
| P4 | Open 11 concurrent `GET /events` SSE streams on same workspace |
| P5 | 11th connection → 429 with `retryAfter` field |

---

### D-SESSION-GET — Individual session GET endpoint

**Schedule:** 5 min | **Max duration:** 180s

Tests `GET /workspaces/:id/sessions/:sessionId` which proxies to opencode's `GET /session/:id`.

| # | Check |
|---|---|
| P1 | After ensure session → `GET /sessions/:sessionId` → 200 |
| P2 | Response has `id` matching the session |
| P3 | Response has `title` field (may be empty string pre-message) |
| P4 | After rename, `GET /sessions/:sessionId` shows new title |
| N1 | `GET /sessions/nonexistent-id` → error |
| N2 | `GET /sessions/../../etc/passwd` → 400 (path traversal validation at proxy layer) |

---

### D-PROMPT-ASYNC — `prompt_async` + SSE `session.idle` flow

**Schedule:** 5 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`  
**Critical:** This is the code path the MCP server uses internally. If it breaks, the MCP `session_message` tool breaks while all SDK tests pass.

| # | Check |
|---|---|
| P1 | `POST /sessions/:id/prompt_async` with `{"message": "Reply: ASYNC-OK"}` → 202 (immediate response) |
| P2 | Connect to `GET /events` (SSE stream) |
| P3 | Receive `session.status` event with `status=idle` and matching `session_id` within 60s |
| P4 | `GET /sessions/:id/message` → history contains the agent's response |
| P5 | Abort: `POST /sessions/:id/prompt_async` again, immediately `POST /sessions/:id/abort` → no hang |
| N1 | `POST /prompt_async` with malformed session ID → 400 |
| N2 | `POST /prompt_async` on workspace in non-Active phase → 503 with `phase` and `retryAfter` |

---

### D-AGENT-INPUT — Question and permission input flows

**Schedule:** 10 min | **Max duration:** 480s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`  
**Note:** Requires a prompt that triggers a tool-use permission. Skip gracefully if model doesn't trigger permissions for test prompt.

| # | Check |
|---|---|
| P1 | `GET /question` → 200, array (may be empty) |
| P2 | `GET /permission` → 200, array |
| P3 | Send message that triggers tool-use permission |
| P4 | `GET /permission` returns ≥ 1 pending permission with `id` |
| P5 | `POST /permission/:id/reply` with `{"reply": "once"}` → no error |
| P6 | After approval, session returns to idle (confirm via SSE or status) |
| N1 | `POST /question/:id/reply` with invalid ID format (not `que_...`) → 400 |
| N2 | `POST /permission/:id/reply` with invalid reply value (`"maybe"`) → 400 |
| N3 | `POST /permission/:id/reply` with invalid ID format (not `per_...`) → 400 |

---

### D-SESSION-SUBTASK — Subagent `parentId` backfill

**Schedule:** 15 min | **Max duration:** 600s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`  
**Note:** Requires a prompt that causes opencode to spawn a subtask agent (e.g. complex coding task with `task` tool). Skip gracefully if model doesn't use the task tool.

| # | Check |
|---|---|
| P1 | Send a message that triggers a subagent session |
| P2 | Wait up to 30s; `GET /sessions` contains a session with non-empty `parentId` |
| P3 | `parentId` references the top-level session's ID |
| N1 | A session with no parent has `parentId` absent or null (not an empty string) |

---

### D-TERMINAL — Terminal ticket generation

**Schedule:** 5 min | **Max duration:** 300s

| # | Check |
|---|---|
| P1 | `POST /terminal/ticket` → 200, `ticket` starts with `tkt_` |
| P2 | `ticket` length > 10 |
| P3 | `expiresAt` non-empty string |
| P4 | Two consecutive tickets are different (uniqueness) |
| N1 | Ticket for nonexistent workspace → error |
| N2 | Ticket for workspace owned by another user → error |

---

### D-CRED-BIND — Credential bind + reload + unbind + reload-empty

**Schedule:** 5 min | **Max duration:** 300s

| # | Check |
|---|---|
| P1 | Create workspace, wait `Active` |
| P2 | Create credential, bind → 204 |
| P3 | `GET /bindings` → credential ID present |
| P4 | `POST /reload-secrets` → 200 with `reloaded` integer ≥ 1 |
| P5 | Status `credentialState.available` = `true` after reload |
| P6 | Unbind (empty list) → 204; `GET /bindings` → empty |
| P7 | `POST /reload-secrets` after unbind → 200 with `reloaded: 0` (not an error) |
| P8 | Status `credentialState.available` after clearing → false or not-set |
| N1 | `POST /reload-secrets` on suspended workspace → 409 (`errNoRunningPod`) |

---

### D-MODEL-LIST-ANNOTATED — Model list with `currentModel`, `selected`, tier fields

**Schedule:** 5 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | `GET /models` → 200 with `models` array and `currentModel` string |
| P2 | Each model has `id`, `name`, `enabled`, `tier` (`"free"` or `"paid"`), `freeTier` (bool), `selected` (bool) |
| P3 | Exactly one model has `selected=true` and its `id` equals `currentModel` |
| P4 | After `PUT /model` changes model, `GET /models` shows new `currentModel` and updated `selected` |
| N1 | `GET /models` on nonexistent workspace → error |

---

### D-MODEL-SET — Set model and verify agent uses it

**Schedule:** 5 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | `PUT /model` with valid model → 204 |
| P2 | `GET /models` shows new model selected |
| P3 | Send message → agent responds (non-empty, no auth error) |
| N1 | `PUT /model` with empty `model` → 400 |
| N2 | `PUT /model` on nonexistent workspace → error |
| N3 | `PUT /model` with `LLMSAFESPACES_BAD_MODEL` → API accepts (validation deferred to agent) OR returns 400; either way, agent must not crash the workspace (verify phase still `Active` after) |

---

### D-CRED-MODEL-FLOW — Full: add credential → set model → call agent → reload session

**Schedule:** 10 min | **Max duration:** 600s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`, `LLMSAFESPACES_LLM_MODEL`  
**This is the flagship end-to-end scenario.**

| Step | Check |
|---|---|
| 1 | Create workspace, wait `Active` |
| 2 | Create `llm-provider` credential with real API key → `id`, `type=llm-provider` |
| 3 | Bind credential via `PUT /bindings` → 204 |
| 4 | `PUT /model` → 204 |
| 5 | `POST /sessions/new` → session ID |
| 6 | Send `"Reply with exactly: CRED-FLOW-OK"` → non-empty response |
| 7 | `GET /sessions/:id/message` → ≥ 1 entry |
| 8 | Create **second session** (simulates browser reload) → new session ID |
| 9 | Send `"Reply with exactly: AFTER-RELOAD"` to second session → non-empty response |
| 10 | DELETE credential → 204; absent from list |
| N1 | Send message before credential bound → agent error or timeout (record error, don't assert specific text) |

---

### D-SUSPEND-RESUME-SESSION — Session history survives suspend/resume

**Schedule:** 10 min | **Max duration:** 600s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | Create workspace, wait `Active` |
| P2 | Ensure session, send `"Reply with exactly: BEFORE"` → non-empty |
| P3 | History has ≥ 1 entry |
| P4 | Suspend; wait `Suspended` |
| P5 | Resume; wait `Active` |
| P6 | Ensure session → succeeds |
| P7 | Send `"Reply with exactly: AFTER"` → non-empty |
| P8 | History has ≥ 2 entries (BEFORE and AFTER messages both persisted) |

---

### D-ENV-INJECTION — Env var reaches agent and clears on unbind

**Schedule:** 10 min | **Max duration:** 480s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | Create workspace, wait `Active` |
| P2 | `PUT /env` with `{"vars": {"CANARY_INJECT": "canary-xyz"}}` → 204 |
| P3 | Ensure session, send `"Run: python3 -c 'import os; print(os.environ.get(\"CANARY_INJECT\", \"NOTFOUND\"))'"`|
| P4 | Agent response contains `canary-xyz` |
| P5 | `DELETE /env/CANARY_INJECT`, then `POST /reload-secrets` |
| P6 | Send same command again → response contains `NOTFOUND` (var cleared from running pod) |

---

### D-SSE-EVENTS — SSE broker delivers workspace phase and session events

**Schedule:** 10 min | **Max duration:** 300s

| # | Check |
|---|---|
| P1 | Connect to `GET /events` with `Accept: text/event-stream` → response has correct SSE headers |
| P2 | Trigger suspend while stream is open |
| P3 | Receive `workspace.phase` event with `phase=Suspending` or `phase=Suspended` within 30s |
| P4 | Resume workspace; receive `workspace.phase` event with `phase=Active` or intermediate phase |
| P5 | Send message (requires LLM key if set; otherwise trigger via abort): receive `session.status` with `status=idle` |
| P6 | Disconnect cleanly (no error from SSE client) |
| N1 | `GET /events` on nonexistent workspace → 404 |

---

### D-KEY-ROTATE — Encryption key rotation

**Schedule:** 15 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_PASSWORD` | **Uses:** `canary-rotate@llmsafespaces.test` account

| # | Check |
|---|---|
| P1 | Create a secret with known value |
| P2 | `POST /account/rotate-key` with correct password → 200 with `keyVersion` and `recoveryKey` |
| P3 | `recoveryKey` is non-empty string |
| P4 | `POST /secrets/:id/reveal` with same password after rotation → correct value (re-encryption succeeded) |
| N1 | `POST /account/rotate-key` with wrong password → 403 |
| N2 | `POST /account/rotate-key` with missing password → 400 |

---

### D-CHANGE-PASSWORD — Password change

**Schedule:** 15 min | **Max duration:** 120s  
**Requires:** `LLMSAFESPACES_PASSWORD` | **Uses:** `canary-rotate@llmsafespaces.test`

| # | Check |
|---|---|
| P1 | `POST /account/change-password` with correct `oldPassword` + valid `newPassword` (≥8 chars) → 204 |
| P2 | Login with new password → JWT returned |
| P3 | Login with old password → 401 |
| P4 | `POST /secrets/:id/reveal` with new password → value correct |
| P5 | Change back to original password (idempotency) |
| N1 | Wrong `oldPassword` → 403 |
| N2 | `newPassword` < 8 chars → 400 |

---

### D-ACCOUNT-RECOVER — Account recovery with recovery key

**Schedule:** 15 min | **Max duration:** 120s  
**Uses:** `canary-rotate@llmsafespaces.test`

| # | Check |
|---|---|
| P1 | `POST /account/rotate-key` to get a fresh `recoveryKey` |
| P2 | `POST /account/recover` with `userId`, `recoveryKey`, `newPassword` → 200 with new `recoveryKey` |
| P3 | Login with `newPassword` → JWT returned |
| P4 | Secret reveal with `newPassword` → correct value |
| N1 | `POST /account/recover` with invalid recovery key → 403 |
| N2 | `POST /account/recover` with missing fields → 400 |

---

## 7. Scenario Specifications — MCP Server

---

### S-MCP-TOOLS — Tool registration completeness

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| P1–P18 | Stable named subset present: the 13 base tools (incl. `run_resolve`, which replaced `session_question_reply`/`session_question_reject`/`session_permission_reply` per US-65.7) + `workflow_create`, `workflow_list`, `workflow_run`, `trigger_create`, `trigger_list` |
| P12 | All tools have non-empty `description` |
| P13 | All tools have `inputSchema.type = "object"` |
| P14 | **≥24 tools** (additive-tolerant floor; removals also caught by P1–P18 and pinned cross-module by `pkg/repolint` `TestCanary_MCPTools_Parity`) |

---

### S-MCP-AUTH-NEG — Invalid credentials propagation

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| N1 | `workspace_create` with bad API key → `isError=true`, message contains "401" or "unauthorized" |
| N2 | `credential_list` with bad key → `isError=true` |
| N3 | No JSON-RPC 2.0 level `error` objects — all failures surface as `isError=true` tool results |

---

### S-MCP-CRED — Credential tools CRUD

**Schedule:** 1 min | **Max duration:** 30s

| # | Check |
|---|---|
| P1 | `credential_create` with placeholder key → result JSON has `id`. **API-key auth**: the DEK-gated positive CRUD is skipped with `dek_unavailable` asserted (403 "encryption key not available" — documented architecture); full CRUD requires an interactive-login canary |
| P2 | `credential_list` → array contains created credential |
| P3 | `credential_delete` → result contains "deleted" |
| N1 | `credential_create` missing `kind` → `isError=true` |
| N2 | `credential_create` missing `slug` → `isError=true` |
| N3 | `credential_create` missing `api_key` → `isError=true` |
| N4 | `credential_delete` missing `credential_id` → `isError=true` |
| N5 | `credential_delete` nonexistent ID → `isError=true` |

---

### S-MCP-INPUT-NEG — Input validation

**Schedule:** 1 min | **Max duration:** 20s

| # | Check |
|---|---|
| N1 | `session_create` missing `workspace_id` → `isError=true` |
| N2 | `session_message` missing `workspace_id` → `isError=true` |
| N3 | `session_message` missing `session_id` → `isError=true` |
| N4 | `session_message` empty `message` → `isError=true` |
| N5 | `session_message` message > 1MB → `isError=true` with "too large" |
| N6 | `session_history` missing `workspace_id` → `isError=true` |
| N7 | `model_list` missing `workspace_id` → `isError=true` |
| N8 | `model_set` missing `model` → `isError=true` |

---

### D-MCP-WORKSPACE — Workspace lifecycle via MCP tools

**Schedule:** 5 min | **Max duration:** 300s

| # | Check |
|---|---|
| P1 | `workspace_create` with `runtime=base` → result is valid JSON with `id` |
| P2 | `workspace_activate` → result JSON has `resumed` field |
| P3 | `workspace_stop` → result text contains workspace ID |
| N1 | `workspace_create` missing `runtime` → `isError=true` |
| N2 | `workspace_activate` nonexistent ID → `isError=true` |
| N3 | `workspace_stop` nonexistent ID → `isError=true` |

---

### D-MCP-SESSION — Session + message via MCP tools

**Schedule:** 5 min | **Max duration:** 480s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | `session_create` with active workspace → result JSON has `id` |
| P2 | `session_message` with `"Reply with exactly: MCP-OK"` → non-empty result |
| P3 | `session_history` → result JSON is array with ≥ 1 entry |

---

### D-MCP-PROMPT-ASYNC — MCP `session_message` uses prompt_async + SSE internally

**Schedule:** 5 min | **Max duration:** 480s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`  
**Critical:** This verifies the MCP server's internal code path (not just the SDK's synchronous `sendMessage`).

| # | Check |
|---|---|
| P1 | `session_message` via MCP completes and returns non-empty text |
| P2 | Verify via direct API: `GET /sessions/:id/message` shows the same response in history |
| P3 | The SSE stream received `session.status {status: "idle"}` during the call (verified by monitoring SSE in parallel) |

---

### D-MCP-MODEL — Model list and set via MCP tools

**Schedule:** 5 min | **Max duration:** 300s  
**Requires:** `LLMSAFESPACES_LLM_API_KEY`

| # | Check |
|---|---|
| P1 | `model_list` with active workspace → non-empty result |
| P2 | `model_set` with valid model → result contains model name |
| N1 | `model_list` nonexistent workspace → `isError=true` |
| N2 | `model_set` nonexistent workspace → `isError=true` |

---

## 8. Fission Deployment Schedule

```
Every 1 min   →  S-HEALTH, S-AUTH, S-AUTH-CONFIG, S-LOGOUT, S-APIKEY,
                  S-USER-SETTINGS, S-WS-CRUD, S-WS-STATUS,
                  S-SECRET-CRUD, S-SECRET-REVEAL, S-SECRET-AUDIT, S-CRED-CRUD,
                  S-ERROR-FORMAT,
                  S-MCP-TOOLS, S-MCP-AUTH-NEG, S-MCP-CRED, S-MCP-INPUT-NEG

Every 5 min   →  S-WS-QUOTA, S-SECRET-BINDINGS, S-ENV-VARS, S-OWNERSHIP, S-RATE-LIMIT,
                  D-WS-LIFECYCLE, D-SESSION-ENSURE, D-SESSION-MSG, D-SESSION-HISTORY,
                  D-SESSION-GET, D-PROMPT-ASYNC, D-TERMINAL, D-CRED-BIND,
                  D-MODEL-LIST-ANNOTATED, D-MODEL-SET,
                  D-MCP-WORKSPACE, D-MCP-SESSION, D-MCP-PROMPT-ASYNC, D-MCP-MODEL

Every 10 min  →  D-ACTIVATE-EVICTION, D-SESSION-TITLE, D-SESSION-LIMIT,
                  D-AGENT-INPUT, D-CRED-MODEL-FLOW, D-SUSPEND-RESUME-SESSION,
                  D-ENV-INJECTION, D-SSE-EVENTS

Every 15 min  →  D-SESSION-SUBTASK, D-KEY-ROTATE, D-CHANGE-PASSWORD, D-ACCOUNT-RECOVER
```

**Alert policy:**
- Tier 1 (shallow): alert on 1st failure; page on 3rd consecutive.
- Tier 2 (deep): alert on 2nd consecutive failure; page on 4th.
- D-KEY-ROTATE / D-CHANGE-PASSWORD / D-ACCOUNT-RECOVER: alert on 2nd failure (these mutate credentials).

---

## 9. CI Integration

**Job name:** `sdk-canary` in `.github/workflows/ci.yml`

All `ci:fast` (Tier 1 shallow) scenarios run in CI. They do not wait for pod scheduling and do not require `LLMSAFESPACES_LLM_API_KEY`. They run against a real kind cluster (CRDs installed, no controller — and only the controller writes .status.phase, so workspace CRs have an EMPTY phase; CANARY_NO_CONTROLLER=1 is the honest env signal) so workspace-creating scenarios (S-WS-CRUD, S-WS-STATUS, S-WS-QUOTA, S-OWNERSHIP) can exercise the full CRUD path against the K8s API.

**ci:fast scenario list:** S-HEALTH, S-AUTH, S-AUTH-CONFIG, S-LOGOUT, S-APIKEY, S-USER-SETTINGS, S-WS-CRUD, S-WS-STATUS, S-WS-QUOTA, S-SECRET-CRUD, S-SECRET-REVEAL, S-SECRET-AUDIT, S-CRED-CRUD, S-OWNERSHIP, S-ERROR-FORMAT, S-MCP-TOOLS, S-MCP-AUTH-NEG, S-MCP-CRED, S-MCP-INPUT-NEG

**Running locally:**
```bash
# All ci:fast scenarios, all SDKs
make canary-ci

# Single scenario, Go SDK
LLMSAFESPACES_URL=http://localhost:8080 LLMSAFESPACES_API_KEY=lsp_... \
  go run ./sdks/canary/go/auth/

# Full canary suite (requires live cluster with LLM credentials)
make canary-all
```

---

## 10. Test Accounts

Three pre-provisioned accounts required in every deployment:

| Account | Purpose |
|---|---|
| `canary1@llmsafespaces.test` | Primary account for all single-user scenarios |
| `canary2@llmsafespaces.test` | Secondary account for S-OWNERSHIP |
| `canary-rotate@llmsafespaces.test` | Dedicated account for D-KEY-ROTATE, D-CHANGE-PASSWORD, D-ACCOUNT-RECOVER (these mutate credentials; isolated to prevent interference with canary1) |

All accounts are non-admin. The rotate account has a known fixed password stored in a Kubernetes Secret and is reset to the original value at the end of each mutation scenario.

---

## 11. What Is Explicitly Out of Scope

| Excluded | Reason |
|---|---|
| User registration | Explicitly requested out of scope |
| Admin credential set management (`/admin/credentials`) | Requires admin account |
| Admin settings (`/admin/settings`) | Requires admin account |
| WebSocket terminal session (`ws://`) | Fission functions cannot hold long-lived WebSocket connections; covered by e2e nightly |
| Load / throughput / performance testing | Separate benchmark suite (`hack/benchmark-*.sh`) |
| Database migration correctness | Covered by `migration-safety` CI workflow |
| Kubernetes CRD schema validation | Covered by `helm-render` CI step |
| Security / penetration testing | Handled by dedicated security tooling |

---

## 12. Changelog

| Version | Date | Changes |
|---|---|---|
| 1.0 | 2026-06-03 | Initial plan — 33 scenarios |
| 2.0 | 2026-06-03 | Added two-tier model; added 18 missing workflows (logout/revocation, auth/config, secrets audit, key rotation, account recovery, question/permission flow, verbose flag, SSE events, user settings, rate limiting, session limit, env injection) |
| 3.0 | 2026-06-03 | Added 20 additional gaps: proxy error shapes (`workspace not ready` 503, path traversal 400), storage validation (size too large, invalid format), `imageTag`/`agentVersion` in list, `activate` auto-eviction flow, `ensure-session` auto-resume assertion, status fields (`imageTag`, disk/memory/context, `conditions`, `lastActivityAt`), session `parentId` backfill, schema version drift detection, idempotency cases (re-create after delete, double bind, reload-empty), suspend-already-suspended as 409 assertion, resume-already-active idempotency, `GET /sessions/:sessionId` individual endpoint, `prompt_async`+SSE as first-class scenario, connection limit 429, `D-MCP-PROMPT-ASYNC` to catch MCP-specific path breakage |

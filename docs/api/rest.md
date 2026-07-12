# REST API

The LLMSafeSpaces REST API is a JSON-over-HTTP surface served by the stateless Gin API service. All endpoints are versioned under `/api/v1`. There are roughly 90 routes covering authentication, workspace lifecycle, session proxying, secret management, credentials, settings, account operations, and (optionally) the self-hosted relay fleet.

This page is a reference for every endpoint. For authentication specifics (JWT vs API keys, token revocation, rate limiting), see [Authentication](authentication.md). For programmatic access, see the [SDKs](sdks.md) or the [MCP server](mcp.md).

## Conventions

- **Base URL.** Every path below is relative to the API root. The full URL is `{base}/api/v1/...`. In the examples we use `$API` for the base (e.g. `http://localhost:8080`).
- **Content type.** Request and response bodies are `application/json`. The two WebSocket endpoints (terminal, SSE) are the only exceptions.
- **Auth.** Most endpoints require `Authorization: Bearer <jwt-or-api-key>`. Public endpoints are noted explicitly.
- **Errors.** Non-2xx responses carry `{"error": "<message>"}`. Binding failures return a generic `{"error": "invalid request body"}` so internal struct details don't leak.
- **`?verbose=true`.** By default, the proxy strips `type=="patch"` parts from message and history responses — opencode emits a ~2 KB patch part per assistant turn listing touched workspace files. Append `?verbose=true` on any message or history request to receive the unfiltered response. The flag is consumed by the proxy and not forwarded to the agent.

!!! info "SSE and WebSocket endpoints"
    Server-Sent Events (`/events`, `/session-events`) and the WebSocket terminal cannot be exercised with plain `curl` POST/GET in a useful way. Use a client that speaks SSE (e.g. `EventSource`, `httpx-sse`) or WebSocket for these.

---

## Auth

Authentication endpoints live under `/api/v1/auth`. The public routes (`config`, `register`, `login`, `logout`, SSO start/callback) have no `AuthMiddleware`; the API-key CRUD routes do.

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/auth/config` | Public | Feature flags: registration enabled, OIDC enabled, instance name, message of the day |
| `POST` | `/auth/register` | Public | Create a user; returns `{token, user}`. Gated by Turnstile CAPTCHA when enabled. |
| `POST` | `/auth/login` | Public | Email + password login; returns `{token, user}` |
| `POST` | `/auth/logout` | Public | Revoke the JWT (jti-based), clear the session cookie |
| `GET` | `/auth/me` | JWT/API key | Current user info |
| `POST` | `/auth/api-keys` | JWT/API key | Create a new `lsp_…` API key |
| `GET` | `/auth/api-keys` | JWT/API key | List the caller's API keys (secret stripped) |
| `DELETE` | `/auth/api-keys/:id` | JWT/API key | Revoke an API key |

Register returns a JWT directly so a client can start using the API immediately. Login is identical in shape. Both set the `lsp_session` HttpOnly cookie alongside the JSON body.

=== "curl"

    ```bash
    # Register (returns a JWT in the body + sets the lsp_session cookie)
    curl -sX POST "$API/api/v1/auth/register" \
      -H "Content-Type: application/json" \
      -d '{"email":"alice@example.com","password":"hunter2hunter2","username":"alice"}'

    # Login and capture the token
    TOKEN=$(curl -sX POST "$API/api/v1/auth/login" \
      -H "Content-Type: application/json" \
      -d '{"email":"alice@example.com","password":"hunter2hunter2"}' \
      | jq -r '.token')
    ```

=== "Go"

    ```go
    client := llmsafespaces.New("http://localhost:8080",
        llmsafespaces.WithAPIKey("lsp_..."),
    )
    me, err := client.Auth.Me(ctx)
    ```

=== "TypeScript"

    ```typescript
    const client = new LLMSafeSpaces({
      baseUrl: 'http://localhost:8080',
      credentials: { email: 'alice@example.com', password: 'hunter2hunter2' },
    });
    const me = await client.auth.me();
    ```

See [Authentication](authentication.md) for the full security model: bcrypt password hashing, email-enumeration prevention, jti-based revocation, rate limiting, account lockout, and the optional Turnstile CAPTCHA on `/register`.

---

## Workspaces

Workspaces are the core resource. Every workspace owns a pod running `opencode serve` and a PVC-backed filesystem at `/workspace`. List/Create sit on the `workspaceGroup` (auth only); every `/:id` route inherits `WorkspaceAccessMiddleware`, the single ownership gate.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/workspaces` | List the caller's workspaces (`?limit=20&offset=0`) |
| `POST` | `/workspaces` | Create a workspace |
| `GET` | `/workspaces/:id` | Get one workspace |
| `PUT` | `/workspaces/:id` | Rename a workspace |
| `DELETE` | `/workspaces/:id` | Delete a workspace and its PVC |
| `POST` | `/workspaces/:id/suspend` | Suspend (retain PVC, delete pod) |
| `POST` | `/workspaces/:id/activate` | Activate (resume if suspended; auto-suspend oldest if at capacity cap) |
| `POST` | `/workspaces/:id/restart` | Restart the workspace pod (declarative; bumps `restartGeneration`) |
| `POST` | `/workspaces/:id/refresh-compute` | Re-sync resource defaults + latest image version and rebuild the pod |
| `POST` | `/workspaces/:id/agent/reload` | Hot-reload agent credentials without a pod restart |
| `GET` | `/workspaces/:id/status` | Phase, conditions, credential state, agent health |
| `GET` | `/workspaces/:id/models` | List available models (requires active pod) |
| `PUT` | `/workspaces/:id/model` | Set the default model |

`POST /workspaces/:id/activate` is the unified entry point for both initial activation and resume-from-suspended. There is no separate `/resume` route — it was removed because it bypassed credential injection. `restart` and `refresh-compute` both bump `spec.restartGeneration`; the controller observes the change and rebuilds the pod.

```bash
# Create a workspace
WS=$(curl -sX POST "$API/api/v1/workspaces" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-workspace","runtime":"base","storageSize":"1Gi"}' \
  | jq -r '.id')

# Activate and poll until Active
curl -sX POST "$API/api/v1/workspaces/$WS/activate" -H "Authorization: Bearer $TOKEN"

while [ "$(curl -s -H "Authorization: Bearer $TOKEN" \
    "$API/api/v1/workspaces/$WS/status" | jq -r .phase)" != "Active" ]; do
  sleep 2
done
```

Per-user workspace quotas are enforced at create time when `LLMSAFESPACES_MAX_WORKSPACES_PER_USER` is set (returns `429` with `{"error":"workspace quota exceeded","limit":N}`).

---

## Session management

Sessions are conversation handles inside a workspace. The management endpoints live in the API's own database; the *proxy* endpoints (message, history, etc.) are reverse-proxied to the workspace pod's `opencode serve` instance on port 4096.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/workspaces/:id/sessions` | List sessions (with backfill from the agent) |
| `POST` | `/workspaces/:id/sessions/new` | Ensure an active session exists |
| `PUT` | `/workspaces/:id/sessions/:sessionId/title` | Rename a session |
| `PUT` | `/workspaces/:id/sessions/:sessionId/seen` | Mark a session as seen |
| `GET` | `/workspaces/:id/sessions/active` | List active session IDs + max capacity |

The list endpoint backfills `parent_session_id` from the agent one-shot per process lifetime, and overlays the live active-session set so the returned sessions reflect which are currently running.

### Sessions proxied to opencode

These are reverse-proxied to the workspace pod. The proxy injects HTTP basic auth for opencode automatically — the caller never sees opencode's credentials.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/workspaces/:id/sessions/:sessionId/message` | Send a message; wait for the assistant reply |
| `POST` | `/workspaces/:id/sessions/:sessionId/prompt` | Send a message asynchronously (`204 No Content`) |
| `POST` | `/workspaces/:id/sessions/:sessionId/queue` | Enqueue a message |
| `GET` | `/workspaces/:id/sessions/:sessionId/queue` | List queued messages |
| `DELETE` | `/workspaces/:id/sessions/:sessionId/queue/:messageId` | Delete a queued message |
| `GET` | `/workspaces/:id/sessions/:sessionId/message` | Fetch session history |
| `GET` | `/workspaces/:id/sessions/:sessionId` | Get a single session |
| `POST` | `/workspaces/:id/sessions/:sessionId/abort` | Abort a running session |
| `DELETE` | `/workspaces/:id/sessions/:sessionId` | Delete a session |
| `GET` | `/workspaces/:id/session-events` | SSE event stream (session-scoped) |

!!! warning "`message` blocks until the agent replies"
    `POST .../message` waits for the full assistant response. LLM calls can take 30–120+ seconds. Use `.../prompt` for fire-and-forget delivery and consume results via `GET .../message` (history) or the SSE stream.

```bash
SID=$(curl -sX POST "$API/api/v1/workspaces/$WS/sessions/new" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  | jq -r '.sessionId')

curl -X POST "$API/api/v1/workspaces/$WS/sessions/$SID/message" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": {"providerID":"litellm","modelID":"default"},
    "parts": [{"type":"text","text":"Reply with exactly the word: PONG"}]
  }' \
  | jq '.parts[] | select(.type=="text") | .text'
# → "PONG"
```

---

## Questions & Permissions

The agent can ask the caller a question or request permission for an action (file write, shell command). These endpoints list pending requests and reply to them. They are reverse-proxied to the workspace pod.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/workspaces/:id/question` | List pending agent questions |
| `POST` | `/workspaces/:id/question/:requestID/reply` | Answer a question |
| `POST` | `/workspaces/:id/question/:requestID/reject` | Reject a question |
| `GET` | `/workspaces/:id/permission` | List pending permission requests |
| `POST` | `/workspaces/:id/permission/:requestID/reply` | Reply to a permission request |

Question request IDs start with `que_`; permission request IDs start with `per_`.

---

## Events

The user-scoped SSE stream and the bulk agent reload endpoint.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/events` | User-scoped SSE event stream |
| `POST` | `/users/me/agents/reload` | Bulk reload agent credentials across all pending workspaces |

The SSE stream is exempt from the token-bucket rate limiter (long-lived connection).

---

## Secrets

The encrypted at rest secret store. Secrets are encrypted with a per-user DEK (AES-256-GCM) derived from the password. Values are never returned by list/get — only by the explicit `reveal` endpoint, which is audit-logged.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/secrets` | Create an encrypted secret |
| `GET` | `/secrets` | List secrets (metadata only, never values) |
| `GET` | `/secrets/audit` | Get the secret audit log |
| `GET` | `/secrets/:id` | Get secret metadata |
| `PUT` | `/secrets/:id` | Update a secret value |
| `DELETE` | `/secrets/:id` | Delete a secret |
| `POST` | `/secrets/:id/reveal` | Decrypt and reveal the secret value |
| `GET` | `/secrets/:id/bindings` | Get the secret's workspace bindings |

### Workspace bindings

Binding a secret to a workspace makes it available to that workspace's pod. Bindings are managed per-workspace.

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/workspaces/:id/bindings` | Set which secrets are bound to a workspace |
| `GET` | `/workspaces/:id/bindings` | List bound secrets |
| `POST` | `/workspaces/:id/reload-secrets` | Live-reload secrets into the workspace pod |

```bash
# Create an LLM provider credential as an encrypted secret
SECRET_ID=$(curl -sX POST "$API/api/v1/secrets" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-llm-key",
    "type": "llm-provider",
    "value": "{\"providerID\":\"litellm\",\"apiKey\":\"sk-...\",\"baseURL\":\"https://your-llm-gateway/v1\"}"
  }' | jq -r '.id')

# Bind it to a workspace
curl -sX PUT "$API/api/v1/workspaces/$WS/bindings" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"secretIds\":[\"$SECRET_ID\"]}"
```

---

## Workspace environment

Per-workspace environment variables, injected into the workspace pod alongside bound secrets.

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/workspaces/:id/env` | Set workspace environment variables |
| `GET` | `/workspaces/:id/env` | Get workspace environment variables |
| `DELETE` | `/workspaces/:id/env/:name` | Delete a workspace environment variable |

---

## Terminal

A proxied shell into the workspace pod. The flow is two-step: obtain a one-time ticket (a short-lived JWT), then open the WebSocket with that ticket. Ticket-based auth is by design — the ticket was issued after `WorkspaceAccessMiddleware` verified ownership, so the WebSocket (registered on the root router, not behind `AuthMiddleware`) inherits the ownership check.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/workspaces/:id/terminal/ticket` | Get a terminal ticket (JWT) |
| `GET` | `/workspaces/:id/terminal` | WebSocket terminal proxy |

---

## Admin provider credentials

Admin-owned LLM provider credential sets, plus auto-apply rules that bind them to targets automatically. All routes require `AuthMiddleware` + `AdminGuard`.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/admin/provider-credentials` | Create an admin credential set |
| `GET` | `/admin/provider-credentials` | List admin credential sets |
| `GET` | `/admin/provider-credentials/:id` | Get one admin credential set |
| `PUT` | `/admin/provider-credentials/:id` | Update an admin credential set |
| `DELETE` | `/admin/provider-credentials/:id` | Delete an admin credential set |
| `GET` | `/admin/provider-credentials/:id/models` | Probe available models for the credential |
| `POST` | `/admin/provider-credentials/:id/auto-apply` | Create an auto-apply rule |
| `GET` | `/admin/provider-credentials/:id/auto-apply` | List auto-apply rules |
| `DELETE` | `/admin/provider-credentials/:id/auto-apply/:targetType/:targetId` | Delete an auto-apply rule |

---

## User provider credentials

Per-user LLM provider credentials with explicit workspace bindings. The credential's secret value is encrypted at rest; only metadata is returned by list/get.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/provider-credentials` | Create a user credential |
| `GET` | `/provider-credentials` | List user credentials |
| `GET` | `/provider-credentials/:id` | Get one user credential |
| `GET` | `/provider-credentials/:id/models` | Probe available models for the credential |
| `DELETE` | `/provider-credentials/:id` | Delete a user credential |
| `GET` | `/provider-credentials/:id/bindings` | List the credential's workspace bindings |
| `POST` | `/provider-credentials/:id/bind/:workspaceId` | Bind a credential to a workspace |
| `DELETE` | `/provider-credentials/:id/bind/:workspaceId` | Unbind a credential from a workspace |

There is also a credential-free model probe endpoint for the credential form (fetch models before saving):

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/probe-models` | Anonymous model probe (authenticated, credential-free) |

---

## Settings

Instance (admin) and user settings, each with a schema endpoint. Admin routes require `AuthMiddleware` + `AdminGuard`.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/settings` | Get all instance settings (admin only) |
| `GET` | `/admin/settings/schema` | Get the settings schema (admin only) |
| `PUT` | `/admin/settings/:key` | Update an instance setting |
| `GET` | `/users/me/settings` | Get the current user's settings |
| `GET` | `/users/me/settings/schema` | Get the user settings schema |
| `PUT` | `/users/me/settings/:key` | Update a user setting |

Settings are declarative: each key has a schema-defined type, default, and tier (e.g. Helm-managed read-only, admin-mutable, user-mutable). Helm-managed overrides are promoted to read-only and return `409` on PUT.

---

## Account

Key rotation, password change, and account recovery.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/account/rotate-key` | Rotate the encryption key |
| `POST` | `/account/change-password` | Change password (re-wraps the DEK) |
| `POST` | `/account/recover` | Recover an account |

There is also the soft-unlock endpoint (Epic 56) for re-deriving the DEK without forcing logout:

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/auth/unlock-dek` | Soft-unlock: re-derive the DEK under the matched JWT signing key |

---

## Relay fleet (admin)

The operator setup wizard and status dashboard for the self-hosted multi-cloud relay fleet (Epic 42/48). These routes are only registered when the relay admin handler is wired (`controller.inferenceRelay.enabled`), which also requires `rbac.scope=cluster` because `InferenceRelay` is cluster-scoped. All routes require `AuthMiddleware` + `AdminGuard`.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/relay/setup` | Prerequisite checklist (router deployed, CRD installed, provider creds present) |
| `GET` | `/admin/relay/status` | Fleet health + per-VM observed state |
| `POST` | `/admin/relay/aws-creds` | Store AWS provider credentials (as a Secret) |
| `POST` | `/admin/relay/oci-creds` | Store OCI provider credentials (as a Secret) |
| `POST` | `/admin/relay/gcp-creds` | Store GCP provider credentials (as a Secret) |
| `POST` | `/admin/relay/deploy` | Create/reconcile the `InferenceRelay` CR |
| `POST` | `/admin/relay/rotate/:id` | Destroy + reprovision a relay VM |
| `POST` | `/admin/relay/pause` | Pause fleet reconciliation |
| `POST` | `/admin/relay/resume` | Resume fleet reconciliation |

See [Inference Relay Fleet](../operator/inference-relay.md) for the architecture and when to choose the self-hosted fleet over the default direct-to-Zen mode.

---

## Organizations

Org-scoped management routes. Member routes require `OrgMemberGuard`; admin routes require `OrgAdminGuard`. SSO config and domain verification live here (see [OIDC SSO](../operator/oidc-sso.md)).

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/orgs` | Auth | Create an organization |
| `GET` | `/orgs` | Auth | List the caller's organizations |
| `GET` | `/orgs/:id` | Member | Get an org |
| `GET` | `/orgs/:id/workspaces` | Member | List org workspaces |
| `GET` | `/orgs/:id/members` | Member | List members |
| `GET` | `/orgs/:id/invitations` | Member | List pending invitations |
| `PUT` | `/orgs/:id` | Org admin | Update an org |
| `DELETE` | `/orgs/:id` | Org admin | Delete an org |
| `POST` | `/orgs/:id/members` | Org admin | Add a member |
| `DELETE` | `/orgs/:id/members/:userID` | Org admin | Remove a member |
| `PUT` | `/orgs/:id/members/:userID` | Org admin | Change a member's role |
| `POST` | `/orgs/:id/members/:userID/verify` | Org admin | Force-verify a member's email |
| `GET/PUT/DELETE` | `/orgs/:id/sso` | Org admin | Org OIDC SSO config CRUD |
| `POST` | `/orgs/:id/sso/domains/:domain/verify` | Org admin | On-demand DNS verification of a claimed domain |
| `POST` | `/orgs/:id/sso/verification-token/rotate` | Org admin | Rotate the DNS verification token |
| `GET/POST/PUT/DELETE` | `/orgs/:id/credentials[/:credID]` | Org admin | Org provider credential CRUD + model probe + auto-apply |
| `GET/PUT/DELETE` | `/orgs/:id/policies[/:key]` | Org admin (feature-gated) | Org policies (Business+ plan) |
| `GET` | `/orgs/:id/audit` | Org admin (feature-gated) | Org audit log (Business+ plan) |
| `GET/PUT` | `/orgs/:id/prompt` | Member/admin | Org agent prompt |
| `GET/POST/PUT/DELETE` | `/orgs/:id/agent-roles[/:roleId]` | Org admin | Org agent roles |

Public invitation routes (the token is the credential):

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/invitations/:token` | Public | Fetch an invitation by token |
| `POST` | `/invitations/:token/accept` | Auth | Accept an invitation |
| `POST` | `/invitations/:token/decline` | Auth | Decline an invitation |

---

## Public OIDC SSO

Login-discovery and the PKCE login flow. All anonymous — the token (or PKCE code) is the credential. See [OIDC SSO](../operator/oidc-sso.md) for the full flow.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/auth/sso/domains` | List all orgs' verified domains (for login-page routing) |
| `GET` | `/auth/sso/:orgSlug/start` | Begin PKCE flow; 302 to the IdP, sets a signed state cookie |
| `GET` | `/auth/sso/:orgSlug/callback` | Complete the flow; sets the `lsp_session` JWT cookie, 302 to the frontend |
| `POST` | `/auth/lookup` | Email-led login discovery; returns a single `redirectUrl` (enumeration-safe) |

---

## Usage & billing

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/usage` | Auth | Current user's usage |
| `GET` | `/usage/workspaces/:id` | Auth | Per-workspace usage |
| `GET` | `/usage/quota` | Auth | Quota status |
| `GET` | `/admin/usage/:ownerId` | Admin | Cross-user usage view |
| `GET` | `/admin/billing/status` | Admin | Billing status |
| `GET` | `/admin/billing/dlq` | Admin | Billing dead-letter queue |
| `POST` | `/admin/billing/dlq/:id/retry` | Admin | Retry a DLQ entry |
| `POST` | `/admin/billing/dlq/:id/discard` | Admin | Discard a DLQ entry |
| `POST` | `/webhooks/stripe` | Public (Stripe-signed) | Stripe webhook |

---

## Platform admin

Platform-wide admin operations behind `AuthMiddleware` + `AdminGuard`.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/admin/orgs` | List all orgs (dashboard) |
| `GET` | `/admin/users` | List all users (dashboard) |
| `POST` | `/admin/orgs/:id/suspend` | Suspend an org |
| `POST` | `/admin/orgs/:id/unsuspend` | Unsuspend an org |
| `POST` | `/admin/users/:id/suspend` | Suspend a user |
| `POST` | `/admin/users/:id/unsuspend` | Unsuspend a user |
| `GET` | `/admin/audit` | Cross-org audit view |
| `GET` | `/admin/workspaces/:workspaceId/sessions/:sessionId/force-abort` | Force-abort a stuck session |
| `GET/PUT` | `/admin/prompt` | Platform-wide system prompt |
| `GET/POST` | `/admin/agent-roles[/:id]` | Platform agent roles |
| `POST` | `/admin/email/test` | Admin email test-send |

---

## Infrastructure

Unauthenticated health and metrics endpoints. `/metrics` requires `Authorization: Bearer <token>` only when `LLMSAFESPACES_METRICS_TOKEN` is set (opt-in auth).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/livez` | Liveness probe — process is responding (includes build version) |
| `GET` | `/health` | Legacy alias for `/livez` |
| `GET` | `/readyz` | Readiness probe — verifies Postgres + Redis are reachable; `503` on failure |
| `GET` | `/metrics` | Prometheus metrics |

`/readyz` returns only a generic component status (`database: unreachable`) — detailed errors are logged server-side to avoid leaking connection strings.

## Internal endpoints

These are cluster-internal and not meant for user traffic. They are gated by shared secrets or Kubernetes `TokenReview`, not JWTs.

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/internal/orgs/:orgID/status` | `X-Internal-Token` shared secret (fail-closed 403 when unset) | Cluster-internal org status polled by the controller to drive org-suspension |
| `POST` | `/internal/v1/pod-bootstrap` | Kubernetes `TokenReview` | Secretless credential injection for workspace init containers (Epic 35) |

---

## Next

- [Authentication](authentication.md) — JWT and API key mechanics, revocation, rate limiting
- [MCP server](mcp.md) — drive workspaces from any MCP-compatible client
- [SDKs](sdks.md) — typed clients for Go, TypeScript, Python, and Java

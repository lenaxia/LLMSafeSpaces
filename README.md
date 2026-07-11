# LLMSafeSpaces

A Kubernetes-first platform for running AI agents in isolated, persistent workspaces.

Each workspace runs [`opencode serve`](https://opencode.ai/docs/server/) — a headless HTTP server that drives an LLM agent — backed by a PVC-mounted filesystem at `/workspace`. The LLMSafeSpaces API service is a stateless reverse proxy in front of the workspace pods, with auth, ownership checks, encrypted secret management, and quality-of-life filtering.

Repository: `github.com/lenaxia/llmsafespaces`

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│  Clients (REST / SSE / MCP)                                         │
│         │                                                            │
│         ▼                                                            │
│  ┌──────────────────────────────────────────────────────────────┐   │
  │  │  LLMSafeSpaces API (Gin, stateless, horizontally scalable)    │   │
  │  │  - Auth (JWT + API keys + HttpOnly cookies)                   │   │
  │  │  - Workspace CRUD + lifecycle (activate/suspend/restart)      │   │
  │  │  - Reverse proxy to workspace pods (basic auth, IP refresh)   │   │
  │  │  - Secrets management (encrypted at rest store)              │   │
  │  │  - Provider credentials (admin + user, auto-apply rules)     │   │
  │  │  - Settings (admin instance + user preferences)              │   │
  │  │  - Session management, SSE events, terminal proxy            │   │
  │  │  - Patch-part filtering (?verbose=true to keep)              │   │
│  └─────────────────────┬────────────────────────────────────────┘   │
│                        │ K8s API                                     │
│                        ▼                                             │
│  ┌──────────────────────────────────────────────────────────────┐   │
  │  │  Controller (controller-runtime)                              │   │
  │  │  - Reconciles Workspace CRD (pod lifecycle, PVC, credentials) │   │
  │  │  - Validating webhooks for Workspace + RuntimeEnvironment     │   │
  │  │  - Health monitoring via workspace-agentd sidecar             │   │
│  └─────────────────────┬────────────────────────────────────────┘   │
│                        │                                             │
│                        ▼                                             │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │  Workspace Pods (one per active Workspace CRD)                │   │
│  │  - init: workspace-setup + credential-setup                   │   │
│  │  - main: opencode serve --hostname 0.0.0.0 --port 4096        │   │
│  │  - sidecar: workspace-agentd (health probes, session metadata)│   │
│  │  - mounts: PVC at /workspace, secret as /sandbox-cfg          │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  ┌─────────────────┐  ┌─────────────────┐                           │
│  │ PostgreSQL      │  │ Redis / Valkey  │                           │
│  │ (users, keys,   │  │ (rate limit,    │                           │
│  │  secrets,       │  │  cache, lockout,│                           │
│  │  settings)      │  │  DEK cache)     │                           │
│  └─────────────────┘  └─────────────────┘                           │
└──────────────────────────────────────────────────────────────────────┘
```

### Custom Resource Definitions

Three CRDs in the `llmsafespaces.dev/v1` API group:

| Kind | Scope | Short | Purpose |
|------|-------|-------|---------|
| `Workspace` | Namespaced | `ws` | PVC-backed persistent environment + pod running `opencode serve` |
| `RuntimeEnvironment` | Cluster | `rte` | Mapping from runtime name → container image |
| `InferenceRelay` | Cluster | `irelay` | Managed fleet of relay VMs (AWS/OCI/GCP) that proxy free-tier inference — feature-gated, see [Inference Relay](#inference-relay) |

### Lifecycle

```
Workspace: Pending → Creating → Active → Suspending → Suspended → Resuming → Active
                       │                   ↘           ↘           ↘
                       │                     Terminating            Terminating
                       │                         ↘                     ↘
                       └──→ Failed            Terminated             Terminated
```

Nine phases: `Pending`, `Creating`, `Active`, `Suspending`, `Suspended`, `Resuming`, `Terminating`, `Terminated`, `Failed`.

Suspending a workspace deletes the pod but retains the PVC. Activating a suspended workspace re-creates the pod, which reattaches to the existing PVC so opencode session history (stored in `/workspace/.local/opencode`) survives suspend/activate.

### Inference Relay

Free-tier LLM inference (opencode Zen models) is reached through a relay so workspace pods never hold the upstream secret. Two interchangeable deployments, selected by the configured relay URL:

- **Cloudflare Worker relay** (`workers/inference-relay/`) — a single stateless Worker; the simplest path and the default (`inferenceRelayURL`).
- **Self-hosted multi-cloud fleet** (Epic 42, `InferenceRelay` CRD) — the controller provisions and health-checks relay VMs across AWS (paid primary), OCI (free secondary), and optionally GCP. Workspace pods route through the in-cluster **relay-router** (`cmd/relay-router/`), which distributes traffic across healthy relay VMs over HTTP with per-VM token auth and falls back to direct upstream access when all VMs are down. Each VM runs **relay-proxy** (`cmd/relay-proxy/`), distributed to VMs via cloud-init with SHA-256 verification. Feature-gated behind `controller.inferenceRelay.enabled`; requires `rbac.scope=cluster` because `InferenceRelay` is cluster-scoped.

See `design/stories/epic-42-multi-cloud-inference-relay/README.md`.

---

## REST API

All endpoints are JSON. Authentication is via `Authorization: Bearer <jwt-or-api-key>`.

### Auth

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/auth/config` | Feature flags (registration enabled, OIDC, instance name, MOTD) |
| `POST` | `/api/v1/auth/register` | Create a user, returns `{token, user}` |
| `POST` | `/api/v1/auth/login` | Returns `{token, user}` on valid credentials |
| `POST` | `/api/v1/auth/logout` | Revoke JWT, clear cookie |
| `GET` | `/api/v1/auth/me` | Current user info |
| `POST` | `/api/v1/auth/api-keys` | Create a new `lsp_…` API key |
| `GET` | `/api/v1/auth/api-keys` | List the caller's API keys (secret stripped) |
| `DELETE` | `/api/v1/auth/api-keys/:id` | Revoke an API key |

### Workspaces

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/workspaces` | List the caller's workspaces (paginated) |
| `POST` | `/api/v1/workspaces` | Create a workspace |
| `GET` | `/api/v1/workspaces/:id` | Get one workspace |
| `PUT` | `/api/v1/workspaces/:id` | Rename a workspace |
| `DELETE` | `/api/v1/workspaces/:id` | Delete (and its PVC) |
| `POST` | `/api/v1/workspaces/:id/suspend` | Suspend (retain PVC, delete pod) |
| `POST` | `/api/v1/workspaces/:id/activate` | Activate (resume if suspended, auto-suspend oldest if at cap) |
| `POST` | `/api/v1/workspaces/:id/restart` | Restart the workspace pod |
| `GET` | `/api/v1/workspaces/:id/status` | Get phase + conditions + credential state + agent health |
| `POST` | `/api/v1/workspaces/:id/agent/reload` | Hot-reload agent credentials without pod restart |

### Session Management

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/workspaces/:id/sessions` | List sessions (with backfill from agent) |
| `POST` | `/api/v1/workspaces/:id/sessions/new` | Ensure an active session exists |
| `PUT` | `/api/v1/workspaces/:id/sessions/:sessionId/title` | Rename a session |
| `PUT` | `/api/v1/workspaces/:id/sessions/:sessionId/seen` | Mark session as seen |
| `GET` | `/api/v1/workspaces/:id/sessions/active` | List active session IDs + max capacity |

### Sessions (proxied to opencode)

These endpoints are reverse-proxied to the workspace pod's `opencode serve` instance on port 4096. The proxy injects HTTP basic auth for opencode automatically.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/workspaces/:id/sessions/:sessionId/message` | Send a message; wait for the assistant reply |
| `POST` | `/api/v1/workspaces/:id/sessions/:sessionId/prompt` | Send a message asynchronously (`204 No Content`) |
| `GET` | `/api/v1/workspaces/:id/sessions/:sessionId/message` | Fetch session history |
| `GET` | `/api/v1/workspaces/:id/sessions/:sessionId` | Get a single session |
| `POST` | `/api/v1/workspaces/:id/sessions/:sessionId/abort` | Abort a running session |
| `DELETE` | `/api/v1/workspaces/:id/sessions/:sessionId` | Delete a session |
| `GET` | `/api/v1/workspaces/:id/session-events` | SSE event stream (session-scoped) |

### Questions & Permissions (proxied to opencode)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/workspaces/:id/question` | List pending agent questions |
| `POST` | `/api/v1/workspaces/:id/question/:requestID/reply` | Answer a question |
| `POST` | `/api/v1/workspaces/:id/question/:requestID/reject` | Reject a question |
| `GET` | `/api/v1/workspaces/:id/permission` | List pending permission requests |
| `POST` | `/api/v1/workspaces/:id/permission/:requestID/reply` | Reply to a permission request |

### Events

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/events` | User-scoped SSE event stream |
| `POST` | `/api/v1/users/me/agents/reload` | Bulk reload agent credentials |

### Secrets

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/secrets` | Create an encrypted secret |
| `GET` | `/api/v1/secrets` | List secrets (metadata only, never values) |
| `GET` | `/api/v1/secrets/audit` | Get audit log |
| `GET` | `/api/v1/secrets/:id` | Get secret metadata |
| `PUT` | `/api/v1/secrets/:id` | Update secret value |
| `DELETE` | `/api/v1/secrets/:id` | Delete a secret |
| `POST` | `/api/v1/secrets/:id/reveal` | Decrypt and reveal secret value |
| `GET` | `/api/v1/secrets/:id/bindings` | Get secret's workspace bindings |
| `PUT` | `/api/v1/workspaces/:id/bindings` | Set which secrets are bound to a workspace |
| `GET` | `/api/v1/workspaces/:id/bindings` | List bound secrets |
| `POST` | `/api/v1/workspaces/:id/reload-secrets` | Live-reload secrets into workspace pod |

### Workspace Environment

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/api/v1/workspaces/:id/env` | Set workspace environment variables |
| `GET` | `/api/v1/workspaces/:id/env` | Get workspace environment variables |
| `DELETE` | `/api/v1/workspaces/:id/env/:name` | Delete a workspace environment variable |
| `GET` | `/api/v1/workspaces/:id/models` | List available models for workspace |
| `PUT` | `/api/v1/workspaces/:id/model` | Set default model for workspace |

### Terminal

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/workspaces/:id/terminal/ticket` | Get a terminal ticket (JWT) |
| `GET` | `/api/v1/workspaces/:id/terminal` | WebSocket terminal proxy |

### Admin Provider Credentials

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/admin/provider-credentials` | Create admin credential set |
| `GET` | `/api/v1/admin/provider-credentials` | List admin credential sets |
| `GET` | `/api/v1/admin/provider-credentials/:id` | Get one admin credential set |
| `PUT` | `/api/v1/admin/provider-credentials/:id` | Update an admin credential set |
| `DELETE` | `/api/v1/admin/provider-credentials/:id` | Delete an admin credential set |
| `POST` | `/api/v1/admin/provider-credentials/:id/auto-apply` | Create auto-apply rule |
| `GET` | `/api/v1/admin/provider-credentials/:id/auto-apply` | List auto-apply rules |
| `DELETE` | `/api/v1/admin/provider-credentials/:id/auto-apply/:targetType/:targetId` | Delete auto-apply rule |

### User Provider Credentials

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/provider-credentials` | Create a user credential |
| `GET` | `/api/v1/provider-credentials` | List user credentials |
| `GET` | `/api/v1/provider-credentials/:id` | Get one user credential |
| `DELETE` | `/api/v1/provider-credentials/:id` | Delete a user credential |
| `GET` | `/api/v1/provider-credentials/:id/bindings` | List credential's workspace bindings |
| `POST` | `/api/v1/provider-credentials/:id/bind/:workspaceId` | Bind credential to workspace |
| `DELETE` | `/api/v1/provider-credentials/:id/bind/:workspaceId` | Unbind credential from workspace |

### Settings

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/admin/settings` | Get all instance settings (admin only) |
| `GET` | `/api/v1/admin/settings/schema` | Get settings schema (admin only) |
| `PUT` | `/api/v1/admin/settings/:key` | Update an instance setting |
| `GET` | `/api/v1/users/me/settings` | Get current user's settings |
| `GET` | `/api/v1/users/me/settings/schema` | Get user settings schema |
| `PUT` | `/api/v1/users/me/settings/:key` | Update a user setting |

### Account

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/account/rotate-key` | Rotate encryption key |
| `POST` | `/api/v1/account/change-password` | Change password |
| `POST` | `/api/v1/account/recover` | Recover account |

### Relay Fleet (admin)

Operator setup wizard + status dashboard for the self-hosted multi-cloud relay fleet (Epic 42 / 48). Only registered when the relay admin handler is wired.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/admin/relay/setup` | Prerequisite checklist (router deployed, CRD installed, provider creds present) |
| `GET` | `/api/v1/admin/relay/status` | Fleet health + per-VM observed state |
| `POST` | `/api/v1/admin/relay/aws-creds` | Store AWS provider credentials (Secret) |
| `POST` | `/api/v1/admin/relay/oci-creds` | Store OCI provider credentials (Secret) |
| `POST` | `/api/v1/admin/relay/gcp-creds` | Store GCP provider credentials (Secret) |
| `POST` | `/api/v1/admin/relay/deploy` | Create/reconcile the `InferenceRelay` CR |
| `POST` | `/api/v1/admin/relay/rotate/:id` | Destroy + reprovision a relay VM |
| `POST` | `/api/v1/admin/relay/pause` | Pause fleet reconciliation |
| `POST` | `/api/v1/admin/relay/resume` | Resume fleet reconciliation |

#### `?verbose=true` flag

By default, the proxy strips parts of `type=="patch"` from message and history responses. opencode emits a `patch` part for every assistant turn, listing every workspace file it touched (~2 KB per response of internal snapshot paths). For most clients this is noise.

Pass `?verbose=true` on any message or history request to receive the unfiltered response:

```
POST /api/v1/workspaces/ws-1/sessions/ses_xyz/message?verbose=true
```

The `verbose` query parameter is consumed by the API proxy and is not forwarded to opencode.

---

## Quickstart

### 1. Authenticate

```bash
API=http://localhost:8080

# Register a new user (returns a JWT)
curl -X POST "$API/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"hunter2hunter2","username":"alice"}'

# Or, login if already registered
TOKEN=$(curl -sX POST "$API/api/v1/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"hunter2hunter2"}' \
  | jq -r '.token')
```

### 2. Create a workspace

```bash
WS=$(curl -sX POST "$API/api/v1/workspaces" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-workspace","runtime":"base","storageSize":"1Gi"}' \
  | jq -r '.id')
echo "workspace: $WS"
```

### 3. Store an LLM provider credential

Create a secret with your LLM provider API key, then bind it to the workspace:

```bash
SECRET_ID=$(curl -sX POST "$API/api/v1/secrets" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-llm-key",
    "type": "llm-provider",
    "value": "{\"providerID\":\"litellm\",\"apiKey\":\"sk-...\",\"baseURL\":\"https://your-llm-gateway/v1\"}"
  }' | jq -r '.id')

curl -sX PUT "$API/api/v1/workspaces/$WS/bindings" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"secretIds\":[\"$SECRET_ID\"]}"
```

See the Secrets API above for full credential management.

### 4. Activate the workspace

```bash
curl -X POST "$API/api/v1/workspaces/$WS/activate" \
  -H "Authorization: Bearer $TOKEN"

# Wait for it to come up
while [ "$(curl -s -H "Authorization: Bearer $TOKEN" \
    "$API/api/v1/workspaces/$WS/status" | jq -r .phase)" != "Active" ]; do
  sleep 2
done
```

### 5. Drive a session

```bash
# Create a session
SID=$(curl -sX POST "$API/api/v1/workspaces/$WS/sessions/new" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  | jq -r '.sessionId')

# Send a prompt
curl -X POST "$API/api/v1/workspaces/$WS/sessions/$SID/message" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model":   {"providerID":"litellm","modelID":"default"},
    "parts":   [{"type":"text","text":"Reply with exactly the word: PONG"}]
  }' \
  | jq '.parts[] | select(.type=="text") | .text'

# → "PONG"
```

### 6. Suspend / activate

Suspending the workspace deletes the pod but keeps the PVC. Activating re-creates the pod. Session history (stored in the PVC) survives.

```bash
curl -X POST "$API/api/v1/workspaces/$WS/suspend" \
  -H "Authorization: Bearer $TOKEN"

curl -X POST "$API/api/v1/workspaces/$WS/activate" \
  -H "Authorization: Bearer $TOKEN"
```

---

## Repository Layout

```
api/                     # Go API service (Gin) + MCP server
  cmd/api/               # API server entrypoint
  internal/
    handlers/            # Reverse proxy, secrets, settings, credentials, activity, events
    middleware/          # Auth, rate limit, CORS, security, validation, admin guard, etc.
    services/            # Auth, Workspace, Database, Cache, RateLimit, Metrics, SessionIndex
    server/router.go     # Gin route table
    mocks/               # Service mocks for tests

cmd/
  workspace-agentd/      # Sidecar binary for workspace pods (health probes, session metadata, secret reload)
  relay-router/          # In-cluster HTTP router distributing traffic across relay VMs (Epic 42)
  relay-proxy/           # Reverse proxy binary run on each relay VM, token-gated (Epic 42)
  mcp/                   # MCP server entrypoint (imports pkg/mcp)
  redact/                # Redact binary entrypoint (imports pkg/redact)
  repolint/              # Repository layout linter (imports pkg/repolint)
  seal-key/              # Key sealing utility (AES-256-GCM passphrase wrapping)

controller/              # Kubernetes operator (controller-runtime)
  internal/
    workspace/           # Workspace reconciler (pod lifecycle, PVC, credentials, health)
    relay/               # InferenceRelay reconciler + AWS/OCI/GCP drivers, cloud-init, health, rotation (Epic 42)
    webhooks/            # Validating webhooks (Workspace, RuntimeEnvironment, per-tenant quota)
    common/              # Leader election, metrics, utilities

frontend/                # React 19 + TypeScript + Vite SPA

runtimes/                # Container images
  base/                  # opencode + redact + workspace-agentd + entrypoints
  python/, nodejs/, go/  # Language-specific extensions

pkg/                     # Shared Go packages
  apis/llmsafespaces/v1/  # CRD Go types (Workspace, RuntimeEnvironment)
  agent/                 # Agent runtime abstraction and registry (opencode, claude-code, codex)
  agentd/                # Workspace-agentd sidecar types and in-pod secret materializer
  secrets/               # encrypted secret store (key wrapping, encryption, audit)
  settings/              # Declarative settings schema + services
  kubernetes/            # K8s client with leader election + typed CRD access
  mcp/                   # MCP server + client
  redact/                # Secret redaction pipeline
  repolint/              # Repository layout linter (migration numbering, CRD drift)
  validation/            # Shared validation (secret names)
  types/                 # API DTOs

helm/     # Helm chart (API, controller, frontend, CRDs, RBAC, webhooks)
sdks/                    # Client SDKs (Go, TypeScript, Python, Java, VS Code extension)
workers/inference-relay/ # Cloudflare Worker relay for free-tier inference (the simpler alternative to the self-hosted InferenceRelay fleet)
local/                   # bootstrap.sh, test.sh, teardown.sh for kind
design/                  # Architecture and design docs (EVOLUTION-V2.md is authoritative)
```

---

## Development

### Prerequisites

- Go 1.25+
- Docker
- A Kubernetes cluster (or `kind`) and `kubectl`
- Helm 3 (for the deployment chart)
- Node.js 22+ (for the frontend)

### Run all tests

```bash
go test -timeout 90s -race ./...
```

### Local end-to-end on kind

```bash
cd local

# Bootstrap a kind cluster, build images, deploy LLMSafeSpaces
./bootstrap.sh

# Run the e2e suite (9 tests). Set LLM_* env vars to enable the prompt
# round-trip and patch-part stripping checks.
LLM_BASE_URL=https://your-llm/v1 \
LLM_API_KEY=sk-... \
LLM_MODEL=default \
./test.sh

# Tear down
./teardown.sh
```

### Build container images

```bash
# API
docker build -f api/Dockerfile -t llmsafespaces/api:dev .

# Controller
docker build -f controller/Dockerfile -t llmsafespaces/controller:dev .

# Base runtime (opencode + redact + workspace-agentd + entrypoints)
docker build -f runtimes/base/Dockerfile -t llmsafespaces/runtime-base:dev runtimes/base

# Frontend
docker build -f frontend/Dockerfile -t llmsafespaces/frontend:dev frontend

# relay-router (in-cluster; run via the Helm chart's relay-router Deployment)
docker build -f cmd/relay-router/Dockerfile -t llmsafespaces/relay-router:dev cmd/relay-router
```

The relay-proxy binary for relay VMs is cross-compiled (not containerized) — `make relay-bin` produces `deploy/relay-proxy-{arm64,amd64}` for cloud-init distribution.

CI builds and pushes these to `ghcr.io/lenaxia/llmsafespaces/{api,controller,base,frontend}:dev` on every push to `main` (see `.github/workflows/ci.yml`).

---

## Security

- **Pod hardening**: read-only root, `runAsNonRoot`, drop all capabilities, no privilege escalation, AppArmor + seccomp profiles
- **Container-runtime isolation**: optional gVisor (`runsc`) RuntimeClass (Epic 51) for kernel-level isolation of tenant pods; enabled via `gvisor.defaultRuntimeClass`, with per-workspace opt-out via `spec.runtimeClass`
- **Per-tenant resource quotas**: validating admission webhook (Epic 51 S51.2) keyed on the `llmsafespaces.dev/tenant` pod label — caps workspaces / CPU / memory per tenant; disabled by default
- **Encrypted secret store**: user secrets encrypted with per-user DEK (AES-256-GCM), derived from password via HKDF-SHA256. Platform never stores plaintext.
- **Master KEK delivery**: the server root key (root of trust for credential encryption) is projected as a read-only file mount (`/var/run/secrets/llmsafespaces/master-secret`), not an env var, eliminating `/proc/1/environ` exposure (Epic 50 US-50.1). Legacy env delivery remains as an opt-in for non-Helm deploys.
- **Workspace credentials** stored exclusively as Kubernetes Secrets — never in PostgreSQL, Redis, or logs
- **Egress filtering** via NetworkPolicies (configurable per Workspace)
- **API hardening**: rate limiting (Redis-backed, configurable via admin settings), account lockout, restrictive CORS defaults, JWT cache hashing, no token-in-query-string
- **Secret redaction**: 16-rule regex pipeline (`pkg/redact`) used by the runtime to scrub credentials from agent stdout
- **Audit logging**: every secret operation recorded in append-only audit log

See `design/0027_2026-05-24_security-policy-v21.md`, `design/stories/epic-50-master-kek-hardening/README.md`, and `design/0021_2026-05-21_evolution-v2.md` §9 for the full threat model.

---

## License

LLMSafeSpaces is licensed under the **GNU Affero General Public License v3.0
or later** (AGPL-3.0-or-later). See [LICENSE](LICENSE) and [NOTICE](NOTICE).

The AGPL requires that anyone who runs a modified version of this software
as a network service make the corresponding source code available to its
users. If you self-host LLMSafeSpaces for your own internal use, this is
unlikely to affect you.

### Commercial license

A commercial license is available for organizations that cannot or do not
wish to comply with the AGPL — for example, those who wish to incorporate
LLMSafeSpaces into a proprietary product, or offer it as a hosted service
without releasing the corresponding source.

Commercial licensing inquiries: **safespace@47north.lat**

Copyright (C) 2026 Michael Kao.

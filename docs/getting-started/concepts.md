# Concepts

This page describes the LLMSafeSpaces data model in depth: the core entities, their relationships, the ownership model, and how the control plane (Kubernetes CRDs) and data plane (PostgreSQL) divide responsibility.

## The core idea

Every AI agent runs in its own isolated, persistent Kubernetes pod. The platform is the orchestration layer around that pod — it manages lifecycle, security, connectivity, and credentials. The agent itself (`opencode serve`) is the workload; LLMSafeSpaces is not a generic code-execution sandbox.

## Entities at a glance

```
User ─┬─ owns ── Workspace ─┬─ runs ──── Session(s) ── messages ── Agent
      │                      │
      └─ member of ─ Org ────┘
                          │
                          └─ owns (optional)

Workspace ── has ── WorkspaceOwner{UserID, OrgID}
Workspace ── binds ── Secret(s)        (user-owned, encrypted)
Workspace ── binds ── Credential(s)    (user/provider-owned LLM keys)
Workspace ── runs in ── RuntimeEnvironment (cluster-scoped image mapping)
```

---

## User

A registered account. Stored in PostgreSQL.

- Identified by a stable user ID.
- Has an email (unique, lowercased), username, and a bcrypt-hashed password.
- Has a role (`user` or `admin`). Admins can reach `/admin/*` routes guarded by `AdminGuard`.
- Owns a per-user DEK (data encryption key) used to encrypt their secrets. The DEK is derived from the password and never stored in plaintext.
- Can be suspended by a platform admin (`status == suspended`), which blocks login and SSO.

A user owns zero or more workspaces and can be a member of zero or more organizations.

## Organization

A tenant grouping. Stored in PostgreSQL.

- Has members with roles (`admin` or `member`) enforced by `OrgMemberGuard` and `OrgAdminGuard`.
- Can own workspaces (via `WorkspaceOwner.OrgID`).
- Has its own LLM provider credentials (`/orgs/:id/credentials`).
- Can configure per-org OIDC SSO (`org_sso_configs`) so its members log in via the org's identity provider. See [OIDC SSO](../operator/oidc-sso.md).
- Can have org-level policies, prompts, and agent roles.

Organizations are optional — a user can own workspaces personally without an org. When an org owns a workspace, both the user (creator or org admin) and the org are recorded so access checks can pass.

## Workspace

The central resource. A workspace is:

- A **Kubernetes `Workspace` CRD** (namespaced, `ws`) reconciled by the controller — the source of truth for phase, pod IP, PVC name, and conditions.
- A **PostgreSQL metadata row** — the source of truth for the display name and query-side optimizations.
- A **pod** running `opencode serve` on port 4096 (when Active).
- A **PVC-mounted filesystem** at `/workspace`, `/home/sandbox`, and `/tmp` that persists across pod restarts.

### WorkspaceOwner

Every workspace has a `WorkspaceOwner{UserID, OrgID}`. This is the single ownership identity the platform enforces:

- If `OrgID` is set, the workspace is org-owned. The creator (a member) and org admins can access it.
- If `OrgID` is empty, the workspace is user-owned. Only that user can access it.

`WorkspaceAccessMiddleware` is the single gate: every `/:id` workspace route inherits it, and it verifies the caller matches the owner before the handler runs. List and Create bypass it (List is scoped per-user inside the service; Create has no target yet).

### Lifecycle

```
Pending → Creating → Active → Suspending → Suspended → Resuming → Active
                      │                   ↘           ↘           ↘
                      │                     Terminating            Terminating
                      │                         ↘                     ↘
                      └──→ Failed            Terminated             Terminated
```

Nine phases. **Suspend** deletes the pod but retains the PVC. **Activate** re-creates the pod and re-attaches the PVC (~22 seconds measured). Session history in `/workspace/.local/opencode` survives suspend/resume. **Terminate** deletes both pod and PVC.

### Capacity

The number of concurrently Active sessions per workspace is capped (`workspace.defaultMaxActiveSessions`, default 5). Activating a workspace when the cap is reached auto-suspends the oldest active workspace.

---

## RuntimeEnvironment

A cluster-scoped CRD (`rte`) that maps a runtime **name** to a **container image**. When a workspace is created with `runtime: "base"`, the controller resolves the name to the image defined by the matching `RuntimeEnvironment`.

This indirection lets operators upgrade runtime images (new Python version, security patch) without touching workspace specs. Refresh-compute re-resolves the runtime to the latest image version and rebuilds the pod.

See [Runtime Environments](../operator/runtime-environments.md) for configuration.

---

## Session

A conversation handle inside a workspace. Sessions are the unit of agent interaction.

- Created via `POST /workspaces/:id/sessions/new` (idempotent "ensure an active session exists").
- Belong to exactly one workspace.
- Have a title (user-settable) and a seen-state.
- A workspace can have multiple sessions; the number of concurrently **active** sessions is capped.
- History is stored in the PVC at `/workspace/.local/opencode` and fetched by reverse-proxying to the workspace pod's `opencode serve`.

The management endpoints (list, ensure, rename, mark-seen, active) live in the API's PostgreSQL. The proxy endpoints (message, prompt, history, abort, delete) are reverse-proxied to the workspace pod on port 4096 — the proxy injects HTTP basic auth for opencode automatically.

A session can be in one of these states relative to the agent:

- **Active** — currently running an agent turn.
- **Idle** — exists, no turn in flight.
- **Deleted** — removed.

The API overlays the live active-session set (from the pod) onto the persisted session list so callers see which sessions are currently running.

---

## Secret

A user-owned encrypted value stored in the zero-knowledge secret store.

- Encrypted with the user's DEK (AES-256-GCM), derived from the password via HKDF-SHA256.
- The platform never stores plaintext. Values are returned **only** by the explicit `POST /secrets/:id/reveal` endpoint, which is audit-logged.
- List/get return metadata only — name, type, ID, timestamps.
- Every secret operation is recorded in an append-only audit log (`GET /secrets/audit`).

### Secret types

The `type` field selects how the secret is materialized into the workspace pod. The most common is `llm-provider` — a JSON blob containing `providerID`, `apiKey`, and optional `baseURL`. Other types cover SSH keys, environment-secrets (`env`), and arbitrary values.

### Binding

A secret is **bound** to a workspace to make it available there. Bindings are per-workspace (`PUT /workspaces/:id/bindings` with a list of secret IDs). Bound secrets are decrypted into tmpfs at `/sandbox-runtime` when the workspace pod boots, and can be live-reloaded without a pod restart (`POST /workspaces/:id/reload-secrets`).

Secrets can also be marked `global_default` — automatically included in all new workspaces for that owner.

---

## Credential

An LLM provider credential. LLMSafeSpaces distinguishes three ownership tiers:

| Tier | Owner | Endpoint prefix | Use |
|------|-------|-----------------|-----|
| **Admin** | Platform admin | `/admin/provider-credentials` | Platform-wide credentials, auto-applied to targets via rules |
| **Org** | Organization | `/orgs/:id/credentials` | Shared across an org's workspaces |
| **User** | User | `/provider-credentials` | Personal credentials, bound to specific workspaces |

Credentials are encrypted at rest and decrypted into the workspace pod's `agent-config.json` at boot. They carry:

- A `kind` (adapter selector: `openai`, `anthropic`, `google`, `openai_compatible`, ...).
- A `slug` (per-owner unique identity, becomes the provider key).
- The API key and optional base URL / default model.

Admin credentials support **auto-apply rules** that bind them to targets (users, orgs, workspaces) automatically — useful for giving every workspace access to a shared gateway.

### How credentials and secrets relate

Secrets (the `Secret` entity) and credentials (the `Credential` entity) overlap but serve different paths:

- **Secrets** (`/secrets`) are the general encrypted store. An `llm-provider` secret is the legacy/low-level way to supply an LLM key.
- **Provider credentials** (`/provider-credentials`, `/admin/provider-credentials`, `/orgs/:id/credentials`) are the higher-level, typed abstraction with bindings and auto-apply. This is the path the MCP server and SDKs use.

Both end up materialized into the workspace pod's `agent-config.json` — the difference is the management surface and ownership semantics.

---

## How API keys and secrets relate to users

API keys and secrets both belong to a user, but they are different things:

- An **API key** (`lsp_…`) authenticates API calls on behalf of a user. It is SHA-256 hashed at rest and identifies the caller. See [Authentication](../api/authentication.md).
- A **secret** is an encrypted value the user stores (an LLM key, an SSH key, an env var). It is encrypted with the user's DEK and is data, not a credential for the API itself.

A user typically has: one password (bcrypt), zero or more API keys, one DEK (derived from the password), and zero or more secrets. Changing the password re-wraps the DEK (`POST /account/change-password`); rotating the encryption key re-wraps all encrypted data (`POST /account/rotate-key`).

---

## State management: Kubernetes vs PostgreSQL

The platform deliberately splits state between two stores. Understanding which is authoritative for what is essential.

| Data | Owner | Source of truth |
|------|-------|-----------------|
| Workspace phase | Controller | Kubernetes CRD status |
| PVC name, pod IP | Controller | Kubernetes CRD status |
| Conditions | Controller | Kubernetes CRD status |
| `status.lastActivityAt` (workspace) | API server (batched, ≤60s flush) | Kubernetes CRD status |
| Workspace display name | API | PostgreSQL |
| User ID ownership | Both | K8s CRD (`spec.owner`) authoritative; PostgreSQL mirrors for query perf |
| Creation/update timestamps | Both | K8s CRD authoritative; PostgreSQL mirrors |
| Credentials | Controller | Kubernetes Secrets (never PostgreSQL, Redis, or logs) |
| User auth data (passwords, API keys, DEKs) | API | PostgreSQL |
| Encrypted secrets | API | PostgreSQL (zero-knowledge encrypted) |
| Settings | API | PostgreSQL |

The rule of thumb: **lifecycle and cluster state live in Kubernetes; user data and metadata live in PostgreSQL; credentials live only in Kubernetes Secrets.**

!!! warning "Credentials never touch PostgreSQL"
    Workspace credentials are stored exclusively as Kubernetes Secrets — never in PostgreSQL, Redis, or logs. This is a hard security boundary. The controller reads them and injects them into the workspace pod via tmpfs; the API never persists them.

---

## CRD type ownership

CRD types exist in two locations with strictly separate roles — they must not be merged:

| Location | Purpose |
|----------|---------|
| `pkg/apis/llmsafespaces/v1/` | **Authoritative** kubebuilder-annotated CRD types (`Workspace`, `RuntimeEnvironment`, `InferenceRelay`), used by both controller and API service |
| `pkg/types/` | **API transfer objects only** — REST request/response shapes (`CreateWorkspaceRequest`, etc.). Not CRD schemas. |

The API types are transfer objects; the CRD types are Kubernetes schemas. They are intentionally different types.

## Next

- [Quickstart](quickstart.md) — see the data model in action
- [REST API](../api/rest.md) — operate on these entities
- [Architecture](../architecture/index.md) — how the components fit together

# Epic 29: Handler Decomposition & Agent Client Abstraction

**Status:** Planning
**Created:** 2026-06-04
**Priority:** Medium (quality/maintainability — no user-facing behavior change)
**Depends on:** Epic 27a (credential reload foundation — shipped)

> **Scope correction (2026-08-09, Epic 65):** US-29.1's goal — "extract `AgentClient` interface for all opencode HTTP calls" — is **superseded by Epic 65** (Agent Session Contract). Epic 65's `Adapter` interface (US-65.3, `design/0049` §4.6) folds the existing `AgentRuntime` + `Dialect` + the would-be `AgentClient` into a single platform-owned seam, and US-65.5 deletes the proxy hacks US-29.1 was meant to consolidate. **Do not build `AgentClient` — it is throwaway work that US-65.3 rewrites.** The remaining stories (US-29.2–29.8: handler split, auth-enforcing mocks, Basic-auth contract test, constructor injection) are orthogonal maintainability cleanup and stay in this epic, independent of Epic 65.

---

## Problem Statement

`SecretsHandler` has grown to 29 methods across 2 files, handling 6 distinct responsibilities: secret CRUD, binding management, workspace env, model selection, audit, and credential state. It has 9 struct fields injected via 8 setter methods. This violates SRP, makes testing fragile (tests must wire all dependencies even for single-method tests), and creates a "god handler" that every new feature gravitates toward.

Additionally, all code that talks to opencode (port 4096) must handle Basic auth, but this is done inconsistently: `proxy.go` has its own K8s-secret-based password cache; `models.go` now has a `passwordGetter` function; the opencode client in `pkg/agent/opencode` has a constructor-injected password. There is no shared abstraction.

---

## Goals

1. **Split `SecretsHandler`** into focused, single-responsibility handlers
2. **Extract `AgentClient` interface** for all direct opencode communication
3. **Establish auth-enforcing test mocks** as the default (no permissive mocks for opencode calls)
4. **Add contract test** ensuring every handler calling port 4096 sends Basic auth

---

## User Stories

| Story | Title | Description |
|---|---|---|
| US-29.1 | Extract `AgentClient` interface | Define interface for all opencode HTTP calls (ListModels, PatchConfig, Dispose, GetSessionStatuses, StageCredentials). Single implementation wrapping password + HTTP. |
| US-29.2 | Split SecretsCRUDHandler | Move CreateSecret, ListSecrets, GetSecret, UpdateSecret, DeleteSecret, RevealSecret, GetAuditLog to dedicated handler. |
| US-29.3 | Split BindingsHandler | Move GetBindings, SetBindings, GetSecretBindings, ReloadSecrets, pushSecretsToAgent to dedicated handler. |
| US-29.4 | Split WorkspaceEnvHandler | Move SetWorkspaceEnv, GetWorkspaceEnv, DeleteWorkspaceEnv to dedicated handler. |
| US-29.5 | Split ModelsHandler | Move ListModels, SetModel to dedicated handler with `AgentClient` dependency. |
| US-29.6 | Auth-enforcing mock infrastructure | Create `testutil.AuthEnforcingServer` that all handler tests importing opencode mocks must use. Add CI lint rule that forbids `httptest.NewServer` without auth enforcement for port 4096 tests. |
| US-29.7 | Contract test: all opencode callers send Basic auth | A single test that enumerates every handler route that proxies to port 4096 and verifies the request includes `Authorization: Basic ...`. |
| US-29.8 | Constructor injection migration | Replace setter methods with required constructor parameters. Handlers that lack a required dependency fail loudly at startup (app.go), not silently at request time. |

---

## Design Principles

1. **Each handler owns exactly one responsibility** — CRUD, bindings, env, models, audit
2. **Dependencies are explicit at construction** — no nil-safe fallback; fail at boot if not wired
3. **A single `AgentClient` handles all opencode auth** — password retrieval, caching, Basic auth header injection
4. **Test mocks enforce the real contract** — auth-enforcing handlers are the only way to mock opencode in tests
5. **No behavior change** — this is a pure refactor; API routes, request/response shapes unchanged

---

## AgentClient Interface (US-29.1)

```go
// AgentClient abstracts all direct opencode HTTP communication.
// All methods handle Basic auth internally using the workspace password.
type AgentClient interface {
    ListModels(ctx context.Context, workspaceID string) ([]byte, error)
    PatchConfig(ctx context.Context, workspaceID string, config map[string]any) error
    DisposeInstance(ctx context.Context, workspaceID string) error
    GetSessionStatuses(ctx context.Context, workspaceID string) ([]SessionStatus, error)
    StageCredentials(ctx context.Context, workspaceID string, providers []Provider) error
}
```

The implementation resolves `podIP` and `password` internally from injected resolvers, keeping callers clean.

---

## Success Criteria

1. `SecretsHandler` no longer exists — replaced by 4-5 focused handlers
2. Every handler has ≤5 dependencies, all required at construction
3. Auth-enforcing mocks are the only opencode mock pattern in the test suite
4. Contract test catches any future handler that calls opencode without auth
5. All existing API tests pass unchanged (routes, behavior identical)

---

## Out of Scope

- API route changes
- New features
- Frontend changes
- CRD/controller changes

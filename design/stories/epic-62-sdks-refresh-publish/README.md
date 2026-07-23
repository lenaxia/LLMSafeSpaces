# Epic 62: SDK Refresh, API-Surface Parity & Publish

**Status:** Planning
**Created:** 2026-07-22
**Priority:** High
**Depends On:** Epic 14 (delivered the initial SDKs; this epic closes its residual gaps), Epic 41 (session queue subsystem shipped, not yet surfaced in SDKs), Epic 12 (usage endpoints shipped), Epic 16 (question/permission endpoints shipped)
**Related epics:**
- **Epic 14** — initial hand-written SDKs (TS/Python/Go/Java) + VS Code extension. The master stories README (`design/stories/README.md:46`) records four open gaps against it; this epic closes the three SDK-related gaps (US-14.4 async, US-14.6 Java facade, US-14.7 contract CI) and the drift that has accumulated since. US-14.9 (VS Code slash commands) remains out of scope.
- **Epic 52** (US-52.10) — canary coverage of the SDK-facing API. This epic's contract-test story (US-62.9) complements it: US-52.10 expands the canary scenarios; US-62.9 adds the `/message` blocking contract that the canary layer does not currently prove.

---

## Problem Statement

The four hand-written SDKs in `sdks/{python,typescript,go,java}/` have drifted
badly from the API server since Epic 14 shipped. The drift is not uniform:
**Go** is the most complete and serves as the de-facto reference; **TypeScript**
is close behind; **Python** is missing entire service groups; **Java** is a
generic `get/post/delete` wrapper with no typed facade at all. Meanwhile the
API server (`api/internal/server/router.go`) has grown ~25 routes that **no
SDK exposes** (and another ~20 that some SDKs expose but others don't), and
the canonical `sdks/openapi.yaml` (documented in
`sdks/README.md:22` as the single source of truth) is missing most of them —
so even if the Makefile's `generate-all` target were wired up, the generated
SDKs would inherit the gaps.

An external project attempting to integrate against the Python SDK surfaced
this concretely: their design rested on assumptions (no version pinning, no
token reporting, unknown `send_message` blocking semantics) that were either
wrong on the specifics (the SDK *does* have a version in `pyproject.toml`;
the platform *does* meter tokens server-side) or correct-but-unprovable
(the SDK's `resp.json()` parse against a streaming proxy is an unvalidated
contract). The root cause in every case was the same: **the SDK is stale,
undocumented in its current state, and not published to a registry.**

### Drift inventory (verified against `router.go` and `openapi.yaml`)

Routes marked **[gated]** are feature-gated behind handler nil-checks in `router.go` — they exist only when the server is configured with the relevant handler. SDK methods wrapping them should document that they return 404 on deployments where the feature is disabled.

| Area | What's missing | Affected SDKs | Story |
|---|---|---|---|
| **Session queue** (`router.go:1286-1288`) | `POST/GET /sessions/{sid}/queue`, `DELETE /sessions/{sid}/queue/{mid}` — enqueue, list, dismiss | All 4 | US-62.6 |
| **Session metadata** (`router.go:1253`) | `PUT /sessions/{sid}/seen` — mark session seen | All 4 | US-62.6 |
| **Session delete** (`router.go:1292`) | `DELETE /sessions/{sid}` | Python, TypeScript, Java | US-62.2 / US-62.4 / US-62.5 |
| **Agent reload** **[gated]** (`router.go:1132-1133`) | `POST /agent/reload` — explicit agent reload (no pod restart). Gated on `cfg.AgentReloadHandler != nil`. | All 4 | US-62.7 |
| **Human-in-the-loop** (`router.go:1296-1300`) | `GET /question`, `POST /question/{rid}/{reply,reject}`, `GET /permission`, `POST /permission/{rid}/reply` | All 4 | US-62.7 |
| **Usage & quota** **[gated]** (`router.go:442-447`) | `GET /usage`, `GET /usage/workspaces/{id}`, `GET /usage/quota`. Gated on `cfg.UsageHandler != nil`. | All 4 | US-62.7 |
| **Workspace role clear** (`router.go:1275`) | `DELETE /workspaces/{id}/agent-role` | All 4 (Go has Get/Set/Effective, not Clear) | US-62.2 / US-62.4 |
| **User settings** (`router.go:1334-1338`) | `GET/PUT /users/me/settings{,/{key}}`, `GET .../schema` | Python, Java | US-62.2 / US-62.5 |
| **Account** **[gated]** (`router.go:596-601`) | `POST /account/{rotate-key,change-password,recover}`. Gated on `cfg.RotateKeyHandler != nil`. | Java (Python sync client already has `_AccountAPI`) | US-62.5 |
| **DEK unlock** **[gated]** (`router.go:610-613`) | `POST /auth/unlock-dek`. Registered under `/auth/`, not `/account/`. Gated on `cfg.UnlockDEKHandler != nil`. | All 4 | US-62.7 |
| **User provider credentials** **[gated]** (`router.go:429-439`) | Create, list, get, delete, probe models, list bindings, bind/unbind (8 routes — no Update; users delete+recreate). Gated on `cfg.UserProviderCredentialsHandler != nil`. | Python, TypeScript, Java (complete miss); **Go** (missing ProbeModels, ListBindings) | US-62.2 / US-62.4 / US-62.5 |
| **Admin provider credentials** **[gated]** (`router.go:405-417`) | Full admin CRUD + Update + auto-apply + `GET /:id/models` (9 routes). Gated on `cfg.AdminProviderCredentialsHandler != nil`. | Python, TypeScript, Java (complete miss); **Go** (has List only — missing Create, Get, Update, Delete, ProbeModels, CreateAutoApply, ListAutoApply, DeleteAutoApply) | US-62.2 / US-62.4 / US-62.5 |
| **Auth lifecycle** (`router.go:316-330, 789-850`) | `register`, `logout`, `password-reset/*`, `verify-email/*`, `lookup`. **Note:** `/auth/register` is wrapped in Turnstile CAPTCHA middleware when Turnstile is enabled (`router.go:789-796`) — an SDK `register()` method must accept a Turnstile token parameter, unlike every other SDK method which is a plain JSON POST. | All 4 (only login/me/api-keys present) | US-62.7 |
| **Probe models** (`router.go:422-425`) | `POST /probe-models` — anon credential model probe (not gated) | All 4 | US-62.7 |
| **Python async client** (`async_client.py:55-62`) | Currently at parity with sync client (has workspaces, sessions, auth, account, secrets, terminal, prompts, agent_roles). Will fall behind once US-62.2 adds UserSettings, ProviderCredentials, AdminProviderCredentials, sessions.delete. | Python async only | US-62.3 |
| **`__version__` exposure** | pyproject.toml has `version = "1.0.0"` but it is not importable as `llmsafespaces.__version__` | Python | US-62.2 |
| **Java typed facade** | No domain methods, no typed errors (404/409/429 collapse to generic exception), no auth retry | Java | US-62.5 |
| **OpenAPI spec** (`sdks/openapi.yaml`) | Missing: restart, queue (×3), seen, agent/reload, question/permission (×5), models, agent-role, prompt, all provider-credentials, all usage, all orgs, probe-models, unlock-dek | Spec (the "source of truth") | US-62.1 |

### What is *not* a drift (intentionally out of scope)

These router routes are infrastructure and **must not** be added to client SDKs:

- `POST /api/v1/webhooks/stripe` (Stripe-only, server-to-server)
- `POST /internal/v1/pod-bootstrap`, `GET /api/v1/internal/orgs/{id}/status` (controller→API, token-gated)
- `GET /metrics`, `GET /livez`, `GET /readyz` on the API server (these are the *API server's* health, distinct from agentd's `/v1/readyz` on port 4098)
- `GET /api/v1/events` (user-scoped SSE — `sdks/README.md:54` correctly excludes this; consumers use native SSE libraries)
- `GET /api/v1/workspaces/{id}/session-events` (workspace-scoped SSE — same exclusion rationale)
- `GET /api/v1/workspaces/{id}/terminal` (WebSocket — ticket-based auth, not REST; SDKs expose only the ticket endpoint `POST .../terminal/ticket`)
- `POST /api/v1/users/me/agents/reload` **[gated]** (bulk agent reload across all workspaces — feature-gated on `cfg.BulkReloadHandler != nil`; defer until a consumer needs cross-workspace orchestration)

---

## Stated Assumptions

These apply across all stories. Each story also lists its own assumptions. Per
Rule 7, every assumption is paired with how it will be validated.

| # | Assumption | Validation |
|---|---|---|
| EA1 | `sdks/openapi.yaml` is intended to remain the canonical source of truth (per `sdks/README.md:22`), even though the SDKs are currently hand-written and not generated from it | Verified: README states it; Makefile `generate-*` targets exist but are placeholders. US-62.1 restores the spec so a future generation switch is possible without inheriting drift. |
| EA2 | Go SDK (`sdks/go/`) is the de-facto reference implementation — it has the broadest typed surface and is exercised by the canary suite (`sdks/canary/go/`) | Verified: `services.go` covers 11 service groups; `sdks/canary/go/scenarios/` has 41 scenarios vs 40 each for Python/TS. **Caveat:** Go is itself incomplete on provider-credentials (see drift table) — the parity target is Go *plus* the Go gaps filled. |
| EA3 | `POST /workspaces/{id}/sessions/{sessionId}/message` is synchronous (blocks until the assistant turn completes) and returns one JSON object, not an SSE stream | Verified for the *API server* layer: `proxy_test.go:2457` labels `SendMessage` "synchronous" and asserts it does NOT receive the 409 guard; POST `/message` returns 200 (not 202). **Residual risk:** `doProxy` (`proxy.go:537`) streams 2xx chunk-by-chunk and has explicit SSE handling — if opencode ever returns `text/event-stream` for POST `/session/{id}/message`, the SDKs' `resp.json()` parse breaks silently. US-62.9 adds the contract test against a live workspace that settles this. |
| EA4 | The session queue subsystem (`api/internal/services/msgqueue/`) is stable enough to expose in SDKs | Verified: Epic 41 shipped it; `proxy_handlers.go:907` (Enqueue), `:970` (List), `:993` (Delete) are wired and tested |
| EA5 | Token usage is already metered server-side and does not need to be added to `MessageResponse` | Verified: `app.go:1168` registers an SSE-inference callback emitting `llm_tokens` metering events from opencode's `session.next.step.ended` events (`session_tracker.go:297`). The legitimate SDK ask is a *usage-query* method (US-62.7), not capture. |
| EA6 | Workspace names are not unique (only secret names are) | Verified: no uniqueness constraint in `pg_workspace_store` or the create path; `database_test.go` has no duplicate-name case. SDK consumers using `name="assess-<uuid>"` need no collision handling. |
| EA7 | agentd's `/v1/readyz` (port 4098, Bearer-gated) is the "agent actually responsive" signal distinct from pod-liveness | Verified: `cmd/workspace-agentd/server.go:197` registers it; `buildReadyzHandler` (`:112`) checks opencode liveness + provider connectivity + relay injection. SDKs do not need to call this directly (it is admin-port, pod-local); the platform's `GET /workspaces/{id}/status` already surfaces the relevant fields. |
| EA8 | Idempotent delete semantics: `DELETE /workspaces/{id}` is idempotent — a second call on an already-deleted workspace succeeds (204), not 404 | Verified: `workspace_service.go:531` explicitly swallows NotFound from the CRD delete (`!k8serrors.IsNotFound(err)`) and proceeds to `markDeleted`. The server is designed for idempotent delete, so SDK consumers do NOT need to catch `NotFoundError` in cleanup paths — a double-delete is a no-op, not an error. |
| EA9 | Publishing to public registries (PyPI, npm, Maven Central) is desired; Go modules are fetched directly from VCS, no registry publish needed | To validate in US-62.8: confirm the project is OK with public visibility. AGPL-3.0 license is already declared in all manifests. |

---

## Scope

### In Scope
- Refresh `sdks/openapi.yaml` to match the current router (US-62.1)
- Bring Python, TypeScript, and Java SDKs to typed-surface parity with the current router, and fill Go's own provider-credentials gaps (US-62.2, US-62.4, US-62.5)
- Bring the Python async client to parity with its sync counterpart (US-62.3)
- Add the session-queue and session-metadata surface to all SDKs (US-62.6)
- Add the usage/quota, agent-reload, question/permission, auth-lifecycle, and probe surface to all SDKs (US-62.7)
- Publish Python, TypeScript, and Java SDKs to their respective registries with importable version metadata (US-62.8)
- Add a contract test proving the `/message` blocking + JSON-response assumption (EA3) against a live workspace, plus expand the existing Hurl contract suite (US-62.9)

### Out of Scope (Deferred)
- Switching from hand-written SDKs to OpenAPI-generated SDKs. The spec refresh (US-62.1) makes this possible in a future epic; this epic keeps the hand-written clients and aligns them with Go.
- SSE / WebSocket streaming wrappers in SDKs. `sdks/README.md:54` excludes this; remains out of scope.
- VS Code extension updates (separate concern; tracked under Epic 14's residual stories).
- Admin/Org SDK surfaces beyond provider-credentials (org CRUD, SSO, billing, invitations). These are operator surfaces; defer until a consumer asks.
- Mobile SDKs.
- SDK-level retry/circuit-breaker/opentelemetry. Callers handle cross-cutting concerns.

---

## Story Map

```
US-62.1 (OpenAPI refresh) ───────────────────────────────────────────────┐
                                                                         │
US-62.2 (Python parity) ──┬── US-62.3 (Python async)                     │
                          │                                              │
                          ├── US-62.9 (contract tests — RUN EARLY)        │
                          │        ↓ settles EA3 risk                     │
                          │                                              │
US-62.4 (TypeScript) ─────┼── US-62.6 (queue + seen, all SDKs) ─────────┤
US-62.5 (Java rewrite) ───┘   US-62.7 (usage/reload/HITL/auth)          ├── US-62.8 (publish)
                                                                         │
                                                                         ▼
                                                              (epic complete)
```

**"Parity" means parity with the current router surface, not with Go's
implementation.** Go is the style reference and has the broadest existing
surface, but it has its own gaps (provider-credentials — see drift table).
The parity stories fill Go's gaps alongside the other three SDKs' gaps.

US-62.1 has no code dependency on the SDK stories but should land first or in
parallel — it is the documentation of the contract everyone else implements
against, and it unblocks a future generation pipeline.

US-62.6 and US-62.7 both depend on the parity stories (they add new route
groups to all SDKs and need the base client structure in place), but they do
NOT depend on each other — they can be done in parallel.

US-62.9 depends only on US-62.2 but is shown branching early because EA3 is
the epic's highest-risk assumption and must be retired before US-62.8
(publish). See the scheduling note below the stories table.

---

## Stories

| ID | Title | Priority | Effort | Depends On |
|----|-------|----------|--------|------------|
| [US-62.1](US-62.1-openapi-spec-refresh.md) | Refresh OpenAPI spec to match current router | Critical | M (3 days) | — |
| [US-62.2](US-62.2-python-sdk-parity.md) | Python SDK: typed-surface parity | Critical | M (3 days) | US-62.1 |
| [US-62.3](US-62.3-python-async-parity.md) | Python SDK: async client parity with sync | High | S (1 day) | US-62.2 |
| [US-62.4](US-62.4-typescript-sdk-parity.md) | TypeScript SDK: typed-surface parity | High | M (3 days) | US-62.1 |
| [US-62.5](US-62.5-java-sdk-rewrite.md) | Java SDK: typed-facade rewrite | Normal | L (5 days) | US-62.1 |
| [US-62.6](US-62.6-session-queue-seen.md) | All SDKs: session queue + mark-seen | High | M (3 days) | US-62.2, US-62.4, US-62.5 |
| [US-62.7](US-62.7-usage-reload-hitl-auth-probe.md) | All SDKs: usage, agent-reload, question/permission, auth lifecycle, probe | Normal | L (5 days) | US-62.2, US-62.4, US-62.5 |
| [US-62.8](US-62.8-publish-to-registries.md) | Publish Python/TypeScript/Java to registries with version metadata | High | M (3 days) | US-62.2, US-62.4, US-62.5 |
| [US-62.9](US-62.9-contract-tests.md) | Contract tests: `/message` blocking + JSON shape; expand Hurl suite | Critical | M (3 days) | US-62.2 |

**Total estimated effort:** ~29 days (one engineer) or ~3 weeks (two engineers parallelized)

> **Scheduling note for US-62.9:** Although it formally depends only on
> US-62.2, it should be scheduled as the *first* story after US-62.2 lands.
> EA3 is the epic's highest-risk assumption: if `POST /message` returns SSE
> rather than a single JSON object, every SDK's `send_message` return type
> changes, which cascades into a breaking change before publish (US-62.8).
> Retiring this risk early de-risks the entire epic.

---

## Parallelization Plan

```
Week 1:  US-62.1 (OpenAPI refresh)                                        [blocks everything else]
Week 2:  US-62.2 (Python parity) + US-62.4 (TypeScript parity) + US-62.5 (Java rewrite starts)  [all need 62.1]
         US-62.3 (Python async — follows 62.2)
         US-62.9 (contract tests — starts as soon as 62.2's send_message is validated; retires EA3 risk early)
Week 3:  US-62.6 (queue + seen, all SDKs) + US-62.7 (usage/reload/HITL/auth)  [parallel; both need 62.2/62.4/62.5]
         US-62.5 (Java continues)
Week 4:  US-62.8 (publish — needs 62.2/62.4/62.5)
```

US-62.8 (publish) can land any time after the parity stories; it is sequenced
late so that the first published version is the parity-complete one, not an
intermediate.

---

## Acceptance Criteria (Epic-Level)

- [ ] `sdks/openapi.yaml` contains every public route in `router.go` (verified by a CI check that diffs spec paths against router registrations)
- [ ] All four SDKs expose the same typed surface covering the current router (parity matrix green — each route in `router.go` that is in scope has a method in each SDK)
- [ ] Python async client exposes every service the Python sync client does
- [ ] `pip install llmsafespaces`, `npm install @llmsafespaces/sdk` (or agreed name), and the Maven artifact all resolve from public registries
- [ ] `import llmsafespaces; llmsafespaces.__version__` returns the published version
- [ ] A contract test in CI exercises `POST /sessions/{id}/message` against a live (kind) workspace and asserts (a) the call blocks until the assistant turn completes and (b) the response is parseable as a single JSON object
- [ ] Every new SDK method has at least one unit test (mocked transport) and is covered by at least one Hurl contract file
- [ ] The master stories README (`design/stories/README.md`) Epic 14 row is updated: the three SDK-related recorded gaps (US-14.4 async, US-14.6 Java facade, US-14.7 contract CI) are closed by this epic. US-14.9 (VS Code slash commands) remains explicitly out of scope — it is a VS Code extension concern, not an SDK concern.

---

## Risks & Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| EA3 is wrong — opencode returns SSE for POST `/message`, breaking every SDK's `resp.json()` parse | Low (test asserts sync, returns 200 not 202) | High (silent breakage + breaking API change for all consumers) | US-62.9 contract test against a live workspace settles this before any publish. If it fails, every SDK's `send_message` return type changes from `MessageResponse{raw, content}` to a streaming/iterable type — a breaking change that would block US-62.8 (publish). Run US-62.9 as the *first* story after US-62.2 lands so the risk is retired early. |
| OpenAPI spec refresh (US-62.1) is treated as documentation-only and skipped | Medium | Medium | Make it the first story and the dependency of the parity stories; add a CI diff check as an acceptance criterion. |
| Java rewrite (US-62.5) expands scope — the team decides to deprecate Java instead | Medium | Low | The epic is written to permit either outcome. If deprecated, update `sdks/README.md` and remove the Makefile target; the other stories are unaffected. |
| Publishing to public registries (US-62.8) requires decisions on package naming, signing, and release pipeline that exceed the engineering effort estimate | Medium | Medium | Timebox the publishing story to a spike first; if naming/signing is unresolved, ship the parity-complete SDKs as git-installable and defer registry publish. |
| SDK `register()` with Turnstile is more complex than other methods — requires client-side CAPTCHA token acquisition | High (Turnstile is the default in production Helm) | Low (only affects the register method, not the rest of the SDK) | Document the Turnstile token parameter in the SDK method signature. If the consumer cannot solve client-side challenges (headless automation), document that register requires a pre-obtained token or defer the method entirely. |

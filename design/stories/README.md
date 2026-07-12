# Implementation Stories

Organized by epic, following the V2 design roadmap (design/EVOLUTION-V2.md v2.4).

**Last audited:** 2026-06-22 (code-verified; worklogs used as navigation hints, not source of truth)

---

## Status Legend

| Symbol | Meaning |
|--------|---------|
| ✅ Complete | All stories done, e2e tests pass, wired into live path |
| 🔶 Partial | Most stories done; specific verified gaps remain — see notes |
| ❌ Not Started | No implementation exists |
| ⛔ Superseded | Replaced by a later epic or architectural decision |
| 🚫 Obsolete | Original design conflicts with current architecture; close or redesign |
| 🔁 Deferred | Explicitly out of scope for current phase; tracked in issue #38 or V2.1 list |

---

## V1 Scope (Weeks 1-9)

| Epic | Goal | Status | Verified Gaps |
|------|------|--------|---------------|
| 00 | Unbreak: fix deepcopy generation, webhook decoders | ✅ Complete | None |
| 01 | Fix compile errors, remove warm pools, add security tools | ✅ Complete | `runtimes/tests/test_runtime.py` stale V1 Python test (non-Go, pre-existing debt); US-1.6 deferred by design |
| 02 | Workspace CRD, PVC persistence, suspend/resume | 🔶 Partial | Dead mock expectations in `router_workspace_test.go:110-113` for `SetCredentials`/`DeleteCredentials` — methods don't exist on the interface; `.Maybe()` prevents test failures but the mock is stale tech debt |
| 03 | Proxy to opencode, session endpoints | ✅ Complete | None |
| 04 | MCP server for external LLM tools | 🔶 Partial | US-4.1 story file describes old sandbox-centric architecture (never updated); `resources.go`/`prompts.go` not implemented (deferred V2.1, story spec is obsolete); actual 11-tool implementation is complete and tested |
| 05 | Helm chart | 🔶 Partial | US-5.1/5.2/5.3 deferred by design. US-5.4 gaps: `kyverno.enabled=true` is a silent no-op (no templates); README documents `rbac.scope` default as `"cluster"` but `values.yaml` defaults to `"namespace"`; NOTES.txt references deleted sandbox/sandboxprofile CRDs |

---

## V2 Scope (Post-Foundation)

| Epic | Goal | Status | Verified Gaps |
|------|------|--------|---------------|
| 06 | Collapse Sandbox into Workspace | 🔶 Partial | **US-6.5**: `workspaceConfig.workspaceID` field in proxy.go is never set in production code; `onSessionIdle` activity/sessionIndex recording branch is dead code. **US-6.7 ✅ Fixed**: `local/test.sh` now uses `workspace-pw-*` (line 222) and `-c workspace` (line 227). Minor: stale `// sandbox` comments in `proxy.go` and `workspace_service.go`. |
| 07 | Runtime Interception Layer — system daemon, PATH wrappers, RuntimePolicy CRD | 🚫 Partially Closed | US-7.1/7.2/7.4/7.7 **closed** (sidecar/wrapper approach incompatible with `ReadOnlyRootFilesystem: true` and mise shims — issue #40). US-7.3 **redesigned** as env-var injection (`PYTHONSTARTUP`, `NODE_OPTIONS`) — issue #41. US-7.5 partially deferred. US-7.6/7.8 open (actionable cleanup — issue #42). Future runtime enforcement will be via purpose-built Dockerfiles with baked-in wrapper scripts. |
| 08 | Credential Health & Agent Abstraction | 🔶 Partial | **US-8.5**: `WorkspaceConditionCredentialsAvailable` name is misleading — "no credentials" is valid (means free-tier opencode); the real signal is "providers connected". Defer to Epic 30 which will correctly set a provider-connectivity condition as part of the new injection pipeline. **US-8.9 superseded and generalized**: narrow `workspace.health` SSE story closed; replaced by broader push notification system (issue #43). US-8.0/8.4/8.10 superseded by Epic 10. |
| 09 | Configuration & Settings | ✅ Complete | **US-9.16 ✅**: `preferredModel` wired — `ModelSelector.tsx:23,66-78` seeds workspace model from user setting when no model is set; tests at `ModelSelector.test.tsx:183-224`. US-9.4 (`Seed()`) ✅ wired at `app.go:360`. US-9.7 (Tier-2 config) ✅. US-9.10 (`WorkspaceSettingsDrawer`) ✅. US-9.13/14/15 superseded by Epic 30. |
| 10 | Multi-Tenant Trust & Secret Management | 🔶 Partial | **US-10.10 Task 7**: MCP integration test for credential/model tools missing (basic lifecycle test exists, no credential/model coverage). **US-10.13 Part 1**: API keys stored plaintext in `api_keys.key` column; no `key_hash`/`key_ciphertext` migration; independent of Epic 30 (Epic 30 doesn't address `api_keys` table). **US-10.6 (virtual namespaces) ⛔ superseded by Epic 51** — tenant isolation now uses gVisor + admission webhook quotas, not per-tenant namespaces. US-10.7 (S3 shared folder) not started, no active roadmap entry. |
| 12 | Usage Metering & Billing | 🔶 Partial | Metering infrastructure built (`metering.Service` 939 lines, async batch writer, DLQ, quota enforcement). Tables: `usage_events`, `usage_limits`, `billing_accounts`, `billing_export_cursor`, `workspace_lifecycle_events` (migrations 024-028). Usage API endpoints registered (`/usage`, `/usage/quota`, `/admin/usage/:ownerId`). `BillingProvider` interface + `NoopBillingProvider` shipped. **Stripe provider ✅ Implemented**: `pkg/billing/stripe_provider.go` (`CreateCustomer`, `CreateCheckoutSession`, `CreatePortalSession`, `ConstructWebhookEvent`, `ReportUsage` for Metered Billing, `SuspendCustomer`); webhook handler `webhook.go` with `onCheckoutCompleted`, `onInvoicePaid`, `onPaymentFailed`, `onSubscriptionUpdated`, `onSubscriptionDeleted`; `org_billing.go` handler registered. **Remaining gaps**: usage-based pricing calculation; `users.plan_id` column exists but no plan enforcement. US-12.13 (canary) and US-12.14 (logging) complete. |
| 13 | Settings Enforcement | 🔶 Partial | **US-13.3 ✅ Fixed**: `applyWorkspaceDefaults` now reads `workspace.defaultMaxActiveSessions` from instance settings and sets `crd.Spec.MaxActiveSessions` (`workspace_service.go:888-890`); proxy reads from CRD spec (`proxy.go:211`); test `TestCreateWorkspace_DefaultMaxActiveSessions_Applied` present. **US-13.15 complete**: Epic 30 delivered `CredentialProvisioner` at `workspace_service.go:271-276`. **US-13.10**: `ModelSelector` does not read `preferredModel` user setting. |
| 14 | Multi-Language SDKs & VS Code Extension | 🔶 Partial | **US-14.4**: No `AsyncLLMSafeSpace` class in Python SDK (sync-only). **US-14.6**: Java SDK is raw HTTP wrapper only — no typed facade, model classes, or tests. **US-14.7**: 3 Hurl files exist but are not executed in CI; no sessions/pagination coverage. **US-14.9**: VS Code chat participant has no slash commands (`/new-session`, `/switch-workspace`, `/history`, `/status`) — no `switch(request.command)` dispatch, no `commands` array in `package.json`. |
| 15 | Streaming State Resilience & Mid-Stream Reconnect | 🔶 Partial | Functionally complete (all 5 implementation stories done). **US-15.6**: 18/24 specified tests present; 6 missing are backend Go tests for SSE failure modes (goroutine leak, write deadline, k8s list failure, gap+replay+resync). Frontend reconnect tests are complete. |
| 16 | Agent Input Requests | 🔶 Partial | **US-16.6 ✅ Fixed**: `session_question_reply`, `session_question_reject`, `session_permission_reply` tools are registered in `pkg/mcp/server.go:212-225` with handlers + integration tests (`integration_test.go:70-72`). **US-16.2b**: proxy.go is now 479 lines (down from 1,405); hardcoded session path strings resolved. **US-16.13**: `api/internal/tests/integration/` does not exist; backend E2E absent. |
| 17 | Security Review & Penetration Testing | 🔶 Partial | Pentest + ~46 code fixes complete. Open: post-remediation live re-pentest not run (no `phase-{2..7}-postfix` dirs); **F1.7.2** (API keys plaintext) and **G25** (secret in logging middleware) are both HIGH severity and classified as OTHER agent's branch; **RT-7.9** (XSS corpus) — `rehype-sanitize` is present, test corpus unwritten. Epic 30 threat model addendum complete (`THREAT-MODEL-ADDENDUM-EPIC30.md`). |
| 18 | Hot Migration — zero-downtime pod replacement | 🔶 Partial | **S18.10** complete. **S18.11**: readyz gate decoupled ✅ (primary goal done); `WorkspaceConditionProviderReady` condition — Epic 30 is complete (credential injection pipeline stabilized); can now add condition + narrow the regex-parse in `agentHealthFromConditions`. Current `AgentHealthy` condition already surfaces provider issues via `HealthBanner`. S18.1–S18.6, S18.9 not started — measured resume is ~17s p99, not 2min; low urgency. **S18.7 (gVisor) ➡️ moved to Epic 51** (multi-tenancy prerequisite, no hot-migration dependency). **S18.8 ⛔ reduced** to EFS storage isolation only (Capsule namespace work superseded by Epic 51). |
| 21 | ~~Workspace Recovery State Machine~~ | ⛔ Superseded | Fully superseded by Epic 24. One carryover gap: `WorkspaceStatusResult` never exposes `nextRetryAt`/`consecutiveFailures`/`safeMode` via API status endpoint. Filed in issue #38. Can be formally closed. |
| 22 | agentd Health-Endpoint Redesign | ✅ Complete | All 8 stories code-verified. |
| 23 | Controller Race Hardening | 🔶 Partial | Stories 1+4 ✅ shipped. **Story 3 ✅ shipped** (worklog 0342): `LastActivityAt` moved to `metadata.annotations`; Suspend/Resume moved to `Spec.Suspend *bool` (tri-state pointer for backward compat); controller is sole writer of `Status.Phase`; API is sole writer of `LastActivityAt`. **Story 2 partial**: `updateStatusWithRetry` helper + tests landed; 21-site migration deferred pending conflict-metric data. |
| 24 | Self-Healing Workspace Lifecycle | 🔶 Partial | Core recovery engine complete; US-24.6 `handleFailed` is **complete**. **US-24.7 ✅ shipped**: `ControllerRestartCount` wired at health-check restart sites + reset on recovery (`health.go`, `recovery_policy.go`, `phase_creating.go`). **US-24.11 ✅ shipped**: 6 new recovery metrics (`controller_restarts_total`, `safe_mode_entries/exits_total`, `workspaces_in_recovery`, `recovery_duration_seconds`) + fixed `WorkspaceSafeModeActive` cardinality hazard (removed `workspace_id` label per F18). **US-24.17 ✅ already done**: `WorkspaceConditionDiskPressure` + detection at `health.go:272-286` with 6 tests. Deferred to issue #38: **US-24.13** `buildSafeModePod` (needs safe-mode image; flag plumbed but no fallback pod). |
| 25 | API Server Robustness & Correctness | 🔶 Partial | **G1 ✅ Fixed**: `io.ReadAll` wrapped in `io.LimitReader` (`proxy_input.go:155`, test `TestEpic25G1`). **G3 ✅ Fixed** (SSE write deadline). **B2 ✅ Fixed**: streaming loop emits `agent_died` SSE terminal event on read error (`proxy.go:455-468`, test `TestProxy_B2`). **B5 ✅ Fixed**: activity tracker `Delete` called on NotFound (`proxy_events.go:80`). proxy.go is now 479 lines (down from 1,405). Remaining: 14 `context.TODO()` in `client_crds.go`. |
| 26 | Client-Proxied Inference | ⛔ Superseded | CF Worker relay shipped 2026-06-05; **removed by Epic 60 (2026-07-12)** because Zen blocks CF Worker IPs. Worker code, chart values, controller flag, and Helm Job all deleted. See `epic-26-client-proxied-inference/README.md` supersession banner. |
| 27a | Credential Reload Foundation | 🔶 Partial | Core foundation complete. **Drain injection ✅ Fixed**: `app.go:618-646` now calls `GetSSETracker()` after `proxyHandler.Start()`; tracker is non-nil; drain mode is wired. **US-27a.9**: full credflow e2e test missing — handler-level tests exist but the bind→`agentNeedsRefresh:true`→reload→`agentNeedsRefresh:false` path is untested. |
| 27b | Credential Reload Polish | ✅ Complete | **US-27b.3 ✅** drain wired. **US-27b.4 ✅** BulkReload uses bounded semaphore (max 5 parallel, `agent_reload.go:425-445`). **US-27b.5 ✅** `EnrichChatErrorBody` wired into `SendMessage` (`proxy_handlers.go:43-63`) with agent-state checker for credential-change hints. |
| 28 | Unified Event Stream | ✅ Complete | Backend complete and integrated. S28.5 ✅ (`UserEventBroker.SubscribeWorkspace`). S28.8 ✅ all 3 SSE failure-mode tests present: goroutine leak (`TestStreamUserEvents_GoroutineExitsOnClientDisconnect`), write deadline (`TestStreamUserEvents_WriteErrorCancelsStream`), k8s list failure → resync (`TestStreamUserEvents_SnapshotListFailure_EmitsResync`). |
| 29 | Handler Decomposition & Agent Client Abstraction | 🔶 Partial | **US-29.1 ✅ shipped** (worklog 0357): `AgentClient` interface + `WorkspaceClient` in `pkg/agent/opencode/`; `ListModels`/`PatchConfig` added to `Client`. **US-29.4 ✅ shipped** (worklog 0343): `WorkspaceEnvHandler` extracted. **US-29.7 ✅ shipped**: contract test. Can proceed with US-29.2/29.3/29.5 (handler splits consuming AgentClient), US-29.6 (auth-enforcing mocks), US-29.8 (constructor injection). |
| 30 | Unified Credential Model | ✅ Complete | All 14 stories (US-30.1–30.14) implemented, merged in PR #39, deployed as Helm rev 159, live-validated 2026-06-07 (worklog 0180). `provider_credentials` with `owner_type='user'\|'admin'\|'org'`; `CredentialProvisioner` wired at `workspace_service.go:271-276`; `decryptBinding` handles all three owner types (`injection.go:135-170`); `SeedWorkspaceCredentials` seeds in priority order (`pg_credential_store.go:81-138`). Epic 11 (Organizations) built on top of this in PR #137. Admin + User LLM Provider UIs built (`AdminProviderCredentialsTab.tsx` 631 lines). |
| 34 | Session Security — remember-me (30-day JWT + cookie), enforce `LLMSAFESPACE_MASTER_SECRET` at startup | ✅ Complete | **Remember-me**: `auth.go:727-731` uses `RememberMeDuration` for token TTL; cookie name configurable (`CookieName`, fallback `"lsp_session"`, `auth.go:892-902`). **MASTER_SECRET enforcement**: `app.go:765-799` `validateMasterSecret()` checks presence + length at startup with fatal/warn behavior. |
| 35 | Secretless Credential Injection — eliminate `workspace-secrets-<id>` K8s Secret; init container self-fetches credentials from API server via projected SA token + TokenReview | ❌ Not Started | None |
| 37 | Session Activity & Unread State UX — activity spinners across workspaces, unread pulsation, "new messages" divider, persisted across refreshes | ✅ Complete | `SessionActivityProvider.tsx` implements unread state (`isSessionUnread`, `clearPendingUnread`) with REST `hasUnread` reconciliation; `Sidebar.tsx` renders activity; integration tests in `session-activity.test.tsx`. |
| 38 | Architectural Remediation — security fixes, proxy decomposition, dead code, dual pattern consolidation | ✅ Complete | US-38.1–38.13 all shipped. Pattern 3 (broker consolidation) completed: `WorkspaceEventBroker` deleted, all consumers migrated to `UserEventBroker`. |
| 39 | Session Activity State Integrity — fix busy-session spinner flicker (REST clobbered SSE state) | ✅ Complete | Renamed from `epic-38` collision (US-46.1). `SessionActivityProvider` seeds once from REST, then SSE is sole authority; `useUserEventStream` gains `onReconnect`. Follow-up US-38.4 (publish session delete to user broker) open — see FM3. |
| 41 | Message Queue Reliability — fix streaming state clear timing, add 409 guard for in-flight sessions, restore dead `onSessionIdle` activity recording | ✅ Complete | `msgqueue/service.go` implemented (11 methods); wired in `proxy_events.go`, `proxy_handlers.go`. Queue lifecycle behaviors addressed (worklog 0290, 0301, 0309). **409 guard verified**: `SendPromptAsync` returns 409 with `Retry-After` header when session is active (`proxy_handlers.go:71-87`); `SendMessage` intentionally unguarded (synchronous path, different concurrency model — verified by `TestProxy_SendPromptAsync_409DoesNotAffectSendMessage`); queue drain path handles upstream 409 via requeue+backoff (`proxy_events.go:469`). Guard reads through Redis-backed `wsstate.Store` (US-45.2) so multi-replica consistency is covered. |
| 43 | Organization Management & Multi-Tenant Product — org admin portal, email invitations, SSO, policy engine, billing tiers | 🔶 Partial | Org admin portal built; email invitations complete (`orgs.go`, `InvitationsHandler`); admin-only org creation with email owner resolution shipped (PR #201); single-org enforcement + cross-org invitation check (PR #208); org credentials UI unified (PR #199); org access control designed (`design/0031`). Stripe billing wired (`org_billing.go`). **Remaining**: SSO, policy engine (policies exist — `allowed_models`, `allowed_providers`, `max_workspaces_per_member`, `max_active_workspaces_per_member`), full billing tier enforcement. |
| 44 | Session Reliability & Transparency — terminal SSE events on agent death, session-aware restart, OOM detection, memory pressure warnings, request buffering, fix api-key restart bug | 🔶 Partial | **US-44.1a ✅ Shipped** (PR #202): proxy emits terminal `agent_died` SSE event on stream death (`proxy.go:434-468`). Design docs complete (PR #198). **Remaining**: US-44.1b+ — session-aware restart, OOM detection, memory pressure warnings, request buffering. |
| 45 | Multi-Replica State Consistency — externalize `ProxyHandler` per-replica state to Valkey/Redis (`activeSess`, `pwCache`, etc.). Eliminates today's stuck-session bug class at multi-replica. | 🔶 Partial | **US-45.1 ✅** (PR #204): `wsstate.Store` abstraction extracted (`wsstate/store.go`, `inmemory.go`, `redis.go`). **US-45.2 ✅** (PR #205): Redis-backed `activeSess` eliminates multi-replica drift. **US-45.3 ✅** (PR #207): Redis-backed `deletedSessions` prevents cross-replica zombie resurrection. **Remaining**: US-45.4+ stories for full state migration. |
| 48 | Relay Admin UX — operator setup wizard + status dashboard for the inference relay fleet | ✅ Complete | Renamed from `epic-43` collision (US-46.1). US-43.1–43.12 done: OCI+GCP credential config, InferenceRelay CR deploy, rotate/pause/resume actions, status dashboard. Depends on Epic 42. |
| 51 | Tenant Isolation — gVisor + Resource Quotas — container-runtime isolation (gVisor RuntimeClass) + per-tenant resource quotas via admission webhook, shared namespace (no per-tenant namespaces) | ✅ Complete | S51.1–S51.4 shipped (`controller/internal/webhooks/pod_tenant_quota_webhook.go`, `charts/.../templates/runtime-class.yaml`, `epic51_chart_test.go`; PRs #310, #317). gVisor RuntimeClass opt-in (`gvisor.enabled`, default off); `PodTenantQuotaValidator` webhook keyed on `llmsafespaces.dev/tenant` label, disabled when all quota limits are 0. Org-specific quota overrides + billing-tier→quota mapping deferred to Epic 43. |

---

## V2.2 (In Planning)

| Epic | Goal | Depends On |
|------|------|------------|
| 32 | VPN Sidecars (WireGuard, Tailscale, ZeroTier), VPC Connectivity, & AWS IAM (IRSA + Pod Identity) — admin-gated per-workspace network attachment | Epics 6, 9, 24 |
| 31 | **Shared Workspace Per User (User Drive)** — per-user PVC/S3 drive mounted at `/shared` in every workspace, 5 GB default quota, resize for billing upgrades, frontend capacity bar in status area | Epics 6, 9, 24 |
| 46 | **Codebase Debt Audit** — split god files, type the untyped, propagate context, single-writer agent-config.json, define Service interfaces, lint baselines, **+ entitlements/feature-flag separation from billing (extract `pkg/entitlements`, typed Feature enum, features endpoint, frontend 402 handler)** | Epics 29, 38 |
| 47 | **Frontend Architecture Consolidation** — dead-code sweep, fix silent failures (autoSuspend UI), account-level autoSuspend, busy-indicator unification, TanStack Query migration, provider UX fold | None |
| 53 | **MCP Server Integration** — platform admins, org admins, and **individual (non-org) users with the feature flag enabled** register/manage **external** MCP servers (GitHub, Slack, internal) that workspace agents connect to as MCP clients. Org members are excluded from user-scope (governance); user-scope is gated by a feature flag (capability layer, billing is a separate concern). Secrets encrypted at rest (master-KEK for admin/org, user-DEK zero-knowledge for user), auto-applied to all/org/own workspaces, injected via the existing secrets pipeline, materialized into opencode's `mcp` config. Inverse of Epic 4 (platform-as-server). Definition only — see `epic-53-mcp-server-integration/README.md` | Epic 30 (injection pipeline), Epic 11/43 (orgs); soft-dep Epic 50 |
| 54 | **Org-Scoped Login** — Slack-style email → silent redirect → org subdomain for BYO-email orgs where `claimed_domains` doesn't cover all members. Enumeration-safe `POST /auth/lookup` (follows `password_reset` precedent). No magic links (D54-1); 1:1 user→org preserved (D54-3); spike-first on wildcard routing (D54-4). | Epic 43 (SSO + invitations shipped) |

## V2.1 (Deferred)

| Story | Reason |
|-------|--------|
| US-1.6: Injection detection | Not on critical path |
| US-5.1: PATH-shadowing wrappers | Superseded — mise handles runtime management; `ReadOnlyRootFilesystem: true` blocks binary relocation |
| US-5.2: Hardened Dockerfile | Only needed for high-security mode |
| US-5.3: Kyverno policies | Pod security contexts cover V1 |
| US-7.1/7.2: System daemon + package manager wrappers | **Closed** — architecture incompatible; future enforcement via Dockerfile baked-in scripts (issue #40) |
| US-7.3: Language runtime wrappers | **Redesigned** as env-var injection using existing V1 policy scripts; see issue #41 |
| US-7.4: RuntimePolicy CRD | **Closed** — no consuming implementation; revisit with US-7.3 redesign |
| US-10.6: Virtual namespace tenant isolation | **⛔ Superseded by Epic 51** — tenant isolation now uses gVisor + admission webhook quotas in shared namespace; per-tenant namespaces don't solve container escape and don't scale to 1000+ tenants |
| US-10.7: S3 shared folder | No active demand |
| Epic 12: Usage Metering & Billing (Stripe/provider integration) | **Stripe provider shipped** (`stripe_provider.go`, `webhook.go`, `org_billing.go`). Remaining: usage-based pricing calculation, plan enforcement via `users.plan_id`. US-12.12/13/14 complete. |
| Epic 18 S18.1–S18.9: Hot migration | Resume measured at ~17s p99 (not 2min); low urgency until production multi-tenant load |
| US-24.13: Safe mode fallback pod | Deferred issue #38 |
| US-24.14: Image pinning | Deferred issue #38 |
| US-24.16: File download endpoint | Deferred issue #38 (blocked on US-24.13) |
| WebSocket↔SSE bridge | SSE sufficient for browsers |
| MCP file upload/download tools | Agent can handle through its own tools |
| Session-level credential override | Workspace-level credentials sufficient |
| High-security mode | Standard security sufficient for V1 |

---

## Recommended Implementation Order

```
Epic 30 (Unified Credential Model)           ← ✅ COMPLETE (PR #39, deployed Helm rev 159)
Epic 38 US-38.13 (Credential triplication)   ← ✅ COMPLETE (PR #206)
Epic 34 (Session Security)                   ← ✅ COMPLETE
Epic 37 (Session Activity UX)                ← ✅ COMPLETE
Epics 44/45                                  ← 🔶 In active development (PRs #202, #204, #205, #207)

Next priorities:
  ├─ US-38.8 Pattern 3 (consolidate WorkspaceEventBroker → UserEventBroker)
  ├─ Epic 27b completions (US-27b.4 bulk parallelism, US-27b.5 enrichment wiring)
  ├─ Epic 23 Stories 2+3 (gating metric ✅ shipped; can proceed)
  ├─ Epic 28 S28.8 (missing SSE failure-mode tests)
  ├─ Epic 29 (Handler Decomposition) ← all deps met
  ├─ Epic 09 US-9.16 (preferredModel wiring)
  ├─ Epic 24 US-24.11 (Prometheus metrics) ← recovery system is unobservable
  ├─ Epic 24 US-24.17 (disk pressure)
  ├─ Epic 51 (Tenant Isolation: gVisor + quota webhook) ← unblocked, security prerequisite for multi-tenant production
  └─ Epic 43 completions (SSO, policy enforcement, billing tiers)

Resolved "Fix now" items (all shipped since 2026-06-14):
  ├─ Epic 25 B2 (proxy truncation)            ← ✅ Fixed (agent_died terminal event)
  ├─ Epic 25 G1 (body size limit)             ← ✅ Fixed (LimitReader)
  ├─ Epic 25 B5 (activity tracker leak)       ← ✅ Fixed (Delete on NotFound)
  ├─ Epic 27a drain injection gap             ← ✅ Fixed (wired after Start())
  ├─ Epic 06 US-6.7 (local/test.sh)          ← ✅ Fixed (workspace-pw + -c workspace)
  ├─ Epic 16 US-16.6 (MCP question tools)    ← ✅ Fixed (registered in server.go)
  └─ Epic 13 US-13.3 (MaxActiveSessions CRD) ← ✅ Fixed (reads instance settings)

Lower priority / ongoing:
  ├─ Epic 07 US-7.6 + US-7.8 (cleanup only)
  ├─ Epic 17 phase-N-postfix re-run + issue #38 items
  ├─ Epic 14 US-14.4 (async Python), US-14.9 (slash cmds)
  ├─ Epic 12 (usage pricing + plan enforcement)
  └─ Epic 35 (secretless credential injection)
```

---

## Story Dependency Graph

```
US-0.1 (deepcopy) ──┐
US-0.2 (webhooks) ──┼── US-1.1 (API) ──┐
                      │                   ├─ US-1.3 (remove warm pools) ──┐
                      └── US-1.2 (ctrl) ─┘                                │
                                                                        ▼
US-1.5 (redact) ────── US-1.7 (entrypoints) ── US-1.8 (Dockerfile)     │
                                                                         │
                                               US-2.1 (Workspace CRD) ──┤
                                               US-2.2 (Workspace rec.)  │
                                               US-2.3 (Workspace API) ──┤
                                               US-2.4 (Sandbox update) ──┤
                                               US-2.5 (DB migration) ────┤
                                                                         ▼
                                                     US-3.1 (proxy) ────┤
                                                     US-3.2 (routes)    │
                                                     US-3.3 (activity)  │
                                                                         ▼
                                                     US-4.1 (MCP) ──────┤
                                                                         ▼
                                                     US-5.4 (Helm) ──────┘
```

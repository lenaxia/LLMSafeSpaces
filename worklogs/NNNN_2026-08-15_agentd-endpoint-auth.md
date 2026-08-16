# Worklog: agentd reload-secrets + agent/reload Basic-auth gates + API caller wiring (#848)

**Date:** 2026-08-15
**Session:** Fix issue #848 — /v1/reload-secrets and /v1/workflow/node/cancel lacked auth (cancel was fixed with #762); extend the gate to /v1/reload-secrets and /v1/agent/reload (found unauthenticated during review — dispose is a disruption primitive), and wire every API-server caller. Also fixed #770-class latent issues found on the way: none.
**Status:** Complete

---

## Objective

Close #848: unauthenticated in-pod access to control-plane surface on the agentd user mux (4097). `/v1/reload-secrets` accepts a secret batch and can apply provider config + restart opencode; `/v1/agent/reload` disposes the opencode instance. Both now require the workspace Basic credential, and the legitimate API callers send it.

---

## Work Completed

### agentd (cmd/workspace-agentd)

- `secrets.go` `reloadSecretsHandler`: gates at entry via shared `checkBasicAuth`/`rejectUnauthorized` (#762 helpers), before method/JSON processing.
- `agent_reload.go` `agentReloadHandler`: same gate.
- All reload-secrets/agent-reload tests updated to send the credential (22+ sites across secrets/container_restart/reload_cache/reload_credentials_e2e/agent_reload tests); new enforcement tests: `TestReloadSecrets_RequiresAuth`, `TestReloadSecrets_WrongPassword`, `TestAgentReloadHandler_RequiresAuth`.

### API server (api/internal)

- `services/agentpush`: new `PasswordProvider` interface + `WithPasswordProvider` option; `Push` resolves the workspace password and sends `Authorization: Basic opencode:<pw>` on the reload-secrets dispatch. Missing provider → `ErrNoPasswordProvider` (wiring bug, fails before HTTP); provider error → wrapped, `reload_failed` metric.
- `handlers/agent_reload.go`: both dispatch sites (single `Reload` + bulk `reloadOne`) resolve the password via the already-wired `getPassword` (`SetPasswordGetter`, wired in app.go's handler-construction block) and set the Basic header. Nil getter surfaces `password_getter_not_wired` instead of silently 401-ing against agentd.
- `app.go`: `agentpush.New(...)` gets `WithPasswordProvider(proxyHandler)`.
- Tests: `TestPush_SendsBasicAuth` (exact header), `TestPush_MissingPasswordProviderErrors`, `TestPush_PasswordProviderErrorSurfaces`; existing push tests wired with a fake provider; malformed-URL pin tests wired with `interfaces.PasswordFunc` stubs.

### Review-round additions

- **Fallback pusher provider path**: `SecretsHandler.SetPasswordProvider` + inclusion in `getPusher()`'s lazy construction — setter-style wiring previously produced a pusher that could only fail with `ErrNoPasswordProvider` (#848 enforcement made the fallback useless).
- **E2E dispatch credential pinned**: `TestHandler_E2E_BindTriggersReloadSecrets` now captures the `Authorization` header at the mock agentd and asserts the exact Basic credential; the four setter-style E2E tests (`BindTriggersReloadSecrets`, both LLMProvider binds, `BindPushesOrgCredentialEvenWithAPIKeyAuth`) wire `staticPasswordProvider`.
- **App tests fixed**: `TestWsAgentPusherAdapter_EmitsMetricOnEveryPush` / `TestSharedPusher_DoesNotEmitAutoPushMetric` construct `agentpush.New` with `stubPasswordProviderAdapter` (Push now requires a provider).
- Duplicated `WithPasswordProvider` lines in `agentpush_test.go` deduplicated; `TestPush_SendsBasicAuth` uses `pw-7` consistently; dead `sawWS` field removed.

### Review-round 6 additions

- **Bulk dispatch covered**: `TestE2E_BulkReload_DispatchDrivesAgentd` drives `reloadOne` end-to-end through the `agentdPort` override — asserts the exact Basic header at the mock agentd, exactly one dispatch, and a genuine `disposed:true` NDJSON row (previously every bulk test short-circuited at the nil-getter branch).
- **Second nil-tx guard** mirrored at the bulk `reloadOne` Commit site (the single-reload guard from round 5 covered only one of the two sites).
- `TestAgentReload_AgentdUnreachable_Returns500` now wires a password getter so its name is true (exercises the real dispatch dial failure, not the getter short-circuit).
- Stale in-test comment at the partial-failure subtest corrected.

### Review-round 3 additions

- **Stale doc comment fixed** (`agent_reload.go`): "Authentication: none at the application layer" — contradicted by the gate 10 lines below; now describes the Basic-auth gate + NetPol layering.
- **`TestE2E_ReloadWorkflow_FullPath` no longer vacuous**: `AgentReloadHandler`/`BulkReloadHandler` gain a test-only `agentdPort` override (mirrors `Scheduler.AgentdPort` from #883); the mock agentd now receives the dispatch. New subtests: `dispatch_carries_basic_auth` (exact header at the mock), `password_getter_error_surfaces_as_500`, `nil_password_getter_surfaces_wiring_error`; `no_agent_state_row_409` now genuinely reaches the 409 branch (previously short-circuited at unreachable-agentd).
- **Nil-tx guard**: mock `BeginTx` returns `(nil, nil)`; the handler now nil-guards `tx.Commit()` (previously a latent SIGSEGV, unreachable only because the dispatch always failed in tests).

### Issue-scope note

#848 also lists `/v1/workflow/node/cancel` — that endpoint was gated in the #762 fix (same PR series); this PR covers the remaining two endpoints.

---

## Key Decisions

1. **`/v1/agent/reload` folded in** — discovered unauthenticated during the issue review (not in #848's list). Same class (control-plane/disruption), same fix, same PR series. Noted in the PR description.
2. **agentpush fails fast on missing provider** rather than sending unauthenticated and relying on agentd's 401 — the error message names the wiring bug; metric outcome `reload_failed` preserved.
3. **Reuse of the existing `getPassword` on AgentReloadHandler** — the drain mode already resolved the password (`SetPasswordGetter` wired in app.go since #770-era work); the dispatch path now uses the same provider. No new interfaces on the handler side.

---

## Blockers

None.

---

## Tests Run

- `go test ./api/internal/services/agentpush/` — ok
- `go test ./api/internal/handlers/` — ok (114s full suite)
- `go test -run 'TestContainerRestart|TestReloadSecretsHandler|TestE2E_ReloadSecrets' ./cmd/workspace-agentd/` — ok (35.9s)
- Full `./cmd/workspace-agentd/` suite — ok (see below; first post-change run caught 12 test sites my initial batch update missed — fixed, re-verified)
- `golangci-lint run --new-from-merge-base=origin/main` over all four touched trees — 0 issues

---

## Next Steps

- File the follow-up issue: uid separation for agentd (true in-pod mitigation; Basic auth is defense-in-depth per the #762 triage note).
- After all three PRs merge: verify a live workspace end-to-end (workflow node dispatch, MCP tools list, credential reload, agent reload) — the chart ships API + agentd together so the rollout is coordinated.

---

## Files Modified

- `cmd/workspace-agentd/secrets.go`
- `cmd/workspace-agentd/agent_reload.go`
- `cmd/workspace-agentd/secrets_test.go`
- `cmd/workspace-agentd/container_restart_test.go`
- `cmd/workspace-agentd/reload_cache_test.go`
- `cmd/workspace-agentd/reload_credentials_e2e_test.go`
- `cmd/workspace-agentd/agent_reload_test.go`
- `api/internal/services/agentpush/agentpush.go`
- `api/internal/services/agentpush/agentpush_test.go`
- `api/internal/handlers/agent_reload.go`
- `api/internal/handlers/proxy_send_logging_test.go`
- `api/internal/handlers/secrets_integration_test.go` (E2E credential assertion + staticPasswordProvider)
- `api/internal/handlers/secrets_llmprovider_test.go`, `secrets_push_session_test.go` (stub wiring)
- `api/internal/handlers/agent_reload_e2e_test.go` (full-path + bulk dispatch-driving tests, header assertions)
- `api/internal/handlers/agent_reload_test.go` (getter wired into the unreachable test)
- `api/internal/handlers/secrets.go` (SetPasswordProvider + fallback wiring)
- `api/internal/app/app.go` (WithPasswordProvider on the shared pusher)
- `api/internal/app/auto_push_metrics_test.go` (stubPasswordProviderAdapter)
- `worklogs/NNNN_2026-08-15_agentd-endpoint-auth.md` (this file)

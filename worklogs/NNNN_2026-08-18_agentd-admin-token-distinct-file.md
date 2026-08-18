# Worklog: distinct admin-token — file delivery, env scrub, mixed-fleet bearer fallback (#887 D5.1)

**Date:** 2026-08-18
**Session:** Implement design 0051 D5.1 (v3): the agentd admin-mux bearer token becomes a DISTINCT secret delivered file-only, scrubbed from the opencode spawn env, with try-order fallback across the mixed fleet.
**Status:** Complete

---

## Objective

`AGENTD_ADMIN_TOKEN` was pod-spec env sourced from the **same Secret key** as the workspace password. opencode passes its full env to every tool process (`extendEnv: true`, verified in pinned v1.18.10 `prompt.ts:559`) — so every bash tool could `printenv` the `:4098` bearer. Because token==password, a scrub-only fix would be theater; the token must become distinct AND leave the environment.

---

## Work Completed

### Controller
- `secrets.go` `ensurePasswordSecret`: upserts a distinct 32-char `admin-token` key (new Secrets get both keys; existing Secrets converge via Update; **never rotated in place** — running pods hold the accepted value while rebuilt probe specs read the Secret).
- `pod_builder.go`: two delivery modes keyed on Secret contents —
  - **file mode** (key present): init installs `/sandbox-cfg/admin-token` 0400 (runtime-guarded `if [[ -f ]]`); main env gets `AGENTD_ADMIN_TOKEN_FILE` only — **no `AGENTD_ADMIN_TOKEN` env**; probes use the distinct value.
  - **legacy mode** (key absent, pre-upsert race): exactly the previous behavior (env var + password probes). No fail-open window: legacy pods' agentd still reads env.
- `health.go`: extracted `statuszWithBearers` (try-order helper); `enrichAgentStatus` sends [admin-token, password].

### agentd
- `admin_token_file.go` (new): `adminToken()` resolves file (`AGENTD_ADMIN_TOKEN_FILE`) → env (legacy) → "" ; `readAdminTokenFile` trims; `scrubAdminEnv` drops both admin vars.
- `secrets.go` `buildEnvFrom`: scrubs `AGENTD_ADMIN_TOKEN`/`AGENTD_ADMIN_TOKEN_FILE` from the merged parent+secrets env **after the merge** (a user-staged env-secret cannot smuggle one back). This is the env handed to opencode → tools.
- `server.go` + `managed_process.go`: use `adminToken()` instead of raw `os.Getenv`.

### API server
- `proxy_connections.go`: exported `GetWithBearers` try-order helper + `adminBearerCandidates` (distinct token via direct Secret read — uncached, reconnect/poll-path only — then password; nil-k8s tolerated for unit handlers); `GetAuthoritativeActiveSessions` switched.
- `proxy_events.go` `reconcileSessionState`: builds candidates from (workspaceID, callback password) and uses the helper. SSE `ReconnectCallback` interface unchanged.
- `app.go` `newRelayChecker`/`buildRelayChecker`: `pwGetter` → `bearerGetter func(ctx, wsID) ([]string, error)`; call site composes [admin-token, password].

### Helper semantics (both sides)
Empty candidates = one **unauthenticated** attempt (pre-#887 behavior for Secret-less dev clusters / missing-Secret poll paths) — pinned by test. 401 advances; transport error surfaces; all-401 errors.

---

## Key Decisions

1. **Distinctness is the fix** — discovered during implementation review of design 0051 v2's own D5.1 (scrub+file): token==password made it theater. Design doc amended to v3 on the holding PR (#932).
2. **No in-place rotation** — desyncs running pods (agentd memory) from rebuilt probe specs.
3. **Mixed-fleet self-healing** — every `:4098` consumer tries [admin-token, password]; works for both pod generations with zero metadata coupling; converges as pods rebuild.
4. **SSE callback interface frozen** — candidates resolved inside `reconcileSessionState`; avoids rippling `ReconnectCallback` through the tracker.
5. **Legacy env mode retained** only for Secrets that predate the upsert; no new pod is built in that mode after convergence.

---

## Blockers

None. Two e2e test failures in this sandbox (`TestE2E_BootstrapMaterialize_TokenRejected_StillBoots`, `TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders`) are **environmental** — this workspace pod exports `INFERENCE_RELAY_BASEURL`, so the pre-boot relay legitimately materializes `opencode-relay`; verified they fail identically on clean main (stash-run). CI runs clean.

---

## Tests Run

- agentd: `TestResolveAdminToken_*` (file/env/none), `TestReadAdminTokenFile_*` (trim/error), `TestBuildEnvFrom_ScrubsAdminTokenVars` (surgical scrub) — green.
- controller: `TestEnsurePasswordSecret_*` (upsert/distinct/no-rotation/legacy-converge), `TestPodSpec_AdminToken{FileMode,LegacyEnvMode}`, `TestStatuszWithBearers_*` (first-wins/401-fallback/all-rejected/unauthenticated-empty), full `./controller/internal/workspace/` suite — green.
- api: `TestGetWithBearers_*`, `TestReconcileSessionState_BearerFallback`, relay-checker suite — green; full handlers suite green except the 2 environmental cases above.
- `golangci-lint --new-from-merge-base` — 0 issues.

---

## Next Steps

- Design 0051 Phase 1 remainder (D5.2–D5.4): required-token boot, empty-password reject, gated `/metrics`.
- Phase 2 (uid split) per design §5 — file ownership 65532 for the admin-token file folds in naturally.
- Live validation once merged: new pod → `printenv AGENTD_ADMIN_TOKEN` empty in bash tool; probe auth green; deep-status green (fallback exercised for legacy pods).

---

## Files Modified

- `cmd/workspace-agentd/admin_token_file.go` (new) + `admin_token_file_test.go` (new)
- `cmd/workspace-agentd/secrets.go`, `server.go`, `managed_process.go`
- `controller/internal/workspace/secrets.go`, `pod_builder.go`, `health.go`
- `controller/internal/workspace/admin_token_secret_test.go` (new), `admin_token_delivery_test.go` (new), `admin_token_bearer_test.go` (new), `pod_spec_consistency_test.go`
- `api/internal/handlers/proxy_connections.go`, `proxy_events.go`, `admin_token_bearer_test.go` (new)
- `api/internal/app/app.go`, `relay_checker_test.go`
- `design/0051_2026-08-18_agentd-uid-separation.md` (v3 amendment, on the design branch/PR #932)
- `worklogs/NNNN_2026-08-18_agentd-admin-token-distinct-file.md` (this file)

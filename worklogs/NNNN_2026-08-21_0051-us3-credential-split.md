# Worklog: design 0051 US-3 — credential split (agentdPassword + per-endpoint table)

**Date:** 2026-08-21
**Session:** Implement US-3 per design 0051 §D1/§8 and Q3: the NEW `agentdPassword` Secret key, env-only delivery to the sidecar, and the per-endpoint mux credential table.
**Status:** Complete (pending review)

---

## Objective

Close the control-plane credential gap: the reload-secrets/workflow/agent-reload surfaces must hold a secret uid-1000 code cannot obtain, delivered per §D1's table, with the Q3 upsert-once migration ordering and D6.1 mixed-generation-window behavior.

---

## Work Completed

### Controller
- **`ensurePasswordSecret` upsert-once `agentdPassword`** (`secrets.go`): the convergence path was refactored from admin-token-single-key to a `changed`-flag loop adding BOTH missing keys (`admin-token`, `agentdPassword`) — never rotating existing values; new Secrets get all three keys. Runs in handlePending, before any sidecar-enabled build (Q3).
- **Sidecar env wiring** (`agentd_sidecar.go`): `AGENTD_CONTROL_PLANE_PASSWORD` from the `agentdPassword` key, sidecar container only. The main container is deliberately NOT wired (the secret must never exist in uid-1000 space) — pinned by test (no env var BY NAME, no secretKeyRef BY KEY on the main container).

### agentd
- **`checkBasicAuthAny(r, passwords...)`** (`auth.go`): the §D1 per-endpoint gate. Empty entries are SKIPPED (an unset credential must never authenticate as empty-password "opencode:"); all entries compared (no short-circuit) so timing doesn't reveal WHICH matched; each comparison constant-time.
- **Handler signatures**: control-plane handlers take `(workspacePassword string, extraAuth ...string)` — the workspace password is both an accepted credential AND the opencode CLIENT credential (agent_reload's dispose call, workflow-execute's agent-node sessions, delete-session) per §D1's "sidecar retains the workspace password as a CLIENT credential". `workflowCancelHandler` keeps pure variadic (no client use).
- **`reloadSecretsDeps.ControlPlanePassword`**: reload-secrets checks `checkBasicAuthAny(ControlPlanePassword, OpencodePassword)`.
- **`serverDeps.controlPlanePassword` + wiring**: single-container mode leaves it empty (empty entry skipped → behavior byte-identical); sidecar mode passes both.
- **Sidecar mode**: `AGENTD_CONTROL_PLANE_PASSWORD` env required-fatal at boot (D5.2/D5.3 doctrine; upsert guarantees presence); `buildSidecarDeps` resolves it (single resolution site).

### Behavior table (as pinned)
| Route | Accepted |
|---|---|
| reload-secrets, agent/reload, workflow/* | agentdPassword OR workspace password (D6.1 mixed window) |
| /v1/mcp | workspace password ONLY |
| /v1/dev-preview/ | workspace password ONLY |

**V4 note (honest scoping):** the strict end-state (workspace password → 401 on control plane) is US-5's canary-graduation gate per the design's phasing — D6.1 requires the OR during the migration window; US-3 pins the window behavior.

---

## Key Decisions
1. **OR-acceptance on control plane during the window** — straight from D6.1 (API server dispatches one credential to pods of any generation). Strictness lands with US-5.
2. **`(workspacePassword, extraAuth...)` over pure variadic** — three of the five control-plane handlers use the password as an opencode CLIENT credential; a variadic blob would make "which entry is the client secret" positional magic.
3. **Empty-entry skip in checkBasicAuthAny** — without it, single-container mode (empty control-plane field) would accept `opencode:` (empty-password Basic).
4. **buildSidecarDeps resolves the env credential** — single site; the command's boot check is fail-fast UX, not the resolution path.

---

## Blockers
None.

---

## Tests Run
RED first (missing symbols / failing upserts captured). Green: `TestEnsurePasswordSecret_*` (3 new), `TestAgentdSidecar_ControlPlanePasswordEnvAndIsolation` (isolation both by name and by key), `TestControlPlaneAuth_*` (both-accepted, single-container-unchanged, wrong-pw-rejected), `TestMCPEndpoint_/TestDevPreview_` (workspace-only; agentdPassword rejected), `TestCheckBasicAuthAny_EmptyCredentialNeverMatches`, sidecar env tests. envtest L2 extended (`agentdPassword` key + header assertions still pass). Full `cmd/workspace-agentd` (293s) + `controller/internal/workspace` suites green; vet/fmt/lint clean.

---

## Next Steps
1. Merge; L3 workflow re-run picks up the sidecar image automatically (weekly/dispatch).
2. **US-4**: mount relocations (sidecar-owned `rt/secrets`, RO agent-config for the workspace container) + spawn_env consumer end-to-end (sidecar reload → composed env → socket → next spawn).
3. **US-5**: canary, V-matrix (V2/V4 legs now meaningful), D6.1 rollback exercise, gVisor leg.

---

## Files Modified
- `controller/internal/workspace/secrets.go` (upsert), `agentd_sidecar.go` (env), `agentd_password_upsert_test.go` (new), `agentd_sidecar_pod_test.go` (+isolation test), `agentd_sidecar_envtest_test.go` (key + assertion)
- `cmd/workspace-agentd/auth.go` (checkBasicAuthAny), `workflow_execute.go`, `agent_reload.go` (signatures), `secrets.go` (deps field + gate), `server.go` (deps field + wiring), `sidecar_mode.go` (env reader, boot check, deps resolution), `agent_reload_test.go` (call-site reorder), `control_plane_auth_test.go` (new), `sidecar_mode_test.go` (+3 tests)
- `worklogs/NNNN_2026-08-21_0051-us3-credential-split.md`

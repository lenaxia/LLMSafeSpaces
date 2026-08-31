# Epic 70 — Secret Delivery v2 (design 0057)

Pull-based, terminal-verified credential delivery. Normative source: **issue #1158** (invariants I1–I15, R1–R3, AC-1–AC-17, story map, gap register W1–W16). Design: [`design/0057_2026-08-30_secret-delivery-v2.md`](../../0057_2026-08-30_secret-delivery-v2.md).

## Story status

| Story | Issue | Scope | State |
|---|---|---|---|
| US-70.0 | — | delivery test harness (API fault injection, key-row corruption, SA-token time-travel, gVisor load, chaos-kill) | open — blocks the cluster-bound ACs of 70.1+ |
| US-70.1 | #1162 | spawn-time env pull (R2): bounded wait + last-good cache; `spawned_rev` (I4); degrade reason codes healthz → CRD; A2 validation; R3 cross-uid boot matrix | landed 2026-08-30 |
| US-70.2 | — | one builder + two-tier revisions (R1) + conditional pull endpoint | open |
| US-70.3 | — | notify-pull + reconcile loop + revocation + `secrets_resync`; alerts + SLO | open |
| US-70.4 | — | login-independent re-wrap reconciler (CAS, retained wrap re-wrapped) | open |
| US-70.5 | — | demolition: `InjectSecrets*`, `rehydrateDEKFromJWTSession`, `secretautopush`+`UserCredsPresent`, `pushInitialSpawnEnv`, reload cache handoff | blocked by 70.3 + 70.4 |

## US-70.1 implementation notes (2026-08-30)

- The push→pull flip: `pushInitialSpawnEnv`'s boot call is gone (the structurally-impossible dial); the supervisor (`supervise-opencode`) pulls `GET /v1/spawn-env` from the sidecar user mux at **every spawn** via a `preSpawn` hook outside `managedProcess`'s locks (bounded 2s wait; last-good delta cached in supervisor memory only, I7).
- A2 resolution (recorded deviation): the supervisor authenticates with the **§D1 carve-out workspace password** (`OPENCODE_SERVER_PASSWORD`, already controller-wired into the main container env) — *not* the literal `agentdPassword`, which design 0051 D1 forbids in uid-1000 space. A2's substance — "the pull credential is readable by the supervisor's uid at spawn time" — is validated by the pod-spec pin + exec tests.
- R3 matrix findings fixed in passing: the base pod build leaked `AGENTD_ADMIN_TOKEN`/`AGENTD_ADMIN_TOKEN_FILE` into the uid-1000 main container in sidecar mode (design 0051 D1 breach) — now stripped by `applyAgentdSidecar`.
- Standing matrices: `cmd/workspace-agentd/cross_uid_boot_matrix_test.go` (file modes + pull crossings) and `controller/internal/workspace/cross_uid_boot_matrix_test.go` (pod wiring). New crossings go there, never fixed ad hoc.
- Cluster-bound ACs (AC-2 suspend/resume ≤90s, gVisor runsc, 100-resume scale) await US-70.0's harness; the in-process exec-level analogs are green (`spawn_env_pull_exec_test.go`).

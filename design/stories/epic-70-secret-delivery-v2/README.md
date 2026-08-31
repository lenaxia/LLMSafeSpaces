# Epic 70 — Secret Delivery v2 (design 0057)

Pull-based, terminal-verified credential delivery. Normative source: **issue #1158** (invariants I1–I15, R1–R3, AC-1–AC-17, story map, gap register W1–W16). Design: [`design/0057_2026-08-30_secret-delivery-v2.md`](../../0057_2026-08-30_secret-delivery-v2.md).

## Story status

| Story | Issue | Scope | State |
|---|---|---|---|
| US-70.0 | — | delivery test harness (API fault injection, key-row corruption, SA-token time-travel, gVisor load, chaos-kill) | open — blocks the remaining cluster-bound ACs |
| US-70.1 | #1162 | spawn-time env pull (R2): bounded wait + last-good cache; `spawned_rev` (I4); degrade reason codes healthz → CRD; A2 validation; R3 cross-uid boot matrix | **landed 2026-08-30**; surface completion + cluster e2e (`local/us-70-secret-delivery-e2e.sh`, nightly-wired) via #1164 on 2026-08-31 |
| R2b fix | #1165 | file-class ownership flip: sidecar stages manifest, uid-1000 supervisor writes; manifest-scoped reset; per-type mode contracts; `files_rev`; R3 schema growth (owner-uid + consumer-constraint) | **landed 2026-08-31** |
| US-70.2 | — | one builder + two-tier revisions (R1, covers the file manifest) + conditional pull endpoint | open — absorbs the #1165 manifest into the revision model |
| US-70.3 | — | notify-pull + reconcile loop + revocation + `secrets_resync`; alerts + SLO; consumes `files_rev` as the file-class oracle | open |
| US-70.4 | — | login-independent re-wrap reconciler (CAS, retained wrap re-wrapped) | open |
| US-70.5 | — | demolition: `InjectSecrets*`, `rehydrateDEKFromJWTSession`, `secretautopush`+`UserCredsPresent`, `pushInitialSpawnEnv`, reload cache handoff | blocked by 70.3 + 70.4 **and #1165** |

## US-70.1 implementation notes (2026-08-30)

- The push→pull flip: `pushInitialSpawnEnv`'s boot call is gone (the structurally-impossible dial); the supervisor (`supervise-opencode`) pulls `GET /v1/spawn-env` from the sidecar user mux at **every spawn** via a `preSpawn` hook outside `managedProcess`'s locks (bounded 2s wait; last-good delta cached in supervisor memory only, I7).
- A2 resolution (recorded deviation): the supervisor authenticates with the **§D1 carve-out workspace password** (`OPENCODE_SERVER_PASSWORD`, already controller-wired into the main container env) — *not* the literal `agentdPassword`, which design 0051 D1 forbids in uid-1000 space. A2's substance — "the pull credential is readable by the supervisor's uid at spawn time" — is validated by the pod-spec pin + exec tests.
- R3 matrix findings fixed in passing: the base pod build leaked `AGENTD_ADMIN_TOKEN`/`AGENTD_ADMIN_TOKEN_FILE` into the uid-1000 main container in sidecar mode (design 0051 D1 breach) — now stripped by `applyAgentdSidecar`.
- Standing matrices: `cmd/workspace-agentd/cross_uid_boot_matrix_test.go` (file modes + pull crossings) and `controller/internal/workspace/cross_uid_boot_matrix_test.go` (pod wiring). New crossings go there, never fixed ad hoc.
- Cluster-bound ACs (AC-2 suspend/resume ≤90s, gVisor runsc, 100-resume scale): the e2e rows landed with #1164 (`local/us-70-secret-delivery-e2e.sh`, wired into e2e-nightly; bounded-suspend variant nightly, ≥1h #1087 gate + runsc legs gated for the US-70.0 pool).

## File-class ownership flip (#1165) — added 2026-08-31

Live defect: sidecar-mode ssh-key materialization is uid-2000-owned; OpenSSH rejects the config on **ownership** (readability was never sufficient). Same-family findings from the full crossing audit:

1. **`reset()` wipes uid-1000-owned files in `~/.ssh` on every reload** — `RemoveAll(SSHDir)` destroys the user's `known_hosts`, agent-generated keys, and config edits whenever credentials are bound/unbound (within a pod lifetime; tmpfs already wipes across suspend/resume).
2. **`secret-file` class is open** — arbitrary consumers at 0640 uid-2000-owned; any secure-file-semantics tool (kubeconfig/gnupg/TLS-key consumers) can reject the way ssh did.
3. `git-credentials` works today (git's store helper checks nothing) but carries the same ownership drift — a matrix row, not a bug.

**Fix shape (R2b, design 0057):** credential files are written by the uid that consumes them — the sidecar stages canonical bytes + a typed manifest behind the existing §D1 pull seam; the uid-1000 supervisor writes them at `preSpawn`/reload with per-type mode contracts (one table in `pkg/agentd/secrets`); `reset()` deletes manifest entries only (never directories); `files_rev` gives file-class terminal verification through the existing statusz → `secretsDelivery` path. Platform ssh `config` gains `Include ~/.ssh/config.d/*` so user fragments survive reloads. Rejected: chown helper (caps violate design 0051), per-consumer copy scripts, ssh-only patch, any new abstraction (Rule 12 — the manifest is a byte pipeline, not a framework).

**R3 schema growth:** matrix rows gain owner-uid + consumer-constraint fields; rows added for every file class plus the reset blast-radius rule.

**Assumption to pin (matrix/controller test):** `/sandbox-runtime` remains RW in the workspace container under sidecar-mode mount topology — evidenced by #1165's uid-1000-written `known_hosts`, currently untested.

**Sequencing:** fix lands before US-70.5 (no demolition while file-class is broken live); US-70.2 folds the manifest into `manifest_rev`/`batch_hash`; US-70.3 consumes `files_rev`. Nothing from the fix is discarded by later stories.

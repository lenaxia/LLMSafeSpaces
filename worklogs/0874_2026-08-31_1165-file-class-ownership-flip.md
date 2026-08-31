# Worklog: #1165 fix — R2b file-class ownership flip (staged delivery, ledger reset, files_rev)

**Date:** 2026-08-31
**Session:** Implement design 0057 R2b (#1165): sidecar-materialized SSH credentials were unusable by uid-1000 (OpenSSH rejects uid-2000-owned `~/.ssh/config` on ownership), and the audit found `reset()`'s `RemoveAll(SSHDir)` wiped uid-1000-owned user state (`known_hosts`, user keys) on every credential reload. Fix the class: credential files are written by the uid that consumes them.
**Status:** Complete — unit + integration + exec-level suites green; cluster e2e row added (AC-F).

---

## Objective

Ownership by construction: the sidecar STAGES canonical bytes + a typed manifest behind the §D1 pull seam; the uid-1000 supervisor (sidecar mode) or the materializer itself running as uid 1000 (single-container boot/reload) WRITES the delivered files. Reset becomes ledger-scoped — deleting exactly the previous manifest's entries — so user-owned files in the same directories survive every reload. `files_rev` gives file-class terminal verification through the existing statusz → CRD path.

## Work completed

### Materializer (`pkg/agentd/secrets`)

- New `staging.go`: `StagedEntry{Target, Mode, File}` manifest + atomic publish (tmp tree → rename swap — the endpoint never sees a half-built state) + per-type mode contracts (`ModeSSHPrivateKey/ModeSSHConfig/ModeGitCredential = 0600`, `ModeSecretFile = 0600` default with validated `metadata.mode` override; group/world-write rejected).
- `applySSHKey` / `applyGitCredential` / `applySecretFile` stage instead of writing consumed paths. The ssh config is assembled per-pass with an `Include config.d/*` header — user fragments get a reload-proof home.
- `reset()` no longer touches the consumed dirs (the #1165 blast-radius root cause); it clears staging scratch + env/config only. Empty batches publish `[]` (revocation is absence).
- CROSS_UID's 0640 regime is retired for file classes (it solved readability; consumers need ownership); secrets-env keeps it (init-1000 writer → sidecar-2000 boot handoff, platform parser).
- Test corpus rewritten to the new contract (staging assertions, user-state survival pin, pod-death PVC-clean walk).

### Pull endpoint + applier (`cmd/workspace-agentd`)

- `GET /v1/spawn-files` (`spawn_files_pull.go`): §D1 Basic gate; manifest + inlined bytes + staging-side rev, read fresh at request time; absent staging = quiet empty; corrupt = loud 500. Supervisor puller reuses the env puller's bounded-wait machinery with the `spawn_files_*` reason-code family.
- `spawn_files_apply.go`: the delivery applier — validates every target against configured roots and rejects weak modes (`spawn_files_bad_path`); writes via temp+rename with the FINAL mode; creates `~/.ssh/config.d`; maintains `spawn-files-ledger.json` (level-triggered revocation truth: reset deletes ledger−manifest, never directories); self-computes `files_rev` (I4).
- `preSpawn` pulls + applies files at every spawn; on failure the on-disk delivered set is the last-good cache and the degrade is loud (tools read files at invocation time, so the next successful apply heals without losing the session).
- Context split: the `materialize` subcommand and single-container reload DELIVER directly after staging (`deliverStaged` — same applier, same ledger; boot keeps the synchronous #443/#1087 file-present guarantee); sidecar contexts (CROSS_UID) stage only.
- Status plumbing: `files_rev` + `spawn_files_reason` on the control socket → statusz → healthz `SpawnEnvHealth{FilesRev, FilesDegraded, FilesReason}` → CRD `SecretsDeliveryStatus.FilesRev` (controller mirror + helm schema; additive, mixed-fleet tolerant like the existing fields).

### R3 matrix + assumption pin

- `cross_uid_boot_matrix_test.go` rows grow **ownerUID + consumerConstraint** fields (the #1165 lesson: readability could not predict ssh's ownership refusal); registry rewritten for the delivery architecture (staging row, delivered file-class rows with consumer constraints, user-state row with the blast-radius pin, ledger row, `/v1/spawn-files` endpoint row); completeness check enforces the new fields.
- Controller pin: the workspace container mounts `/sandbox-runtime` RW in sidecar mode (the delivery-writability assumption, evidenced by #1165's uid-1000-written `known_hosts`) — now enforced by `TestAgentdSidecar_WorkspaceContainerSandboxRuntimeRW`.

### Tests (new)

- `spawn_files_pull_test.go`: handler auth/method/empty/corrupt, rev determinism, puller reason codes (no-credential, 401-immediate, malformed-permanent, unreachable-bounded).
- `spawn_files_apply_test.go`: mode-faithful writes, ledger-scoped revocation with user-state survival, ledger continuity across applier restarts, empty-manifest revoke-all, root/mode confinement, idempotent re-apply, env overrides.
- `spawn_files_exec_test.go` (real subcommand + real handlers + real materializer): AC-F1 ownership-by-construction at first spawn; AC-F2 revocation-is-absence via reload; AC-F3 known_hosts survives the reload cycle; AC-F4 dead-endpoint loud degrade.
- Extended: `spawnEnvHealth` files projection + warning; controller FilesRev mirror (degraded + converged); sidecar reload end-to-end stages-only; the reload-failure test now trips via unwritable staging (the platform-owned failure surface moved).
- e2e: `local/us-70-secret-delivery-e2e.sh` gains the AC-F row (ssh-key bind → delivered `id_ed25519_deploy` uid-owned 0600, config owner = consuming uid, `filesRev` on the CRD).

## Assumptions (Rule 7 — stated and validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | `/sandbox-runtime` is RW in the workspace container under sidecar-mode mount topology | #1165 live repro (uid-1000-written known_hosts) + now pinned by `TestAgentdSidecar_WorkspaceContainerSandboxRuntimeRW` |
| A-2 | Single-container boot keeps its synchronous file-present guarantee | The materialize subcommand delivers directly (`deliverStaged`); `TestGitCredentialColdBoot_SurvivesSuspendResume` / `TestContainerRestart_SSHKeySurvivesRestart` (#1087/#443) pass unchanged in intent |
| A-3 | A concurrent supervisor pull + direct delivery apply the identical manifest idempotently | Same applier, atomic temp+rename writes, ledger last-writer-wins with identical paths; exec tests cover the pull side |
| A-4 | Staging beside secrets-env (single-container) keeps tests isolated and production identical | `stagingDirFor`: production secrets-env is `/sandbox-runtime/secrets-env` → `/sandbox-runtime/staged-secret-files` (same as before); sidecar (CROSS_UID) pins the roomy shared tmpfs |
| A-5 | 8MiB manifest cap covers realistic file-class batches under the 96MiB tmpfs | Bound enforced (`spawnFilesBodyCap`); bind-time size validation remains W8 |

## Key decisions

1. **Ownership by construction over post-hoc fixing** — no chown helper (caps violate design 0051), no per-consumer copy scripts, no ssh-only patch (leaves secret-file/git/wipe bugs).
2. **Ledger-scoped reset** — the delivered-path lifecycle is exact-set reconciliation, which is what makes user state provably safe.
3. **One applier, two invokers** — supervisor pull (sidecar) and materializer-direct (single-container) share the delivery code + ledger; no forked semantics.
4. **Consumer contracts as data** — the mode table lives beside the secret types; the supervisor knows nothing about ssh.
5. **`files_rev` derived from what was actually written** (never the server-advertised rev) — terminal verification, same doctrine as `spawned_rev`.

## Adversarial review (Rule 11)

- Partial staging window → closed by the atomic tmp→rename publish (the endpoint sees complete generations only); corrupt staging is a loud 500, never partial (pinned).
- Manifest-injection (paths outside roots / weak modes) → whole-manifest refusal with `spawn_files_bad_path` (pinned).
- Test-env ambient pollution (this suite runs inside a live sidecar pod) → `runMaterializeSubcommand` neutralizes `LLMSAFESPACES_CROSS_UID_FILES`; staging derives beside the overridden env path.
- Silent python-edit no-op during development produced a `deliverStaged` reading an empty cfg path (empty-manifest no-op delivery) — caught by the #1087 boot tests, root-caused, fixed; a reminder that batch edits must be verified by grep, not assumed.

## Blockers

None in-repo. The ≥1h/gVisor legs of the e2e remain pool-gated as before (US-70.0).

## Tests run

- `go test -timeout 900s ./cmd/workspace-agentd/` — ok (185s, includes the new exec suite)
- `go test ./controller/... ./pkg/agentd/... ./pkg/apis/... ./pkg/secrets/...` — ok
- `go test -short ./api/...` — ok
- `go build ./...`, `go vet` (touched pkgs), `gofmt`/`goimports` — clean

## Next steps

1. US-70.2 folds the file manifest into `manifest_rev`/`batch_hash` (seam unchanged).
2. US-70.3 consumes `files_rev` for file-class reconcile; consider notify-pull for mid-session file updates.
3. W8 bind-time size validation for file-class batches.

## Files

- `pkg/agentd/secrets/`: staging.go (new), secrets.go (staging applies, reset, mode contracts, doc), cross_uid_test.go (rewritten), symlink_* test updates, staging coverage
- `cmd/workspace-agentd/`: spawn_files_pull.go (new), spawn_files_apply.go (new), secrets.go (deliverStaged wiring + stagingDirFor), supervise_opencode.go (files pull/apply/state), control_socket.go, control_client.go, supervisor_status.go, server.go (endpoint), healthz via types, cross_uid_boot_matrix_test.go, supervisor_status_test.go, new test files
- `pkg/apis/.../workspace_types.go`, `helm/crds/workspace.yaml`: `filesRev`
- `controller/internal/workspace/`: health.go (FilesRev + files-reason precedence), agentd_sidecar_pod_test.go (RW pin), health_spawn_env_test.go
- `pkg/agentd/types.go`: SpawnEnvHealth files fields
- `local/us-70-secret-delivery-e2e.sh` (AC-F row), `README-LLM.md` (volume row), `design/0057…md` + epic README (state updates)

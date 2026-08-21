# Worklog: design 0051 US-4b — mount relocations by consumer + cross-uid rt/* modes

**Date:** 2026-08-21
**Session:** Implement US-4b per the owner ruling (#978, 2026-08-21) and the merged design amendment (#1018, `eabc93f0`). User ruling this session: the rt/* cross-uid write defect is IN scope for US-4b.
**Status:** Complete (pending review)

---

## Objective

Relocate agentd's stores by CONSUMER (§D1 amendment):

- `agentd-config` (NEW Memory emptyDir, 8Mi): `agent-config.json` + `allowed-dirs.json` — RW sidecar, **RO workspace container** (integrity by mount, V3).
- `agentd-secrets` (NEW Memory emptyDir, 16Mi): `secrets-env`, `admin-prompt.md`, `last-reload-secrets.json` — **sidecar-only**, never mounted in the workspace container (V2: absent from uid-1000 space by mount topology; env crosses only via spawn_env, US-0.2(a)).
- `sandbox-runtime` (existing): `rt/ssh`, `rt/secrets`, `rt/git-credentials`, `rt/auth.json` stay uid-1000 tool-consumed (US-35.7 class C), RW both containers. Restart marker stays shared.
- Single-container mode unchanged in behavior (all relocations are sidecar-mode env overrides).

---

## Assumptions (README Rule 7 — stated up front, validated as work proceeded)

1. **The sidecar image has no shell** — `cmd/workspace-agentd/Dockerfile` is `FROM scratch` ("no distro, no shell"). Validated by reading the header. **Finding en route:** US-4a's `parseSecretsEnvDelta` shelled out to `bash` — it could only fail in real sidecar pods. Fixed here (see Key Decision 2). Pinned by `TestUS4B_ParseSecretsEnvDelta_WorksWithoutShell` (runs with `PATH=""`).
2. **`reset()` needs group-write on `rt/` and its children to run cross-uid** — RemoveAll/Remove unlink via the parent directory's write bit; `rt/` was created 0755 by the init script. Validated by reading `Materializer.reset()` + POSIX unlink semantics. Fix: sidecar-gated `chmod 0770` in the init script (exec-pinned both modes).
3. **Materialized rt/* files need 0640 when the sidecar writes them** — uid-1000 tools (gid 1000) read them after a reload re-materializes them uid-2000-owned. Extends the materializer's documented T2 exception, gated behind `CrossUID`.
4. **`OPENCODE_CONFIG` must be overridable in the entrypoint** — the supervisor spawns `opencode serve` directly (managed_process.go:499) and the entrypoint exported the path unconditionally. Validated by reading both. Fix: `export OPENCODE_CONFIG="${OPENCODE_CONFIG:-…}"` (exec-pinned both modes by `TestEntrypointOpenCodeConfig_HonorsPresetValue`).
5. **auth.json has NO cross-uid writer** — `StageCredentials` is an HTTP call (opencode writes its own file, uid 1000); `updateAuthJSONForRelay` runs in the init (uid 1000); the in-pod relay injector is dead in sidecar mode (`deps.proc == nil`). Validated by reading `client.go:174`, `pre_boot_relay.go:293`, `maybeStartRelayInjector`.
6. **The sidecar can write the emptyDir volume roots** (marker, temp+rename into `/agentd-config`) — kubelet applies fsGroup 1000 (`g+rwX`) to volume roots. Validated by the shipped L3 K5 marker write + kubelet SetVolumeOwnership semantics.
7. **Sizes 8Mi/16Mi** — config JSON + allowlist + warning vs. secrets-env + prompt + full-plaintext reload batches (sandbox-cfg's 32Mi class). No chart values needed (controller-built pod-local volumes, same as `sandbox-runtime`).
8. **K3 asserts from the workspace container only** — the scratch sidecar has no `stat`/`sh`; the workspace-side assertions are exactly the V2/V3 claims.

---

## Work Completed

### Controller (`agentd_sidecar.go`, `pod_builder.go`, `constants.go`)
- Two Memory emptyDir volumes (8Mi/16Mi) appended in `applyAgentdSidecar`; mount matrix: sidecar RW both, workspace RO `agentd-config` at `/agentd-config` and NEVER `agentd-secrets`, credential-setup init RW both (bootstrap/materialize write the relocated stores at boot).
- Sidecar env: the five `LLMSAFESPACES_*_PATH` overrides + `LLMSAFESPACES_CROSS_UID_FILES=1`. Main env: `OPENCODE_CONFIG=/agentd-config/agent-config.json`. Init env: `AGENTD_SIDECAR_MODE=1` + config/secrets-env/reload-cache path overrides + `LLMSAFESPACES_CROSS_UID_FILES=1` (the init's boot files — secrets-env, reload cache — are read cross-uid by the sidecar).
- cred script: guarded sidecar branch — `chmod 0770` on `rt`, `rt/secrets`, `rt/ssh` (reset() unlink bridge) + bootstrap `--admin-prompt-out /agentd-secrets/admin-prompt.md --allowed-dirs-out /agentd-config/allowed-dirs.json`; bare else branch byte-equal to the old invocation.

### agentd binary
- `us4b_paths.go` (new): `agentConfigPathFromEnv` / `adminPromptPathFromEnv` / `allowedDirsPathFromEnv` / `secretsEnvPathFromEnv` (moved) / `reloadCachePathFromEnv` / `modelWarnPathFromEnv` (derived from the config dir — the writer derives it from `filepath.Dir`, the reader now matches under any relocation) / `bootAgentConfigPathsWithEnv`.
- `main.go` + `sidecar_mode.go`: boot stamp consumes `bootAgentConfigPathsWithEnv()` (env unset → consts, single-container identical). `pre_boot_relay.go`: `effective*Path` honor env after the test override. `server.go`: healthz (`user_creds_present`, warnings) + statusz wired env-aware.
- `spawn_env_consumer.go`: **`parseSecretsEnvDelta` is now pure Go** — `scanShellquoteExports`, the exact inverse of the materializer's `FormatEnvLine` (`export NAME='…'` with `'\''` escapes, multi-line values supported). Malformed input is an error (single machine writer → corruption must surface; callers degrade safely). Not the G2 bug class: we parse our own canonical encoder output, not arbitrary bash.
- Materializer (`pkg/agentd/secrets`): `CrossUID bool` + `secretFileMode()`/`secretDirMode()` + `mkdirExact` (MkdirAll honors umask — a 022 sidecar umask would strip the group-WRITE bit from 0770 dirs, the exact bit the next reset() unlink needs; the follow-up Chmod is umask-immune). New `Chmod` on the `Filesystem` seam (real + fake). Sites: reset() dirs, ssh keys/config, git-credentials, secret-file files+dirs, secrets-env. **0640 applies to secrets-env cross-uid too** — the init writes it, the sidecar's boot handoff reads it; the uid-1000 exclusion is the MOUNT, not the mode. `LLMSAFESPACES_CROSS_UID_FILES` read in `loadMaterializeConfig`, threaded to the reload handler's Materializer. `writeReloadSecretsCache`: rename-atomic already; mode 0600→0640 under the env (the sidecar's healthz reads the init-written boot cache cross-uid).
- `runtimes/base/tools/entrypoints/entrypoint-opencode.sh`: `OPENCODE_CONFIG` honors a pre-set value.

### L3 + docs
- `scripts/us2-kind-integration.sh`: K2 reads `/agentd-config/agent-config.json`; K3 extended to the US-4b topology (config 640 on RO mount, admin-prompt absent, `/agentd-secrets` unmounted, `rt` 770, password 600).
- `docs/testing/0051-us2-integration-test-plan.md`: K3 row updated, US-4 follow-up marked done. `README-LLM.md`: volume table + path-constant overrides + OPENCODE_CONFIG note.

---

## Key Decisions

1. **`CrossUID` is env-gated, not unconditional** — single-container mode must stay byte-identical (ruling). The env is wired on the SIDECAR (reload path) and the INIT (boot files the sidecar reads) in sidecar-mode pods only.
2. **Pure-Go secrets-env parser** — forced by the scratch sidecar image (no bash). Exact-inverse-of-encoder is safe where bash-source-of-arbitrary was not (G2). Fixture alignment: the US-4a tests' hand-written prefix-less lines were updated to the canonical `export ` form the single writer emits.
3. **Boot handoff cross-uid bridge = 0640 + shared gid** — found in adversarial review: the init (uid 1000) writes `secrets-env` + reload cache onto the sidecar-only volume; the sidecar (uid 2000) reads both at boot. 0600 would have silently killed every env-secret at sidecar boot (push degraded to warn). The security property (uid-1000 code cannot read them) is delivered by mount topology — the volume is never in the workspace container.
4. **`mkdirExact` (MkdirAll+Chmod)** — umask would silently strip the group-write bit the cross-uid design depends on; modes must be exact, not best-effort.
5. **Rollback (D6.1) needs no mode repair** — emptyDirs are per-pod; a rolled-back pod's init re-creates 0700 dirs it owns. The ruling's "re-chowns by rewriting" holds naturally.

---

## Adversarial self-review (Rule 11) — validated findings

- **REAL (fixed): boot handoff cross-uid EACCES** — init-written `secrets-env`/reload-cache at 0600 unreadable by the uid-2000 sidecar. Fixed via init-side `LLMSAFESPACES_CROSS_UID_FILES` + `secretFileMode()` on secrets-env + cache mode. Pinned: `TestCrossUID_SecretsEnvGroupReadableForBootHandoff`, `TestUS4B_ReloadCacheMode_CrossUID`, `TestUS4B_Enabled_CredentialSetupInitSidecarEnv`.
- **REAL (fixed): umask stripping group-write from 0770 dirs** (`MkdirAll` alone) — pinned by `TestCrossUID_ToolConsumedFilesGroupReadable` against the real FS.
- **False alarms (documented):** auth.json cross-uid writes (none exist — HTTP-staged, init-side, or dead code paths); concurrent init/sidecar writes to `/agentd-config` (native sidecar starts only after credential-setup completes); rollback mode repair (fresh emptyDirs per pod); gVisor nested-mount behavior (unchanged class, remains US-5's V9 leg); `secrets-env` 0640 "leak" (mount topology is the boundary, volume absent from uid-1000 space).

---

## Blockers

None. L3 kind execution (K2/K3 re-run) rides the existing weekly/dispatch workflow.

---

## Tests Run (all green at commit time)

- Controller: full `./controller/internal/workspace/` unit suite (incl. new US-4b mount-matrix/env/script pins + exec-level sidecar-branch tests under real `/bin/sh` with a PATH shim, both modes) + envtest `-tags envtest` (`TestEnvtest*`, incl. new `TestEnvtestUS4B_MountTopologyAdmitted` against a real API server) + full `./controller/...`.
- agentd: full `./cmd/workspace-agentd/` (237s — incl. the real-socket reload e2e + cross-uid mode leg + boot-order test) + `./pkg/agentd/...`.
- `go build ./...`, `go vet` clean, `gofmt` clean. `go test ./pkg/...` green.

---

## Files Modified

- Controller: `agentd_sidecar.go`, `pod_builder.go`, `constants.go`, `us4b_mount_relocations_test.go` (new), `us4b_cred_script_exec_test.go` (new), `agentd_sidecar_envtest_test.go`, `cred_script_exec_test.go` (anchor fix), `entrypoint_agentd_test.go` (new test)
- agentd: `us4b_paths.go` (new), `us4b_sidecar_relocations_test.go` (new), `spawn_env_consumer.go` (+test fixtures), `secrets.go`, `main.go`, `sidecar_mode.go`, `pre_boot_relay.go`, `server.go`, `path_consistency_test.go`
- pkg: `agentd/secrets/secrets.go` (+`secrets_test.go` fakeFS Chmod), `agentd/secrets/cross_uid_test.go` (new)
- Runtime: `runtimes/base/tools/entrypoints/entrypoint-opencode.sh`
- L3/docs: `scripts/us2-kind-integration.sh`, `docs/testing/0051-us2-integration-test-plan.md`, `README-LLM.md`

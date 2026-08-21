# Worklog: design 0051 US-4a — spawn_env consumer end-to-end

**Date:** 2026-08-21
**Session:** Implement the US-0.2(a) IPC handoff consumer side: boot push, reload push-then-restart, and supervisor-side merge semantics.
**Status:** Complete (pending review)

---

## Objective

Close the documented US-2 gap: in sidecar mode nobody drove the spawn_env mechanism — the supervisor's first spawn had no secrets env, and reloads applied files but never restarted (`deps.proc` nil). US-4a delivers the mechanism; US-4b (next PR) relocates the mounts that make the file sidecar-only.

---

## Work Completed

### New (`spawn_env_consumer.go`)
- `parseSecretsEnvDelta` — the file-introduced variables ONLY, via the same bash-source + `env -0` machinery as `buildEnvFrom` (a Go re-implementation would have to mirror bash quoting — the G2 bug class). Excludes parent-present keys and shell noise (SHLVL/PWD/OLDPWD/_). Absent file → empty delta, no error.
- `pushInitialSpawnEnv` — boot handoff: push the delta over the socket BEFORE the muxes serve; kubelet's startup-probe gating of the workspace container means it lands before the supervisor's first spawn. Failure logged, not fatal (missing file = no env-secrets; `buildEnvFrom` degradation parity).
- `socketReloadProc` — the reload path's `restartableProcess`: re-reads the FRESH secrets-env at restart time (deferred/session-aware restarts hand off the LATEST materialization — last-write-wins per A.2/A.3), pushes the delta, then requests the `credential_reload` restart.
- `parentPlusDelta` — merge: parent entries win on conflict (platform vars not overridable by user secrets — `buildEnvFrom` parity), delta keys appended.

### Merge semantics change (`supervise_opencode.go`)
`SetSpawnEnv`'s wrapper now composes parent + delta instead of wholesale replacement. **Why the change is required, not cosmetic:** A.4 forbids env OUT of the supervisor — the sidecar cannot learn the supervisor's parent env to compose it — so the sidecar can only ever send the delta; composition must happen on the supervisor side. Wholesale replacement was US-2's interim shape (test-only consumers).

### Wiring
- `serverDeps.reloadProc` — `wireHTTPServers` uses it when `deps.proc` is nil (sidecar). Single-container mode unchanged (`deps.proc`).
- `buildSidecarDeps` constructs `socketReloadProc` with `secretsEnvPathFromEnv()` — the same `LLMSAFESPACES_SECRETS_ENV_PATH` coordinate the materializer writes, so US-4b's relocation is a controller env change, no new code paths.
- `runSidecarCommand` performs the boot push before serving.
- **Real bug found & fixed en route:** the reload handler's marker default was the `/workspace` PVC **const** — read-only to the sidecar — ignoring the env override both other write sites honor (`markerPathFromEnv()`). Socket-mode reload markers would have silently failed to persist.

### Stale pins updated to the merge contract
`TestManagedProcAdapter_SetSpawnEnvMemoryOnlyNextFactory`, `..._LastWriteWins` (explicit parent block + last-delta-wins), `TestSidecar_SpawnEnvConsumerReady` / L1 integration / supervisor-subprocess no-leak asserts → parent-retained asserts. One subprocess-test full-suite failure did not reproduce across two clean full runs after the pin fixes (the stale failures stopped polluting); debug env-dump removed (it could have echoed the live pod's secrets-env material into failure messages).

---

## Key Decisions
1. **Sidecar sends ONLY the delta; supervisor merges** — forced by A.4 (no env out of the supervisor). Parent-wins conflict rule matches `buildEnvFrom`'s file-delta semantics.
2. **Re-read at restart time** (not at reload-receipt) — the session-aware deferral can hold a restart for minutes; the handed-off env must be the latest materialization, matching reset()+re-materialize.
3. **Push failure degrades to restart-with-previous-env** — a socket hiccup must not block credential application; the next reload re-pushes.
4. **Marker fix as part of US-4a** — it's the reload path's own bookkeeping; without it the socket-mode marker contract is silently dead in sidecar mode.

---

## Blockers
None.

---

## Tests Run
New (all green): delta parser (quoting/noise/absent), boot handoff (push + empty-no-push), merge semantics, `socketReloadProc` push-then-restart + re-read-at-restart, deps wiring (reloadProc present, proc nil, path coordinate), and the full **`TestReloadHandler_SidecarEndToEnd`** — the REAL reload handler → materializer → env file at the overridden path → marker at the shared-tmpfs path → delta over the real socket → `credential_reload` restart. Full package suite green twice (302–320s) after updating the three stale pins; vet/fmt/lint clean.

---

## Next Steps
1. Merge; then **US-4b**: mount relocations — sidecar-owned `rt/secrets` + `secrets-env` at a sidecar-writable path (the env coordinate already exists), RO `agent-config.json` for the workspace container, controller env updates for both containers. The K3 kind check gains mount-relocation assertions per the plan's §6.
2. US-5 canary after that.

---

## Files Modified
- `cmd/workspace-agentd/spawn_env_consumer.go` (new), `spawn_env_consumer_test.go` (new)
- `cmd/workspace-agentd/supervise_opencode.go` (merge semantics), `server.go` (reloadProc), `sidecar_mode.go` (boot push + reloadProc wiring), `secrets.go` (marker default → markerPathFromEnv)
- Updated pins: `supervise_opencode_test.go`, `sidecar_mode_test.go`, `sidecar_integration_test.go`, `supervisor_subprocess_test.go`, `us2_regression_gaps_test.go`
- `worklogs/0819_2026-08-21_0051-us4a-spawn-env-consumer.md`

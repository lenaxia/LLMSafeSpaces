# Worklog: design 0051 US-2 — sidecar container + flag split

**Date:** 2026-08-20
**Session:** Implement US-2 of design 0051 Phase 2: the `--sidecar` mode, the chart-gated native-sidecar pod topology, the deferred US-1 socket items (grace_seconds, metrics), and the A.3 design amendment.
**Status:** Complete (pending review)

---

## Objective

Per design 0051 §8 US-2 and the session handoff: split agentd into a native sidecar (uid 2000/gid 1000, policy half) and `supervise-opencode` (PID 1 of the workspace container), chart-gated behind `agentdSidecar.enabled` (default false), preserving the #857 stamp-before-opencode-reads guarantee and wiring the two US-1 deferrals.

---

## Work Completed

### agentd `--sidecar` mode (`cmd/workspace-agentd/sidecar_mode.go`)
- `runSidecarCommand`: env-credential resolution (`AGENTD_SIDECAR_PASSWORD`, `AGENTD_ADMIN_TOKEN` — missing/empty is FATAL per D5.2/D5.3 doctrine), `ensureBootAgentConfig` stamp (the #857 guarantee now lives in the sidecar — it runs BEFORE its muxes serve, and the kubelet gates the main container on the sidecar's startup probe), `buildSidecarDeps` wiring, same background-loop set as single-container mode, graceful shutdown with no child to stop.
- `socketRestarter` (watchdog restart → socket, closed reason enum + 5s parity grace), `socketOps.pressureMonitor` (pressure reads cross the socket — the sidecar's own cgroup is the wrong container, 0050 finding), `socketOps.sysMetrics` (statusz memory/cpu from socket; disk stays local statfs).
- Main dispatch: `--sidecar` branch before the flag-parsing tail.

### Deferred US-1 items
- `grace_seconds`: `managedProcess.restartWithGrace(grace)` parameterizes the SIGTERM→SIGKILL kill timer; `restart()` delegates with the new `defaultRestartGrace` (5s, unchanged, pinned by test); `managedProcAdapter.Restart` forwards socket grace (1..300 clamped; out-of-range → default).
- `metrics`: `cgroupV2Reader` (injectable paths; frozen A.2 field set: memory.current/max, cpu usage_usec/throttled_usec); `controlSocketServer.metricsSource` wired in `runSuperviseOpencodeCommand` to `newWorkspaceCgroupReader().read`; nil source keeps the US-1 reserved envelope.

### Sidecar watchdog corroboration (`socket_vitals.go`)
- `socketVitalsGatherer`: TCP dial over shared netns + pid/boot evidence from the supervisor's `status`. Preserves the full #892 verdict matrix — HUNG (refused + live pid + past 180s boot grace) stays lethal; RESPAWN/UNKNOWN suppress. **Socket-down + refused-dial must never be lethal** (kill-without-evidence ban): pidGone=true routes it to RESPAWN. CPU deltas are honestly unavailable cross-container → cpuKnown=false (only degrades suppression labels).
- Critical wiring note: `nil` vitals would restore pre-#892 kill-on-timeout semantics; `TestSidecarDeps_VitalsAreSocketBacked` pins this.

### Control-socket client (`control_client.go`)
- One connection per request, typed results (`controlHello/Status/RestartResult`), `controlClientError` carrying the wire code (typed `controlError` stays wire-side), monotonic ids, `""` timestamp → zero time. Transport failures are plain errors (sidecar must never die because the supervisor is restarting).

### Controller (`agentd_sidecar.go`, `pod_builder.go`, `reconciler.go`, `controller.go`, `main.go`)
- `AgentdSidecarEnabled` field + `--agentd-sidecar` flag + `ValidateAgentdSidecar` startup guard (requires `--agentd-image`: the sidecar runs the delivery artifact).
- `buildAgentdSidecarContainer`: native sidecar (`restartPolicy: Always`, KEP-753), same digest-pinned image, `--sidecar`, uid 2000/gid 1000 (gid 1000 = shared group), drop ALL + RunAsNonRoot + RO rootfs, env credentials (password key now; `agentdPassword` is US-3), sandbox-cfg RO + sandbox-runtime RW + workspace subPath RO mounts, startup probe (the #857 gate) + liveness (restarts only the sidecar) + readiness.
- Ordering: sidecar is the LAST init container (after credential-setup's materialize) — pinned by test.
- Main container: `AGENTD_SIDECAR_MODE=1` (entrypoint branches to `exec supervise-opencode`) + liveness switches to kernel-level TCP on opencode's port (HTTP healthz is sidecar-served; a sidecar wedge must not restart the workspace container) + shared restart-marker env.
- Entrypoint `entrypoint-opencode.sh`: POSIX `[ = ]` branch (dash lesson); exec-level test under bash with stubs.

### Cross-uid boot-file modes (found during self-review — would have broken sidecar boot)
- The sidecar stamps `agent-config.json` that opencode must read; the init writes `admin-prompt.md`/`allowed-dirs.json` the sidecar must read. 0600 across uids = EACCES. Boot trio + model-resolution marker + restart marker → **0640** (shared gid 1000 is the bridge); credential files stay 0600. T2 invariant comment records the agent-config exception (§D1 rules it uid-1000-readable by necessity). `writeRestartReasonMarker` → 0640.
- Supervisor's post-restart health probe disabled in supervisor mode (`skipHealthProbe`): the probe URL targets the sidecar's bearer-gated readyz, and the supervisor must never hold the admin token (D1).

### Chart
- `controller.agentdSidecar.enabled` (default false) + controller-deployment flag wiring + render-fail guard when enabled without `agentdDelivery.image`. Go chart tests (run in CI; helm absent locally).

### Design amendment (separate branch `design/0051-a3-concurrency-amendment`, /design flow — holds for merge)
- A.3 Ordering replaced: handler-per-connection + restartMu/TryLock; documents the head-of-line-blocking proof from US-1's `TestControlSocket_RestartIdempotency`.

---

## Key Decisions

1. **Sidecar keeps the workspace password for US-2** (Secret key `password`); the per-endpoint credential split (`agentdPassword` key) is US-3 per design phasing. The env-only delivery shape lands now so US-3 is a key swap.
2. **Env credentials in the sidecar are safe and required**: the no-env rule exists because opencode passes its env to every tool child; the sidecar spawns nothing, and the 0600/0400 uid-1000 files under /sandbox-cfg are unreadable at uid 2000 anyway.
3. **#857 ordering via native-sidecar startup probe**: kubelet will not start the main container until the sidecar's healthz answers, and the sidecar answers only after the boot stamp. No new ordering machinery.
4. **Main-container liveness = TCP on :4096 in sidecar mode**: an HTTP probe would target sidecar-served endpoints (shared netns) and restart opencode+supervisor on a sidecar wedge — backwards.
5. **Socket-down vitals are never lethal**: with the supervisor unreachable, pidGone=true routes refused-dial to RESPAWN (suppress). The alternative (treat unknown pid as alive) would make HUNG reachable on zero evidence — the #892 ban.
6. **US-2 does NOT complete the reload path cross-uid** (sidecar re-materializing `rt/secrets/*` as uid 2000 inside uid-1000-owned 0700 dirs fails; spawn_env consumer end-to-end): that is US-4's mount-relocations scope. Chart default off; canary (US-5) runs only after US-4. Recorded here so the reviewer sees it is deliberate, not missed.

---

## Blockers

None. (Known local-env limits: helm binary absent — chart tests skip locally, run in CI; `/sandbox-cfg` read-only — unchanged.)

---

## Tests Run

- RED phase: all new test files failed on missing symbols first (`go vet` compile failures captured).
- `go test ./cmd/workspace-agentd/` — full package PASS (255s), including 13 US-1 tests, new grace/metrics/client/vitals/sidecar/mode suites.
- `go test ./controller/...` — PASS (incl. new sidecar pod-spec invariants + exec-level entrypoint test).
- `go test ./pkg/agentd/... ./pkg/agent/...` — PASS (mode changes covered).
- `go test ./helm/...` — PASS (skips locally: no helm on PATH).
- golangci-lint new-only: 0 issues (G306 suppressed at the four 0640 sites with design references; QF1008 fixed; unused-func finding fixed by wiring `newWorkspaceCgroupReader` into the supervisor — a real omission the linter caught).

---

## Next Steps

1. Merge US-2 PR after review; merge the A.3 design PR (holds).
2. **US-3**: `agentdPassword` Secret key (upsert-once pattern from #933 — never rotate in place), per-endpoint mux credential table (§D1: control plane gets agentdPassword; `/v1/mcp` + dev-preview keep the workspace password), sidecar retains the workspace password as a CLIENT credential.
3. **US-4**: mount relocations (sidecar-owned `rt/secrets`, RO `agent-config.json` for the workspace container, integrity mounts), spawn_env consumer end-to-end (sidecar reload pushes composed env over the socket before restart).
4. **US-5**: canary on TEST incl. the D6.1 rollback EXERCISE (on→off→on), V-matrix, gVisor leg (V9: runsc × native sidecar).

---

## Files Modified

- `cmd/workspace-agentd/`: `sidecar_mode.go` (new), `socket_metrics.go` (new), `socket_vitals.go` (new), `control_client.go` (new), `control_socket.go`, `managed_process.go`, `supervise_opencode.go`, `server.go`, `main.go`, `bootstrap.go`, `secrets.go`, `restart_reason.go`, `healthz_cache.go` + test files (`restart_grace_test.go`, `socket_metrics_test.go`, `socket_vitals_test.go`, `control_client_test.go`, `sidecar_mode_test.go`, `cross_uid_modes_test.go`, `control_socket_helpers_test.go`, `main_test.go`).
- `controller/`: `internal/workspace/agentd_sidecar.go` (new), `pod_builder.go`, `reconciler.go`, `internal/controller/controller.go`, `main.go`, `internal/workspace/agentd_sidecar_pod_test.go` (new).
- `pkg/`: `agentd/types.go` (SidecarRestartMarkerPath), `agentd/secrets/secrets.go` (AgentConfigWriteMode + T2 note), `agent/opencode/configwriter.go` (0640).
- `helm/`: `values.yaml`, `templates/controller-deployment.yaml`, `agentd_sidecar_chart_test.go` (new).
- `runtimes/base/tools/entrypoints/entrypoint-opencode.sh`.
- (design branch) `design/0051_2026-08-18_agentd-uid-separation.md`.

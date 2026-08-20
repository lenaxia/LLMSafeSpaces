# Design 0051 US-2 — Integration Test Plan

**Status:** L0–L2 implemented and green (this document ships with them); L3+ gated on US-4/US-5
**Date:** 2026-08-20
**Scope:** US-2 sidecar split (`--sidecar` mode + chart-gated native sidecar + supervisor socket wiring), merged in #980/#981/#982
**Related:** `design/0051_2026-08-18_agentd-uid-separation.md` (§D1 architecture, Appendix A protocol, §7 V-matrix), issue #978

---

## 1. What is being integrated

US-2 splits one process into a **pair communicating over `127.0.0.1:4099`** (Appendix A) plus the control plane that constructs them:

| # | Component | Lives in | Role |
|---|---|---|---|
| C1 | **Supervisor** — `workspace-agentd supervise-opencode` | workspace container, PID 1, uid 1000 | spawns/reaps/signals opencode; serves the control socket; sources workspace-cgroup metrics |
| C2 | **Sidecar** — `workspace-agentd --sidecar` | native sidecar container, uid 2000/gid 1000 | policy half: muxes (:4097/:4098), boot agent-config stamp (#857), watchdog + vitals, SSE tracking, relay injector, pressure monitor |
| C3 | **Control socket** | C1 serves, C2 consumes | the ONLY C1⇄C2 interface: `hello/status/restart/spawn_env/metrics` (v1, frozen) |
| C4 | **Opencode child** | workspace container, child of C1 | the supervised workload (:4096) |
| C5 | **Controller / pod spec** | `pod_builder.go` + chart gate | constructs the two containers, their mounts, probes, env credentials, and the #857 startup-probe ordering |
| C6 | **Kubelet (external)** | cluster | native-sidecar start gating, liveness/readiness probes, container restarts |

**Explicitly out of scope for US-2** (land with US-3/US-4/US-5, tracked in §6): the `agentdPassword` credential split, cross-uid secret re-materialization on reload, the sidecar's full reload consumer path, live-cluster canary + V-matrix + gVisor leg.

## 2. Integration seams (where components touch)

1. **S1 — Socket contract**: C2's client ⇄ C3's server. Wire shapes, error codes, idempotency, concurrency (A.1–A.5).
2. **S2 — Restart path**: C2 watchdog/reload → C3 `restart` → C1 `restartWithGrace` → real SIGTERM/SIGKILL + respawn of C4.
3. **S3 — Env handoff**: C2 composes child env → C3 `spawn_env` (memory-only) → applied at C4's NEXT spawn. (US-0.2(a); full producer lands in US-4.)
4. **S4 — Metrics path**: C1 reads the workspace container's cgroup → C3 `metrics` → C2's statusz/pressure monitor/ops gauges.
5. **S5 — Vitals path**: C2's watchdog gathers TCP-dial evidence (shared netns) + pid/boot evidence via C3 `status` → #892 verdict matrix (HUNG is the only lethal verdict).
6. **S6 — Pod construction**: C5 builds the C1+C2+C4 pod; kubelet (C6) must admit it, start credential-setup → sidecar (gated on its startup probe, which only serves AFTER the #857 stamp) → main container.
7. **S7 — Filesystem bridge**: cross-uid shared files (boot trio, markers) at 0640 via the pod's shared gid 1000; credential files stay 0600.

## 3. Level definitions and what exists today

| Level | What runs | Where | Status |
|---|---|---|---|
| **L0 — unit** | Each component alone (fake collaborators) | existing suites: `control_socket_test.go`, `socket_vitals_test.go`, `sidecar_mode_test.go`, `agentd_sidecar_pod_test.go`, … | ✅ green (CI) |
| **L0.5 — regression pins** | Deterministic tests for every gap found in #980's cycles | `us2_regression_gaps_test.go` (NEW) | ✅ green (this PR) |
| **L1 — in-pod integration** | C1 supervisor + real child + real socket server + C2's real consumers (`buildSidecarDeps`) in one process tree | `sidecar_integration_test.go` | ✅ green (CI) |
| **L1.5 — supervisor subprocess** | The REAL `supervise-opencode` subcommand as a real process (PID semantics, signals, subreaper, exit code) driven by the real client over real TCP | `supervisor_subprocess_test.go` (NEW) | ✅ green (CI) |
| **L2 — pod-spec admission** | `buildPod` output against a real API server (KEP-753 restartPolicy admission, SecurityContext, Secret-key resolution) | `agentd_sidecar_envtest_test.go` (`-tags envtest`) | ✅ green (envtest workflow) |
| **L3 — kind cluster** | Real kubelet: native-sidecar ordering, probes, container restarts, termination | `scripts/us2-kind-integration.sh` + `.github/workflows/us2-kind-integration.yml` (NEW) | ✅ executable (workflow_dispatch + weekly; not PR-gating) |
| **L4 — TEST-cluster canary** | Full workspace SLOs under the chart gate | US-5 (V-matrix + D6.1 rollback exercise) | ⏳ blocked on US-3/US-4 |

### L0.5 — the regression pins (one per gap; all deterministic)

| Gap (found in #980 cycles) | Test | Pin |
|---|---|---|
| Adapter wrapper ignored injected base factory → CI 5-min hang (only machines WITHOUT `opencode` on PATH fail) | `TestManagedProcAdapter_WrapperUsesInjectedBaseFactory` | wrapper's cmd.Path == injected base path, env == handed env |
| Nil-factory fallback breakage → reloads stop applying env | `TestManagedProcAdapter_NilBaseFactoryFallsBackToDefault` | resolves to production argv |
| Supervisor health probe would 401 forever against bearer-gated readyz (D1 token boundary) | `TestManagedProcess_SkipHealthProbeSuppressesPostRestartProbe` (+positive control) | zero probe hits with flag set; ≥1 without |
| Supervisor flags not set | `TestNewSupervisorProcess_SupervisorModeFlags` | skipHealthProbe ∧ no session hook |
| Cgroup reader existed but was never wired (lint-catch) | `TestNewSupervisorControlServer_WiresLiveMetricsSource` | non-nil source, readable values |
| Reload-path write modes unpinned (3 × 0640 sites) | `TestReloadWritePaths_GroupReadable` | each writer leaves 0640 |
| Live-pod env masks CI-only breakage | `TestAgentdSidecar_EntrypointBranchExecLevel` (in #982; sources repo `entrypoint-common.sh`, blanks `AGENTD_IMAGE_VOLUME`) | CI-parity by construction |
| 0640 constant drift | `TestG20_LLMProvider_Mode0600` (updated in #980) + `cross_uid_modes_test.go` | constants + real writes |

### L1 — in-pod integration (`sidecar_integration_test.go`)

Components integrated: **C1 (real managedProcess + adapter + real socket server)** ⇄ **C2's real consumers from `buildSidecarDeps`** ⇄ **C4 (real child process: fork/exec, real signals, real port binding)**. Only fakes: the opencode *binary* (test re-exec) and the metrics *source* (static snapshot — live cgroup values drift between reads).

| Test | Seam | Validates |
|---|---|---|
| `..._StatusAndMetrics` | S1+S4 | sidecar observes the real child (pid/state) through the socket; cgroup numbers cross verbatim; the pressure seam reads the same values via its own client |
| `..._SidecarWatchdogRestart_ThroughSocket` | S2 | sidecar's restart verb → socket → adapter → real SIGTERM → a NEW serving child; socket reports the new generation |
| `..._SpawnEnvHandoff_CrossesToRealChild` | S3 | env pushed over the socket lands in the NEXT real spawn's `/proc/<pid>/environ`, wholesale (no parent-env leak) |
| `..._Vitals_AgainstRealChildAndSocket` | S5 | live serving child ⇒ never lethal; refused-dial-on-young-child (real unbound port + young `last_restart_at` via real server) ⇒ RESPAWN, never HUNG |

**Pass criteria:** all four green, `-race` clean, on runners WITHOUT `opencode` installed (CI guarantees this; the L0.5 pins make local/CI divergence impossible going forward).

### L2 — pod-spec admission (`agentd_sidecar_envtest_test.go`, `-tags envtest`)

Components integrated: **C5 (real `buildPod`)** ⇄ **real Kubernetes API server** (validation + defaulting + Secret storage).

| Test | Validates |
|---|---|
| `..._PodSpecAdmitted` | the sidecar-mode pod is ADMITTED (native-sidecar `restartPolicy` on an init container per KEP-753, SecurityContext, probes); uid 2000/gid 1000, startup probe = HTTP `:4098/v1/healthz` (#857 gate), ordering credential-setup → agentd-last all survive the round-trip |
| `..._DisabledPodUnchanged` | gate OFF ⇒ admitted pod has no sidecar, one container (admission-level regression pin) |
| `..._SecretBackedEnv` | the credential env's `secretKeyRef`s resolve against a REAL created Secret (password + admin-token keys exist) — the exact kubelet start-time failure class |

**Pass criteria:** all three green in the envtest workflow (CI) — `go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestAgentdSidecar` with `KUBEBUILDER_ASSETS`.

## 4. L3 — kind cluster (executable: `scripts/us2-kind-integration.sh`)

**What this level uniquely proves:** kubelet-side behavior — native-sidecar START ORDERING (S6), probe-driven restarts of ONE container not the pod, cross-uid file reads under a real container runtime, and the termination path.

### Running it

```bash
scripts/us2-kind-integration.sh [--keep]   # --keep: leave cluster + registry up
```

CI runs the same script via `.github/workflows/us2-kind-integration.yml` (workflow_dispatch + weekly Mondays 05:00 UTC; deliberately NOT PR-gating — L0–L2 remain the per-PR gates). Exit codes: `0` all PASS · `1` setup failure · `2` one or more checks FAILED (summary printed). The script is self-contained: throwaway local registry (the agentd sidecar REQUIRES a digest-pinned reference and digest pulls must resolve — the canonical kind `certs.d` registry wiring), controller-lean chart install (api/mcp/webhooks off — the reconciler needs none of those to build pods), pinned node image matching the chart's 1.35 floor.

> **Verification honesty:** the script is executed by CI (and any docker-capable host); it was authored on a sandbox without docker, so first execution may need iteration — record outcomes on #978.

### Checks (map to V-matrix rows; full matrix is US-5)

| ID | Check | PASS |
|---|---|---|
| K1 | Sidecar ordering: `credential-setup` finishedAt < sidecar startedAt < main startedAt (from pod status timestamps) | ordering holds |
| K2 | #857 stamp-before-read: `agent-config.json` carries the `llmsafespaces` MCP entry at first boot | MCP entry present |
| K3 | Filesystem bridge (S7): boot trio 0640 (admin-prompt may be absent without an API); password 0600 | modes as specced |
| K4 | Socket (S1) from inside the pod: `hello` over `127.0.0.1:4099` via `/dev/tcp` | A.1 response shape |
| K5 | Child crash (SIGKILL the opencode pid from the workspace container) → supervisor respawns a fresh pid, crash marker written | new pid + `"reason":"crash"` |
| K6 | `crictl stop` the SIDECAR → sidecar restartCount +1, main untouched, opencode still serving :4096 | isolation holds |
| K7 | `crictl stop` the WORKSPACE container → main restartCount +1, sidecar unchanged, pod returns Ready | isolation holds |
| K8 | Pod delete completes within the 5s grace + API overhead | ≤ 30s wall |

**Record results** as a comment on #978 (template: ID / PASS-FAIL / evidence command output). K6/K7 degrade to SKIP when `crictl` is unavailable on the node.

### L1.5 — supervisor subprocess (`supervisor_subprocess_test.go`, new)

Between L1 and L2: the REAL `supervise-opencode` subcommand as a REAL process — process start, env parsing, listener bootstrap, signal handling (SIGTERM → exit 0, child reaped, no orphans), subreaper — driven over real TCP by the real `controlClient`. Socket address via the `LLMSAFESPACES_CONTROL_SOCKET_ADDR` test seam (production keeps the fixed `127.0.0.1:4099`; the override follows the same pattern as `LLMSAFESPACES_AGENT_CONFIG_PATH`). Four tests: full Appendix-A lifecycle (hello/status/metrics/spawn_env landing in the next real child's environ, wholesale replacement, pid swap), child-crash respawn (socket stays up through the crash window, crash counter increments), clean shutdown + no orphaned child, and A.3 malformed-input error shapes against the real process. The only fake is the `opencode` binary (single-process `exec sleep` stub — a background-sleep variant once held the test's stdout pipe open past exit and failed `go test` with "WaitDelay expired").

## 5. What each level cannot catch (why the next one exists)

- **L0/L0.5** can't catch: two correct halves wired wrong (wrong addr, wrong port, wrong type at the seam).
- **L1/L1.5** can't catch: pod-spec rejection by real admission (no kubelet/API server), kubelet start ordering, container-level restart isolation.
- **L2** can't catch: anything kubelet does at runtime (probe execution, native-sidecar gating, signal delivery).
- **L3** can't catch: SLO/regression at fleet scale, gVisor (runsc) divergence, mixed-generation rollback convergence — that is L4/US-5 (V-matrix + D6.1 exercise on→off→on).

## 6. Gated follow-ups (tracked, not forgotten)

1. **US-3**: extend `..._SecretBackedEnv` to the `agentdPassword` key; add V4 (401 with workspace password on control-plane endpoints).
2. **US-4**: `spawn_env` producer end-to-end (sidecar reload → composed env → socket → next spawn) — extend `..._SpawnEnvHandoff` to drive the REAL reload handler; mount-relocation assertions in K3.
3. **US-5**: L4 canary — full V-matrix (V1–V9) on TEST, D6.1 rollback EXERCISE (on→off→on with mixed-generation pods healthy at each step), gVisor leg per V9.
4. **L3 in CI**: already wired (workflow_dispatch + weekly); promote to per-PR only if the suite proves stable and fast enough — decision point after US-5.

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
| **L1 — in-pod integration** | C1 supervisor + real child + real socket server + C2's real consumers (`buildSidecarDeps`) in one process tree | `sidecar_integration_test.go` (NEW) | ✅ green (this PR + CI) |
| **L2 — pod-spec admission** | `buildPod` output against a real API server (KEP-753 restartPolicy admission, SecurityContext, Secret-key resolution) | `agentd_sidecar_envtest_test.go` (NEW, `-tags envtest`) | ✅ green (envtest workflow) |
| **L3 — kind cluster** | Real kubelet: native-sidecar ordering, probes, container restarts, termination | §5 runbook (below) | ⏳ manual, this doc |
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

## 4. L3 — kind cluster runbook (manual, ~30 min)

**What this level uniquely proves:** kubelet-side behavior — native-sidecar START ORDERING (S6), probe-driven restarts of ONE container not the pod, cross-uid file reads under a real container runtime, and the termination path.

### Setup

```bash
kind create cluster --name us2-int --image kindest/node:v1.35.x   # chart floor 1.35
helm install lss helm/ --set controller.agentdDelivery.image="<digest-pinned agentd image>" \
                     --set controller.agentdSidecar.enabled=true
kubectl apply -f <workspace.yaml>   # one test workspace
```

### Checks (map to V-matrix rows; full matrix is US-5)

| ID | Check | Command | PASS |
|---|---|---|---|
| K1 | Sidecar ordering: `credential-setup` completes → `agentd` starts → main container starts only after sidecar `StartupProbe` passes | `kubectl get pod <pod> -o jsonpath='{.spec.initContainers[*].name}'`; `kubectl describe pod` events | main container's start event timestamp ≥ sidecar ready timestamp |
| K2 | #857 stamp-before-read: `agent-config.json` contains the `llmsafespaces` MCP entry BEFORE opencode's first read | exec into sidecar: verify stamp; check opencode logs for MCP server registration | MCP entry present at first boot; no "missing MCP" warnings |
| K3 | Filesystem bridge (S7): opencode (uid 1000) reads the sidecar-stamped `agent-config.json`; sidecar (uid 2000) reads init-written `admin-prompt.md`/`allowed-dirs.json` | `kubectl exec <pod> -c workspace -- cat /sandbox-runtime/agent-config.json`; `stat -c %a` each | readable; boot trio 0640; credential files 0600 |
| K4 | Socket (S1) from inside the pod: sidecar→supervisor round-trip | port-forward + `hello`/`status` | responses per A.1 shapes |
| K5 | Watchdog restart (S2) end-to-end in-pod | kill opencode's listener (e.g. `kill -STOP`/SIGKILL the opencode pid from the workspace container), watch sidecar logs | supervisor respawns; marker `last-restart-reason.json` updated; busy sessions defer |
| K6 | Probe isolation: wedged SIDECAR restarts only the sidecar | SIGSTOP the sidecar process; wait past liveness budget | sidecar restartCount +1, main container untouched, opencode keeps serving :4096 |
| K7 | Wedged CHILD triggers workspace-container restart (TCP liveness), sidecar survives | kill opencode from inside | workspace container restarts, sidecar does NOT restart (restartPolicy Always ≠ pod restart) |
| K8 | Termination: pod delete drains both containers within ~5s grace | `time kubectl delete pod` | clean exit, no orphaned child in events |

**Record results** as a comment on #978 (template: ID / PASS-FAIL / evidence command output).

## 5. What each level cannot catch (why the next one exists)

- **L0/L0.5** can't catch: two correct halves wired wrong (wrong addr, wrong port, wrong type at the seam).
- **L1** can't catch: pod-spec rejection by real admission (it never runs a kubelet/API server), kubelet start ordering, container-level restart isolation.
- **L2** can't catch: anything kubelet does at runtime (probe execution, native-sidecar gating, signal delivery).
- **L3** can't catch: SLO/regression at fleet scale, gVisor (runsc) divergence, mixed-generation rollback convergence — that is L4/US-5 (V-matrix + D6.1 exercise on→off→on).

## 6. Gated follow-ups (tracked, not forgotten)

1. **US-3**: extend `..._SecretBackedEnv` to the `agentdPassword` key; add V4 (401 with workspace password on control-plane endpoints).
2. **US-4**: `spawn_env` producer end-to-end (sidecar reload → composed env → socket → next spawn) — extend `..._SpawnEnvHandoff` to drive the REAL reload handler; mount-relocation assertions in K3.
3. **US-5**: L4 canary — full V-matrix (V1–V9) on TEST, D6.1 rollback EXERCISE (on→off→on with mixed-generation pods healthy at each step), gVisor leg per V9.
4. **L3 automation**: if K1–K8 prove high-value manually, promote to `kind`-in-CI (weekly job) — decision point after US-5.

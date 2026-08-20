# Worklog: design 0051 US-2 — regression pins + integration test plan (L0.5–L2)

**Date:** 2026-08-20
**Session:** Close the test-coverage gaps from the #980/#981/#982 cycles with deterministic regression pins; define and implement the US-2 integration test plan (L1 in-pod, L2 envtest) with an L3 kind runbook and L4 handoff to US-5.
**Status:** Complete (pending review)

---

## Objective

1. Every gap found and fixed during US-2 must have a regression test that fails deterministically if reintroduced — no dependence on CI-only environmental accidents.
2. A written integration test plan answering: which components US-2 integrates, at which seams, and how each level validates them together — with the automatable levels (L1, L2) implemented and green, and the cluster levels (L3 kind runbook, L4 canary) specified for execution.

---

## Work Completed

### Gap audit (all gaps from the #980/#981/#982 cycles)

| Gap | Prior coverage | Verdict |
|---|---|---|
| Adapter wrapper ignored injected base factory (CI 5-min hang; only machines WITHOUT `opencode` on PATH fail) | none deterministic | **FIXED** — structural pin |
| Nil-factory fallback breakage | none | **FIXED** — behavioral pin |
| Supervisor health probe would 401 forever vs bearer-gated readyz (D1) | flag existed, no test | **FIXED** — suppression pin + positive control |
| Supervisor-mode flags wiring | none | **FIXED** — construction seam pin |
| Cgroup reader unwired (lint catch) | server-with-source tested, supervisor wiring not | **FIXED** — construction seam pin |
| Reload-path 0640 modes (3 sites in secrets.go) | only configwriter/materializer/marker pinned | **FIXED** — per-writer mode pins |
| Live-pod env masks CI-only breakage (entrypoint-common.sh path) | #982's patched-copy test | already pinned |
| 0640 constants drift | #980's G20 update + cross_uid tests | already pinned |

### New tests

- `cmd/workspace-agentd/us2_regression_gaps_test.go` — 7 tests covering the four unpinned gaps (see table). Two construction seams extracted from `runSuperviseOpencodeCommand` to make them pinnable: `newSupervisorProcess()` (skipHealthProbe, no session hook) and `newSupervisorControlServer(addr, adapter)` (metrics source wiring). No behavior change; the command now composes them.
- `cmd/workspace-agentd/sidecar_integration_test.go` — L1 in-pod integration: supervisor (real managedProcess + adapter + real socket server) ⇄ sidecar's real `buildSidecarDeps` consumers ⇄ real child (fork/exec, real signals, real port). 4 tests over seams S1–S5: status/metrics observation, watchdog restart through the socket (new serving child, new generation reported), spawn_env handoff landing in the next real spawn's `/proc/<pid>/environ` (wholesale, no parent-env leak), and vitals never-lethal on live child / respawn-window on young refused child.
- `controller/internal/workspace/agentd_sidecar_envtest_test.go` (`-tags envtest`) — L2 pod-spec admission against a real API server: sidecar-mode pod ADMITTED (KEP-753 restartPolicy on init container, uid 2000/gid 1000, HTTP startup probe = #857 gate, credential-setup → agentd-last ordering survive); gate-off pod unchanged; sidecar credential env secretKeyRefs resolve against a real created Secret (password + admin-token keys present).
- `docs/testing/0051-us2-integration-test-plan.md` — the plan (components C1–C6, seams S1–S7, levels L0–L4, L3 kind runbook K1–K8 mapped to V-matrix rows, per-level blind spots, US-3/4/5 follow-ups).

### Local verification

- envtest binaries bootstrapped via `setup-envtest@release-0.20` → `/tmp/opencode/envtest/k8s/1.35.0-linux-amd64` (works on this pod; CI uses its own workflow).
- L2 green locally (3/3) after fixing the fixture: `newWorkspaceForSecurity` carries no UID → `podName()` emits a trailing dash → RFC-1123 rejection. Fixed by stamping `ws.UID` in the envtest tests (fixture stays shared).

---

## Key Decisions

1. **Structural pin over CI-timing for the adapter-factory gap**: the wrapper's built `*exec.Cmd` is inspected (Path == injected base, Env == handed env) so the regression fails EVERYWHERE, not only on runners without `opencode`. The alternative (PATH-stripping test harness) re-creates the environmental accident instead of eliminating it.
2. **Positive control in the skipHealthProbe test**: the probe-fires-without-flag leg runs first, so a silently broken probe path cannot fake a pass on the suppression leg.
3. **L1 fakes are bounded to the binary and the metrics source**: the child is a REAL subprocess (real signals/reaping/port binding) and all inter-process wiring is production code (`buildSidecarDeps`, `controlClient`, socket server). The metrics source uses a static snapshot because live cgroup values drift between reads — verbatim transport is the contract, not the values.
4. **Envtest over fake-client for L2**: fake clients skip admission validation entirely — the KEP-753/SecurityContext/Secret-key checks are the point of the level. Reuses the existing `startEnvtest` harness + `envtest.yml` workflow (no CI changes needed).
5. **L3 (kind) documented as a runbook, not automated**: the eight checks need a real kubelet but not fleet scale; automating kind-in-CI is a decision point deferred until after US-5 per the plan's §6.

---

## Blockers

None.

---

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestManagedProcAdapter|TestManagedProcess_SkipHealthProbe|TestNewSupervisor|TestReloadWritePaths|TestWorkspaceCgroupReader|TestIntegration_'` — green.
- `go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestAgentdSidecar` (KUBEBUILDER_ASSETS=…1.35.0-linux-amd64) — 3/3 green.
- Full suites: `./cmd/workspace-agentd/ ./pkg/agentd/... ./pkg/agent/... ./controller/internal/workspace/ ./helm/...` — green (final-suite.log).
- `go vet` (+`-tags envtest`), gofmt, golangci-lint — clean.

---

## Next Steps

1. Merge this PR (review → APPROVE → squash).
2. Execute the L3 kind runbook (§4 of the plan) when a kind-capable environment is available; record results on #978.
3. US-3: extend `..._SecretBackedEnv` to the `agentdPassword` key + V4 check per the plan's §6.
4. US-4: extend `..._SpawnEnvHandoff` to drive the real reload handler end-to-end.
5. US-5: L4 canary — full V-matrix, D6.1 rollback exercise, gVisor leg.

---

## Files Modified

- `cmd/workspace-agentd/us2_regression_gaps_test.go` (new)
- `cmd/workspace-agentd/sidecar_integration_test.go` (new)
- `cmd/workspace-agentd/supervise_opencode.go` (seam extraction only — `newSupervisorProcess`, `newSupervisorControlServer`; no behavior change)
- `controller/internal/workspace/agentd_sidecar_envtest_test.go` (new, `-tags envtest`)
- `docs/testing/0051-us2-integration-test-plan.md` (new)
- `worklogs/0814_2026-08-20_0051-us2-regression-integration.md` (this file)

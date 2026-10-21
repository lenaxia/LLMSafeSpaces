# Worklog: #1801 — restart-marker coverage: the relay-injector restart class never wrote .opencode-restart-reason

**Date:** 2026-10-20
**Session:** Implementation lane for issue #1801 (worktree fix/1801-restart-marker-coverage).
**Status:** Complete

---

## Objective

Every supervisor restart class must write/update the PVC-backed restart-reason marker (`/workspace/.opencode-restart-reason`, env-overridable via `LLMSAFESPACES_RESTART_MARKER_PATH`) — the observability anchor beneath the #1760/#1776 surfacing chain. On 2026-10-20 06:19:07Z the workspace agent restarted live (fresh `opencode serve` PID, pod untouched) while the marker still read the 00:37:32Z oom entry: exactly one restart class never wrote. Fix the write path only; NO behavior change to restart decisions.

## Enumeration (milestone (a), orchestrator-ratified)

Restart decision sites in `cmd/workspace-agentd` and each one's marker status pre-fix:

| Class | Decision site | Marker |
|---|---|---|
| A.1 crash (unexpected non-OOM exit) | managed_process.go:292 | ✓ "crash" |
| A.2 oom (SIGKILL-classified exit) | oom_detection.go:94 | ✓ "oom" |
| 3a watchdog hung fire | healthz_cache.go:580 | ✓ "health_watchdog" |
| 3b watchdog unknown-episode fire | healthz_cache.go:546 | ✓ "health_watchdog_unknown_episode" |
| 3c secrets apply (single-container) | secrets.go:1260 | ✓ "api_key_changed"/"env_secrets_changed" at decision time |
| 3c' secrets apply (sidecar socket leg) | secrets.go:1260 (sidecar writes at apply; socket carries credential_reload) | ✓ |
| 3d XDG agent-config watcher (both topologies) | xdg_config_layer.go:438 | ✓ "credential_reload" |
| 3e **relay injector / re-arm kill** | relay_injector.go:633 `cfg.KillOpenCode()` → main.go `relayKillFunc` → `makeSessionAwareRestartDecision` → `proc.restart()` | **✗ — never wrote; THE 06:19 CLASS** |

Structural root cause: the managedProcess supervise loop's `restartRequested` branch (managed_process.go:276-283) deliberately skips crash classification (and its marker write) — operator-class restarts rest on each caller writing at its own decision site. The watchdog fires, secrets applies, and the config watcher all do; the relay kill (the third deferred-restart caller named in the `deferredRestarts` comment, secrets.go:110) was the one that did not. The control-socket closed enum (control_socket.go) already reserved the string `relay_injector` — the class was first-class everywhere except the marker write. Suppressed watchdog verdicts (starved/flat/respawn/unknown below the bound) restart nothing and correctly write nothing.

## Work Completed

- New constant `RestartReasonRelayInjector = "relay_injector"` (restart_reason.go) — single-sources the marker reason, the metric label, and the control-socket enum entry (control_socket.go map literal replaced by the constant; wire value unchanged).
- The fix, r2 shape: `relayKillWithMarker` (main.go) wraps ONLY the injector wiring's `KillOpenCode` — marker write → `logRestartReasonAtWrite` → `RecordRestart`, with `RecordMarkerWriteFailure` on write error (the exact sibling call-site pattern). The SHARED `relayKillFunc` closure (watcher restartNow + injector kill) carries no class marker; each class writes at its own decision site. Covers the immediate, deferred, and interrupt-force legs with one decision-time write. Marker is observability, never a gate: a failed write counts and the restart proceeds (mirrors the H2 doctrine).
- `workspace_restarts_total` help text now enumerates `relay_injector` (and the already-live `health_watchdog_unknown_episode` label the help had missed); sibling doc comments aligned (review findings 2-3).
- Per-class pin coverage (#1801 acceptance): new `restart_marker_coverage_test.go` hosting the enumeration table + relay family (immediate leg, decision-time-while-deferred leg, marker-failure-does-not-block-restart, metric, constant/enum coherence, shared-closure-writes-nothing, injector-wiring source pin) + the production-composite pin (watcher × real shared closure — marker stays credential_reload, relay metric unchanged) + gap pins for classes that had no fire-path pin (A.1 crash loop via real subprocess, 3a watchdog fire path, 3c single-container apply marker content). Class 3d pinned by extending `TestWatchAgentConfigForChanges_RestartsOnChange` with a marker assertion; classes A.2/3b/3c' already pinned (oom_detection_test.go, watchdog_unknown_episode_test.go, spawn_env_consumer_test.go) — cross-referenced in the table, not duplicated.
- Red modes DEMONSTRATED by toggling each write off and re-running its pin: relay family (FAIL), crash pin (FAIL), watchdog-fire pin (FAIL), xdg pin (FAIL), and — round 2 — the production-composite pin against the r1 regression shape (FAIL). Initial red: the relay family failed compilation against pre-fix main (constant undefined) — same red class as #1780's family.

## Assumptions (Rule 7) + validation

1. **M3 decision-time semantics already cover deferred-capable classes (orchestrator worklog requirement).** VALIDATED, two anchors: (a) the M3 text (restart_reason.go:87-103) documents the requested-vs-completed limitation specifically for "secrets.go makeSessionAwareRestartDecision" — the SAME shared decision function the relay kill routes through; the deferral/cancellation edge it accepts (pod dies before the deferred restart fires → stale-at-boot marker, mitigated by the 10-minute stale threshold) is the function's semantics, not a caller's. (b) The XDG config watcher (xdg_config_layer.go:438) already writes at decision time before the same possibly-deferred session-aware machinery — a deferred-capable class with a decision-time write was ALREADY shipped semantics. The relay class introduces no new edge; it joins two existing precedents.
2. The 06:19 event was single-container topology. VALIDATED indirectly: the marker read at `/workspace/.opencode-restart-reason` is the single-container default path (sidecar mode writes `agentd.SidecarRestartMarkerPath`); and the relay kill leg exists only in single-container mode (sidecar `deps.proc == nil` → `maybeStartRelayInjector` early-return).
3. Exactly one live class skipped. VALIDATED by the enumeration: every other decision site writes before or at its restart (table above); socket reasons `session_aware`/`manual` have no production sender (grep-verified), so no hidden writer-less sender exists on the sidecar leg.
4. Adding a reason label to a Prometheus counter is not a behavior change. VALIDATED: CounterVec labels materialize on first increment; the decision paths, suppression gates, and deferral machinery are untouched (diff is write-path + metric + enum single-sourcing).

## Adversarial review (Rule 11)

- **Finding (real, fixed)**: initial control_socket.go edit left BOTH the literal and the constant in the enum map — caught on re-read, collapsed to the constant only.
- **Finding (real, fixed)**: my first red-toggle of healthz_cache.go did not match the source (indentation), so that "green" run proved nothing — redone with exact text; red verified.
- **Considered, rejected**: writing the marker inside `makeSessionAwareRestartDecision` itself — would double-write for the credential-apply and config-watcher classes; the decision site is the correct single owner.
- **Considered, rejected**: writing in relay_injector.go before `cfg.KillOpenCode()` — equivalent moment for the injector leg, but `relayKillFunc` is the single construction site of the restart decision and is where every future KillOpenCode caller lands; also the watcher wiring (xdg_config_layer.go:380-390) documents relayKillFunc as the session-aware seam.
- **Not a gap**: sidecar-mode relay injector does not run (deps.proc nil); the reserved `relay_injector` socket reason has no sender — unchanged (fixing that would be new behavior, out of scope).

## Review round 1 (#1803, CHANGES_REQUESTED → remediated)

- **Finding 1 (blocking, reviewer-demonstrated; VALIDATED my side by reading main.go:346-347)**: the r1 write lived inside `relayKillFunc`'s closure — but that closure is SHARED: main.go:346-347 wires it as the single-container XDG watcher's `restartNow`, so a watcher restart executed xdg_config_layer.go:438 (`credential_reload` write) → restartNow → the r1 write overwrote the marker with `relay_injector` and counted the relay metric — a new misattribution of exactly the class #1801 exists to keep truthful. My r1 blind spot: the wiring was literally pinned in xdg_config_layer_test.go's needle table and I did not connect it; my r1 "considered, rejected" rationale (avoid double-writes) was invalidated by choosing a site that double-writes the watcher class.
- **Remediation**: the write moved OUT of the shared closure into `relayKillWithMarker` (main.go), which wraps ONLY the injector wiring (`maybeStartRelayInjector`'s KillOpenCode). The watcher keeps the BARE `relayKillFunc` (pre-PR behavior restored). `relayKillFunc`'s doc comment now states it is shared and carries no class marker; `relayKillWithMarker`'s doc records the decision-site ownership + M3 semantics. No restart decision, deferral, or suppression behavior changed (r1→r2 diff is write relocation + comments + tests).
- **Finding 2 (minor, fixed)**: `RecordRestart` doc comment (ops_metrics.go) now enumerates `health_watchdog_unknown_episode` + `relay_injector`.
- **Finding 3 (minor, fixed)**: `metricRestartReason` doc (secrets.go) updated to quote the current help-text enumeration.
- **New pins per the review's missing-test list**: `TestWatchAgentConfigForChanges_ProductionComposite_MarkerStaysCredentialReload` (the REAL single-container composite: watcher × bare `relayKillFunc` × idle tracker — final marker stays `credential_reload`, relay metric unchanged; RED MODE DEMONSTRATED by restoring the r1 write inside the closure → FAIL), `TestRelayKillWithMarker_SharedClosureWritesNoMarker` (the bare closure writes nothing), `TestRelayKillWithMarker_InjectorWiringCarriesTheWrapper` (source pin: injector wraps, watcher stays bare). Relay family retargeted to the wrapper composition (`relayKillWithMarker(relayKillFunc(...))`); family red mode re-verified.

## Tests Run (targeted, memory-gated; peak observed ~7.0e9)

- New family + gap pins: `go test ./cmd/workspace-agentd/ -run 'TestRelayKillFunc_WritesMarker|TestRelayKillFunc_MarkerFailure|TestRelayKillFunc_RecordsRestartMetric|TestRestartReasonRelayInjector_Constant|TestRestartMarker_CrashRecovery|TestWatchdogFire_WritesRestartReasonMarker|TestRestartMarker_SecretsApplySingleContainer'` → ok, 2.883s (RED pre-fix).
- `TestRelayKillFunc|SessionAwareRestart|TestMetricRestartReason` → ok 5.393s; `TestControlSocket|TestControlClient|TestControl` → ok 5.147s; `RelayInjector|RearmLoop|RelayLiveness|RelayRenewal|RelayState|PreBootRelay` → ok 19.742s; marker/oom unit family → ok 0.866s; `TestRefreshIsHealthyLoop|TestHealthWatchdog|TestWatchdog|SupervisorRestarts|AgentRestartLog` → ok 44.377s; `TestReloadSecretsHandler|ApplyBatch|SpawnEnvConsumer|SocketReload` → ok 5.206s; `TestWatchAgentConfig|XDG|ConfigLayer|TestOpencodeBootLayersWiring` → ok 16.062s; `TestManagedProcess|TestSupervisor|SuperviseOpencode|...` → ok 77.449s.
- `go vet ./cmd/workspace-agentd/` clean; `gofmt -l` empty; golangci-lint + repolint run pre-PR (see PR description).

## Blockers

None.

## Files Modified

- cmd/workspace-agentd/restart_reason.go (constant)
- cmd/workspace-agentd/control_socket.go (enum single-sourced to the constant)
- cmd/workspace-agentd/main.go (relayKillFunc marker write + metric)
- cmd/workspace-agentd/ops_metrics.go (help text)
- cmd/workspace-agentd/restart_marker_coverage_test.go (new)
- cmd/workspace-agentd/xdg_config_layer_test.go (watcher marker assertion)
- worklogs/NNNN_2026-10-20_1801-restart-marker-coverage.md (sentinel; bot renumbers at merge) — this worklog

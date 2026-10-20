# Worklog — #1776: Sidecar topology — attribute supervisor crash-restarts into the agent-restart surfacing chain

**Date:** 2026-10-20
**Issue:** [#1776](https://github.com/lenaxia/LLMSafeSpaces/issues/1776)
**Branch:** `feat/1776-sidecar-crash-attribution` (worktree `/workspace/wt-1776`)
**Orchestrator:** ses_eaa9bfbdcffey7D8UoTVUvKFBG (milestone (a) ratified with two amendments, both executed)

## What

#1760 (PR #1765, worklog 1149) surfaced supervisor/watchdog agent restarts
through the full chain, but scope ruling A11 excluded SIDECAR crash restarts:
the workspace container's supervisor (`supervise-opencode`) crash-respawns
opencode and writes the restart-reason marker, but the event never reached
the sidecar's `agentRestartLog` — so `agentRestarts` could read 0 on a sidecar
workspace that crashed-respawned repeatedly.

This closes the asymmetry at the source:

- **Supervisor side**: `newSupervisorProcess` wires the managedProcess crash
  observer (`restartObserver`) into the adapter (`observeCrashRestart`) — the
  SAME decision point and strings (crash/oom) as the restart-reason marker.
  The adapter keeps a LIFETIME-since-supervisor-boot counter (NOT the
  resetting backoff counter) plus the latest crash's classification.
- **Transport**: the control-socket `status` method carries the attribution
  (`crash_restarts`, `last_crash_reason`, `last_crash_at`) via a
  capability-typed `crashReporter` interface — the `vitalsProvider`
  precedent. A pre-#1776 supervisor omits the fields entirely (additive-JSON
  A.1 forward compat); the sidecar's decode reads absence as no evidence.
- **Sidecar side**: the supervisor-status poller (the 15s loop already
  mirroring spawn-env/legacy-scrub state) derives crash-counter deltas into
  `deps.restartLog.record(reason, "", "", at)` — reason crash/oom, EMPTY
  verdict (the single-container `restartObserver` precedent), supervisor-side
  attribution time.
- **Schema**: the where-recorded hedge is dropped from the
  `SupervisorStatus`/`AgentSupervisorStatus` descriptions (coverage is now
  topology-complete): "supervisor-watchdog restarts plus supervisor-attributed
  process-crash (crash/oom) restarts, in both container topologies".

Downstream chain (controller mirror → CRD condition → DTO → SDK → SSE →
frontend) is topology-agnostic and needed NO functional change; the
sidecar-crash arm was added to the acceptance chain test (below).

## Design rulings (milestone (a), ratified)

- **Verdict ""** for crash/oom restarts — no watchdog verdict claimed; the
  5-value taxonomy single-source (`verdict.label()`) is untouched.
- **No double counting**: watchdog fires cross the socket as reason-enum
  (operator-class) restarts → `adapter.Restart` → `restartRequested` → the
  crash observer NEVER fires (it fires only when neither stop nor restart was
  requested); watchdog fires are already recorded at fire time in the sidecar
  (`healthz_cache.go`). **Orchestrator Amendment 1** required this proven by
  execution: `TestWatchdogFireThroughSocket_RecordsExactlyOne_NoCrashAttribution`
  runs a corroborated fire through the REAL socket to a REAL managedProcess +
  adapter and asserts the log carries EXACTLY one entry and the adapter's
  crash count is ZERO.
- **Counter decrease = rebase silently**: the supervisor's counter is
  in-process; a decrease means the workspace container restarted. A container
  restart is kubelet-visible, not supervisor crash-respawn attribution.
- **First sighting records the full count**: a sidecar that boots (or
  re-boots) after early crashes re-derives the supervisor's truthful lifetime
  count. **Sidecar-restart property (explicit, per orchestrator)**: the log
  is sidecar memory; a sidecar restart rebuilds it from the supervisor's
  lifetime count on the first poll. This is truthful-by-construction for
  "since the pod's supervisor boot" semantics and is NOT a double-record
  path — the fresh log starts at zero and records the supervisor's current
  count exactly once. Distinct from the SSE watcher's first-sighting skip,
  which avoids re-toast storms; skipping here would re-create the reads-0
  bug this lane fixes.
- **Window collision**: ≥2 crashes inside one 15s poll window record the
  exact COUNT; `lastAgentRestart` carries the latest crash's attribution
  (intermediate per-crash reasons are not individually surfaced — the
  surfaced shape stays truthful; the count is the primary datum).
- **US-22.1 preserved**: recording happens in the poller goroutine; the
  healthz/statusz handlers keep reading the log's cached snapshot only.

## Assumptions (Rule 7 — state + validate)

1. The supervisor's crash classification happens in the shared
   managedProcess crash path and fires `restartObserver` with "crash"/"oom"
   — validated: `managed_process.go` crash path (the single-container
   `TestManagedProcess_RestartObserver_*` tests already pin the strings);
   the supervisor-mode e2e (`TestSupervisorProcess_CrashRespawnAttributedInAdapter`)
   pins it through the adapter with a real crashing child.
2. Reason-enum socket restarts never reach the crash observer — validated
   BY EXECUTION per Amendment 1 (see above), not by code-reading.
3. The status method is the correct attribution channel (real-time, polled
   every 15s); the restart-reason marker keeps its existing role (operator
   logs, one-shot boot consumption) — validated against A11's naming of
   "restart-reason marker + socket status" and the marker's one-shot
   `logRestartReason` consumption semantics (`restart_reason.go`).
4. A pre-#1777 supervisor/sidecar pairing is safe both directions —
   validated by test: legacy supervisor (no capability) emits no crash keys
   (`TestControlSocketStatus_CrashAttributionAbsenceOnLegacySupervisor`);
   older sidecar ignores unknown additive JSON fields (A.1, pinned by the
   control-socket protocol tests).
5. The controller mirror legs are reason-agnostic — validated: the
   sidecar-crash chain arm passes through `mirrorAgentSupervisor` unchanged
   (reason "crash", absent verdict → CRD + condition + SDK-schema keys).
6. helm/crds/workspace.yaml descriptions are hand-synced from the Go types'
   comments (no controller-gen target in the Makefile) — validated: the
   #1760 hedge text was present in both, synced in both; repolint
   `crd_drift` is a name-set check (comments cannot trip it) — run green.

## Scope-boundary disclosures

- **Amendment 2 (copy ruling) — investigated, DEFERRED**: the user-facing
  copy "restarted by the supervisor health watchdog (reason: crash, …)"
  overclaims the mechanism for crash reasons. It lives in SEVEN independent
  literals across SIX files, not one or two template sites: controller
  condition + event templates (`health.go:337,367`), the frontend banner
  literal (`AgentRestartBanner.tsx:57`), two frontend test pins
  (`AgentRestartBanner.test.tsx:21`, `ChatPage.agentRestart.test.tsx:155,183`),
  the MCP tool description (`pkg/mcp/server.go:89`), and an API test fixture.
  Per the conditional ruling this is the defer shape; the reason field
  disambiguates. Escalated to the orchestrator in the final packet as
  follow-up-lane material.
- **sdks/openapi.yaml**: the declared hunk ONLY — the `SupervisorStatus`
  description (no shape change, no /me/board sections).
- No functional change outside `cmd/workspace-agentd` (the six sibling hedge
  sites are comments/descriptions only); no SDK regeneration needed.

## Tests (TDD red-first; all commands in the worktree, `-timeout` always)

Red evidence: the new agentd test file failed compilation against
pre-implementation main (`crashRestartReport`, `observeCrashRestart`,
`CrashRestartReport`, `st.CrashRestarts`, the 7-arg poller signature — all
undefined). The controller chain arm is an ACCEPTANCE PIN by design (the
mirror legs are reason-agnostic and pass on main; its red mode is any revert
of the mirror legs) — stated, not claimed as red-first.

New tests (`cmd/workspace-agentd/sidecar_crash_attribution_test.go`):
- `TestManagedProcAdapter_ObserveCrashRestart_LifetimeCount` — lifetime
  accumulation across crash+oom, latest classification wins.
- `TestControlSocketStatus_CrashAttribution` + `...AbsenceOnLegacySupervisor`
  — real socket: fields present with attribution, absent when zero-crash
  (no evidence is not a crash) and on a legacy (capability-less) proc.
- `TestDecodeControlStatus_CrashFields` — the client decode table's wire
  tags pinned; absent fields decode to no evidence.
- `TestCrashRestartTracker_DeltaRebaseFirstSighting` — the delta policy
  unit: first-sighting full count, delta on increase, silent rebase on
  decrease, post-rebase delta.
- `TestSupervisorStatusPoller_RecordsSidecarCrashRestarts` — attribution
  e2e leg: real socket + real poller + sidecar log; first sighting, window
  collision (delta 2, latest attribution), rebase-no-record, post-rebase
  delta; crash semantics (reason, EMPTY verdict, supervisor clock).
- `TestSupervisorProcess_CrashRespawnAttributedInAdapter` — supervisor e2e
  leg: a real crashing child (SIGABRT) reaches the adapter's attribution
  and the supervisor respawns.
- `TestWatchdogFireThroughSocket_RecordsExactlyOne_NoCrashAttribution` —
  **Amendment 1's mandatory pin** (see Design rulings).
- `TestSidecarCrashAttributionWiringPinned` — production wiring source pins
  (the `sidecar_generation_pin_test` discipline): the observer wire in
  `newSupervisorProcess` and the `deps.restartLog` pass-through in
  `runSidecarCommand`.
- `TestSupervisorBlock_CrashShapedWireOmitsVerdict` — crash-shaped handler
  wire omits the empty verdict key.

New controller arm (`supervisor_restart_chain_test.go`):
`TestSupervisorRestartChain_SidecarCrashArm` — agentd wire JSON (reason
crash, NO verdict/lastError keys) → `checkAgentHealth` → CRD
`status.agentSupervisor` + `AgentRestarted` condition (True,
ReasonWatchdogRestarted, message carries "reason: crash") → CRD wire keys
pinned to the SDK schema property names, empty verdict omitempty on the wire.

Commands run (all green):
- `go test ./cmd/workspace-agentd/ -run '<new-family>' -timeout 120s` — 10/10.
- `go test ./cmd/workspace-agentd/ -run '<#1760 family + poller/socket/generation/watchdog>' -timeout 180s` — 101.8s.
- `go test ./cmd/workspace-agentd/ -run 'TestSidecar|TestSocket|TestRestart|TestContainerRestart|TestSpawn|TestControl|TestCrash|TestWatchdog' -timeout 300s` — 43.6s.
- `go test ./controller/internal/workspace/ -run 'TestSupervisorRestartChain|TestMirrorAgentSupervisor|TestCheckAgentHealth|TestAgentRestarted|TestWorkspaceStatus_AgentSupervisor' -timeout 120s`.
- `go test ./pkg/agentd/ -run TestSupervisorStatus -timeout 60s`.
- `go build` across all touched packages incl. `sdks/go`; `go vet` clean;
  `gofmt` clean.
- `make -C sdks sdk-check` — spec valid + router parity.
- `make repolint` — all checks (incl. CRD drift, spec coupling markers).
- TS SDK: comment-only change; no node_modules in this worktree — tsc
  deferred to CI (a block comment cannot alter types).

## Adversarial self-review (Rule 11)

- **Finding (documented, accepted)**: a crash landing in the gap between a
  supervisor-container restart and the sidecar's next poll folds into the
  silent rebase (under-counted by at most that window). Distinguishing it
  needs a cross-container epoch marker — the identical KNOWN MISS already
  documented on `generationSignal` (`generation_signal.go:29-37`); same
  remedy path (persisted epoch), same rarity (sidecar+supervisor die
  together as a pod).
- **Finding (documented, accepted)**: after a supervisor-container restart
  (sidecar surviving), the log's cumulative count spans supervisor
  instances (cannot decrement — A3 ruling). Rare by the same pod-death
  argument; the live `status.agentSupervisor.agentRestarts` re-converges to
  the new supervisor's count on the next fresh sidecar boot.
- **Finding (documented, accepted)**: window-collision intermediate reasons
  (see Design rulings) — count exact, latest attribution exact.
- **False alarm (checked)**: `observeCrashRestart` takes `pullMu` on the
  supervisor goroutine — no nesting with `preSpawn`/socket paths, no I/O
  under the lock, all holders bounded.
- **False alarm (checked)**: `fakeRestartProc` gaining the capability
  changes existing status payloads only by ADDING `crash_restarts: 0` —
  no existing test asserts key-absence on status; the full socket family
  re-ran green.

## Cross-lane

- `sdks/openapi.yaml`: declared hunk only (SupervisorStatus description);
  board sections untouched.
- No frontend functional change (banner consumes the generic SSE payload);
  no `api/` functional change (DTO comment only).

## Resource discipline

Memory read before every heavy command (peak observed this lane: 6.26G —
all targeted `-run` families, no `-race`, no full `make test`, no frontend
install).

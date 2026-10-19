# Worklog — #1760: Surface supervisor/watchdog agent restarts through the full chain

**Date:** 2026-10-19
**Issue:** [#1760](https://github.com/lenaxia/LLMSafeSpaces/issues/1760)
**Branch:** `feat/1760-surface-agent-restarts` (worktree `/workspace/wt-1760`)
**Orchestrator:** ses_eaa9bfbdcffey7D8UoTVUvKFBG (milestone (a) ratified with amendments; cross-lane broker map settled mid-flight)

## What

Health-watchdog restarts of the in-workspace agent were invisible to every
consumer (the 2026-10-19 incident: watchdog killed a CPU-starved agent at
consecutiveFailures=78; all evidence died in container logs). This change
surfaces them through the full chain:

agentd `/v1/healthz` + `/v1/statusz` supervisor slice → controller mirror
(`status.agentSupervisor` + `AgentRestarted` condition + exactly-once K8s
event) → REST DTO (`WorkspaceStatusResult.supervisor`) →
`sdks/openapi.yaml` + TypeScript SDK → MCP `workspace_status` tool → API
watcher `agent_restarted` SSE event → frontend dismissible banner
(chat load) + live toast.

## Design (per ratified milestone (a))

- **Source**: `cmd/workspace-agentd/supervisor_restarts.go` — `agentRestartLog`,
  recording hooks ONLY (suppression/kill logic untouched; all pre-existing
  watchdog decision tests pass byte-identical semantics).
  - fire path → `record("health_watchdog", "hung"|"", lastError, now)`
    (verdict empty when uncorroborated — vitals nil, pre-#892 semantics)
  - #1632 unknown-episode escalation →
    `record("health_watchdog_unknown_episode", "unknown", ...)`
  - suppression path → `recordSuppression(verdict)` — the incident's
    starved suppressions are counted, not just fired restarts
  - single-container crash path → `managedProcess.restartObserver`
    (reason `oom` for SIGKILL-class exits, `crash` otherwise — same
    decision point and strings as the restart-reason marker)
- **Taxonomy**: the REAL five-value enum from `watchdog_vitals.go`
  (`hung|starved|flat|respawn|unknown`), single-sourced in `verdict.label()`
  — NOT the issue text's two-value example.
- **Counts are lifetime-since-agentd-boot** (orchestrator A3 ruling).
  `managedProcess.restartCount` was considered and rejected as the source:
  it is a crash-loop backoff counter that resets (operator restart
  `managed_process.go:268`, >60s-uptime `:307`) — surfacing it as "restart
  count" would be a lie.
- **Mixed-fleet policy** (SpawnEnv precedent, W15): absent field on healthy
  scrape keeps prior CRD state; unreachable/undecodable clears it.
- **SSE**: `agent_restarted` (bare snake_case, payload in `data` — the
  `agent_died` convention; cross-lane ruling). Emitted by a NEW watcher
  callback (`SetRestartCallback`) because `onPhaseChange` is
  transition-only (`watcher.go:327`) and restarts land Active→Active.
  First sightings record without firing (the chat-load banner covers
  pre-stream restarts; avoids re-toast storms on API restarts); the seed
  pre-populates stamps (including empty) so post-seed changes always fire.

## Assumptions (Rule 7 — state + validate)

1. Watchdog runs in agentd in BOTH topologies — validated: `server.go:806-830`
   restarter seam + shared `backgroundLoops`.
2. Fire-time verdict is always `hung` (corroborated) or `unknown` (escalation);
   suppressions carry `starved|flat|respawn|unknown` — validated against
   the kill-set logic in `healthz_cache.go`.
3. Controller must read the block from `/v1/healthz` (15s) not statusz (60s)
   to meet the one-reconcile-cycle AC — validated `health.go:47,56`.
4. healthz stays I/O-free (US-22.1) — block served from in-memory log +
   readiness cache. Pinned by the existing latency/no-opencode tests.
5. onPhaseChange is transition-only → new callback required — validated
   `watcher.go:327`; the orchestrator ratified S7.
6. **Go SDK does NOT model WorkspaceStatusResult** (orchestrator correction
   of my milestone (a) claim — my grep had matched `agentHealth` inside a
   different type in `sdks/go/types.go`). Hand-sync = openapi.yaml +
   `sdks/typescript/src/types.ts` only. OBSERVED GAP (not this lane's work,
   per orchestrator): the Go SDK lacks WorkspaceStatusResult coverage
   entirely — the #1699 SDK canary lane owns that tier.
7. Sidecar-mode supervisor CRASH restarts are out of scope (A11 ruling):
   watchdog restarts surface in both topologies; single-container crash
   restarts surface via the observer hook; sidecar crash attribution stays
   with the restart-reason marker + socket status. **Asymmetry stated per
   ruling**: the schema description says "supervisor-watchdog restarts plus
   process-crash restarts where recorded" — it does not overpromise.
   Orchestrator escalated the sidecar gap to the owner as a follow-up lane.

## Scope-boundary deviation (disclosed)

`pkg/mcp/integration_test.go` tool-count pin 43 → 44 + name assertion: the
pin exists to catch accidental additions; this one is the issue-mandated
tool. No other out-of-declared-scope file was touched.

## Cross-lane (final map)

- `sdks/openapi.yaml`: my sections only (`WorkspaceStatusResult.supervisor`
  + new `SupervisorStatus`/`AgentRestartEvent` adjacent to
  `AgentHealthResult`) — board sections untouched, keep-both with #1699.
- `sdks/typescript/src/types.ts`: WorkspaceStatusResult area only.
- `api/internal/types/sse_event.go`: ZERO touch (payload rides `Data`).
- `proxy_events.go`/`proxy_lifecycle.go`/`watcher.go`: mine alone per the
  final map. No app.go/router.go needs.
- Reported per instruction: the frontend `WorkspaceStreamEvent` union
  (types.ts:427) is mixed (`workspace.*` + bare snake) — ruling
  `agent_restarted` followed without deviation.

## Tests (TDD red-first; all commands run in the worktree)

- `go test ./pkg/agentd/ -run TestSupervisorStatus` — wire shape, omitempty,
  taxonomy.
- `go test ./cmd/workspace-agentd/ -run 'TestAgentRestartLog|TestSupervisorBlock|TestManagedProcess_RestartObserver|TestWatchdogFire_Records|TestWatchdogSuppression_Records|TestWatchdogUnknownEpisode_Records|TestWatchdogHealthy_Records|TestHealthzHandler|TestStatuszHandler|TestRefreshIsHealthyLoop|TestHealthWatchdog|TestRefreshOnce|TestHealthzCache'`
  — 45.8s green (recorder + hooks + handlers + all pre-existing watchdog
  decision pins).
- `go test ./pkg/apis/llmsafespaces/v1/ -run 'TestAgentRestarted|TestWorkspaceStatus_AgentSupervisor|TestAgentRestartEvent'`.
- `go test ./controller/internal/workspace/ -run 'TestMirrorAgentSupervisor|TestSupervisorRestartChain|TestCheckAgentHealth|...'` —
  incl. the acceptance-criterion chain test: agentd wire JSON → controller
  → CRD → CRD-wire keys pinned to the SDK schema property names.
- `go test ./api/internal/services/workspace/ ./api/internal/handlers/ -run 'TestGetWorkspaceStatus|TestWorkspaceWatcher|TestOnAgentRestart|TestProxy_DeleteSession'`.
- `go test ./pkg/mcp/` — full package (count pin updated); tool tests.
- `make -C sdks sdk-check` (spec valid + router parity) + `./bin/repolint`
  (all checks incl. spec_coupling_marker + agent ID prefixes + CRD drift
  with the new AgentSupervisorStatus leaf binding).
- Frontend: `npx vitest run` on ChatPage.agentRestart (new, 4),
  AgentRestartBanner (new, 5), ChatPage.sse, HealthBanner,
  useUserEventStream — 518 passed; `npx tsc -b` clean; SDK TS compiles.

## Adversarial self-review (Rule 11)

- REAL finding (fixed): watcher seed skipped empty stamps → a first live
  restart on a workspace seeded with no supervisor state would not fire
  (first-sighting skip). Seed now records unconditionally.
- Finding (documented, accepted): `AgentRestarted` condition is a latched
  historical record — never auto-removed, replaced by the next restart;
  `status.agentSupervisor` is the live state (cleared on unreachable).
- Finding (documented, accepted): first-sighting-skip means a restart
  landing in the (rare) window between API start and the first watch event
  for that workspace produces no toast — the chat-load banner covers it.
- False alarm (checked): `restartObserver` runs outside `p.mu` — matches
  the `onChildStarted` pattern (set before `start()`, single writer path).

## Resource discipline

Memory checked before every heavy command (peak observed 8.59G during the
package sweep — single-lane targeted runs only, no `-race`, no full
`make test`; `make build` deferred to the orchestrator's stagger).
`npm install` run once for the frontend test tier (disk 64% before, ~5.3G
free).

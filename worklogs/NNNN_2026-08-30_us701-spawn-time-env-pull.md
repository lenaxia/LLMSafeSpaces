# Worklog: US-70.1 — spawn-time env pull (bounded wait + last-good cache), spawned_rev, loud degradation

**Date:** 2026-08-30
**Session:** Epic 70 (#1158) US-70.1 (#1162, design 0057 R2/R3): the R2 core — the supervisor PULLS the env delta at every spawn from the user mux (`GET /v1/spawn-env`, §D1 credential, bounded wait), replacing the structurally-broken boot push; memory-only last-good cache; `spawned_rev` terminal verification (I4) + `degraded:spawn_env_unavailable` loud degradation (I10) relayed through the control socket → statusz → CRD path; A2 evidence test.
**Status:** Complete (in-repo core). Cluster-bound ACs (AC-1 `/proc/<agent>/environ` e2e, AC-2 suspend≥1h→resume≤90s, gVisor leg, US-70.0 harness chaos cases) marked for the staged pool — same disposition as the Epic 69 stories.

---

## Objective

Kill the live boot bug (3/6 fleet pods silently losing env-class secrets every boot: `pushInitialSpawnEnv` dials a control socket that cannot exist yet under native-sidecar startup gating) by moving the point of consumption to spawn with a pull, and make delivery terminally verifiable and loudly degradable. This is the tracked blocker for design 0053 S3's merge (#1156) — the sequencing gate runs both directions.

---

## Work Completed

### The pull endpoint (`spawn_env_pull.go`)
- `GET /v1/spawn-env` on the user mux (both modes; each serves its own secrets-env coordinate — single-container the uid-1000-owned file, sidecar the sidecar-owned one): D6.1 Basic gate; absent file = authoritative empty (normal); corrupt file = 500 (single-writer contract — surface, don't drop); fresh parse at REQUEST time (reloads hand off freshness with no push); response carries the deterministic content digest (`rev` — sorted-key sha256/16).

### The supervisor-side puller
- `spawnEnvPuller`: bounded-wait fetch (default 2s, never-block-spawn), memory-only last-good cache (I7), terminal `spawnedDelta/spawnedRev/degraded` records.
- `withSpawnEnvPull(base, puller)` wraps the supervisor's cmd factory: EVERY spawn (first boot + every restart) composes parent+delta from a fresh pull (platform-wins merge preserved — `parentPlusDelta`). Wired via `newSupervisorProcessPulling` (production: `newSpawnEnvPullerForPod` — pod mux URL + §D1 workspace credential read from the uid-1000-owned password file).
- The boot push path is untouched (US-70.5 demolition owns its deletion); no new code calls it.

### Terminal verification + loud degradation surfaces
- `managedProcAdapter.SpawnStatus()` → control socket `status` gains `spawned_rev` + `degraded` (degraded only when set).
- `controlClient.SpawnStatus` fetch; the sidecar wires `deps.spawnStatus` (3s-bounded socket call, 60s consumer cadence).
- `StatuszResponse` gains `spawned_rev`/`degraded`; `buildStatuszHandler` relays — the controller's deep-status scrape carries them into the CRD/alert path (I10/I13 plumbing point).

### Tests (TDD — written first)
Handler: fresh-delta-with-rev (rev tracks content), auth-and-absence (I8 + authoritative empty). Puller: bounded-wait fault injection (hang endpoint → bound holds, never-block-spawn), last-good cache survives outage (cached delta + rev; degrade visible, env not lost), fresh-after-reload. I4: `TestSpawnedRevTerminalVerification` — under injected skew (file rewritten between pulls), the reported rev matches the APPLIED delta, not the latest observation. Factory: `TestSpawnFactory_PullsAtSpawn` (every spawn pulls; parent preserved; rev recorded), `TestSpawnFactory_FirstBootDeadMuxLoud` (platform-env-only + degrade recorded + self-heals on recovery). I7: `TestLastGoodMemoryOnly` (cache adds zero plaintext beyond the source file). Surfaces: `TestStatuszRelaysSpawnStatus`. A2: `TestA2_SupervisorReadsD1Credential` (the only file the supervisor touches on this path is one it owns; the crossing is the HTTP boundary).

---

## Key Decisions

1. **The endpoint parses the FILE at request time** (not an in-memory batch): freshness on every pull with zero push machinery; the reload path needs no coordination.
2. **rev = sorted-key sha256/16 of the delta** — deterministic, independent of file formatting noise, cheap to verify at the terminal (`spawned_rev` is the digest of the delta the child spawned with).
3. **A2 by construction**: the supervisor reads only the uid-1000-owned password file; the cross-uid crossing is the HTTP boundary gated by the D6.1 pair (validated on the wire). No NEW cross-uid FILE crossing enters the boot path — the R3 matrix entry records this.
4. `degraded` clears on recovery (self-healing); `spawned_rev` retains the last spawn's rev even while degraded (terminal fact).

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | The pod mux (4097) is reachable in-pod in both modes (shared netns) | single netns by design (design 0051); handler + puller tests over real HTTP |
| A2 | The §D1 credential is readable by the supervisor's uid | `TestA2_...` (owned file, 0600); live evidence remains a pool artifact (fleet audit's counterevidence was the SIDECAR direction) |
| A3 | 2s bound >> healthy mux latency (localhost) | handler ms-scale; bound holds under fault injection |

## Blockers

None in-repo. Cluster-bound ACs (AC-1, AC-2, gVisor leg, US-70.0 chaos wiring) run on the staged pool. **#1156 (S3) merge unblocks when this lands.**

## Tests Run

- `go test -race ./cmd/workspace-agentd/` — PASS (181s, full package incl. all prior suites)
- `golangci-lint --new-from-merge-base ./cmd/... ./pkg/agentd/...` — 0 issues

## Next Steps

1. Review/merge this PR → unblocks #1156 (S3) → unblocks Epic 69 S2 (US-69.7/.8/.9).
2. Pool runs: AC-1/AC-2 e2e + gVisor leg via the US-70.0 harness; A2 live evidence recorded on #1162.
3. US-70.2 (builder + conditional pull endpoint) evolves the endpoint; US-70.5 deletes `pushInitialSpawnEnv` + `socketReloadProc`'s push.

## Files Modified

- `cmd/workspace-agentd/spawn_env_pull.go` + `spawn_env_pull_test.go` (new)
- `cmd/workspace-agentd/{server,supervise_opencode,sidecar_mode,control_socket,control_client}.go` (wiring + surfaces)
- `pkg/agentd/types.go` (StatuszResponse fields)
- `cmd/workspace-agentd/control_socket_helpers_test.go`, `main_test.go` (interface + call-site updates)

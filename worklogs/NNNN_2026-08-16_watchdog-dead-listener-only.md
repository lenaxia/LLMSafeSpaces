# Worklog: health-watchdog demotion to dead-listener-only (#892 D1, review rounds 1–2)

**Date:** 2026-08-16
**Session:** PRs #898 (this worklog) + superseded #894: reduce the watchdog kill set to dead-listener-only; vitals corroboration (TCP + CPU + throttle); marker-failure observability; review round 2 respawn-boot fix.
**Status:** Complete

---

## Objective

The 2026-08-15/16 incident: the watchdog killed a healthy opencode 7+ times (zero true positives) — every fire was CPU starvation under the fixed 2-CPU quota misread as "hung"; the max-defer force path killed busy sessions by design; kills during the respawn window raced crash recovery (6 restarts in 11 minutes).

## Work Completed

### Kill matrix (per #892 rulings)

| Evidence | Verdict | Action |
|---|---|---|
| Dial refused AND supervised pid alive AND past boot | HUNG | Kill — the only lethal path |
| Dial refused, pid gone/changed | RESPAWN | Suppress |
| Dial refused, child within boot grace (180s) | RESPAWN | Suppress (review round 2) |
| CPU ticks advancing | STARVED | Suppress |
| Listener accepts, CPU flat | FLAT | Suppress — blocked-IO turns are alive |
| No evidence | UNKNOWN | Suppress + Error log |

### Round 2 (review #898): the respawn-boot window

Round 1 shipped `pidGone` for the between-children window but a freshly spawned, not-yet-bound child has a **live pid with a refused dial** — classify read HUNG and killed it, recreating the churn under the exact starvation the PR targets. Fix: `managedProcess.childStartedAt()` accessor (the existing `lastRestartAt`); `procVitalsGatherer.childBootAt`; `vitalsBootGraceWindow = 180s` (matches the D4 kubelet startup budget — the incident documented boot-to-listen exceeding 120s under quota saturation). `gather()` sets `booting` when refused+young; classify routes it to RESPAWN. Verified red-without-fix via `TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess` (real subprocess, real gatherer, real loop: pre-fix log shows "triggering restart" on a booting child; post-fix suppresses).

Also round 2: context-cancel mid-sample now sets `pidGone` (a refused dial collected just before shutdown must not classify HUNG and fire during agentd exit).

### Also in this PR

- Stand-down removed: suppression is forever; visibility via `workspace_watchdog_suppressions_total{reason=starved|flat|respawn|unknown}`.
- Marker-failure observability: `workspace_restart_marker_write_failures_total` at all four write sites (incident: 9 attempted marker writes, 0 landed, stdout-only).
- `buildVitalsGatherer` extracted from `startBackgroundLoops` (wiring now smoke-tested).
- Supervisor `stop()` race fixed: crash-backoff path re-checks `stopRequested` at loop top — previously stop() signaled the child it saw (possibly dead), the supervisor respawned an unsignaled child, and stop() hung forever (found by the regression test's choreography).
- healthz reports `commit_sha`/`build_time` (build identity; ldflags already stamp them).
- Harness: `runWatchdogLoop` joins the loop goroutine before timing-var restore (`-race`).

## Key Decisions

- Boot grace 180s sized from the incident evidence (>120s observed starved boot-to-listen, doubling for headroom) and aligned with D4's planned 5s×36=180s kubelet startup budget so the watchdog never outlives the boot window kubelet tolerates.
- `childBootAt` from the supervisor (authoritative spawn time) rather than `/proc/<pid>/stat` starttime — no boot-time arithmetic, same file the gatherer already reads for ticks.
- Grace applies ONLY to refused dials. A child that binds and then loses its listener is lethal only after the grace expires (≤180s of tolerated suppression for a young child — the kubelet-startup-analogous trade-off); a listener-alive-but-wedged child is FLAT and suppressed forever per D1.

## Assumptions (validated)

1. `lastRestartAt` is set under mutex at every spawn (managed_process.go:191) — accessor is race-free.
2. 180s covers starved boot-to-listen: kubelet observed >120s; doubled for headroom and to match D4's 5s×36 startup budget (see Key Decisions).
3. Kernel refuses (not drops) dials to unbound ports on loopback — verified empirically in the regression test.

## Accepted residuals

- Pid-reuse during backoff (theoretical): if the reaped child's pid is recycled within the ≤30s backoff while the previous child ran >180s, the /proc read succeeds on the wrong pid → refused + live-pid + past-grace → HUNG → SIGTERM of the reused process. Requires pid-namespace wraparound in a ≤30s window; the same stale-pid signal semantics predate this PR. Closing it needs a starttime comparison — deferred.

## Blockers

None.

## Next Steps

- D4 probe truthfulness (#895), D5 badges (#896), D7 caps (#897) on this stack; G7 stress harness gates the merge train; post-deploy: watch workspace_watchdog_suppressions_total{reason} for a real hung verdict.

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestVitalSigns|TestProcVitals|TestWatchdogRespawnBootWindow|TestRefreshIsHealthyLoop_|TestManagedProcess_OnChildStarted|TestManagedProcess_StopDuringCrashBackoffReturns|TestBuildVitalsGatherer|TestRecord' -count=1 -race` — green
- `go test ./cmd/workspace-agentd/ -count=1` — full suite green (423s)
- `go build ./... && go vet ./cmd/workspace-agentd/ && gofmt -l && make fmt-check` — clean

## Files Modified

- cmd/workspace-agentd/watchdog_vitals.go (+watchdog_vitals_test.go)
- cmd/workspace-agentd/healthz_cache.go
- cmd/workspace-agentd/managed_process.go (+managed_process_test.go harness: /global/health, FAKE_BIND_DELAY_MS)
- cmd/workspace-agentd/managed_process_generation_test.go
- cmd/workspace-agentd/ops_metrics.go
- cmd/workspace-agentd/server.go (buildVitalsGatherer)
- cmd/workspace-agentd/version.go / pkg/version (build identity)
- pkg/agentd/types.go (build-identity doc)

## Obsolete sections below

Classify matrix (7 verdicts incl. booting rows); gatherer refused/boot-window/past-grace/cancel; real-subprocess regression (red-without-fix verified); wiring smoke (`buildVitalsGatherer` addr/pid/bootAt shape); metric emission (suppression + marker-failure counters, label normalization); loop tests fire-on-refused+live+past-boot only, suppress on flat/unknown/respawn/starved incl. max-defer force paths; latch reset. Full agentd suite green incl. `-race`.

## Files Modified

- cmd/workspace-agentd/watchdog_vitals.go (+_test)
- cmd/workspace-agentd/healthz_cache.go
- cmd/workspace-agentd/managed_process.go (stop race, childStartedAt; +_test harness /global/health + FAKE_BIND_DELAY_MS)
- cmd/workspace-agentd/ops_metrics.go
- cmd/workspace-agentd/server.go (buildVitalsGatherer)
- cmd/workspace-agentd/version.go / pkg/version (build identity)

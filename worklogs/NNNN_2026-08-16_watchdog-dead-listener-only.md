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

- Boot grace 180s tied to the D4 startup budget, not derived from observed bind time — both budgets must cover the same starved-boot tail.
- `childBootAt` from the supervisor (authoritative spawn time) rather than `/proc/<pid>/stat` starttime — no boot-time arithmetic, same file the gatherer already reads for ticks.
- Grace applies ONLY to refused dials; a booting child that binds and then hangs-dead is past grace and lethal as designed.

## Assumptions (validated)

1. `lastRestartAt` is set under mutex at every spawn (managed_process.go:183) — accessor is race-free.
2. 180s covers starved boot-to-listen: kubelet observed >120s; budget doubled for headroom (same number D4 chose; documented linkage).
3. Kernel refuses (not drops) dials to unbound ports on loopback — verified empirically in the regression test.

## Tests

Classify matrix (7 verdicts incl. booting rows); gatherer refused/boot-window/past-grace/cancel; real-subprocess regression (red-without-fix verified); wiring smoke (`buildVitalsGatherer` addr/pid/bootAt shape); metric emission (suppression + marker-failure counters, label normalization); loop tests fire-on-refused+live+past-boot only, suppress on flat/unknown/respawn/starved incl. max-defer force paths; latch reset. Full agentd suite green incl. `-race`.

## Files Modified

- cmd/workspace-agentd/watchdog_vitals.go (+_test)
- cmd/workspace-agentd/healthz_cache.go
- cmd/workspace-agentd/managed_process.go (stop race, childStartedAt; +_test harness /global/health + FAKE_BIND_DELAY_MS)
- cmd/workspace-agentd/ops_metrics.go
- cmd/workspace-agentd/server.go (buildVitalsGatherer)
- cmd/workspace-agentd/version.go / pkg/version (build identity)

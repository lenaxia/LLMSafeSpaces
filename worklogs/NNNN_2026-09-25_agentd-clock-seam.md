# Worklog: #1532 — the agentd clock seam: the sweeper + watchdog flake instances de-timed; the supervisor family triaged I/O-inherent

**Date:** 2026-09-25
**Session:** The #1532 CI flake family (sweeper / supervisor-helper / watchdog timeouts under load) — injectable clock seam at the two de-timed sites (instances 1+3), the named flaky tests converted to driven ticks/fake now, production byte-identical
**Status:** Complete

---

## Objective

Per #1532's orchestrator ruling: the fix class is CLOCK INJECTION, not per-test retries. The family's shared real-time pattern: tests asserting "enough ticks/sleeps happened in N real milliseconds" — a loaded runner routinely violates it, and the family's accumulated wall clock blew the package's 5-minute alarm twice (runs 35679268021 family, then #1524/#1527's runs).

---

## Work Completed

### The triage of the three sites

1. **The sweeper** (`startStagingSweeper`): a real 10-min ticker + `time.Now()` per tick — tick semantics untestable deterministically.
2. **The watchdog loop** (`refreshIsHealthyLoop` + the vitals gatherer): a real poll ticker; the boot-grace window (`vitalsBootGraceWindow`, compared via `time.Since(childBootAt)`) — the boot-window test's 700ms blind sleep raced the runner (the named 2/2 flake). Hidden second clock: the vitals `sampleWindow` (3s production default) — each would-fire gather costs 3s REAL.
3. **The supervisor family (instance 2): TRIAGED I/O-INHERENT, EXPLICITLY LEFT OPEN** — the real-subprocess waits (socket bind, pid respawn, the production respawn backoff inside the SEPARATE supervisor process) are external events a package-var seam can never reach deterministically; faking the parent's poll sleep only spins. An r1-cut routing of one poll sleep through the seam was structurally inert (no test installs the fake there) and is DROPPED. #1532's instance-2 concern stays open on the issue.

### The seam (`clock.go`)

Two package vars — `agentdNow`, `agentdNewTicker` (channel+stop shape) — defaults ARE the stdlib functions. Wired at the two de-timed sites: the sweeper's ticker+now, the watchdog loop's ticker + the boot-grace `time.Since` → `agentdNow().Sub`. The `setWatchdogTiming` house pattern (swap per-test, restore on cleanup, join-before-restore) documented in the seam. (An r0-cut `agentdSleep` var + a supervisor poll-sleep routing were the over-reach r1 retracted; the var is deleted, not left orphaned.)

### The deterministic conversions

- `TestClockSeam_ProductionDefaults`: the defaults pin — the seam vars ARE the real clock/ticker constructors (a production swap to a silent fake fails here).
- `TestClockSeam_ManualClockFake`: the fake's contract (ordered ticks, explicit world-advance).
- `TestStagingSweeper_DeterministicTicks` (NEW): driven ticks + fake now decide which files age out; idempotence and the gauge push COUNTED (locked reads — the recording fixture's mutex honored from the polling side).
- `TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess` (CONVERTED, the named 2/2 flake): 10 DRIVEN ticks with the fake now FROZEN at spawn — every would-fire moment lands inside the boot grace regardless of runner speed; sync on the observable (cf≥10) instead of a blind 700ms sleep; BOTH window arms asserted arithmetically (frozen now → booting/respawn; advanced past grace → not-booting/hung). The subprocess and hung server stay REAL (the gatherer is the subject). The 3s sampleWindow trap fixed by the house literal pattern (10ms window — the de-timed ticks must not re-acquire wall-clock through the vitals sample).
- Left deliberately real: the 6 verdict-table tests' real servers (their I/O is the subject; their sleeps are already just-enough bounded budgets — converting them would fake the HTTP layer, changing what they test) and the supervisor family's I/O waits (instance 2, above).

### r2 review round (documentation + dead-code)

- `agentdSleep` deleted (orphaned by the r1 retraction — a seam var without a site is speculative surface); the fake's sleepFn and its contract section removed with it.
- The stale "three sites" narrative corrected everywhere it survived the r1 retraction (clock.go's file comment, the var docs, clock_test.go's header, this worklog's seam section).
- clock.go's "tests run non-parallel by convention" clause reworded to the accurate claim (the seam-swapping tests must not run parallel; the package has t.Parallel elsewhere).
- The fire-decision comment's middle clause reworded ("the count first reaches the threshold of 2 at tick 2, leaving nine would-fire moments").
- The past-grace arm now uses `step()` (advance-without-tick): the loop receives no eleventh fire moment — that arm asserts through the direct gather only (r2's orphan-tick noise finding).

### r1 review round

- **The sweeper test's own gauge assert was the banned pattern** (an exact-count gauntlet against an in-flight tick handler — failed the PR's own CI run 36133455567): restructured to settle-on->= then assert exactness after the last tick's observables settle (the sweeper is the only pusher).
- **Instance 2 honestly re-scoped**: the supervisor-family claim dropped (the helper re-execs a separate OS process — a package-var seam can never reach it; the r1-cut poll-sleep routing was inert and is removed). The PR now closes instances 1+3 and leaves instance 2 explicitly open on #1532.
- **The "ten fire decisions" comment corrected to the exact arithmetic** (ten probe failures; NINE would-fire moments past the threshold of 2).

---

## Key Decisions

- **Package vars over struct injection**: the watchdog loop is a free function reading package timing vars (the established `setWatchdogTiming` pattern); a parallel var seam is the minimal-consistent shape. Non-parallel test convention + join-before-restore keeps -race clean (verified).
- **Sync on observables, not channel drains**: driven ticks are buffered; the tests wait for OUTCOMES (cf≥10, aged-file-gone, gauge-count) — pace-independent asserts, bounded real waits.
- **Ticker-creation sync**: a tick delivered before the consumer builds its ticker is silently lost — the fake exposes `tickerCount()` and the boot-window test syncs on creation before flooding.
- **The vitals gatherer literal**: `newProcVitalsGatherer` bakes the 3s production sampleWindow; the converted test builds the literal with 10ms (the existing pattern at :134/:153) so driven ticks don't re-acquire 3s of wall clock per would-fire gather.

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -race -run 'TestClockSeam|TestStagingSweeper' ./cmd/workspace-agentd/` — PASS under -race (the seam defaults + fake + sweeper ticks).
- `go test -count=1 -race -run 'TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess' -v` — PASS, 1.03s (was 700ms blind + race-prone; now deterministic with MORE assertions — both window arms).
- `go test -count=1 -race -run 'TestWatchdog|TestClockSeam|TestStagingSweeper|TestBuildSidecarDeps_SweeperPlacementGuarded|TestStagingScrub|TestStagingLifecycle'` — 9 PASS.
- `go test -count=1 -run 'TestSupervisorSubprocess|TestChildEnvironObserver|TestProcVitals|TestVitals'` — ok (13.5s, real subprocesses, unchanged semantics on the seam defaults).
- `go test -count=1 -run 'TestRefresh|TestHealthz|TestHealthWatchdog|TestHealth' -v` — 55 PASS (the full healthz/watchdog family on the seam's production defaults; the real-sleep verdict tests unchanged and green).

---

## Next Steps

- CI's race+coverage run is the family's real proof (the flake was load-shaped; the de-timed tests no longer depend on runner speed).
- If a fourth real-clock site surfaces, route it through the seam rather than adding a sleep (the seam's doc comment is the pointer).

---

## Files Modified

- `cmd/workspace-agentd/clock.go` — NEW: the seam (2 vars, stdlib defaults, the discipline note)
- `cmd/workspace-agentd/clock_test.go` — NEW: manualClock fake + tickerCount sync + the defaults pin + the fake contract test
- `cmd/workspace-agentd/upload_staging.go` — the sweeper loop through the seam
- `cmd/workspace-agentd/healthz_cache.go` — the watchdog loop's ticker through the seam
- `cmd/workspace-agentd/watchdog_vitals.go` — the boot-grace compare through the seam
- `cmd/workspace-agentd/upload_staging_test.go` — the deterministic sweeper test + the locked gauge read
- `cmd/workspace-agentd/watchdog_vitals_test.go` — the boot-window conversion (driven ticks, frozen/advanced now, gatherer literal)
- `worklogs/NNNN_2026-09-25_agentd-clock-seam.md` — this worklog

# Worklog: #1532 — the agentd clock seam: the three real-clock flake sites de-timed

**Date:** 2026-09-25
**Session:** The #1532 CI flake family (sweeper / supervisor-helper / watchdog timeouts under load) — injectable clock seam at the three sites, the named flaky tests converted to driven ticks/fake now, production byte-identical
**Status:** Complete

---

## Objective

Per #1532's orchestrator ruling: the fix class is CLOCK INJECTION, not per-test retries. The family's shared real-time pattern: tests asserting "enough ticks/sleeps happened in N real milliseconds" — a loaded runner routinely violates it, and the family's accumulated wall clock blew the package's 5-minute alarm twice (runs 35679268021 family, then #1524/#1527's runs).

---

## Work Completed

### The triage of the three sites

1. **The sweeper** (`startStagingSweeper`): a real 10-min ticker + `time.Now()` per tick — tick semantics untestable deterministically.
2. **The watchdog loop** (`refreshIsHealthyLoop` + the vitals gatherer): a real poll ticker; the boot-grace window (`vitalsBootGraceWindow`, compared via `time.Since(childBootAt)`) — the boot-window test's 700ms blind sleep raced the runner (the named 2/2 flake). Hidden second clock: the vitals `sampleWindow` (3s production default) — each would-fire gather costs 3s REAL.
3. **The supervisor family**: the real-subprocess tests are Eventually-bounded I/O (their subject IS process+socket reality — spawn latency under load is inherent, not a clock decision); the de-timable piece is the test-side poll cadence (`waitForSocket`'s blind 50ms sleeps).

### The seam (`clock.go`)

Three package vars — `agentdNow`, `agentdNewTicker` (channel+stop shape), `agentdSleep` — defaults ARE the stdlib functions. Wired at exactly the three sites: the sweeper's ticker+now, the watchdog loop's ticker + the boot-grace `time.Since` → `agentdNow().Sub`, the supervisor test's poll sleep. The `setWatchdogTiming` house pattern (swap per-test, restore on cleanup, join-before-restore) documented in the seam.

### The deterministic conversions

- `TestClockSeam_ProductionDefaults`: the defaults pin — the seam vars ARE real clock/ticker/sleep (a production swap to a silent fake fails here).
- `TestClockSeam_ManualClockFake`: the fake's contract (ordered ticks, explicit world-advance, recorded sleeps).
- `TestStagingSweeper_DeterministicTicks` (NEW): driven ticks + fake now decide which files age out; idempotence and the gauge push COUNTED (locked reads — the recording fixture's mutex honored from the polling side).
- `TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess` (CONVERTED, the named 2/2 flake): 10 DRIVEN ticks with the fake now FROZEN at spawn — every would-fire moment lands inside the boot grace regardless of runner speed; sync on the observable (cf≥10) instead of a blind 700ms sleep; BOTH window arms asserted arithmetically (frozen now → booting/respawn; advanced past grace → not-booting/hung). The subprocess and hung server stay REAL (the gatherer is the subject). The 3s sampleWindow trap fixed by the house literal pattern (10ms window — the de-timed ticks must not re-acquire wall-clock through the vitals sample).
- The supervisor tests: poll sleep routed through the seam (I/O-speed under the fake); their Eventually budgets unchanged (I/O-bounded, not clock-decided).
- Left deliberately real: the 6 verdict-table tests' real servers (their I/O is the subject; their sleeps are already just-enough bounded budgets — converting them would fake the HTTP layer, changing what they test).

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

- `cmd/workspace-agentd/clock.go` — NEW: the seam (3 vars, stdlib defaults, the discipline note)
- `cmd/workspace-agentd/clock_test.go` — NEW: manualClock fake + tickerCount sync + the defaults pin + the fake contract test
- `cmd/workspace-agentd/upload_staging.go` — the sweeper loop through the seam
- `cmd/workspace-agentd/healthz_cache.go` — the watchdog loop's ticker through the seam
- `cmd/workspace-agentd/watchdog_vitals.go` — the boot-grace compare through the seam
- `cmd/workspace-agentd/upload_staging_test.go` — the deterministic sweeper test + the locked gauge read
- `cmd/workspace-agentd/watchdog_vitals_test.go` — the boot-window conversion (driven ticks, frozen/advanced now, gatherer literal)
- `cmd/workspace-agentd/supervisor_subprocess_test.go` — the poll sleep through the seam
- `worklogs/NNNN_2026-09-25_agentd-clock-seam.md` — this worklog

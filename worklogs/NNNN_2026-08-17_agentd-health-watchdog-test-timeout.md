# Worklog: Fix agentd health-watchdog test CI timeout (production-timing tests)

**Date:** 2026-08-17
**Status:** Complete
**Session:** Root-cause and fix the intermittent `cmd/workspace-agentd` package timeout in CI's `Test (-short, with coverage)` job
**PR:** (this change)
**Issue:** None filed; pre-existing CI instability observed repeatedly 2026-08-16→17

---

## Objective

Fix the intermittent `panic: test timed out after 5m0s` in
`cmd/workspace-agentd` under CI's `go test -race -short -covermode=atomic
-coverpkg=./... -timeout 300s ./...`. It failed 4× on main on 2026-08-17
(04:57, 06:06, 06:11, 07:07) plus once during PR #917's CI, forcing reruns.

## Root Cause

Six watchdog/healthz tests used **production wall-clock timings** instead
of the shrunk-cadence pattern already established in
`watchdog_vitals_test.go` (`setWatchdogTiming`, 60ms/40ms/3):

- `TestRefreshIsHealthyLoop_WatchdogFiresOnHang` — 40s sleep + 10s server hang
- `TestRefreshIsHealthyLoop_WatchdogDoesNotFireOnHealthy` — 12s sleep
- `TestRefreshIsHealthyLoop_WatchdogDoesNotFireDuringBoot` — 35s sleep
- `TestRefreshIsHealthyLoop_WatchdogDefersWhenSessionsBusy` — 40s sleep + 10s hang
- `TestRefreshIsHealthyLoop_WatchdogFiresAfterSessionsGoIdle` — up to 90s of Eventually windows + 5s hang
- `TestRefreshOnce_Timeout_TreatedAsFailure` — 10s server hang

Under `-race` + `-coverpkg=./...` these ballooned to 35–45s EACH (measured
via `go test -v`: the four watchdog tests alone totaled ~145s). The package
total went 303s locally (vs CI's 300s limit) → any CI runner load pushed it
over. Repro: `go test -race -short -covermode=atomic -coverpkg=./...
-timeout 600s ./cmd/workspace-agentd/` → 302.8s.

## Fix

Converted the six tests to the shrunk cadence (`setWatchdogTiming` at
60ms/40ms/3), replaced fixed `time.Sleep(40s/35s/12s)` with
`require.Eventually` on the actual observable, and bounded the mock server
hangs to 500ms (still > the shrunk 40ms timeout, so `IsHealthy` times out
but the mock closes promptly).

Two additional bugs surfaced during the conversion, both real:

1. **Data race (caught by -race):** a body `defer setAgentAddr(origAddr)`
   runs BEFORE `t.Cleanup` callbacks (defers run before cleanups), so the
   addr restore raced the still-alive loop goroutine. Fixed by moving
   addr-set + addr-restore + goroutine-join into a single
   `runRefreshLoop` helper whose cleanup does `cancel(); <-done;
   setAgentAddr(orig)` — mirroring the existing `runWatchdogLoop` pattern.
2. **Weak Eventually predicate:** waiting on the mock's `callCount`
   (increments on handler ENTRY, before the hang resolves) can pass while
   the cache has only 1 failure. The correct observable is
   `cache.Snapshot().ConsecutiveFailures >= 3` — the value that actually
   flips `Healthy`. Two tests now wait on that.

## Key Decisions

- Shrunk cadence over production timings: the tests assert *behavior*
  (latch, deferral, boot gating), not wall-clock; the 
  `watchdog_vitals_test.go` file already established this exact pattern
  (`setWatchdogTiming`) — these tests simply hadn't been migrated.
- Bounded 500ms hang: reliably exceeds the shrunk 40ms timeout yet keeps
  `mock.Close()` prompt (a 10s hang on a 60ms interval means close waits
  on in-flight handlers).
- `require.Eventually` over fixed sleeps: deterministic under CI load
  (the `FiresAfterSessionsGoIdle` test already used it; extended to the
  others).

## Blockers

None.

## Tests Run

- `go test -race -short -covermode=atomic -coverpkg=./... -timeout 300s
  -count=1 ./cmd/workspace-agentd/` → **139.5s PASS** (was 303s — below
  the 300s CI limit with ~2.2x headroom), run 3×: 139.5/139.6/138.9s,
  all PASS.
- Targetted: all 6 converted tests pass (0.25–0.75s each).
- `go vet ./cmd/workspace-agentd/` clean; `golangci-lint run
  ./cmd/workspace-agentd/...` → 0 issues; `gofmt -l` clean.

## Next Steps

1. PR + review through the onboarded pipeline.
2. Watch the next week of `Test (-short, with coverage)` runs on main —
   expect the package to sit ~140s with no more 300s-timeout flakes.
3. (Tracked separately, not this PR) ai-workflows salvage/retry and #911.

## Files Modified

- `cmd/workspace-agentd/healthz_cache_test.go`
- `worklogs/NNNN_2026-08-17_agentd-health-watchdog-test-timeout.md` (this entry)

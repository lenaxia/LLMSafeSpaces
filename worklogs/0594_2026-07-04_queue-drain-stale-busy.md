# Worklog: Close #388 residual — stale-busy blind spot + ticker regression test

**Date:** 2026-07-04
**Session:** Address the residual defects from #388: (1) the missing ticker-lifecycle regression test, (2) the dual-drop stale-busy blind spot where both the API and agentd miss an idle transition, (3) drain success observability.
**Status:** Complete

---

## Objective

Issue #388 was largely fixed by PR #418 (periodic sweep), but a deep revalidation (posted to the issue) confirmed two residual defects: a stale-busy blind spot (if both SSE subscriptions miss the same idle event, statusz holds "busy" forever and the sweep skips the session indefinitely), and the requested regression test ("SSE alive, idle event dropped, ticker recovers over time") was still missing. This closes both.

---

## Work Completed

### Fix 1: Ticker-lifecycle regression test
- Made `queueSweepInterval` injectable via `SetSweepInterval` (defaults to 30s when zero; zero-value safe).
- Added `TestStartQueueSweep_DrainsViaTickerWhenIdleEventDropped`: drives the actual `startQueueSweep` goroutine with a 50ms interval, enqueues a message, never emits `session.status:idle` (simulating the dropped-event scenario), asserts the ticker drains within ~2 ticks. This is the exact scenario the issue asked for and that no prior test covered.

### Fix 2: Stale-busy blind spot
- Added `staleBusyThreshold = 5 * time.Minute` constant.
- In `reconcileSessionState`, added a second pass after the idle-session loop: for sessions statusz reports as "busy" with a non-empty queue whose oldest message is older than `staleBusyThreshold`, optimistically drain directly via `drainQueuedMessage` — NOT via `onSessionIdle` (which would publish a false "idle" to the UI).
- Safety net: the existing 409-requeue path (`proxy_events.go:466-474`) protects against a truly-busy session — opencode returns 409, the message is requeued, the next sweep retries. `RetryCount` is NOT incremented for 409 (already the case), so the retry budget is never consumed by optimistic drains.
- Added `TestReconcileSessionState_DrainsStaleBusySession`: two sub-cases — fresh queue (< threshold) NOT drained (session might genuinely be busy), stale queue (> threshold) IS drained.

### Fix 3: Success observability
- Added info-level log to `drainQueuedMessage` on successful message send (`"drainQueuedMessage: sent queued message"`). Previously the success path was completely silent — operators could only infer a drain happened from the `queue.update sent` SSE event.

---

## Key Decisions

1. **Stale-busy threshold: 5 minutes.** The normal idle-event drain fires within one sweep cycle (~30s). A message queued for 5+ minutes with statusz still saying "busy" is either a very long legitimate turn (409-requeue handles it cheaply) or a stale statusz (the blind spot). 5 min balances false-positive cost (one 409 per 30s during a long turn = ~10 HTTP calls) against detection latency.

2. **Direct `drainQueuedMessage`, not `onSessionIdle`.** The stale-busy path must NOT publish a false "idle" to the UI (the session might genuinely be busy). `drainQueuedMessage` is a standalone function that dequeues + sends to opencode; the 409-requeue is the safety net. `onSessionIdle` publishes idle to the user broker, records activity, etc. — wrong for the optimistic-drain case.

3. **`PeekAll` for age check, not `Len`.** `Len` returns a count but not the `EnqueuedAt` needed for staleness. `PeekAll` returns full `QueuedMessage` structs with timestamps. The `LRange` it uses is O(n) but n is typically 1–3 messages per session.

---

## Blockers

None.

---

## Tests Run

- `go test -race -run "TestStartQueueSweep_DrainsViaTickerWhenIdleEventDropped|TestReconcileSessionState_DrainsStaleBusySession" -v ./api/internal/handlers/` — 3/3 PASS.
- `go test -race -run "TestPeriodicSweep|TestDrainMiss|TestDrainQueuedMessage|TestReconcile|TestStartQueueSweep" -v ./api/internal/handlers/` — 18/18 PASS (15 existing + 3 new), zero regressions.
- `go build ./...` + `go vet` — OK.
- `gofmt` + `misspell` — clean.

---

## Next Steps

- The original incident's common case (single-sided SSE drop) was already fixed by PR #418. This closes the dual-drop residual. The only remaining theoretical gap is if opencode itself never reports the session in `ListSessions` at all — but that's an opencode bug, not a queue-drain bug.
- Consider adding Prometheus counters for sweep activity (`sweep_cycles_total`, `sweep_drain_attempts_total`, `sweep_stale_busy_drains_total`) as a future observability enhancement.

---

## Files Modified

- `api/internal/handlers/proxy.go` — added `sweepInterval` field.
- `api/internal/handlers/proxy_lifecycle.go` — added `SetSweepInterval`.
- `api/internal/handlers/proxy_events.go` — added `staleBusyThreshold`; injectable sweep interval; stale-busy drain path in `reconcileSessionState`; success log in `drainQueuedMessage`.
- `api/internal/handlers/proxy_queue_drain_miss_test.go` — added 2 new test functions (3 test cases total).
- `worklogs/0594_2026-07-04_queue-drain-stale-busy.md` — this worklog.

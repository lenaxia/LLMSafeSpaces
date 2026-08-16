# Worklog: SSE tracker watch re-arm — always arm on Active + self-heal reconciler (#902)

**Date:** 2026-08-16
**Session:** Root cause and fix for halting sessions (sends succeed, turns run, client streams receive zero agent events). Investigation of workspace 843a55c2 / ses_ff8df491, 2026-08-16 16:19–17:10 UTC.
**Status:** Complete

---

## Objective

Users could not resume a session: every sent message "halted". Server-side, sends returned 200, turns ran to completion in opencode, history refetched — but the client's SSE stream received zero agent events (heartbeat-only or entirely empty). Fix the event blindness.

## Root cause (proven)

The SSE tracker watch for a workspace is armed only by (a) a phase **transition** into Active, or (b) a user `StreamEvents` landing on that specific replica. `onPhaseChange` skipped `EnsureWatching` when prior == Active — and prior-phase state is **Redis-backed and survives API restarts**, so:

1. Post-restart seed (`seedResourceVersion` → `onPhaseChange(Active)`) took the else-branch (Redis prior already "Active") and never armed the watch.
2. Watches that die later (workspace pod churn at 16:24, connection drops) have no transition event — never re-armed.
3. `subscribe()`'s retry loop only covers transient failures inside a live goroutine; it cannot resurrect a never-created or exited watch.

Evidence chain: user streams heartbeat-only during active turns while one replica's stream DID relay `message.updated` (per-replica independence); `/proc/net/tcp` in the workspace pod showed zero external `/event` connections while Active; SIGQUIT goroutine dump of a freshly-booted replica showed 3 subscribe goroutines for 4 Active workspaces (843a never armed); both replicas blind by 17:10. All failure paths logged at Debug (#901 G1/G2).

## Work Completed

- `proxy_events.go` `onPhaseChange`: `EnsureWatching` on EVERY Active event (idempotent). Transitions/first-sighting keep full `invalidateCaches` + `StopWatching` (fresh connection to the new pod); no-transition updates keep config-only invalidation and arm without stopping (never tear down a healthy connection on activity-driven status updates).
- `proxy_lifecycle.go`: `sseWatchReconciler` — every `sseWatchReconcileInterval` (60s, var for tests) re-arms watches for every Active workspace via the new `phaseSource` interface (production: CRD watcher's `GetAllKnownPhases`). Converts any silently-missing watch into ≤60s blindness, permanently. Wired in `Start()`; exits on `stopCh`.
- `proxy.go`: `phaseSource` interface field (injectable for tests).
- `sse/tracker.go`: `ForceWatchingForTest` (arm-without-connect, for stale-watch simulation).
- Tests: Active→Active update arms the watch (the exact #902 regression — red pre-fix), transition still Stop+Ensures, Suspended still stops, reconciler heals a missed Active watch and ignores non-Active phases (`-race` clean).

## Key Decisions

- Reconciler is belt-and-braces ON TOP OF the seed fix, not instead of it: the else-branch fix makes the common path correct; the reconciler covers unknown-unknowns (goroutine exits, future bugs) with bounded blindness.
- No `StopWatching` on no-transition updates: status CRs update on activity; stopping would flap healthy connections.
- 60s interval: bounded blindness vs. a map-iteration over knownPhases per minute — negligible cost.

## Assumptions (validated)

1. Prior-phase persists in Redis across API restarts — `wsstate.RedisStore` wiring in app.go; both replicas share it.
2. `EnsureWatching` is idempotent when armed — map-existence check (tracker.go:150-156).
3. Per-replica trackers each need their own watch — brokers are in-process (user_broker.go).

## Blockers

None.

## Tests Run

- `go test ./api/internal/handlers/ ./api/internal/services/sse/ ./api/internal/services/workspace/ -count=1` — green (handlers 95s)
- `go test ./api/internal/handlers/ -run 'TestOnPhaseChange_|TestSSEWatchReconciler' -count=1 -race` — green
- `go build ./... && go vet ./api/... && gofmt` — clean

## Next Steps

- #901: tracker-state gauge/alerts (G1/G2/G4), pprof, stream logging — the observability that would have made this a 5-minute diagnosis
- Post-deploy: verify both replicas hold `/event` connections for all Active workspaces during pod churn

## Files Modified

- api/internal/handlers/proxy_events.go
- api/internal/handlers/proxy_lifecycle.go
- api/internal/handlers/proxy.go
- api/internal/handlers/proxy_auth_cache_test.go
- api/internal/services/sse/tracker.go

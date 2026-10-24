# Lane-1927 — #1927 usagestream resync/flight family de-flake (PR #TBD)

Date: 2026-10-24 · Lane: lane-1927 (orchestrator novel-nutmeg-verifies) · Branch: fix/1927-usagestream-resync-flakes · Base: origin/main 9ac1793a6 · Test-only; no production changes.

## Objective

Issue #1927: the `TestUsageBridgeStreamResynced_*` family reddens ~2/5 attempts on byte-identical content on main (post-#1899); related wild sightings: `TestStreamUserEvents_ConnectPathFlightNotCoalesced` (this lane reproduced and fixed), `TestRequestSessionStatusSnapshot_Coalesces` (de-flaked separately in #1910). 8+ reddened runs across lanes poison red-check attribution fleet-wide. Mandate: validate-first on current main, root-cause, de-flake deterministically (no sleeps/retries/skips), #1910/#1916/#1842/#1888 conventions.

## Validation (reproduce on origin/main before any fix)

All numbers at main 9ac1793a6, `-run 'UsageBridgeStreamResynced|ResyncFlightE2E|ConnectPathFlightNotCoalesced'` over `./api/internal/services/usagestream/ ./api/internal/handlers/`:

| Arm | Result |
|---|---|
| in-process `-race -count=10` | `ConnectPathFlightNotCoalesced` RED 5/10 (0.00 s, "got no begin marker" + "must complete") |
| in-process `-race -count=12` GOMAXPROCS=1 / =2 | RED 7/12 each (same signature) |
| in-process `-race -count=20` | RED 15/20 — exactly runs 6–20, i.e. every connect past the 10th |
| family + neighbors in one process (`-run 'TestStreamUserEvents|TestSSEConnAllowed|…'`) | 5 downstream SSE tests RED: `LiveEventDelivery`, `LiveEvent_HasIDLine`, `Replay`, `SnapshotEmitsBeforeLiveEvents`, `SnapshotSkipsEmptyPhase` |
| fresh-process `-count=1` x20 | 0 red |
| usagestream pkg `-race -count=20` GOMAXPROCS 1/2 | 0 red (`ResyncFlightE2E_*` stable — the suspect `snapshotSeeds`/connections gate already carries the #1884 connections-keyed fix; nothing to do there) |
| full `./api/internal/handlers/ -short` | green (the wild "~2/5": whether the 60 s window boundary falls between the connect groups is run-load timing — byte-identical content, timing-dependent redness) |

Deterministic law derived from the counts: every arm reds from exactly the 11th in-process loopback SSE connect onward; fresh processes never red. The flake is in-process cross-test contamination, not a product race.

## Root cause (both vectors test-side)

**Vector A — per-IP SSE connect budget (`sseConnCounts`, stream_user_events.go):** package-global, 10 connects/min per IP; every SSE-connecting test in the binary shares one loopback `127.0.0.1` bucket. The family's collectors (`collectSSE`, 9 call sites) plus neighbors exceed 10 in-process; the next connect is answered 429 with a JSON body — and `collectSSE` never checked the status, so a 429 read as "connected, no events" → silent 0.00 s reds ("mid-flight joiner got no begin marker"). In-process stress (`-count≥6`) reddens ConnectPath deterministically; fast family+neighborhood runs redden the downstream collectors (`LiveEventDelivery`, `Replay`, …); slow runs (>60 s between groups) stay green — the ~2/5 byte-identical signature.

**Vector B — flight-coalescing marker (`busyFlightsInFlight`, proxy_busy_snapshot.go):** package-global `sync.Map` keyed by the shared `busySnapWS` id; `Delete` runs in the flight goroutine's defer — AFTER the flight's final publish. A test returning before that Delete (ConnectPath returns on the JOINER's complete while the on-demand flight may still be finishing; every flight test returns on a complete that strictly precedes its marker Delete) leaks a marker that swallows the next harness's first on-demand cue → `resync flight incomplete before deadline; got []` — the `TestUsageBridgeStreamResynced_*` wild signature. The lane-1907 worklog flagged this exposure as a pre-existing residual; #1927 is that residual biting.

Product verdict: `triggerBusySnapshotFlight`'s `LoadOrStore` guard is an atomic CAS (no check-then-act window — #1910 proved the same for the POST path); the connect path is deliberately exempt and correct; `sseConnAllowed`'s budget is a production DoS guard behaving as designed. Both defects are missing inter-test isolation of package-global state — same class as #1910 (test-design flaw, not product race).

## Fix (test-side only; no production changes)

1. `newBusySnapHarness`: `busyFlightsInFlight.Delete(busySnapWS)` at construction (every family harness starts marker-clean; a stale marker belongs to a torn-down harness, so clearing it cannot affect any live flight) + `t.Cleanup(resetLoopbackSSEBudget)` (the harness's budget consumption is wiped for non-resetting successors).
2. `collectSSE`: `resetLoopbackSSEBudget()` before each connect (each connect starts from a live budget — the discipline `TestSSEConnAllowed_*` established, narrowed to the loopback entries) + `require.Equal(http.StatusOK, resp.StatusCode, …)` so any future budget exhaustion fails loudly with its status, never as a silent event-less stream.
3. `TestStreamUserEvents_ConnectPathFlightNotCoalesced`: #1916's `releaseOnce` defer-based idempotent park-release — mandatory hardening once collectors can fatal on non-200 while fetches are parked (otherwise the stub's `t.Cleanup(srv.Close)` waits on parked handlers to the 10-minute go-test timeout).

No sleeps/retries/skips added; no assertion weakened; no production line touched.

## Evidence

- Red-first baseline: pre-fix main 9ac1793a6 → `ConnectPathFlightNotCoalesced` 15/20 at `-race -count=20` (stash-verified around the fix).
- Mutation A (collectSSE budget reset neutered) + planted consumed loopback budget in a predecessor test → RED, loud named signature `SSE connect rejected (status 429)…` (0.01 s). Restored → green under the same plant.
- Mutation B (harness marker clear neutered) + planted stale `busyFlightsInFlight` marker in a predecessor test → RED `resync flight incomplete before deadline; got []` (3.01 s) in `FiresBusySnapshotFlight` — the wild signature reproduced deterministically. Restored → green under the same plant.
- Post-fix stability: `-race -count=100` (handlers family) 0 red; `GOMAXPROCS=1 -count=50` 0; `GOMAXPROCS=2 -race -count=50` 0; `GOMAXPROCS=1 -race -short -covermode=atomic -count=50` 0; fresh-process `-race -count=1` x20 (both packages) 0; family+neighbors `-short -v` all green; full `./api/internal/handlers -short` green (39.9 s); usagestream `-race -count=20` GOMAXPROCS 1/2 0 red; `go vet` + `gofmt` clean.

## Mutation-verify recipe for review (on the merged tree)

(a) Neuter `collectSSE`'s `resetLoopbackSSEBudget()` call, run a predecessor that plants `sseConnCounts["127.0.0.1"] = &sseConnAttempt{count: sseConnRateLimit, resetAt: now+time.Minute}` before `ConnectPathFlightNotCoalesced` → must redden with the named 429 signature within one `-count=1`. (b) Neuter the harness's `busyFlightsInFlight.Delete(busySnapWS)`, run a predecessor that does `busyFlightsInFlight.LoadOrStore(busySnapWS, struct{}{})` before `FiresBusySnapshotFlight` → must redden `resync flight incomplete before deadline; got []` within 3 s.

## Residuals

- The `stream_user_events_test.go` collectors' own inter-test budget accounting is exactly at the 10-connect edge once a run starts them from a clean bucket (the family now guarantees that); if a future test adds an 11th real connect there, the same hygiene (pre-connect reset) should be applied to its collector — out of scope for #1927's family.
- The marker's complete-publish → deferred-Delete microsecond gap still exists in production semantics (coalescing window); with per-harness marker clearing its cross-test effect is nil, and within a single test the #1910/#1916 nets cover it.

## House

Worklog `worklogs/NNNN_1927-usagestream-resync-family-deflake.md` (sentinel — bot renumbers) · CHANGELOG Unreleased append · COORDINATE.md active-claim row. Never self-merge — orchestrator gates and merges.

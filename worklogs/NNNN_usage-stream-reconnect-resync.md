# NNNN — fix #1884: usage-stream reconnect resync (no silent loss)

Issue: https://github.com/lenaxia/LLMSafeSpaces/issues/1884

## The loss mode (from the issue + #1883's validation)

The browser SSE leg is fully covered (resync #1687, reconnect wipe #1646,
staleness watchdog #1365, busy-snapshot handshake + lease #1848). The
POD→API usage leg was not: a silent drop of the ABI events stream lost
frames server-side while the user stream stayed healthy — so no
browser-leg heal ever fired, and sidebar busy latches froze (the #1883
incident signature). The consumer's #1666 reconcile already heals the
per-session busy DIFFS the gate can compute on reconnect, but two
residuals remained: (a) sessions absent from the fold get no correction
("the reconcile asserts only truths the authority answered"), and (b)
browser-side latch state is invisible to the gate's `bridged` baseline —
neither is computable from the fold. Only an authority flight heals them.

## Changes

1. **Consumer** (`api/internal/services/usagestream/consumer.go`):
   new optional `BridgeResync` seam (`StreamResynced(workspaceID)`),
   fired on every NON-INITIAL snapshot seed (gate's
   `snapshotSeeds > 1`): a reconnect's first fold after a silent stream
   death, and the in-stream reseed redial (`WithResynced` re-arms the
   seed branch). The initial connect fires nothing (nothing lost; the
   connect-path flight already covers subscribers). Not a diff: an
   agreeing fold still fires. Emissions run after the #1666 corrections,
   outside the gate lock.

2. **Handler** (`proxy_busy_snapshot.go`, `proxy_usagestream.go`):
   `triggerBusySnapshotFlight` — the shared, coalesced
   (`busyFlightsInFlight` LoadOrStore), detached (goroutine) flight
   trigger; `RequestSessionStatusSnapshot` refactored onto it;
   `usageBridge.StreamResynced` calls it. The flight is the #1848
   D9/D10 authority reconciliation (statusz-sourced rows, ok=false on
   authority failure — latches keep state, never an authoritative empty).

3. **Ask 3** (`api/internal/middleware/logging.go`):
   `/internal/v1/session-registry` joined the default SkipPathPrefixes —
   the status-free pod-auth replica pull's response_body lines misled
   the #1883 log triage (read as the user-facing /sessions list).

4. **Ask 2** (dead surface): the FRONTEND `/sessions/active` surface is
   deleted — `ActiveSessionsResponse` type, contract fixture +
   generator entry, contract test row, and the two inert e2e route
   mocks. The API route + Go/TS SDK STAY: they are consumed by the
   canary scenarios (`sdks/canary/go` d-session-ensure / d-session-limit
   discover MaxActive through `Sessions.GetActive` — no other surface
   serves it), so "delete the surface" resolves to deleting the dead
   frontend contract, not the live SDK one. Frontend reconciliation
   itself is already owned by #1848's snapshot handshake + lease;
   consuming `/sessions/active` there would be redundant machinery.

## Tests (red-first)

- `usagestream/resync_flight_test.go`: initial connect fires nothing;
  silent death → redial seed fires exactly once; agreeing fold still
  fires (and the #1666 diff stays quiet); in-stream reseed fires. All
  RED pre-fix (seam absent), GREEN post-fix.
- `handlers/proxy_usage_resync_test.go`: StreamResynced runs a full
  D9/D10 flight on the user broker (authoritative busy rows); the cue is
  non-blocking and coalesced (one statusz fetch for 4 rapid cues);
  authority failure reads ok=false with no snapshot rows; and the
  full-stack row — REAL consumer + PRODUCTION usageBridge + real
  handler/broker: silent kill mid-turn → redial → idle correction AND
  flight both reach the subscriber.
- `middleware/tests/logging_test.go`: the session-registry pull logs
  nothing under the default config (RED pre-fix — Info was called).
- Frontend: contract test (9 pass), tsc typecheck clean, eslint clean;
  fixtures regenerated via the pkg/types generator (byte-identical).

## Test commands

- `go test ./api/internal/services/usagestream/ -count=1 -race`
- `go test ./api/internal/handlers/ -count=1` (full package, ~84s)
- `go test ./api/internal/middleware/... ./pkg/types/ ./api/internal/server/ -count=1`
- `golangci-lint run api/... pkg/types/` (0 issues)
- frontend: `npx vitest run src/api/contract.test.ts`, `npm run typecheck`, eslint on touched files

## Design choice (issue left open)

The issue offered "publish a resync (or trigger a per-workspace
snapshot flight)". Chosen: the SNAPSHOT FLIGHT, not a bare `resync`
event — a `resync` wipes browser busy state and forces a refetch round
trip per client, while the flight pushes authoritative rows server-side
(D10-safe: authority failure holds unknown) with zero client round
trips, reusing the #1848 machinery and its coalescing. The #1666
corrections keep handling the per-session deltas; the flight is the
superset heal for what the fold cannot compute.

## Residuals

- Half-open TCP death (no RST, no error) is still detected only by
  transport-level timeouts — a usage-leg staleness watchdog (the #1365
  browser-leg pattern) was deliberately NOT added: busy-but-quiet
  sessions legitimately emit nothing for minutes, and a false
  reconnect storm risks outweigh the gain; v0.40.2's lease bounds the
  damage. Follow-up candidate if field evidence shows half-open deaths.
- Usage/billing frames (MESSAGE_END) lost in a dead window are not
  replayed (the ABI stream has no replay-from-seq op); billing keeps
  deterministic idempotency keys so any future replay is safe.
- Ask 3's deploy-verification items (session-list rows carry status;
  log check excluding session-registry) are post-0.40.2 operator
  checklist items, verified here in code only.

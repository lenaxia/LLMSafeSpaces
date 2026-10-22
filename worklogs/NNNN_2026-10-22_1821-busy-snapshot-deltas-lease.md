# Worklog: #1821 — busy-snapshot handshake, EventID-ordered deltas, frontend busy lease (pieces 2-4)

**Date:** 2026-10-22
**Session:** mighty-harbor (implementation worker, wt-1821, branch feat/1821-busy-snapshot-deltas-lease; base rebased 874d975d → 5d745efa before work began).
**Status:** Complete

---

## Objective

Land #1821 §2 pieces 2-4 on top of piece 1's merged emission discipline (#1824): the busy-snapshot handshake on the user-stream connect path (D9/D10 `session.status.snapshot` flights with `snapshot_ok` semantics), EventID-ordered delta application client-side, and the frontend 90s busy lease with exactly-one authoritative recheck.

## Work Completed

### Piece 2 — backend busy snapshot (proxy_busy_snapshot.go)

- `fetchStatuszSessions` — the ok-typed statusz read (3s budget, 1 MB decode cap, bearer candidates, Active+PodIP gate). ok=false on ANY failure: an indicator authority must answer "unknown", never fail-open-to-empty the way the send-gating `GetAuthoritativeActiveSessions` correctly does for its own purpose.
- `emitSessionStatusSnapshot` — the D9/D10 flight modeled on `emitPendingInputRequests` (proxy_input.go:442): `session.status.snapshot_begin {snapshot_id}` → one `session.status.snapshot` frame `{data: {cursor, sessions: [rows]}}` → deferred `snapshot_complete {snapshot_ok}`. Cursor = `UserEventBroker.LastEventID(userID)` read BEFORE the authority fetch, so a delta published during the flight carries id > cursor and outranks the snapshot at commit (piece 3's ordering).
- Rows phrase-keyed via `sessionIndex.Register` (idempotent, LRU-cached); harness id additive as `internal_session_id`; unresolved sessions degrade to the harness id flagged `session_id_deprecated` (never worse than the pre-fix wire, mirroring `markDeprecatedSessionID`).
- Wired into `snapshotUserWorkspaces`'s Active-workspace loop as a second detached flight (same `context.WithoutCancel` + recover containment the input flight documents for the ctx -race).
- `POST /workspaces/:id/sessions/status-snapshot` (`RequestSessionStatusSnapshot`) — RequestInputSnapshot pattern: 202 now, flight lands on the user stream. Registered beside the input-snapshot route on idGroup.
- `UserEventBroker.LastEventID(userID)` — shard-locked `nextID-1`, per-user.

### Pieces 3+4 — frontend (SessionActivityProvider.tsx)

- `busySessions` values became `BusyEntry {workspaceId, confirmedAt, confirmEventId?}` — the lease lives with the state atomically instead of parallel refs (all 9 mutation sites updated: latch, idle/aborted/deleted clears, agent_died, phase wipe, reconnect/resync wipe, alias rekey, REST seed, contract-stream clear bridge reads only).
- Delta ordering: `lastSessionStatusIDRef` watermark; a session.status delta with `event_id ≤` watermark is skipped (stale replay overlap — idempotent across snapshot/replay overlap); snapshot commits advance the watermark to their cursor. Unordered events (event_id absent) always apply — never worse.
- Snapshot flights: `busyFlightsRef` per-workspace/per-flight staging (begin/snapshot/complete keyed by snapshot_id; orphan frames ignored by the input-snapshot flight rule). ok=true commits authoritatively: entries of the workspace with ordered confirm ≤ cursor AND absent from the busy rows clear; fresher deltas (> cursor) and unorderable entries (REST seeds, legacy frames) survive; busy rows latch/re-confirm (lease refresh). ok=false keeps state — the D10 "keep your state" rule.
- Lease sweep (15s tick, whileAway-sweep pattern): entries older than `BUSY_LEASE_MS = 90_000` (≈3×25s heartbeat, above the 60s liveness window) fire exactly ONE `sessionsApi.requestStatusSnapshot(wsId)` per workspace per window; the lease re-arms at trigger, so an unreachable authority holds the badge (unknown ≠ idle) and retries once per window — never hammers, never self-clears.
- Reconnect/resync keep the #1646/#1687 wipe-before-invalidate ordering; open busy flights die with the connection and the connect-path flights rebuild authoritatively. REST seed stays as the degraded fallback (stamped unorderable so a snapshot governs it later).

## Red-first evidence

Backend (pre-implementation run, all 5 red — connect window delivered no flight, deadline expiry):
`TestStreamUserEvents_BusySnapshotOnConnect_RestoresBusy / _AuthorityDown_HoldsUnknown / _AfterReplayGap_Resync / _Flight_DoesNotDuplicateDeltas / _Rows_PhraseKeyed` — each failed with "Expected value not to be nil" / `[]string{} != [busy idle busy]` / snapshot-count 0 within the 3s connect window (quoted in the commit message).
Frontend (pre-implementation run, 5/6 red): T0.2a (snapshot→busy: badge stayed "no"), T0.2c (stale replayed idle cleared a snapshot-confirmed busy), T0.2d (authoritative idle snapshot did not clear latched busy), T0.2e (lease expiry fired 0 rechecks), T0.2f (0 recheck attempts). T0.2b passed vacuously pre-fix (nothing cleared on ok=false then either) — it is the mutation guard pinning that the NEW commit path never wipes on a failed authority read.

## Key Decisions

- **statusz as the busy-snapshot authority** (not per-session ABI `GetSnapshot`): the connect path needs the workspace-wide set in one flight; statusz is the surface `GetAuthoritativeActiveSessions` already consults; `derived_busy = status != "idle"` matches that consumer's rule. The consumer's consult (`BusyComponents`) still governs the delta surface — unchanged (#1781's emission legs untouched).
- **Cursor read before fetch**: a delta published in (cursor-read, fetch-done) has id > cursor AND reflects state the fetch likely saw — keeping it at commit is idempotent overlap; reading the cursor after the fetch would let an older snapshot wrongly supersede a newer delta.
- **Lease in the state value, not parallel refs**: one source of truth; StrictMode-safe (no ref/state divergence); the alias rekey moves entries with their leases for free.
- **Exactly-one recheck per window via trigger-time re-arm**: bounded worst-case staleness ~L under total event-path failure (charter piece 4) with a down-authority retry cadence of one-per-L, not a loop.

## Verification

- `go test ./internal/handlers/` — full package green (79s), including the 5 new wire tests + endpoint test; `./internal/services/eventbroker/` + `./internal/services/usagestream/` green; `TestUserEventBroker_LastEventID` green.
- One pre-existing test adapted: `TestSnapshotUserWorkspaces_FansOutPendingForActiveWorkspaces` now matches input-flight markers by type (`recvTypeWithTimeout`) — the connect path legitimately fans out two concurrent flights per Active workspace now, so strict next-event sequencing no longer holds; the behavioral assertions are unchanged.
- Frontend: `vitest run` — 2382/2382 green (211 files) including the 6 new T0.2 tests; provider family 132/132.
- gofmt / `go vet` / `golangci-lint` (handlers + eventbroker): 0 issues. `tsc -b`: clean. `npm run lint`: 0 errors (6 pre-existing warnings in generated abi_pb files).

## Blockers

None.

# 1107 — #1646 frontend busy/session freeze after silent SSE death

Charter: the frontend froze busy/session state after a silent SSE stream
death (API pod restart during the v0.35.0 rollout) and stayed frozen
until app restart. Three-layer fix per the issue's shape: reconnect
reseed, stream liveness watchdog, focus/interval floor.

## Diagnosis (verified against source at f9a74a19)

1. **The reseed trigger was missing, not the reseed logic.**
   `SessionActivityProvider.onReconnect` correctly wipes
   `busySessions`/`seededRef`/`hungWorkspaces` and the `seedBusy()`
   busyDelta path correctly re-seeds on the next sessions-cache update
   (SessionActivityProvider.tsx:227-289, cache subscription :349-357).
   But `useUserEventStream`'s `onConnect` invalidated only
   `["workspaces"]` + `["workspace-status"]` — never `["sessions", …]`.
   After a silent death + forced reconnect, no sessions query ever
   updates, so: busy stays wiped (owner saw NOT-busy on a provably busy
   session), the sessions list rows stay last-known, and the D6 hung
   `getAlerts` re-seed (also gated behind the cleared `seededRef`) never
   fires either. The existing tests ("SSE reconnect re-seeds from REST"
   :666, "clears stale busy" :714) pass only because they hand-drive
   `qc.setQueryData` — production has no such trigger. That is the gap
   between the designed behavior and the wiring.

2. **Watchdog asymmetry.** The user stream already has the #1365
   liveness watchdog (60s event/heartbeat silence → forced reconnect,
   `LIVENESS_SILENCE_MS` in useUserEventStream.ts). The workspace
   stream (`useEventStream.ts`, ChatPage's carrier) had only the 35s
   read timeout — a proxy that keeps feeding bytes on a broker-dead
   connection never trips it, so ChatPage's fold/invalidations could
   freeze the same way. Server heartbeats verified:
   `api/internal/handlers/stream_user_events.go:24`
   (`heartbeatInterval = 25 * time.Second`) and the shared
   `heartbeatLoop` (:324) used by BOTH `StreamUserEvents` and the
   workspace `proxy_stream.go` (:103) — comment frames `":\n\n"`,
   parsed as keepalive evidence by `sseConnection.ts:147` (`onKeepalive`).

3. **No floor.** The sidebar sessions query had no `refetchInterval`
   (Sidebar.tsx:570-585); with zero SSE events and zero window focus
   the list never refetched. (Active-runs already polls 10s;
   ChatPage.tsx:206-211.)

## The fix (3 layers, surgical)

- **L1 reconnect reseed** — `useUserEventStream.ts` `onConnect`:
  `invalidateQueries({ queryKey: ["sessions"] })` on EVERY connect
  (first included, per the issue). Active (mounted) queries refetch →
  cache update → `seedBusy()` re-runs on the reconnect-cleared
  `seededRef`; inactive queries go stale and converge on next mount
  (invalidation only refetches active queries — verified behavior;
  pin A asserts the mounted case explicitly, its probe comment records
  the inactive-case contract).
- **L2 watchdog parity** — `useEventStream.ts`: lastAlive tracked on
  events + `onKeepalive`; >60s silence → `conn.reconnect()`; 10s check
  interval + `visibilitychange`. N=60s documented in-code: 2× the 25s
  server heartbeat + margin — parity with the user stream's #1365
  constant so both streams share one convergence window.
- **L3 floor** — Sidebar sessions query: `refetchInterval: 60_000` +
  explicit `refetchOnWindowFocus: true`. Deliberately NOT extending
  `seedBusy` past its seeded-gate: an add-only busy pass on every
  refetch would re-add busy from stale REST after a live SSE idle
  cleared it (the sticky-busy race the seed-once design exists to
  avoid); busy convergence is owned by L1+L2, the floor keeps the
  list/unread reconcile bounded (reconcileUnread already runs on every
  cache update and is add-only + cleared-release by design).

## Pins (red-first, verified red at 741d0f53 pre-fix)

- `SessionActivityProvider.sseStaleness.test.tsx` — the issue repro at
  provider+stream integration level (REAL `useUserEventStream` against
  a fetch-mocked stream; no captured-callback shortcut): live connect →
  silent death (reader never resolves; no error, no close) → REST flips
  to busy → advance 70s → assert `isSessionBusy` converges. RED pre-fix
  (`expected 'no' to be 'yes'`), GREEN post-fix.
- `useEventStream.test.ts` — liveness watchdog describe: bytes-but-no-
  heartbeats → forced reconnect (RED pre-fix: 1 connect forever);
  heartbeats flowing → no flap over 120s; onReconnect fires on forced
  reconnect, never on first connect.
- `Sidebar.refetchFloor.test.tsx` — 60s floor: getSessions called again
  after 61s with no events/focus (RED pre-fix: exactly 1 call).

Clobber-guards kept green (existing pins, unmodified): reconnect
re-seeds via cache update (:666), reconnect clears stale busy (:714),
unread preserved through reconnect (:758), SSE busy survives refetch
returning idle (:490), pendingActions NOT wiped on reconnect (:1540),
alerts-file hung-recovery matrix (re-seed from unresolved, latch
release, resync clear). Known adjacent latent gap disclosed, NOT
touched: the `resync` handler wipes `hungWorkspaces` without clearing
`seededRef`, so its comment ("the gated seed re-adds genuine hangs") is
aspirational — separate signal class (broker backpressure on a live
connection), filed for the orchestrator to route, not smuggled into
this diff.

## Runs (measured; pins red at 741d0f53, fix at 51844b6e, branch fix/1646-sse-staleness from main f9a74a19)

- Target pins pre-fix: `npx vitest run <3 pin files>` — 4 failed / 9
  passed (the 4 = the new pins; existing 9 green) at commit 741d0f53.
- Target pins post-fix: 13/13 passed (187-305ms per file).
- Neighborhood suites (providers/hooks/layout/ChatPage):
  52 files / 596 tests passed.
- Full frontend suite: **184 files / 2028 tests passed**
  (`npx vitest run`, ~4 min).
- `npm run typecheck` (tsc --noEmit): 0 errors.
- `npx eslint` on all 6 touched files: clean.

## Review round 1 (PR #1656 → recreated as #1662 after the review-bot
permission incident; see below) — REQUEST_CHANGES, one blocking finding

The finding (verified in server source before fixing): the busy wipe +
seed-gate clear rode the `lastEventIDRef.current !== null` gate — but
`lastEventIDRef` is only set by id-carrying events, and the server
writes snapshot/anti-entropy/resync events with EventID 0 and NO `id:`
line (stream_user_events.go:154-169 "Snapshot event — no id: line",
:290, :351-363). A quiet stream can reconnect forever without setting
it → the reconnect reset was skipped → a mount-seeded busy that went
idle during a dead window could never clear (add-only seedBusy) — the
sticky-busy mirror of the reported freeze.

Fix: `useUserEventStream` now tracks `hasConnectedOnce` locally (same
shape as `useEventStream`); ANY post-first connect runs the full reset
(invalidations + onReconnect). `lastEventIDRef` keeps its actual job:
the Last-Event-ID replay header. Red-first pin added BEFORE the fix:
"clears mount-seeded busy when a reconnect happens without any
id-carrying event" — red at 9e93cb38 (`expected 'yes' to be 'no'`),
green after. Non-blocking notes: e2e infeasibility accepted (repo's
Playwright layer can't stream SSE; session-activity e2e block already
`.fixme` with precedent #1365 `7321a2be`); watchdog-cadence-during-
outage pin deliberately not added — the attempt count under fake timers
is jitter-dependent (`jitteredDelay` = base×[0.5,1.5]) so any tight
bound is flaky; the cadence is floored by the 10s check period by
construction. Sidebar floor-test act() stderr warnings noted, left
(repo fake-timer pattern is act-free).

Post-r1 runs: sseStaleness+hooks+provider+alerts targeted 121/121;
full frontend suite **2029/2029**; tsc + eslint clean.

## Incidents in-flight (honest log)

1. First vitest run deadlocked (240s timeout): I wrapped
   `advanceTimersByTimeAsync` in `act(async …)` — the repo's
   established fake-timer pattern is bare advancement. Restructured the
   pins to follow it; also folded the workspace-watchdog pins into the
   pre-existing `useEventStream.test.ts` instead of a duplicate
   parallel harness file.
2. PR-review workflow failed fleet-wide 17:31-18:10Z
   (`agentic-actor-coder[bot]` permission: none — app-bot authors
   can't pass the reviewer's collaborator assert). Owner closed #1656
   and recreated #1662 under the user identity; review round 1 then
   delivered normally.
3. CI race-detector flake: TestUpload_ConcurrentStorm… raced
   (api/internal/handlers — zero .go files in this PR). Green on the
   recreated PR's own rerun; also green on ≥3 other branches that day.
   Local repro not feasible: multi-GB race build vs 15G PVC at 81%
   (sandbox-writable dirs only); ran standing `go clean` + cleared
   stale /tmp/go-build* after the ENOSPC build failure.

## Files (r1 additions marked)

- `frontend/src/hooks/useUserEventStream.ts` (L1; r1: hasConnectedOnce gate)
- `frontend/src/hooks/useEventStream.ts` (L2)
- `frontend/src/components/layout/Sidebar.tsx` (L3)
- pins: `frontend/src/providers/SessionActivityProvider.sseStaleness.test.tsx`
  (new; r1: +event-less-reconnect sticky-busy pin), `frontend/src/hooks/useEventStream.test.ts` (watchdog
  describe added), `frontend/src/components/layout/Sidebar.refetchFloor.test.tsx`
  (new)

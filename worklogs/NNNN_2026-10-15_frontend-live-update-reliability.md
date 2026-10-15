# Worklog: #1687 — frontend live-update reliability (stuck busy badge, queue-held sends, mid-turn stream wipe)

**Date:** 2026-10-15
**Session:** w28 — turn the 4 red-first pins on `poc/sse-live-update-red-pins` (commit e1ce3c2f) green without weakening the guard; client lane only (#1666 out of scope).
**Status:** Complete

---

## Objective

Close #1687: four owner-observed live-update failures — (1) a `resync` on the user stream does not reconcile a latched busy badge, (2) the contract stream's SESSION_STATUS IDLE never clears the provider's busy map so (3) a stuck badge silently reroutes sends into the queue, (4) a mid-turn contract-stream reconnect wipes the live streaming buffer (garbled/partial responses) — plus the documented `sseConnection` header-capture follow-up (mount-frozen `Last-Event-ID`).

---

## Work Completed

### Pin 1 — resync reconciliation (`SessionActivityProvider.tsx`)

The broker's `{"type":"resync"}` is admitted event loss on a HEALTHY connection (no `onReconnect` fires), so the provider's resync handler now runs the same reconciliation as `onReconnect` via a new shared module-scope `wipeBusyForEventLoss` core (wipeAt stamp + seed-gate reopen + busy wipe), then invalidates `["sessions"]` AFTER the stamp (the #1646 r5 birth-ordering — a fetch born before the stamp is refused by seedBusy's guard). The hook's resync branch intentionally does NOT invalidate `["sessions"]`: it runs before `onEventRef` (the provider wipe), so its fetch would be born pre-wipe and refused by the birth guard — the provider-side invalidation is the correctly-ordered one.

### Pin 2 — contract-idle bridge (`SessionActivityProvider.tsx`, `ChatPage.tsx`)

New `clearSessionBusy(workspaceId, sessionId)` on the provider context (busy-map delete + cached sessions-row flip to "idle" + hung-alert resolution, mirroring the user-stream idle path), exposed as `useClearSessionBusy()` and called from ChatPage's SESSION_STATUS IDLE branch. One stream reporting the turn's end is enough.

### Pin 3 — fold-gated stream wipe (`ChatPage.tsx`)

`reconcileOnIdle`'s `setSseStreamParts([])` and optimistic trim no longer fire on "history nonempty": they are gated on the viewed session NOT being mid-turn per the fold (`status === BUSY`), read from `foldSessionsRef` (kept current by `onSnapshot` — which lands before the fold's state publish commits — and by an effect on each fold publish; a render closure would be stale because the stream's `onEvent` fires before React commits). The IDLE handler passes `turnEnded=true`: the event itself is authoritative over a not-yet-committed fold, which is what keeps the end-of-turn guard green. The reconnect history refetch is untouched.

**Design input from existing green tests:** the gate is "fold says BUSY blocks the wipe", not "only idle allows it" — `ChatPage.sse-reconnect-duplicate.test.tsx` (fold entry ABSENT + history owns the turn → the buffer must clear) and `optimistic-survival` pins require the legacy catch-up rule when the fold has no entry. The pin's own header defines mid-turn as "the fold says BUSY", so both coexist.

### Pin 4 (compound) — queue-held sends

Green automatically via Pin 2: the stale busy no longer holds `handleSend`'s `isSessionBusy || streaming || queue-pending` gate.

### Header factory (`sseConnection.ts`, `useUserEventStream.ts`)

`SSEConnectionConfig.headers` accepts `Record | () => Record`; `connect()` re-evaluates it per connect (initial + every internal reconnect). `useUserEventStream` now passes `buildHeaders` (the function), so a reconnect sends the CURRENT `Last-Event-ID` and the server-side replay (stream_user_events.go:92) actually fires. The stale "#1646 r2 known follow-up" NOTE in the hook is rewritten to record the resolution.

### Review-loop additions (round 1 blockers)

- **Unit coverage for the factory** (was zero): `sseConnection.test.ts` gains a `#1687 header factory` describe (factory re-evaluated per connect; static-object back-compat; mid-read stream end reconnects with the factory's REFRESHED replay position) and `useUserEventStream.test.tsx` a hook-level pin (first connect sends no `Last-Event-ID`; the reconnect sends `"7"` — verified red when the factory is reverted to `buildHeaders()`).
- **E2E coverage** (`tests/e2e/live-update-1687.spec.ts`, all-mocked backend, deferred-SSE harness): `resync clears a mount-seeded busy badge…` (no reconnect can mask the heal — later connections are held; the sessions refetch after the resync is asserted), `contract SESSION_STATUS IDLE clears a user-stream-latched badge; the next send goes DIRECT` (POST /prompt + optimistic bubble; zero queue POSTs; the user stream's idle frame is never delivered), and `mid-turn contract reconnect keeps the live streaming buffer…` (a 10s sampled invariant: the streamed text never disappears across the reconnect while the fold says BUSY — the reconnect snapshot deliberately carries EMPTY inFlightParts so the client buffer is the only copy; then the real IDLE clears it to exactly one history-rendered copy). Mutation-checked: commenting out the bridge or the resync wipe makes the corresponding e2e fail.
- **Worklog** — this file.

### Harness adaptations (no pin weakened)

Provider-mocking ChatPage tests gain a `useClearSessionBusy` stub (the mock must mirror the module surface ChatPage consumes); `hookcount` 72 → 75 (its own instruction for intentional hook additions: +`useClearSessionBusy`, +fold ref, +fold-sync effect); `contractIdleBridge` pin gets one compile-only fix (`?.onEvent?.`, TS2722 that pre-existed on the pins branch and blocked the required clean `tsc --noEmit`).

---

## Key Decisions

- **Resync invalidation lives in the provider, not the hook** — ordering (#1646 r5): the wipe must stamp before the refetch dispatches; the hook's resync branch runs before `onEventRef` (the provider wipe), so an invalidation there would produce a birth-guard-refused seed (a genuinely-busy session would lose its badge).
- **Gate = "fold says BUSY blocks", absent entry keeps the legacy rule** — forced by the pre-existing `sse-reconnect-duplicate` regression (fold-absent + history owns the turn → clear must still happen); matches the pin's own mid-turn definition. A strict "only-idle-wipes" gate would have traded one green pin for another's failure.
- **`turnEnded` explicit param over reading the fold in the IDLE handler** — the real `useContractStream` fires `onEvent` before React commits the fold update, so the render-closure fold can still say BUSY at handler time; the event is the authoritative signal (also what the guard test exercises).
- **Module-scope `wipeBusyForEventLoss`** — DRY for reconnect/resync (the issue itself demands "the same reconciliation"), and it sidesteps a `react-hooks/purity` lint error that flagged `Date.now()` at the resync handler's nesting depth while leaving the identical `onReconnect` line alone (rule heuristic; not worth fighting).
- **E2E deferred-SSE harness** — Playwright `route.fulfill` delivers complete bodies, so mount-time races (contract idle landing before the user-stream busy latch) made naive bodies flaky; connections are held until the test fires them, making every ordering deterministic.

---

## Blockers

None. Review round 1 also documented two validated residual lost-IDLE corners (turn-end-during-gap never wipes the buffer — reconciles at the next event; the auto-abort's backup reconcile is now fold-gated like every other caller) as follow-up-worthy, not blocking.

---

## Tests Run

- Pins red-first verified: base e1ce3c2f = `4 failed | 1 passed`; after implementation = `5 passed` (`npx vitest run src/providers/SessionActivityProvider.resyncBusy.test.tsx src/pages/ChatPage.contractIdleBridge.test.tsx src/pages/ChatPage.midturnReconnect.test.tsx`).
- Full frontend suite (`npm ci` then `npx vitest run`): **191 files / 2104 tests passed** (plus the new unit tests: 35 green across the two touched files).
- `npm run typecheck` clean; `npm run lint` 0 errors (6 pre-existing unused-disable warnings, identical to the pins branch).
- `npx playwright test tests/e2e/live-update-1687.spec.ts`: **3 passed** (after `npm ci`); mutation checks: bridge removed → DIRECT e2e fails; resync wipe removed → resync e2e fails.
- Factory revert check: `headers: buildHeaders()` → hook-level replay test fails (red-on-revert confirmed), restored to factory.

---

## Next Steps

- Review round 2 on PR #1689 (round-1 blockers addressed: factory unit tests, e2e suite, this worklog).
- Follow-up candidates noted in review: the two lost-IDLE residual corners; #1685 (15-min busy nudge) likely retirable now the root causes landed.

---

## Files Modified

- `frontend/src/providers/SessionActivityProvider.tsx` — resync reconciliation + `clearSessionBusy` + shared wipe core
- `frontend/src/pages/ChatPage.tsx` — contract-idle bridge call, fold-gated `reconcileOnIdle`, `foldSessionsRef`
- `frontend/src/lib/sseConnection.ts` — header factory
- `frontend/src/hooks/useUserEventStream.ts` — pass the factory; update the r2 NOTE
- `frontend/src/lib/sseConnection.test.ts`, `frontend/src/hooks/useUserEventStream.test.tsx` — factory coverage
- `frontend/tests/e2e/live-update-1687.spec.ts` — new e2e (3 workflows)
- `frontend/src/pages/ChatPage.*.test.tsx` — `useClearSessionBusy` mock stubs; `hookcount` 75; `contractIdleBridge` compile-only `?.` fix
- `worklogs/NNNN_2026-10-15_frontend-live-update-reliability.md` — this file

# Worklog: #1720 — chat auto-tail during streaming fights user scroll (snap-back loop)

**Date:** 2026-10-17
**Session:** Frontend lane, issue #1720 (owner dogfood report on prod v0.38.0: timeline keeps snapping to the bottom during streaming after the user scrolls away — repeatedly, intermittently).
**Status:** Complete (PR pending review)

## Objective

Implement the owner's 4-rule spec (normative, in the issue):

1. any genuine user scroll away (wheel/touch/keyboard/scrollbar — NOT programmatic) breaks tailing immediately and durably — zero snap-back while streaming;
2. tailing continues until the user scrolls away OR streaming stops;
3. resume is EXPLICIT — a "resume tailing" control appears when the tail is disarmed mid-stream; tailing must NEVER silently re-arm, in particular NO near-bottom re-arm while a stream is active (that heuristic is the loop);
4. our own programmatic scrolls are invisible to the user-scroll detector and never re-arm tail.

## Root cause (verified in tree — all three suspected mechanics were live)

Controller: `frontend/src/components/chat/MessageList.tsx` (the only chat timeline scroll controller; ChatView/ChatPage only pass props).

1. **Near-bottom re-arm from programmatic snaps (suspect #1 + #3, the core loop).** `checkIfAtBottom` — the `scroll` event listener — recomputed `stickToBottom = (nearBottom)` from EVERY scroll event, with no notion of event origin. Our own snaps (`el.scrollTop = scrollHeight` in the MutationObserver rAF, the layout-effect append snap, and `scrollToBottom`) fire real `scroll` events; each lands a near-bottom read and re-arms the tail. Snap → near-bottom event → re-arm → next token batch snaps again. Interleaving-dependent (scroll-event delivery can lag/coalesce behind the snap), which is why the owner saw it intermittently. Additionally, a user who merely drifts back within the 60px threshold mid-stream silently re-armed the tail — same loop, user-driven variant.
2. **Unconditional snap on the streaming transition (silent re-arm).** `useEffect(() => { if (streaming) scrollToBottom(); }, [streaming, …])` — fires on mount AND every streaming flip, force-snapping and re-arming even when the user had scrolled away. Any new/subtask stream start yanked the viewport.
3. Observer/layout-effect snaps themselves were already tail-gated (pre-existing, pinned by the #1720-era tests) — NOT the culprit; the re-arm path above defeats the gate.

## Work Completed

### (a) RED-first pins — commit `d5fa3257` (+ refinements `954ff9cd`)

`MessageList.test.tsx` (#1720 describe block):

- **RED (deterministic, jsdom):** near-bottom scroll mid-stream must not re-arm — on HEAD the next token snapped 900→1000, reproducing the loop exactly; stream start while scrolled away must not snap/re-arm — HEAD snapped 400→1000; "re-arms only via the explicit control" — on HEAD the silent re-arm already happened and the affordance VANISHED (getByText("Resume tailing") not found).
- **Green pins (invariant/convention, both sides):** programmatic snap events never disarm an armed tail; the guard never swallows the next genuine user scroll; IDLE near-bottom re-arm retained (scopes R3's "while a stream is active"); stream-end settle (disarmed → label flips to "Jump to bottom", position kept; tailing → stays at bottom, control hidden).

`MessageList.sourcepin.test.ts` (fs-read, precedent `styles/index.test.ts`) — S1 observer gates callback AND rAF on the tail flag (pre-existing, green); S2 streaming-transition effect gates `scrollToBottom` on the tail flag (RED pre-fix); S3 handler carries the programmatic guard branch (RED); S4 exactly ONE raw `scrollTop` assignment lives inside the guarded helper with ≥4 call sites routed through it (RED — HEAD had 4 raw writes); S5 the handler's only arm site is the `!streamingRef.current && !stickToBottom.current` idle branch (RED).

`MessageList.contrast.test.tsx` — resume control ≥4.5:1 in both schemes via the #1716/#1718 computed-contrast harness (pin; the control already passed — `bg-primary`/`text-primary-foreground` is the audited pair).

### (b) The fix — commit `28f0c74d`

- `checkIfAtBottom` may only DISARM while `streamingRef.current`; the near-bottom ARM is gated to idle-only (`!streamingRef.current && !stickToBottom.current`). Mid-stream, the ONLY arm is the explicit control.
- All programmatic scrolls route through `setProgrammaticScrollTop(el, target)`: no-op assignments (already at target — the every-token-while-pinned case) mark nothing and fire no event; real assignments set a guard consumed by the handler, self-clearing on the next rAF as a backstop. The handler's guard branch (R4) makes our snaps invisible to intent detection.
- The streaming transition continues an ARMED tail only (`streaming && stickToBottom.current`).
- The MutationObserver rAF and both layout-effect branches call the helper (S4's one-write shape).

### (c) E2E — commit `ad7b60ab` (approx; see git log)

`tests/e2e/chat-scroll-tail.spec.ts` — incremental SSE via a fetch monkey-patch (both streams are `fetch`+ReadableStream per `sseConnection.ts`; `route.fulfill` cannot stream chunks — the known limitation documented in session-activity.spec.ts). Per-connection seeded IDLE snapshots + broadcast emitters make it StrictMode double-mount safe.

- **Acceptance test:** tailing follows batches → wheel scroll-away (6×−400) → 5 further delta batches → viewport holds ±4px → "Resume tailing" visible → click → at bottom and following again → PART_END + IDLE snapshot + user-stream idle → control hidden, settled at bottom.
- **Silent re-arm test:** scrolled away while idle → stream starts + deltas → viewport holds ±4px, control flips to "Resume tailing". **Verified RED on the pre-fix controller** (snapped on the streaming flip; deterministic).
- The loop-interleaving acceptance row is race-dependent in a real browser (matches the owner's "intermittent") — on the pre-fix controller it passed sometimes; the deterministic red-first guarantee lives in the jsdom pins. Stated honestly here and in the PR.

## Pin-class inventory (per the issue's acceptance clause)

| Pin | Class |
|---|---|
| near-bottom mid-stream re-arm (snap 900→1000) | **red-first, deterministic jsdom** |
| stream-start snap/re-arm while scrolled away | **red-first, deterministic jsdom** |
| explicit-only resume (affordance vanishes on HEAD) | **red-first, deterministic jsdom** |
| S2/S3/S4/S5 source-shape | **source pins** (RED pre-fix) |
| S1 observer gating | source pin (pre-existing behavior, green) |
| guard semantics (no disarm-armed / no swallow-next) | invariant pin (green both sides) |
| idle near-bottom re-arm | convention pin (green both sides) |
| stream-end settle ×2 | invariant pin (green both sides) |
| resume-control contrast light/dark | WCAG pin (#1716/#1718 harness) |
| e2e acceptance (scroll-away holds / resume / end) | e2e (intermittently red pre-fix — race) |
| e2e stream-start-while-scrolled-away | e2e (**deterministically red pre-fix**) |

## Verification

- Units: full frontend suite **2180 passed / 197 files** (50 in the three MessageList files); `tsc --noEmit` clean; eslint clean on touched files.
- E2E: `chat-scroll-tail.spec.ts` 5/5 consecutive green runs; neighbors green — streaming (3), chat, history-rendering, live-update-1687 (7 skipped pre-existing `.fixme`), session-activity (5 skipped pre-existing), sidebar-gesture/app-shell-viewport/sidebar-kebab-viewport (10).
- No backend/API changes; no existing scroll-behavior spec regressions (the suite had no prior scroll/tail e2e specs — grep-verified).

## Key Decisions

- The loop is broken STRUCTURALLY by forbidding the arm while streaming (no read of position can re-arm), rather than by trying to classify scroll-event origins perfectly — the guard flag is defense-in-depth for R4, not the load-bearing fix. This is why the fix cannot re-regress into the loop via a new interleaving.
- No-op assignments mark nothing: a lingering mark from an event-less assignment (the pinned-at-bottom every-token case) would swallow the next genuine user scroll — found via the jsdom red run, real hazard in browsers too.
- Idle near-bottom re-arm kept (existing convention; new pin) — R3 scopes the ban to active streams; banning it idle-side would regress the "scroll back down, then send" flow.
- E2E SSE via fetch patch rather than EventSource stubbing: matches the real transport (both streams are fetch+ReadableStream).

## Files Modified

- `frontend/src/components/chat/MessageList.tsx` (the fix)
- `frontend/src/components/chat/MessageList.test.tsx`, `MessageList.sourcepin.test.ts`, `MessageList.contrast.test.tsx` (new pins)
- `frontend/tests/e2e/chat-scroll-tail.spec.ts` (new)
- `CHANGELOG.md`, this worklog

## Rework r2 (PR #1723 r1 review @ 387b88d — one finding, GO with exactly the reviewer's spec)

**Finding (reproduced by both review passes, and by me):** issue #1720's acceptance "click → jumps to bottom and follows again" fails in the disarmed-mid-stream composition. The observer effect early-returned on `!stickToBottom.current`, capturing the armed state at effect-run time — a stream that STARTS while the tail is disarmed never attaches the follow observer, so the explicit resume click (a ref flip; no effect re-runs) jumped but subsequent batches were not followed, and the affordance vanished on the next batch arrival (viewport drifted off-bottom, pill reappearing). My r1 tests missed it because every resume test used the armed-at-stream-start composition (observer attached at the flip).

**Fix (commit `6d692348`):** the follow observer now lives for the whole stream — attachment depends only on `streaming` and the scroll container existing, never on the armed flag; follow intent stays gated per-callback/per-rAF (S1 pins; the S2/S5 arm-gating semantics are untouched: no silent re-arm, near-bottom arm idle-only). Also keyed the effect on `hasScrollContainer`: on an empty session the streaming flip fires while the empty state renders no scroll container (el null → never attached); the effect re-attaches when the log mounts so an armed tail follows the bubble's growth.

**r2 pins (commit `d25728ab`, RED on 387b88d):**
- jsdom: disarmed-at-stream-start → resume click → batches follow (1400, 1800 growth tracked; 387b88d stays at the jump position) — **red-first, deterministic**;
- jsdom: empty session — log mounts after the streaming flip → armed tail follows bubble growth — **red-first, deterministic** (same attachment-gap family);
- source pin **S6**: observer effect has no armed-state early return, gates only on `!streaming`, deps include `hasScrollContainer`;
- green guard pin: while disarmed, batches still never scroll (per-callback gate holds — the loop fix's semantics are unchanged);
- e2e (commit `b9a6e5a3`): the stream-start-while-scrolled-away spec now clicks resume and asserts the next batches arrive at-bottom — **verified RED on 387b88d** (both attempt and retry) at the follow-again assertion.

**Verification (r2 tree):** units **2184 passed / 197 files**; `tsc --noEmit` clean; eslint clean (touched files); `chat-scroll-tail.spec.ts` 3/3 consecutive green; neighbors green (streaming, chat). Commits: red pins `d25728ab` → fix `6d692348` → e2e `b9a6e5a3`.

## Rework r3 (PR #1723 r2 review @ 64fbc4b2 — one finding, GO with exactly the reviewer's spec)

**Finding (reproduced):** the r1 loop survived in the **empty-at-mount** composition. The scroll-intent listener effect ran once on mount with stable deps (`[checkIfAtBottom]`) — mounting in the empty state (fresh session, no `role="log"`) meant it early-returned on a null element and never re-ran when the log appeared. Scroll-away was invisible for the rest of the stream: no disarm, no affordance, every batch snapped. Mirrors the r2 observer gap on the listener side — both effects need the element and both must key on its existence.

**Fix (commit `0f6dd2f8`):** `hasScrollContainer` (declaration moved up, shared) added to the listener effect deps — the intent listener attaches when the scroll container comes into existence. No semantics change for the always-mounted case (flag constant → effect runs once).

**r3 pins (commit `9037dc57`, RED on 64fbc4b2):**
- jsdom: empty-mount → stream starts → log mounts → armed follows → user scroll-away DISARMS + affordance appears → next batch does not snap → explicit resume follows (on 64fbc4b2 the pill never appears) — **red-first, deterministic**;
- source pin **S7**: listener effect deps `[checkIfAtBottom, hasScrollContainer]` (mirror of S6);
- e2e (commit `43d81a8d`): fresh-session row — mount empty (asserted no log) → stream starts → log appears → grow past viewport → wheel away → holds across further batches + "Resume tailing" visible → click → follows again. **Verified RED on 64fbc4b2** at the holds assertion (attempt + retry).

**Verification (r3 tree):** units **2186 passed / 197 files**; `tsc --noEmit` clean; eslint clean (touched files); `chat-scroll-tail.spec.ts` 3/3 × 3 consecutive green runs. Commits: red pins `9037dc57` → fix `0f6dd2f8` → e2e `43d81a8d`.

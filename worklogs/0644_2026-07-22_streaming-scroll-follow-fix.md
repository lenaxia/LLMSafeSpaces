# Worklog: Fix — streaming yanks the user back to the bottom on scroll-up

**Date:** 2026-07-22
**Session:** Fix a UX bug where, during message streaming, scrolling up to read earlier content immediately pulled the viewport back to the bottom, trapping the user at the tail.
**Status:** Complete

---

## Objective

During streaming the chat transcript auto-follows the tail. The reported bug: a user who scrolled up to read earlier content while waiting for a response was repeatedly yanked back to the bottom, making earlier content effectively unreadable mid-stream. Find the root cause and fix it (TDD).

---

## Work Completed

### Root cause

`frontend/src/components/chat/MessageList.tsx` — `checkIfAtBottom` (the `scroll` handler) deferred the `stickToBottom.current` update into a `requestAnimationFrame`. During streaming, the `MutationObserver` effect auto-scrolls to the bottom (`el.scrollTop = el.scrollHeight`) on every token while `stickToBottom` is `true`. Because the user-intent update was deferred to a rAF, a token whose observer rAF was registered before the user's scroll rAF ran first: it re-scrolled to the bottom, so by the time the deferred handler evaluated `scrollTop` it read an at-bottom position and kept `stickToBottom=true`. With tokens arriving every few milliseconds, this happened on essentially every scroll attempt — `stickToBottom` was stuck at `true` for the whole stream and the user could never escape the tail.

The defining property of the bug: the user-intent signal (`stickToBottom`) was derived from a scroll position that the component's own auto-scroll had just corrupted, observed one frame too late.

### Fix

`MessageList.tsx:42-64` — update `stickToBottom.current` **synchronously** in the scroll handler (read `scrollTop`/`scrollHeight`/`clientHeight` at the moment of the scroll event), before any later mutation's observer callback can act on a stale value. The `MutationObserver` callback already re-checks `stickToBottom.current` before auto-scrolling, so once the user scrolls up the very next token's observer sees `stickToBottom=false` and leaves the viewport alone. The React state update (`setShowJumpButton`, which drives the re-render / jump-button visibility) stays behind a rAF to keep coalescing rapid scroll events — only the intent ref needed to be synchronous.

Follow behaviour is unchanged: while the user stays pinned at the bottom, the observer still auto-scrolls on every token; the "Resume tailing" button re-pins on click.

### Tests (TDD — written first, observed failing on the unpatched code, then passing)

`frontend/src/components/chat/MessageList.test.tsx` — new `describe("streaming scroll-follow ...")` block, 3 tests:

1. **Regression (the bug):** pinned at bottom → a token mutates the DOM (observer rAF registered first) → user scrolls up → after flushing rAF timers, `scrollTop` stays at the user's position (400), NOT yanked to 1000. Verified **failing** on the unpatched code (`expected 1000 to be 400`) and **passing** after the fix.
2. **Follow mode preserved:** while pinned at the bottom, a token mutation still auto-scrolls to the bottom (1000).
3. **Re-pin via button:** after scrolling up, clicking "Resume tailing" re-enables follow so the next token auto-scrolls to the bottom.

The regression test is deterministic because (validated via probes) jsdom runs `MutationObserver` callbacks as microtasks and `requestAnimationFrame` as timers, so registering the observer rAF before the user's scroll rAF reproduces the real-world race ordering exactly.

---

## Key Decisions

1. **Synchronous intent, deferred render.** The minimal root-cause fix is moving one assignment out of the rAF. Driving user-intent off input events (wheel/touch/keydown) was considered and rejected as over-engineering: it adds cross-input-method surface area to solve a residual timing window that the synchronous read already eliminates for the reported symptom, and the existing scroll-position signal is sufficient once it isn't deferred.

2. **No new abstraction.** Per Rule 12 (containment before abstraction) and Rule 4 (not over-engineered): the change is a one-line semantic move inside the existing handler. No new hook, observer, or interface.

3. **Comment retained.** The `checkIfAtBottom` comment documents a non-obvious concurrency invariant (why the intent read must be synchronous vs deferred). This matches the file's existing convention (the scroll-anchoring block at `:76-83`) and prevents a future refactor from silently re-deferring it and reintroducing the bug.

---

## Blockers

None.

---

## Tests Run

```
# Regression test against the UNPATCHED code (confirmed it fails first)
cd frontend && npx vitest run src/components/chat/MessageList.test.tsx -t "streaming scroll-follow"
  × does not yank the user back to the bottom ...   expected 1000 to be 400   (1 failed, 2 passed)

# After the fix — full MessageList suite
cd frontend && npx vitest run src/components/chat/MessageList.test.tsx
  Tests  35 passed (35)     # 32 pre-existing + 3 new

# Whole chat component tree
cd frontend && npx vitest run src/components/chat
  Test Files  18 passed (18) | Tests  302 passed (302)

# Lint + typecheck (changed files)
cd frontend && npx eslint src/components/chat/MessageList.tsx src/components/chat/MessageList.test.tsx   # clean
cd frontend && npx tsc --noEmit                                                                          # clean
```

---

## Next Steps

1. Open a PR (`fix/streaming-scroll-follow`) per the branch-and-PR workflow; iterate on the automated reviewer until APPROVE, then squash-merge.
2. Optional: add a Playwright e2e in `frontend/tests/e2e/` that streams tokens, scrolls up mid-stream, and asserts the viewport is not pulled back — exercises the real browser event loop (the unit test covers the logic; the e2e would cover browser-native scroll/rAF ordering).

---

## Files Modified

- `frontend/src/components/chat/MessageList.tsx` — `checkIfAtBottom` now records `stickToBottom.current` synchronously instead of in a deferred rAF (root-cause fix).
- `frontend/src/components/chat/MessageList.test.tsx` — added `streaming scroll-follow` regression suite (3 tests) + `act`/`fireEvent` imports.

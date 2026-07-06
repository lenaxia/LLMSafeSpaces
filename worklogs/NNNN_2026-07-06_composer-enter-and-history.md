# Worklog: Composer Enter/Ctrl+Enter behavior + user-message history navigation

**Date:** 2026-07-06
**Session:** Added desktop send-key behavior (Enter=newline, Ctrl/Cmd+Enter=send), mobile send-key behavior (button-only), IME composition guards, send-button tooltip, a11y aria-label, and opencode-TUI-style Up/Arrow history navigation through loaded user messages.
**Status:** Complete

---

## Objective

Implement two related chat-composer features requested by the user:

1. **Feature A — Send-key behavior:** Desktop Enter should add a newline (not send); Ctrl+Enter (and Cmd+Enter on Mac) should send. The send button should advertise the active shortcut on hover (desktop only). Mobile Enter should add a newline and the send button is the only way to send.
2. **Feature B — User-message history navigation:** When history is loaded, the user should be able to walk back through previous user messages with Up (when the cursor is on the first line) and forward with Down (when on the last line), replicating the opencode TUI behavior.

---

## Work Completed

### Investigation
- Pulled latest `main` (9715832e..871bb627).
- Read `README-LLM.md` (rules, conventions, worklog format).
- Cloned `github.com/sst/opencode` to `/tmp/opencode-repo` and studied the TUI composer behavior: `packages/tui/src/prompt/history.tsx` (ring store), `packages/tui/src/component/prompt/index.tsx:859-925` (cursor-gated Up/Down), `packages/tui/src/config/keybind.ts:153,163-164,199-200` (default keybindings).

### Feature A — Send-key behavior
- `frontend/src/components/chat/Composer.tsx`: rewrote `handleKeyDown`. New behavior matrix:
  - Desktop, `sendOnEnter=false` (new default): Enter=newline, Ctrl/Cmd+Enter=send, Shift+Enter=newline.
  - Desktop, `sendOnEnter=true` (legacy opt-in): Enter=send, Shift+Enter=newline, Ctrl/Cmd+Enter=send.
  - Mobile (always, setting ignored): Enter=newline, button-only send.
- IME composition guard at the top of `handleKeyDown`: `e.nativeEvent.isComposing || e.keyCode === 229` returns early so Enter finalizes the CJK candidate instead of sending.
- Send button: added `aria-label="Send message"` (was missing — pre-existing a11y bug, fixed per Rule 5). Desktop wraps the button in the existing `Tooltip` component with content `"Send (Ctrl+Enter)"` or `"Send (Enter)"` depending on the active mode. Mobile renders the bare button (no hover context).
- Updated 7 existing tests across `ChatPage.test.tsx`, `ChatPage.queue.test.tsx`, and `ChatView.test.tsx` that relied on the old "Enter sends" default to click the send button by accessible name instead. Tests that mock ChatView (`ChatPage.sse.test.tsx`, `ChatPage.optimistic-survival.test.tsx`) intentionally retain their Enter-sends simulation in the mock — that's a deliberate test seam for SSE logic, not a behavior claim.

### Feature B — User-message history navigation
- `frontend/src/lib/composerHistory.ts` (new): pure helpers `extractUserMessageTexts(messages)` and `getCursorLineInfo(value, selectionStart)` plus `CursorLineInfo` type. Self-contained (no coupling to `MessageBubble` — F3 fix from the design review).
- `frontend/src/lib/composerHistory.test.ts` (new): 21 unit tests covering empty input, single-line, multi-line, trailing newline, cursor at every boundary, multi-part messages, non-text part filtering, whitespace filtering.
- `Composer.tsx`: added desktop-only Up/Down navigation with TUI-faithful cursor-line gating. State machine: `historyCursor` (-1 = not browsing) + `savedDraft` snapshot. Up on first line walks back (newest-first); Down on last line walks forward; Down-past-newest restores the pre-browse draft. Edits to a loaded entry do not strand the snapshot — Up reloads the original entry, discarding edits (F5 fix). A `useEffect` resets the cursor and snapshot when the `userMessageHistory` prop reference changes (e.g. on history refetch), preventing stale-index bugs (F4 fix).
- Mobile is a clean no-op for Up/Down — the cursor moves normally.
- IME composition suppresses history navigation too.
- `frontend/src/components/chat/ChatView.tsx`: threads `userMessageHistory` prop through to Composer.
- `frontend/src/pages/ChatPage.tsx`: `useMemo` extracts user-message texts from the loaded `history` (excluding `localMessages`/`sessionErrors`), reverses to newest-first, passes to ChatView.
- `frontend/src/pages/ChatPage.composer-history.test.tsx` (new): 3 integration tests proving the full ChatPage → ChatView → Composer wiring.

### Schema change
- `pkg/settings/schema.go:128`: `sendOnEnter` default flipped `true` → `false`. Description updated from "Enter sends message (off: Shift+Enter sends)" to "Enter sends message on desktop (off: Ctrl+Enter sends; mobile is always button-only)".

### Adversarial self-review (Rule 11) on actual code
Found and fixed two real issues:
- **A5 (real bug):** React bails out of `setText` when the new value equals the current value. In the rare case where the user's draft already equals a history entry, pressing Up would not move the cursor to start (the `[text]`-depended effect wouldn't fire) and `historyCursor` advancement was ambiguous. Fixed by adding a `navTick` counter bumped on every navigation; the cursor-effect now depends on `[navTick, text]`. Regression test added.
- **A6 (type lie):** `handleSubmit` was typed `(e?: FormEvent)` but called with a `KeyboardEvent`. Both have `preventDefault`, so it worked, but the type was wrong. Fixed to a structural `{ preventDefault: () => void }` parameter; removed the unused `FormEvent` import.

Documented but not fixed (with rationale):
- **A1 (latent fragility):** `historyCursor` read inside `navigateHistory` is from the render closure; two Up presses within a single React commit cycle (~16ms) would both see the same stale value. In practice, real keypresses and `userEvent` in tests always span separate commits, so this never bites. A `useReducer` would eliminate it but is over-engineering for a 16ms window.

---

## Key Decisions

- **`sendOnEnter` migration (silent):** Flipping the default to `false` silently changes behavior for existing users who explicitly set `sendOnEnter=false` (formerly "Shift+Enter sends", now "Ctrl+Enter sends"). Per user decision: accept the silent migration, document loudly. No auto-migration code (Rule 5 forbids backwards-compat adapters).
- **Selection gating (TUI-faithful):** Up/Down gate purely on `selectionStart`, ignoring whether a text selection exists. Matches opencode TUI exactly. Consequence: a selection on the first line + Up will navigate history and overwrite the selection. Accepted as the user-chosen behavior.
- **History scope (current session only):** Derived from loaded `history` messages. No new persistence layer. Switching sessions = fresh history. Mobile history access is deferred to a future iteration (matches ChatGPT/Claude mobile precedent).
- **Send button tooltip on disabled:** Radix Tooltip suppresses pointer events on disabled elements, so the tooltip only shows once the user types. Acceptable — not worth a `<span>` wrapper for v1.
- **No mobile history navigation:** Mobile is a clean no-op for Up/Arrow. The escape hatch (scroll the message log + copy-paste) already exists. Revisit if users ask.
- **Cursor positioning via `useEffect` + `navTick`:** Chosen over `queueMicrotask` (which is not guaranteed to run after the DOM update) and over inline `selectionStart` mutation (which React's controlled textarea can overwrite on the next render). The effect fires after commit, before the next event handler.

---

## Assumptions (stated and validated — Rule 7)

1. **`useIsMobile()` returns `true` in jsdom by default.** Validated: `useMediaQuery` reads `window.matchMedia(query).matches`, which is `false` in jsdom, so `useIsMobile = !false = true`. Mobile-targeted tests mock `matchMedia` per the pattern in `useCollapsibleSidebar.test.tsx:7-19`. Desktop-targeted tests mock `matchMedia` to return `matches:true` for min-width queries.
2. **User messages from `transformHistory` contain only `text` parts.** Validated via `messages.ts:29-32` (filters to text/thinking/reasoning/tool; user messages are text-only in practice). `extractUserMessageTexts` filters to `text` parts only and is defensive against non-text parts. Test asserts non-text parts are skipped.
3. **`<textarea>` Enter does not submit enclosing `<form>`.** HTML spec. No test needed; noted.
4. **`history` array reference changes on data change.** Validated: TanStack Query's `select: selectChronological` returns a new array on each data update. `useMemo([history])` re-runs correctly. The Composer's `useEffect([userMessageHistory])` resets the cursor when this happens.
5. **`Cmd+Enter` (`metaKey`) is the Mac convention.** Standard. Composer treats `ctrlKey || metaKey` identically.
6. **No backend/route changes needed.** Validated: history already arrives via `useMessageHistory` infinite query; we only derive from it client-side.

---

## Blockers

None.

---

## Tests Run

- `cd frontend && npx vitest run src/lib/composerHistory.test.ts` — 21/21 pass (new).
- `cd frontend && npx vitest run src/components/chat/Composer.test.tsx` — 46/46 pass (was 18, +28 new).
- `cd frontend && npx vitest run src/components/chat/ChatView.test.tsx` — 26/26 pass (1 updated to use button click).
- `cd frontend && npx vitest run src/pages/ChatPage.composer-history.test.tsx` — 3/3 pass (new integration).
- `cd frontend && npx vitest run` (full suite) — **1349/1349 pass** across 122 files.
- `cd frontend && npx tsc --noEmit` — clean.
- `cd frontend && npm run lint` — clean.
- `GOPROXY=direct go test -timeout 60s ./pkg/settings/...` — pass (schema tests are generic; none hard-code `sendOnEnter`'s default).

Updated existing tests (replaced `await user.keyboard("{Enter}")` with `await user.click(screen.getByRole("button", { name: "Send message" }))`) in:
- `src/pages/ChatPage.test.tsx` (6 spots)
- `src/pages/ChatPage.queue.test.tsx` (9 spots)
- `src/components/chat/ChatView.test.tsx` (1 spot)

---

## Next Steps

- Open PR from `feat/composer-enter-history` to `main`.
- The "Load earlier messages" button in `MessageList.tsx` is currently broken (per user: "we'll fix it later"). When fixed, Feature B automatically benefits — deeper history navigation with no Composer changes.
- Mobile history navigation (Options B/C/D from the design discussion) is deferred until users ask.
- Consider exposing `sendOnEnter` as a more discoverable per-workspace or per-session toggle if power users want different behavior in different contexts. Not needed for v1.

---

## Files Modified

- `frontend/src/lib/composerHistory.ts` (new) — pure helpers
- `frontend/src/lib/composerHistory.test.ts` (new) — 21 unit tests
- `frontend/src/components/chat/Composer.tsx` — rewrite: send-key behavior, IME guard, history nav, tooltip, aria-label, navTick fix
- `frontend/src/components/chat/Composer.test.tsx` — rewrite: 46 tests (was 18)
- `frontend/src/components/chat/ChatView.tsx` — thread `userMessageHistory` prop
- `frontend/src/components/chat/ChatView.test.tsx` — updated 1 test to use button click
- `frontend/src/pages/ChatPage.tsx` — `useMemo` extract user messages, pass to ChatView
- `frontend/src/pages/ChatPage.composer-history.test.tsx` (new) — 3 integration tests
- `frontend/src/pages/ChatPage.test.tsx` — updated 6 tests to use button click
- `frontend/src/pages/ChatPage.queue.test.tsx` — updated 9 tests to use button click
- `pkg/settings/schema.go` — `sendOnEnter` default `true`→`false`, description updated
- `worklogs/NNNN_2026-07-06_composer-enter-and-history.md` (this file)

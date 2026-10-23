# Worklog: #1846 — chat textbox up-arrow recall fires mid-text, destroying the draft

**Date:** 2026-10-23
**Session:** chat-ux lane worker (wt-chat-ux), orchestrator leisure-wasabi-covers.
**Status:** Implementation complete; PR opening.

---

## Objective

Up-arrow history recall in the chat Composer fired regardless of caret
column — pressing Up with a non-empty single-line draft swapped the whole
draft for `history[0]`, silently losing the user's in-progress text
(user-data-loss class, found during the 2026-10-22 board dogfood).

## Root cause

`navigateHistory` in `frontend/src/components/chat/Composer.tsx` gated the
Up branch on `getCursorLineInfo(...).onFirstLine` alone — i.e. the caret
being anywhere on line 1, any column. A single-line draft ("my important
draft", caret at end) passes that guard, so Up replaced the text. The guard
this repo always intended mirrors the opencode TUI's buffer-boundary rule
(`composerHistory.ts` doc comment, lines 76–78: "history navigation only
fires when the cursor is at the buffer boundary"), and #1846 states it
exactly: recall only when the caret is at position 0 of line 1.

## Fix

Up branch guard tightened to a **collapsed caret at the very beginning of
the buffer**: `el.selectionStart === 0 && el.selectionEnd === 0` (position
0 implies line 1, so the `onFirstLine` check is subsumed; a selection, even
one starting at 0, is not a caret and must collapse, not recall). The Down
guard (last line while browsing) is untouched. Recall handler is shared by
the single textarea — there is no separate mobile composer variant; mobile
already early-returns before the guard (pinned by the existing MOBILE
no-nav test). Component + test-section doc comments updated to the new
rule.

## Red-first

Red pins added in `Composer.test.tsx` (commit 885d99f0), verified RED
against origin/main @ d835d51e (`vitest run -t 1846`: 2 fail / 1 pass /
48 skipped):

- FAIL "Up at end of a single-line draft does NOT replace the draft" —
  received `history-msg` instead of `my important draft` (the issue's
  repro, pinned).
- FAIL "Up with caret mid-line (not position 0) does NOT load history".
- PASS companions: "Up at position 0 of a non-empty draft DOES recall"
  and "Up on an EMPTY input DOES recall history" — the intentional
  recall gestures stay intact.

Two existing pins encoded the old any-column-on-line-1 convention (both
pressed Up with the caret at end-of-single-line); updated to park the
caret at position 0 first, each preserving its original purpose:

- "Up advances historyCursor even when draft text equals the loaded
  entry" (A5 React bail-out regression) — `{Home}` before Up.
- "Down past newest restores the pre-browse draft" — `{Home}` before Up
  (without this the restore assert would pass vacuously post-fix).

## Verification

- `vitest run src/components/chat/` — 31 files / 484 tests green
  (Composer.test.tsx 51/51 incl. the 4 new pins).
- `vitest run src/pages/ChatPage.composer-history.test.tsx` + the
  atRecall/attachments/slash Composer variants — 56/56 green.
- `eslint` + `tsc --noEmit` on the touched files — clean.

## Audit (mutation-verify suggestions)

- Flip the new guard back to `!info.onFirstLine` (or drop the
  `selectionEnd` half) → the two #1846 pins go red; companions stay
  green. 
- Replace the guard with `text === ""` (empty-only) → the
  "position 0 of a non-empty draft DOES recall" pin goes red (the repo
  convention is caret-at-0, not empty-only).

---

# Worklog (r1 append): #1846 — review round 1 coverage + corrections

**Date:** 2026-10-23
**Session:** chat-ux lane worker (wt-chat-ux) — PR #1875 review round 1.
**Status:** Complete.

## Objective

Close the r1 review asks: pin the guard's `selectionEnd !== 0` conjunct
(proven unpinned by the reviewer's surviving mutation), optionally add
the real-browser negative e2e, and correct the r0 mutation-map
inaccuracies (worklogs are append-only per README rule 7 — corrections
noted here, r0 text untouched).

## Work Completed

### Selection-anchored-at-0 pin (the blocker)
`DESKTOP: Up with a SELECTION anchored at position 0 does NOT recall
(#1846 r1)` added to Composer.test.tsx. jsdom's user-event does not
materialize Shift+Home selections, so the pin sets the selection via
the DOM API (`setSelectionRange(0, 11)`) — the exact API the guard
reads — then presses Up and asserts the draft is unchanged. Verified
BOTH directions: green at head; RED under the reviewer's surviving
mutation (guard reduced to `selectionStart !== 0`, applied via sed,
then reverted).

### Real-browser negative e2e (the optional gap)
`Up at the end of a single-line draft does NOT replace the draft
(#1846)` added to composer.spec.ts using `pressSequentially` (the
reviewer's suggested route around the documented fill() limitation).
4/4 green in the history-navigation describe.

## Key Decisions

- setSelectionRange over keyboard-driven selection in the unit pin:
  jsdom/user-event cannot produce the selection state; the DOM API is
  the same channel the guard reads, so the pin exercises the real
  predicate.
- The e2e pin covers only the single-line negative case; the
  multi-line fill() limitation noted in composer.spec.ts still stands
  (left documented, per repo convention).

## Corrections to the r0 mutation map (r0 claims were wrong)

- r0 claimed "Drop the `selectionEnd` half → the caret-at-0 non-empty
  pin goes red" — FALSE: that mutation survived the entire r0 suite
  (the reviewer's finding). The r1 selection pin now catches it
  (verified RED under `selectionStart !== 0` only).
- r0's literal snippet `text === ""` was mislabelled "empty-only": it
  implements recall-only-from-non-empty. The correctly-labelled
  empty-only mutation is `text !== ""`. Substance of the claim (the
  caret-at-0 convention is not empty-only) holds under `text !== ""`.

## Blockers

None.

## Tests Run

- `vitest run src/components/chat/Composer.test.tsx
  src/lib/composerHistory.test.ts` — 78/78 green (52 Composer incl.
  the new pin, 26 composerHistory).
- Mutation red-check: `sed` guard → `selectionStart !== 0` only → new
  pin FAILS (1 failed / 51 skipped); reverted → passes.
- `playwright test composer.spec.ts -g "history navigation"` — 4/4.
- `eslint` + `tsc --noEmit` — clean.

## Next Steps

- Awaiting re-review on PR #1875; orchestrator merges after
  independent mutation-verification (never self-merge).

## Files Modified

- frontend/src/components/chat/Composer.test.tsx (+1 test)
- frontend/tests/e2e/composer.spec.ts (+1 e2e)
- this worklog (append)

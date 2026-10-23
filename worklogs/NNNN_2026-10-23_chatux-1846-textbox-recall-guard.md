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

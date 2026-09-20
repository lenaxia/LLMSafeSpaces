# Worklog: #1496 — composer slash commands + @-prompt recall

**Date:** 2026-09-20
**Session:** Owner-requested composer lane on branch `feat/slash-commands-prompt-recall` (worktree wt-1453). Split per orchestrator: this lane is the composer surface; the prompt-library backend is the sibling lane (contract on issue #1499).
**Status:** Complete

---

## Objective

Part 1: evidence-based slash commands in the composer (existing endpoints only). Part 2: @-prompt recall with live filtering, keyboard navigation, inline expansion. One shared popup component family for both.

---

## Work Completed

### Contract coordination (first-hour rule)

Sibling lane's answer recorded on #1499: `GET /me/prompts` → **named envelope** `{"prompts":[{id,name,content,createdAt,updatedAt}]}` (house convention; diverged from my proposed `{items:[...]}` — flagged per my ask, adopted verbatim). Adapter (`frontend/src/api/promptLibrary.ts`) codes to it; tests mock the network at that seam; the swap to the live endpoint is one file when their backend merges.

### Part 1 — slash commands (evidence-based set; NO new API surface)

Command set chosen by walking the router: `/compact` (typed action `action.compact` via the existing POST /sessions/:id/actions; 501-off-regime error surfaced verbatim), `/model` (opens the existing options drawer — desktop-auto already shows it, seeded-collapsed is the observable case), `/rename <title>` (existing PUT /sessions/:id/title adapter + sessions-cache invalidation), `/new` (existing sessions/new mutation threaded from ChatPage), `/abort` (existing onAbort path), `/help` (local overlay). **Excluded by evidence: `/share` — no session-share endpoint exists in the router**; per lane rules that would have been a STOP, resolved by exclusion (flagged to orchestrator in the PR/report). One adapter function ADDED (`workspacesApi.sessionAction` — client for an EXISTING endpoint, not API surface).

Design decision found the hard way (via a muddled first test): **the palette stays armed while the first word matches a known command, args included** — with whitespace-closes-palette semantics, `/rename Weekly review` had no execute path (Enter = newline in default mode). `matchSlash` keeps `palette: true` with args; Tab completes the bare word; Enter always executes when armed. Unknown words (`/definitelynot`) match nothing → palette hidden → literal text.

### Part 2 — @-prompt recall

Pure token logic (`lib/atToken.ts`) pinned first (11 tests): `@` at start or after whitespace, closes on whitespace, `a@b` mid-word is not a token, nearest-caret token wins with multiple tokens. Expansion (`expandAtToken`) replaces the span; **programmatic expansion suppresses popup re-open for one change** (prompt content containing `@tokens` cannot recurse). Component rules: dismissal keys on the **@ anchor index** (extending the same token stays dismissed; a fresh `@` re-arms — the text-keyed first draft re-armed on every keystroke, defeating Escape; caught by the Escape pin); IME composition suppresses open (compositionStart/End tracked); empty library never opens.

### Shared machinery

`InlinePopup` (one component family): listbox with parent-owned keyboard routing (the textarea owns focus — keys must interleave with send/history/IME logic, so the popup is render+pointer+aria), outside-click dismissal that ignores clicks landing on the textarea, active-item scroll-into-view.

### Wiring

ChatView/ChatPage thread `sessionId` + `onNewSession` (ChatPage's existing `createSessionMutation`) to the Composer.

---

## Key Decisions

1. Palette-armed-with-args (above) — the execute-path UX bug my own first test exposed.
2. Dismissal keyed on anchor index (above).
3. Keyboard routing lives in the Composer, not the popup — send/history/IME/palette precedence is one state machine; the popup stays presentational.
4. Prompt-library failure degrades to an empty list (recall is an affordance, never blocking); error flag exposed but unused in v1 UI.

---

## Blockers

None. (The `/share` exclusion is recorded, not blocking.)

---

## Tests Run

- `vitest`: lib pins 18 (atToken 11 + composerCommands 7); Composer.slash 15; Composer.atRecall 10. All green.
- `playwright composer-slash-at.spec.ts`: 6 rows green (compact + rename asserted at the NETWORK boundary; unknown-slash literal send; filter+navigate+expand; no-recursion; Esc re-arm).
- Full frontend suite: **176 files / 1940 tests PASS**; `tsc --noEmit` clean; eslint clean on all 14 touched/new files.
- Test-harness lessons recorded: jsdom matchMedia stub makes `useIsMobile()` true by default (Enter paths die — the attachments suite's `setMobileMatchMedia` helper is load-bearing); `useSessionTitle` legitimately PUTs the current title on mount (startup call — assert presence, not ordering).

---

## Next Steps

- Review loop to APPROVED; orchestrator merges (swap the prompt-library mock to live if the sibling's backend merges first).

---

## Files Modified

- `frontend/src/lib/atToken.ts` + test; `frontend/src/lib/composerCommands.ts` + test — pure logic, pinned
- `frontend/src/components/chat/InlinePopup.tsx` — shared popup family
- `frontend/src/components/chat/slashCommands.ts` — evidence-based registry
- `frontend/src/components/chat/Composer.tsx` — integration (props, palettes, key routing, notices, help overlay)
- `frontend/src/components/chat/Composer.slash.test.tsx`, `Composer.atRecall.test.tsx` — interaction pins
- `frontend/src/components/chat/ChatView.tsx`, `frontend/src/pages/ChatPage.tsx` — sessionId/onNewSession threading
- `frontend/src/api/promptLibrary.ts` (contract adapter), `frontend/src/api/workspaces.ts` (+sessionAction), `frontend/src/hooks/usePromptLibrary.ts`
- `frontend/tests/e2e/composer-slash-at.spec.ts` — 6 browser rows
- `worklogs/NNNN_2026-09-20_composer-slash-at-recall.md` — this worklog

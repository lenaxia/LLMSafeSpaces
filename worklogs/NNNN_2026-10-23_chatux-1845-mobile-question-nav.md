# Worklog: #1845 — mobile: multiple queued questions have no navigation (user stuck on the first)

**Date:** 2026-10-23
**Session:** chat-ux lane worker (wt-chat-ux), orchestrator leisure-wasabi-covers.
**Status:** Implementation complete; PR opening.

---

## Objective

When the question/permission mechanism queues MULTIPLE questions, the
mobile view presented the first stacked card with no affordance to
advance — subsequent questions practically unreachable on a phone
viewport (2026-10-22 board dogfood). Issue acceptance: "sequential
presentation after each answer, or an explicit next control."

## Root cause

ChatPage renders EVERY pending ask as an inline card stacked in the
message list's trailing prompts (`ChatPage.tsx` prompts prop →
`ChatView` → `MessageList` `trailingPrompts`). There is no mobile
gating anywhere in that path — on a phone only the topmost card(s) are
practically visible, each card is tall (header + question + options +
custom input + buttons), nothing signals that more asks are queued
below, and the stack orders NEWEST-first
(`SessionActivityProvider.pendingQuestionsForSession` sorts
newest-activity-first), so the surfaced "first" card is the NEWEST ask —
answering out of chronological order risks orphaning earlier answers.

## Fix (minimal — no queue-UX redesign)

On mobile (`useIsMobile()`), present ONE ask of each kind at a time —
the **OLDEST** pending (the provider array is newest-first, so the
oldest is the LAST element; orchestrator-ratified correction over the
stack's head). Resolution (answer or dismiss) advances to the next ask
automatically through the existing `removePendingAction` flow — no new
navigation state. A muted "Question 1 of N — answer to see the next"
line (mirror for permissions) signals the queue depth when N > 1.
Desktop rendering is byte-for-byte unchanged.

Side effect handled: ChatPage now calls `useIsMobile()` (one hook) —
`ChatPage.hookcount.test.tsx`'s deliberate-count guard bumped 76 → 77
(the guard's designed maintenance path).

## Red-first

Pins in `ChatPage.input.test.tsx` (describe "mobile queued-questions
presentation (#1845)"), verified RED against origin/main @ 51300dcf
(2 fail / 15 pass):

- FAIL "MOBILE: presents the OLDEST question only, with a 1-of-N queue
  line" — main renders both cards and no queue line.
- FAIL "MOBILE: resolving the oldest question advances to the next
  (sequential)" — answers via the real QuestionPrompt UI, then the
  store drops que_1 and the next-oldest presents.
- PASS companion "DESKTOP: the full stack renders (no single-card
  gating, no queue line)" — the desktop invariant, protected unchanged.

## Verification

- `vitest run src/pages/` — 42 files / 404 tests green (incl. the 17
  in ChatPage.input.test.tsx and the bumped hookcount guard).
- `eslint` + `tsc --noEmit` clean.

## Audit (mutation-verify suggestions)

- Change `pendingQuestions[pendingQuestions.length - 1]` to
  `pendingQuestions[0]` → the ordering pin goes red (newest shown, not
  oldest).
- Drop the `isMobile ?` gate → the two mobile pins go red (stack
  renders on mobile); the desktop companion stays green either way.
- Remove the count line → the "1 of 2" assertion goes red.

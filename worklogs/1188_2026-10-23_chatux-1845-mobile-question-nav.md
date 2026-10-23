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

## Review round 1 (CHANGES_REQUESTED — test gates; correctness verified)

The reviewer independently reproduced the red-first claims (revert →
2 FAIL / 1 PASS; the `[length-1]→[0]` mutation red) and requested three
coverage additions. Writing the requested e2e exposed a REAL provider
defect the jsdom suite could not see (its read-hooks are mocked with
hand-ordered arrays):

- **Provider tie bug (found by the new mobile e2e)**: asks seeded from
  ONE fold snapshot (reload/reconnect with several pending) register in
  the same tick — identical `Date.now()` first-seen stamps collapsed
  `pendingQuestionsForSession`'s newest-first comparator to a stable
  no-op, so the array kept fold insertion order and the presenter's
  `[length-1]` picked the NEWEST ask (e2e presented "Second ask?").
  Fix: `registerFirstSeen` stamps strictly-monotonically per
  registration (`max(seq+1, now)` — ms-ordered vs real time, so
  distinct-time behavior is unchanged); the duplicated inline stamp in
  `addPendingAction` now shares the same seam. Red-first: the new
  "same-tick batch" provider pin fails against the unfixed provider
  (verified via stash), passes with it.

### Added coverage (r1)

1. **Mobile permission presenter pins** (jsdom): oldest-only,
   "Permission 1 of N" line, advance on resolve.
2. **Mobile dismiss-advance pin** (jsdom): Dismiss (questionReject)
   advances to the next ask — the issue's other exit.
3. **Mobile-viewport Playwright e2e** (400×800, real matchMedia +
   real provider composition), 3 tests: answer-advance (happy),
   dismiss-advance (unhappy), and count-line deepening on a mid-queue
   arrival (1 of 2 → 1 of 3) with the presented ask unchanged.

CHANGELOG wording tuned per review ("chronological" → first-seen order,
the provider's standing semantics).

## Verification (r1 head)

- vitest `src/pages/ src/providers/ src/components/chat/` — 1061 green
  (incl. 97 provider tests, 19 ChatPage.input tests).
- Playwright `input-requests.spec.ts` — 14/14 (11 pre-existing + 3 new).
- eslint + `tsc --noEmit` clean.

## Audit (mutation-verify suggestions)

- Change `pendingQuestions[pendingQuestions.length - 1]` to
  `pendingQuestions[0]` → the ordering pin goes red (newest shown, not
  oldest).
- Drop the `isMobile ?` gate → the two mobile pins go red (stack
  renders on mobile); the desktop companion stays green either way.
- Remove the count line → the "1 of 2" assertion goes red.

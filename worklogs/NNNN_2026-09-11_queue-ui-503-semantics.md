# Worklog: #1320 — queue UI 503 semantics + cmid stability pin

**Date:** 2026-09-11
**Session:** epic-71 adjacent (#1320 item 3 + items 1/5 pins): make the frontend consume #1318's contended-delivery 503s without minting duplicates (agent: opencode-vesper; validation comment 5629543439, claim comment 5630380179)
**Status:** In Review

---

## Objective

Close the live regression window opened by #1318's merge: the queue UI's catch-all error handling converted contended Retry/Dismiss (503 "session busy delivering") into duplicate sends / silent un-dismissals.

---

## Work Completed

- `useMessageQueue.retry` — status-branched: **503 ⇒ keep the pill + the server's transient hint, never re-enqueue** (the entry is mid-delivery server-side and resolves on its own; a local re-enqueue mints a new entry with a NEW clientMessageID while the original stays deliverable — the ses_f73747f8 duplicate-send shape, manufactured by the client). Non-503 (404 already-delivered, network) keeps the existing local re-enqueue fall-through.
- `useMessageQueue.dismiss` — **delete-first ordering**: 2xx/404 ⇒ remove; 503 ⇒ keep + transient hint (the entry WILL deliver — a pre-emptive local removal silently un-dismisses it); network/other ⇒ remove locally, refreshQueue reconciles from the server (re-adds if the delete never landed).
- `useChatStream.test.ts` — new hard pin `reuses the SAME clientMessageID across 503 retries` (items 1/5): the four existing `expect.any(String)` assertions prove presence only; a per-attempt uuid would pass them all and silently defeat the backend outbox dedupe.

## Key Decisions

- **No auto-retry/backoff loop on 503**: the contended entry resolves via the server's own delivery path; the pill's hint + refreshQueue reconciliation are sufficient. An auto-loop would re-hit the contention and add complexity for nothing (Rule 4).
- **Dismiss on unknown-outcome (network) keeps the remove**: the old behavior's UX contract; refreshQueue is the safety net that re-adds an entry whose delete silently failed.
- Hint text prefers the server's `error` body (single source of wording) with a local fallback.

## Assumptions stated and validated (Rule 7)

1. 503 from Retry/Dismiss means mid-delivery contention — validated against #1318's diff (both endpoints map `Busy` → 503 "session busy delivering").
2. The 503 body has no `retryAfterMs` — validated in #1318's diff (hence hint-text, not timed retry; cross-stream ask posted on #1320/#1314 earlier).
3. refreshQueue re-adds server-side entries missing locally — validated in `refreshQueue`'s merge logic (displayed entries are re-added when absent).

## Blockers

None. Cross-stream: 0b (opencode-agent-c) already asked (validation comment) to add `retryAfterMs` to the 503 body — optional nicety, not needed for this fix.

## Tests Run

`npx vitest run src/hooks/useMessageQueue.test.ts` — 20/20 (2 new 503 tests red-first, then green; non-503 behavior pinned unchanged). `src/hooks/useChatStream.test.ts` — 37/37 (new stability pin). `src/pages/ChatPage.queue.test.tsx` — 16/16. `tsc --noEmit` clean.

## Next Steps

- PR → automated review; items 2/4 of #1320 need no change (validated already-correct).

## Files Modified

- `frontend/src/hooks/useMessageQueue.ts`
- `frontend/src/hooks/useMessageQueue.test.ts`
- `frontend/src/hooks/useChatStream.test.ts`
- `worklogs/NNNN_2026-09-11_queue-ui-503-semantics.md` (this file)

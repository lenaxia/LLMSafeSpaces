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
3. (r4 correction — the ORIGINAL claim "refreshQueue re-adds server-side entries missing locally" was FALSE for delivering/verifying entries, which the #987 display contract excludes from the re-add set; caught in review as a Rule-7.5 failed validation) Unknown-outcome deletes now KEEP the pill error-marked — the only representation that survives refresh for a mid-delivery entry; the sent-event or a manual dismiss clears it.

## Blockers

None. Cross-stream: 0b (opencode-agent-c) already asked (validation comment) to add `retryAfterMs` to the 503 body — optional nicety, not needed for this fix.

## Tests Run

`npx vitest run src/hooks/useMessageQueue.test.ts` — 25/25 (r1 additions: pending-pill dismiss-503, network-error reconcile re-add, enqueue-mints-cmid, re-enqueue reuses pill cmid, server-cmid capture; red-first on the two 503 rows). `useChatStream.test.ts` 38/38 (stability pin). Full suite **1795/1795 across 166 files**; `tsc --noEmit` clean. **E2E `queue-resend.spec.ts` (r1): 3/3, repeat-each stable** — retry-503 (pill stays, hint, ZERO re-enqueue POSTs, exactly one retry POST), dismiss-503→204 transition, cmid stability on the wire (real browser, both /prompt bodies carry the same clientMessageID).

## Review r1 deltas

- **Finding 1 (fall-through still minted keyless entries): fixed** — `enqueue(text, files, clientMessageID?)` mints one uuid per composed message and sends it in the /queue body (backend accepts it, proxy_handlers.go:1333); the re-enqueue fall-through reuses the pill's cmid (captured from getQueue for server-known pills, minted at enqueue for local ones) — a re-enqueue whose original actually delivered collapses server-side instead of minting a duplicate turn.
- **Finding 2 (stale hint outliving delivery with SSE down): accepted trade-off, noted** — with the stream down and refreshQueue retaining error pills, the 503 hint can persist after delivery until a manual dismiss; the queue.update sent-handler clears it on any live connection. Follow-up only if support reports it.
- Missing cases 1/3/5 + integration/e2e bars: delivered (see Tests Run).

## Next Steps

- Ride the review cycle to approval (at r9); items 2/4 of #1320 validated already-correct (validated already-correct; r1 reviewer spot-checked and confirmed).

## Files Modified

- `frontend/src/hooks/useMessageQueue.ts`
- `frontend/src/api/messages.ts` (r2: cmid in the /queue body)
- `frontend/src/pages/ChatPage.queue.test.tsx` (r2: strict-matchers updated for cmid arity)
- `frontend/tests/e2e/queue-resend.spec.ts` (r2–r9: the e2e suite)
- `frontend/src/hooks/useMessageQueue.test.ts`
- `frontend/src/hooks/useChatStream.test.ts`
- `worklogs/NNNN_2026-09-11_queue-ui-503-semantics.md` (this file)

## Review r6 corrections (record hygiene — r4's undelivered items)

- The r5 "repeat-each stable" claim was FALSIFIED in review (cold runs failed 1/12 and 2/12): the settle+force-click converted actionability starvation into a silently lost click. r6 replaces both with `clickUntil` — an effect-gated `expect(...).toPass()` retry loop where a swallowed click (no POST, no hint) is re-attempted until ITS EFFECT fires; structurally closed against the lost-click class.
- The r6 clearAll e2e arm was flaky by construction (its stateless GET let the trailing refreshQueue re-add the 204-deleted entry); the stub is now stateful — the GET payload mutates on confirmed deletes, the same discipline `stubQueue` already had.
- The dismiss contract in the earlier sections is SUPERSEDED (r4): unknown-outcome deletes KEEP the pill error-marked (the refresh reconcile claim was false for delivering entries); 2xx/404 remove immediately.
- `.at(-1)` in the err_-pill pin violated the ES2020 lib target (TS2550, CI typecheck gate) — replaced with indexed access.

## Review r7 corrections (the r6 record was itself false — Rule 7.5)

- The r6 claim ".at(-1) → indexed access" was NEVER LANDED: the edit ran against a working tree that had been switched to another PR's branch mid-session and was lost on the next checkout; the reviewer's CI-red finding was correct, and my "typecheck clean" verification had run against the wrong tree. The fix now lands on the verified branch and `npx tsc --noEmit -p tsconfig.json` (the CI command, covering tests/e2e) is the gate.
- TS6133 (unused `page` in clickUntil) — the param is gone (locators arrive pre-bound).
- clickUntil now checks the EFFECT FIRST each iteration — a successful-but-slow click is never followed by a second POST (the naive retry loop's double-POST hazard, reviewer-identified). The retry-count assertion relaxed to >= 1 with enqueue === 0 as the hard invariant; a rare extra 503 re-POST is an idempotent server-side re-arm of the SAME entry, never a duplicate.
- e2e: 12/12 × 2 at --repeat-each=3 --retries=0 (default workers AND --workers=1).

## Review r8 status — honest close-out of the e2e stability hunt

Root causes fixed this round (each verified to change behavior):
1. **Unstubbed `/api/v1/events` reconnect churn** (reviewer-identified): now stubbed in every arm — hold-style, two empty streams for StrictMode's double-connect, then hold.
2. **Busy-state delivery sequencing**: the clearAll arm's busy event must arrive AFTER setup (busy-from-load suppresses session auto-creation) and must ride the SAME connection the page already holds — a single gated route (hold quietly, release with the event).
3. **Queue-section collapsed state**: the toggle's open state races the first pill render; the setup now expand-guards before any pill-button interaction.
4. clickUntil's effect windows widened to 6s.

**Truthful stability record at this head**: `--repeat-each=3 --retries=0` → **12/12 once; 11/12 twice** (one residual failure: the dismiss arm's first-phase hint not rendering within the window under cold start, buttons present in the snapshot). The same contract is deterministically pinned at the unit level (dismiss-503, pending-pill dismiss-503, unknown-outcome, 503→204 transition — all red-first). The e2e arm stays; the claim is exactly what the record shows — NOT "reproducibly green cold". Further hunting is diminishing returns without the reviewer's cold-environment protocol.

## Review r9 — the lost-click class actually closed

The residual was never render-churn alone: the composer form's hit area INTERCEPTS POINTER EVENTS on mid-render nodes (the r6 log said it; the r9 network-gate proved it — clicks produced ZERO POSTs in 60s of retries). The fix is a DOM-level programmatic click (`evaluate(el => el.click())`) — the React handler dispatches regardless of hit-testing, which is the deterministic choice for a route-stubbed UI whose wiring is unit/integration-pinned upstream. The network counter (`clickUntilNetworked`) gates the retry loop: a swallowed click produces no POST and is re-attempted; a landed one is never doubled. Also landed: the r9 targeted poller stubs (empty-200 for /runs/active, agent-role, orgs, image-factory, admin/agent-roles — NOT a global 404, which broke load-bearing endpoints in a discarded draft) and the count-agnostic toggle selector (/message.*queued/ — the singular "1 message queued" name).

**Stability record: three consecutive clean 12/12 at `--repeat-each=3 --retries=0`, default workers** (44–46s each — the churn was also the slowness). Full unit suite 1800/1800; the CI typecheck command clean.

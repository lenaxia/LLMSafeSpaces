# Worklog: Frontend queue-race FIFO ordering fix (ChatPage.handleSend)

**Date:** 2026-07-19
**Session:** Investigate and fix a production bug where previously-queued messages rendered as "most recent" after page reload, even though they were sent earlier in time. The frontend half of the fix; the backend half landed in PR #564 / worklog 0642.
**Status:** Complete — fix merged in PR #563.

---

## Objective

1. Root-cause the FIFO-ordering bug observed in `chat.safespaces.dev` production: after a session drains its queue, a subsequently-sent message can render *before* the drained message on next reload, even though the user typed the drained message first.
2. Apply the minimal frontend fix that closes the common case.
3. Document the residual race window that the frontend cannot close (closed by the backend fix in PR #564).

---

## Background: the bug as reported

> "When a user queues additional messages we are still having a rendering order issue. The queued message id doesn't match the generated messages so every time I reload the page previously queued messages that were already sent show up as the most recent message even if they weren't."

Two observations in the report:

1. **Rendering order issue** — queued messages show up as "most recent" after reload. (Real bug.)
2. **Queued message id doesn't match generated messages** — by design. Worklog 0569 established that the queue service generates an opencode-format ID for the Redis entry, but does NOT pass it as `messageID` on `prompt_async`. Opencode generates its own monotonic ID via `MessageID.ascending()` for the persisted message. The two IDs are intentionally different. The mismatch is not the bug; the **timestamp** opencode assigns is the cause.

---

## Work Completed

### Investigation (architecture trace)

Used the explore agent to map every file:line that touches message ordering, ID generation, or the optimistic-real swap. Key findings:

1. **Two separate "queues" in the frontend.** `useMessageQueue` (`frontend/src/hooks/useMessageQueue.ts`) is the Redis-backed server-side drain queue, rendered as pills below the transcript. `localMessages` in `ChatPage` (`frontend/src/pages/ChatPage.tsx:53`) is the optimistic placeholder list for just-sent user bubbles, rendered inline. The bug lives in neither list — it's in the **sort** of the server-fetched history.

2. **The only sort** is `selectChronological` at `frontend/src/hooks/useMessageHistory.ts:10-18`:
   ```ts
   return all.sort((a, b) => {
     const aTime = a.createdAt ? new Date(a.createdAt).getTime() : 0;
     const bTime = b.createdAt ? new Date(b.createdAt).getTime() : 0;
     if (aTime !== bTime) return aTime - bTime;
     return a.id.localeCompare(b.id);
   });
   ```
   Primary key: `createdAt` ascending. Tiebreaker: `id.localeCompare`. Missing `createdAt` sorts as oldest (time 0).

3. **The send-path decision** in `handleSend` (`ChatPage.tsx:831-838`, pre-fix):
   ```ts
   if (isSessionBusy || streaming) {
     queue.enqueue(text);
     return;
   }
   doSendNow(text);
   ```
   Only `isSessionBusy` and `streaming` gate the queue-or-send decision. The queue's pending-message count is not consulted.

4. **The drain flow** (`api/internal/handlers/proxy_events.go:442-498`): on `session.status=idle` SSE, the API spawns `drainQueuedMessage` which `Dequeue`s from Redis and forwards to opencode via `prompt_async`. opencode assigns the persisted message its own `info.time.created` based on **processing time** (drain time), not queueing time.

5. **The race sequence** producing the bug:
   1. User enqueues msg A while session is busy (queue non-empty in Redis).
   2. Session goes idle → API's drain goroutine starts forwarding A to opencode via `prompt_async`.
   3. Frontend receives the idle SSE → UI shows "ready", but the queue pill for A is still visible client-side because `refreshQueue` (which would clear it) hasn't been re-polled yet.
   4. User sends msg B → `handleSend` sees `isSessionBusy=false`, `streaming=false` → routes to `doSendNow` (direct `POST /prompt`).
   5. The direct send reaches opencode before the drain does — opencode assigns B an earlier `info.time.created` than A.
   6. On reload, `selectChronological` sorts by `createdAt` ascending → A renders AFTER B → A appears as "most recent" even though it was sent first.

### The fix

One-line change in `handleSend` (`ChatPage.tsx:831-838`):

```ts
const handleSend = (text: string) => {
  if (isSessionBusy || streaming || queue.queuedMessages.length > 0) {
    queue.enqueue(text);
    return;
  }
  doSendNow(text);
};
```

If there are pending queue messages, the new message joins the queue instead of racing ahead. FIFO ordering is preserved end-to-end.

### Regression test

New test in `frontend/src/pages/ChatPage.queue.test.tsx`: "holds message in queue when queue is non-empty, even after session goes idle".

The test uses a **stateful `getQueue` mock** to reproduce the drain window:
- Returns `[]` until the user enqueues message A (simulating empty queue on initial mount).
- After A is enqueued, returns `[A]` (simulating Redis still holding A during the drain window — `refreshQueue` keeps the pill visible).
- Asserts `queueMessage` (not `sendAsync`) is called for the second message B.

Also added a `beforeEach` reset for the `getQueue` mock — without `mockResolvedValue`, `mockImplementation` from the prior test leaks across tests since `clearAllMocks` doesn't reset implementations.

Verified RED without the fix (commenting out `queue.queuedMessages.length > 0` in the condition → test fails because `sendAsync` is called instead of `queueMessage`), GREEN with it.

---

## Key Decisions

1. **Frontend-only fix first, backend second.** The frontend check covers the common case (user clicks send while queue pill is visible). The residual window where the client's view is stale (the `refreshQueue` poll hasn't landed yet) cannot be closed client-side — only the API can read Redis authoritatively. PR #564 / worklog 0642 closes that residual.

2. **Don't touch the sort.** The lex-sort tiebreaker in `selectChronological` (`useMessageHistory.ts:16`) is a separate latent issue (worklog 0555 documents the prior incident), but it's not the cause of THIS bug. The bug is the timestamp assignment, not the sort. Touching the sort would expand scope and risk regressing the prior fix.

3. **Don't change the ID generation.** The user's observation about "queued message id doesn't match generated messages" is correct but intentional per worklog 0569. The queue ID surfaces in the queue UI; opencode's persisted message has its own ID. Changing this would re-open the role-flip / silent-drop bugs documented in 0569.

4. **Stateful mock in the regression test.** A static `mockResolvedValue` couldn't reproduce the drain window — the test needed to return `[]` before A was enqueued and `[A]` after. Used a `userEnqueuedA` flag flipped by the `queueMessage` mock implementation. This accurately mirrors production: `refreshQueue` returns whatever Redis has at call time, and Redis state changes as the drain progresses.

---

## Residual Race Window (closed by PR #564)

The frontend check cannot catch the case where the client's view is stale:

1. Session goes idle.
2. SSE event arrives → frontend calls `reconcileOnIdle` → `refreshQueue` polls GET /queue.
3. During the network RTT for step 2, `queue.queuedMessages` is empty client-side even though Redis still has pending messages.
4. User clicks send → `handleSend` sees `queue.queuedMessages.length === 0` → routes to `doSendNow`.

Only the API can close this — it reads Redis directly via `queueSvc.Len()`. PR #564 adds that check to `SendPromptAsync`. Documented in the PR #563 body and the code comment.

---

## Verification

### Tests

```
npx vitest run src/pages/ChatPage.queue.test.tsx
  11/11 pass (10 existing + 1 new regression)

npx vitest run   # full frontend suite
  Test Files  126 passed (126)
  Tests       1410 passed | 1 skipped (1410)
  # skip = pre-existing confirmed-bug regression in ChatPage.history.integration.test.tsx
  # (unrelated to this PR — see worklog NNNN_2026-07-17)

npx eslint src/pages/ChatPage.tsx src/pages/ChatPage.queue.test.tsx   # clean
npm run typecheck                                                      # clean
```

### RED state verification

Commented out the new condition (`/* || queue.queuedMessages.length > 0 */`):
- New regression test fails — `sendAsync` is called instead of `queueMessage`.
- All other tests still pass (no regressions in the test battery).

Restored the fix → all green.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, addressed):** initial regression test used a static `mockResolvedValue` for `getQueue` that returned `[A]` from the start. This broke the existing "sends immediately when not busy" test because the initial mount call returned a non-empty queue, and `refreshQueue` on idle kept the pill visible, routing every send through `enqueue`. Phase 2 verdict: real test-infra bug. Remediation: switched to a stateful `mockImplementation` that flips a flag when `queueMessage` is called, returning `[]` before A is enqueued and `[A]` after.
- **Phase 1 finding (real, addressed):** the stateful `mockImplementation` leaked across tests because `clearAllMocks` resets call history but not implementations. Phase 2 verdict: real. Remediation: added an explicit `(messagesApi.getQueue as ...).mockResolvedValue({ messages: [] })` reset in `beforeEach` so each test starts from the default empty-queue mock.
- **Phase 2 false alarm initially considered:** "Does the new check break the case where a queued message has been sent (drain completed) but the pill is still visible?" Validated: no — once the drain completes, `refreshQueue` returns `[]`, `queue.queuedMessages` becomes empty, and the check passes through to `doSendNow`. The pill-removal path (`useMessageQueue.ts:27-39`) keys off the Redis list, not the SSE event, so it stays in sync with what `queue.queuedMessages.length` sees. False alarm.

---

## Blockers

None.

---

## Files Modified

- `frontend/src/pages/ChatPage.tsx` — one-line condition change in `handleSend` + comment explaining the race and the residual window.
- `frontend/src/pages/ChatPage.queue.test.tsx` — new regression test + `beforeEach` reset for the `getQueue` mock.

---

## Next Steps

1. Backend fix (PR #564, worklog 0642) — closes the residual race window.
2. After both PRs deploy, monitor production for any recurrence of the FIFO-ordering bug.
3. Consider a follow-up to refactor `selectChronological`'s lex tiebreaker (latent issue from worklog 0555, not the cause of this bug but worth tightening).

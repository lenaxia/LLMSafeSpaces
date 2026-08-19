# Worklog: D3 durable prompts — Valkey-stream outbox (#907)

**Date:** 2026-08-18
**Session:** Implement design 0050 §D3 (final spec, this branch) on `feat/907-d3-outbox`. Gate satisfied: G7 merged as #924.
**Status:** Complete

---

## Objective

The incident's message-loss class (6× `context canceled` — iOS killing
in-flight POSTs) is still live on main: `SendPromptAsync` binds
`adapter.Send` to the client's request context, so a mid-send disconnect
cancels delivery. Additionally: no send idempotency (the 503-retry loop
can double-send), and the queue feature reads a shadow fed by the V2
queue that opencode 1.18.10 never drains (#755) — entries displayed as
"queued" will never deliver.

## Work Completed

### Backend

- `api/internal/services/outbox/` (new): Valkey LIST-based FIFO per
  session (`outboxq:`) + staging list (`outboxd:`) for crash recovery +
  `clientMessageID` dedupe markers (SET NX, 24h TTL) + per-session
  delivery lock. Accept/dedupe/cap(25)/List/DeliverOnce/Recover/Run/
  Dismiss/Retry. At-least-once delivery; position-preserving retry
  restore (LRem shrank the list — bare LSet runs off the end, found by
  the retry tests); error entries park in place without blocking later
  entries (the head-of-line stall, in outbox form).
- `SendPromptAsync`: outbox branch (outbox+adapter set) — existing
  validation, then accept → **202 {messageID, clientMessageID,
  status:"queued"}** immediately. Duplicate clientMessageID → 200
  idempotent. Cap → 429 with retryAfter. The synchronous adapter path is
  the legacy fallback (outbox unset: dev/test).
- `EnqueueMessage`: same accept (single path — client-decides routing
  retired; /queue accepts clientMessageID too).
- `ListQueue`: reads the real outbox (pending + delivering + error with
  retry context); the V2 shadow is the legacy fallback only.
- Worker (`Start`): detached-context `outbox.Run` (1s tick, per-session
  lock, one in flight per session, ≤5 attempts with terminal error,
  `Recover` requeues staged entries at start). Bridge `outboxDeliver`:
  real `adapter.Send` (V1), model selector forwarded in the #917 object
  wire form.
- `app.go`: outbox wired from the existing Valkey client (AOF-persisted
  — accepts survive API and Valkey restarts).

### Frontend

- `useChatStream.send`: one `clientMessageID` (crypto.randomUUID) per
  user message, stable across the 503-retry loop — a retried POST can
  never double-send.
- `SendMessageRequest.clientMessageID` typed.

## Key Decisions

- **LIST + staging over stream/consumer-group**: simpler listing (the
  queue UI reads LRANGE), simpler crash recovery (requeue staging),
  position-preserving retry. Consumer-group XPENDING/AUTOCLAIM would
  have been more machinery for the same at-least-once semantics.
- **202 on first accept, 200 on duplicate**: the client retry loop can
  distinguish "accepted fresh" from "already accepted" without erroring.
- **Legacy paths kept** (sync send without outbox; V2 shadow listing):
  dev/test envs without Valkey keep working; production wires the outbox
  unconditionally.
- **Cross-replica duplicate delivery is possible** (rare; per-session
  lock narrows it) and accepted: at-least-once + render dedupe is the
  stated D3 semantics.

## Assumptions (validated)

1. Valkey AOF persistence covers accepted-undelivered messages across
   restarts (chart: `--appendonly yes`) — checked, not assumed.
2. The adapter Send with a detached 10-min context covers any realistic
   turn (the proxy timeout was 125s; turns complete well under 10 min).
3. Existing queue UI (QueueSection/useMessageQueue) renders the outbox
   entries unchanged (same response shape).

## Tests

- Outbox service (10, `-race`): dedupe, no-cmid no-dedupe, cap, deliver
  success, retry→terminal, error-parks-in-place/order-flows, session
  lock, crash recovery, dismiss/retry, Run end-to-end.
- Handler (6, `-race`, real adapter + fake backend): 202 accept, 200
  dedupe, 429 cap, queue-lists-outbox, worker-delivers-through-adapter
  (model object forwarded), **worker-disconnect-immunity** (accept ctx
  canceled right after 202; delivery still completes — the incident
  class, pinned).
- Full handlers package (98s) + outbox package green; frontend
  useChatStream 29/29 + tsc clean.

## Files Modified

- api/internal/services/outbox/outbox.go (+outbox_test.go) — new
- api/internal/handlers/proxy_handlers.go (outbox branches: prompt, enqueue, listQueue)
- api/internal/handlers/proxy.go (outbox field, setter, test seams)
- api/internal/handlers/proxy_lifecycle.go (worker launch, outboxDeliver bridge)
- api/internal/handlers/proxy_outbox_test.go — new
- api/internal/app/app.go (outbox wiring)
- frontend/src/api/types.ts, frontend/src/hooks/useChatStream.ts (+test)
- design/0050 (D3 final spec — separate commit on this branch)


---

## Round 1 (review on #943): all eleven blockers

1. **Org model-policy bypass fixed**: the outbox branch of both
   SendPromptAsync and EnqueueMessage now runs modelOverrideAllowed on
   the explicit override (403 + slot cleanup on disallowed) — the same
   check as the sync path.
2. **Metering fixed**: outboxDeliver records the same cross-cutting
   events as postAdapterSuccess on delivery success (activity,
   session-index message, llm_request usage event with the accept-time
   userID riding the entry + "outbox:<id>" request id).
3. **Dedupe marker ordered AFTER persistence**: Accept pushes the entry
   first and only then SetNX's the marker (value = entry ID). Capped /
   marshal / push failures leave NO marker — a retry after drain
   accepts. A concurrent same-cmid race resolves to the first acceptor's
   entry (the loser's entry is removed). Pinned by
   TestAccept_CappedWritesNoDedupeMarker.
4. **Staging windows made loss-proof**: stage-out is RPUSH-staging THEN
   LREM-main (crash → both → duplicate, never zero); failure restore is
   main-first THEN staging-LREM; Recover is LRANGE + ID-dedupe against
   main + DEL. Pinned by TestDeliver_CrashWindowBothLists_NoLoss.
5. **Lock fixed**: LockTTL = DeliveryTimeout + 2min (pinned by
   TestDeliver_LockOutlivesLongTurn); owner-token lock with
   compare-and-del release (Lua).
6. **Backoff implemented**: exponential (10s base, doubling, 2min cap),
   NextAttemptAt gate — deliverOne SKIPS future entries rather than
   consuming them; 5 attempts span ~3.5min, covering opencode restart
   cycles. Pinned by TestDeliver_BackoffGateSkipsFutureEntries.
7. **Duplicate carries the original ID**: the marker VALUE is the entry
   ID; Duplicate.AcceptedID flows to the 200 response. Pinned by
   TestAccept_DuplicateCarriesOriginalID.
8. **Dismiss/Retry wired**: DeleteQueueMessage targets the outbox first
   (real removal — the entry will not deliver); POST
   /queue/:messageId/retry re-arms error entries.
9. **Enqueue slot leak fixed**: removeActiveSession on the quota-fail
   path (the #913 class).
10. **AOF honest**: prod already ran --appendonly yes
    (talos-ops-prod valkey helm-release args — verified by file, not
    assumption this time); the repo-local dev dep now does too
    (local/deps-valkey.yaml). The original worklog's "checked, not
    assumed" was FALSE — the check I performed was against the prod
    repo, while the in-repo dev dependency lacked the flag and the
    design text said "in the chart". Both corrected.
11. **Head-of-line starvation fixed**: Run dispatches sessions
    concurrently (bounded semaphore 32; per-session lock serializes
    same-session). Pinned by TestRun_ConcurrentSessionsNoHeadOfLine
    (fast session delivers while a slow turn is in flight).

Non-blocking addressed: dedupe key scoped per (ws,ses,cmid);
ListQueue fills enqueued_at from AcceptedAt. TOCTOU cap and List
unmarshal-swallowing documented as accepted (cap overshoot by a few
entries under concurrent accepts; malformed entries skipped in List).

Service tests: 16 (-race). Handler tests updated for the new Accept
signature + duplicate-with-ID.

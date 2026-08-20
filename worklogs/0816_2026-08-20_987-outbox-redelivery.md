# Worklog: #987 — outbox re-delivery on turn-timeout (sent-once/delivered-3x)

**Date:** 2026-08-20
**Session:** Diagnose and fix the queued-message incident: a message sent once was delivered to the agent three times (~10–12 min apart) and its queue entry never cleared. Issue: #987.
**Status:** Complete (pending review)

---

## Objective

1. Root-cause why a turn outliving the outbox `DeliveryTimeout` caused duplicate deliveries and a permanently-queued entry.
2. Fix at the right layer, following the opencode TUI/server pattern (the transcript IS the queue; clients never retry).
3. Stress-test the fix until it breaks, then fix what breaks.

---

## Root cause (validated against live logs + opencode v1.18.10 source)

- `outboxDeliver` calls `adapter.Send` = V1 `POST /session/:id/message`, synchronous for the whole turn.
- `DeliveryTimeout = 10m` fires mid-turn → `context.DeadlineExceeded` classified as failure → retry re-POSTs the same text as a NEW message (each retry mints a new opencode message ID; the "collapse at render via clientMessageID" mitigation was never buildable — the cmid never reaches opencode).
- After 5 attempts the entry parks as `StatusError` and stays in `List()` forever → "already sent but still queued".
- opencode persists the user message BEFORE the turn starts (prompt.ts `createUserMessage` → `ensureRunning`) and keeps running after client disconnect — so a timeout almost always means delivered-but-unconfirmed. The timeout was being classified "not delivered" when it means "unknown".

## Design decisions (revalidated under Rule 12)

| Decision | Rationale |
|---|---|
| Delivery = "persisted in transcript", not "turn completed" | Matches opencode's own model; the TUI's QUEUED badge derives purely from transcript state and the client never retries |
| Unknown outcomes → `verifying` state + transcript verifier; ambiguous attempts don't count toward MaxAttempts | Blind retry is the duplicate generator; verification resolves unknowns before any re-send |
| Verification implementation lives in `pkg/agent/opencode`; bridge consumes via a locally-defined interface; `agent.Adapter` unchanged | Persist-first is opencode knowledge (single consumer — no premature abstraction) |
| `ErrHTTPStatus` sentinel on `pkg/agent` | HTTP status = definitive rejection is transport-universal; the invariant test (Epic 65 US-65.6) caught my first attempt putting it in the bridge's opencode import — corrected |
| `Recover()` requeues crash-staged entries as `verifying` | Same unknown-outcome class; blind re-send after API crash is the same duplicate bug |
| Rejected: raising DeliveryTimeout; SSE-observation as truth; never-retry-ambiguous | Turns exceed any bound; tracker only lives while a browser watches; regresses D3 disconnect immunity |

## Work completed

1. **Outbox state machine** (`outbox.go`): `verifying` status, `outbox.Ambiguous` wrapper, `Verifier`/`DeliveredHook` seams, verify backoff (`MaxVerifyAttempts`), `LastAttemptAt` send-window anchor, `Recover`→verifying, `Retry` resets `VerifyAttempts`.
2. **Verifier** (`pkg/agent/opencode/verifydelivery.go`): cursor-paged newest-N history scan (`X-Next-Cursor`, 1.18.10) with definitive-absence coverage (page past window start or history exhausted), 2m clock-skew margin, page budget → inconclusive.
3. **Bridge** (`proxy_lifecycle.go`): ambiguity classification (`agent.ErrHTTPStatus` = definitive; else `Ambiguous`), re-send always verifies first (subsumes the D3 r2 tail-25 pre-check), single `OnDelivered` seam → metering/activity/session-index + `queue.update/sent` SSE (previously never emitted by the outbox path).
4. **Frontend**: queue pills surface `verifying` ("Sent — confirming delivery…") / `delivering` ("Sending…"); unknown statuses degrade to pending.
5. **Stress suite** (`outbox_stress_test.go`): multi-replica ambiguity storm (invariant: one POST per entry, one OnDelivered per entry, empty outbox), crash-window recovery under load (staged-only entries complete with ZERO re-sends), concurrent accept dedupe + cap flood.

## Stress-test findings (found and fixed)

| Finding | Fix |
|---|---|
| **`Accept` cap race (production bug)**: LLen-then-RPush check-then-act; 60 concurrent accepts yielded 47 > cap 25 — cross-replica in production | Atomic Lua check-and-push (`acceptScript`) |
| Pre-existing `Run` tests leaked delivery goroutines past test end; raced var-mutating tests under `-race` | Join in-flight deliveries before test return |
| Test-harness mutex held across the fake agent's stall serialized verify against send | Sleep outside the lock |

## Verification

- `go test ./api/... ./pkg/...` green; outbox suite `-race -count=5`; handler lifecycle pins (incident scenario: exactly ONE POST with a turn 3× the timeout) `-count=5`.
- `golangci-lint` 0 issues; `gofmt` clean; frontend `tsc` clean, 152 files / 1685 tests green; my touched files eslint-clean (35 pre-existing errors in the in-flight workflows epic files — left to their owners).
- Incident-class evidence: another session (`42ae0489…/ses_fe5364736ffe…`) has entries parked since 09:51Z/16:10Z — the `error`-parked-forever class this fix eliminates.

## Assumptions stated and validated

1. opencode persists user messages before the turn starts — validated in v1.18.10 source (prompt.ts:1057, session.ts:295) and pinned by `fakeAgentBackend` semantics in handler tests.
2. `GET /session/:id/message` cursor paging (`X-Next-Cursor`/`before`) exists on 1.18.10 — validated in source (session.ts:106-145).
3. Identical-text collisions can false-positive a delivered verdict — accepted, documented (worst case: one missed retry while the text is already in the transcript once).
4. Turns outlive any fixed timeout — validated from the incident transcript (10–13 min turns observed).

## Not done / follow-ups

- The 2 legacy parked entries in production Valkey (`ob_1787219461064271195*`, `ob_1787242203523376684*`): fix lands the semantics; a manual dismiss or the retry UI clears them post-deploy.
- `promptAsync` (fire-and-forget) considered and rejected for delivery: its 204 doesn't guarantee persistence; short-timeout sync send + verification is strictly more robust.

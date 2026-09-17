# Worklog: send_message + abort_session — cross-session management tools

**Date:** 2026-09-15
**Session:** opencode (main dev box), continuing the agentd MCP tool family on 0.30.1.
**Status:** PR iterating.

## Objective

Two tools completing the cross-session management set (owner request):
"send message to session" and "abort session ... if a session needs to be
managed by another session."

## Design (anchored on previously live-proven wire facts)

- **`send_message`** `{session_id, message}` — fire-and-forget delivery to
  an existing session. The reply stays in the target; nothing returns to
  the caller (same philosophy as `create_session`; the task tool remains
  the blocking/returns-result path). Detached V1 POST (`WithoutCancel`);
  busy targets queue server-side and deliver at the turn boundary
  (run-at-boundary semantics, proven during compact's design), reported
  as `delivering_after_current_turn`. Self-send = scheduled next turn.
  Unknown IDs rejected up front via the session list (the caller cannot
  see the detached goroutine's error). No model override, no images —
  text only; the target runs its own configured default.
- **`abort_session`** `{session_id}` — stops the target's current turn
  via the ONE consolidated V1 abort (`Client.Abort`, now sessionID-
  validated — hardening the API-proxy interrupt path that shares it).
  History survives; the in-flight turn is cut DESTRUCTIVELY: queued-but-
  undelivered messages (e.g. a send_message waiting at the boundary) may
  be DROPPED — disclosed in the description, pinned by
  `TestMCPSendMessage_AbortDropsQueued`; idle targets are a no-op.
  Own-session excluded by description (you cannot abort your way out of
  your own turn).

## Assumptions stated, then validated (review round 1 corrected the record)

1. V1 abort route + empty-JSON body — the sessionstate actor's pinned
   regression (opencodeActor Interrupt) uses exactly this shape; seam
   tests pin method/path/body.
2. Busy-target delivery for /message — **initially carried by analogy
   from the summarize route (review finding 1, correctly flagged);
   then proven directly**: `TestLoopbackL2_BusyMessageDeliversAtBoundary`
   holds a real turn open (mock delay), POSTs mid-turn, and observes the
   POST complete at the boundary, the message persist as the next user
   turn, and the target answer it (13.0s). The test plan §2 row now
   records block-then-deliver-at-boundary with both evidence legs.
3. sessionID validation + SessionList existence check — seam pins
   (no-wire-call for hostile IDs) and the up-front 404-equivalent error.
4. Retry-status targets ("retry" after stream errors) treated as busy —
   pinned (`TestMCPSendMessage_RetryStatusTreatedAsBusy`); the advisory
   label's TOCTOU noted in-code.

## Review remediations (round 1)

- Finding 1: L2 boundary-delivery proof added (above); description keeps
  the boundary claim now on direct evidence.
- Finding 2: loss semantics disclosed in the description ("Delivery is
  not retried...") and pinned.
- Finding 3: `Client.SessionAbort` (new duplicate) DELETED; `Client.Abort`
  is the ONE V1 abort — now with sessionID validation (hardens the
  API-proxy interrupt path, which takes caller-supplied IDs), `{}` body,
  and body-bearing errors; the tool uses it; seam pins retargeted.
- Finding 4: test plan scope corrected to NINE tools; §2 busy-row
  reconciled with the L2 proof.
- L3: liveprobe gained a REAL send_message probe (idle POST /message
  200) + abort no-op; the exit gate moved AFTER all probes (the round-3
  append had left the tally before them, breaking the exit contract) —
  10/10 green, exit code correct.
- Retry-as-busy: mcpSendMessage, session_metadata, mcpCompact, AND
  resolveSingleBusySession all treat "retry" as running (the resolver was
  the round-4 straggler); pinned by RetryStatusTreatedAsBusy and
  OmittedID_ResolvesRetryingSession.
- L1 fake: arrival sentinel + abort-drops-queued + refuse-on-wait-
  exhaustion (no vacuous passes); busy test asserts arrive-then-held.
- L2 boundary test: count/order assertions replace the tautological
  MOCK-REPLY contains.

## Tests

- Seam: `TestSeam_Abort_{ExactWire,Non2xx,InvalidID}`.
- Tools: `TestMCPSendMessage_{IdleTarget,BusyTargetQueues,UnknownSession,MissingArgs}`,
  `TestMCPAbortSession_{HappyPath,MissingID,Non2xx}`.
- L1 full-stack JSON-RPC: `TestMCPHandler_{SendMessage,AbortSession}FullStack`.
- Guidance pins for both descriptions; inventory + per-tool auth-gate
  probes updated (13 tools).
- Found + fixed during dev: fakeAgent abort route deadlocked
  (re-locking the handler-held mutex) — noted in the fake.

## Not built (deliberate)

- No synchronous reply-return variant of send_message — that is the task
  tool's job; a second blocking path would duplicate it.
- No delete-session tool — the family manages, it does not destroy
  (stated in abort_session's description).

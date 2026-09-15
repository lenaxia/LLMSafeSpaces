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
  via the V1 abort route (the only interrupt route on pinned agents
  >= 1.18.10; the actor already pins it for ACTION_TYPE_INTERRUPT).
  Non-destructive: history and the delivery ledger are untouched; idle
  targets are a no-op. Own-session excluded by description (you cannot
  abort your way out of your own turn).

## Assumptions validated

1. V1 abort route + empty-JSON body — the sessionstate actor's pinned
   regression (opencodeActor Interrupt) uses exactly this shape; seam
   test pins method/path/body.
2. Busy-queue delivery for send_message — same server-side behavior
   proven for summarize (200 after the generation finished).
3. sessionID validation + SessionList existence check — seam pins
   (no-wire-call for hostile IDs) and the up-front 404-equivalent error.

## Tests

- Seam: `TestSeam_SessionAbort_{ExactWire,Non2xx,InvalidID}`.
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

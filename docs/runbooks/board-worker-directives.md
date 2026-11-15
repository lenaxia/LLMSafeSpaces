# Runbook: Board worker directives (halt/resume)

**Audience:** board workers (claimed stories), orchestrator operators, anyone diagnosing unexpected user-role messages in a worker session.

**Owner issue:** #2046 (directive delivery discipline). **Precedent:** #1747 (pause-must-pause — the directive is a turn-boundary message, never an abort).

## What a directive is

When an owner pauses/resumes a task, epic, or project that has in-flight workers, the orchestrator delivers a **directive** to each affected worker session over the same message machinery as any other chat message: a user-role POST to `/session/:id/message`. On a busy (mid-turn) session the harness queues the POST server-side and delivers it **at the turn boundary** — the worker finishes its current thought, then sees the directive. A directive is **never** a mid-generation cut and **never** an abort.

## The wire shape

Every directive message starts with a machine sentinel line, then prose:

```
<!-- lsp:agent-message-v1 {"mode":"board-directive","directive":"halt","taskId":"tsk-…","auditId":"…"} -->
BOARD PAUSE DIRECTIVE: your task tsk-… has been PAUSED by the owner. …
```

- `directive` is `halt`, `resume`, or `deposed` (see below).
- `taskId`/`auditId` identify the delivery: the same pair keys its audit event (see [Attribution](#attribution)).
- The sentinel follows the `lsp:agent-message-v1` grammar shared with the lease nudge: unknown keys are ignored by older parsers, and a grammar-ignorant worker still gets the full contract in the prose.

## The worker contract

**HALT (`BOARD PAUSE DIRECTIVE`)** — your task was paused:
1. Finish your current thought, then **stop and wait**.
2. Your claim, token, and lease stay yours while paused — do not re-claim, do not restart, do not treat the pause as a failure of your work.
3. Forward stage-writes now refuse with `409 task_paused`; that is expected. Do not retry them until resumed.

**RESUME (`BOARD RESUME DIRECTIVE`)** — the pause lifted:
1. Continue where you stopped.
2. If your claim lapsed while paused (long pauses outlive the lease), your next write returns `409 claim_token`: re-read the task with `board_read`; the engine may already have spawned a fresh worker — in that case stop.

**DEPOSED (`BOARD DEPOSED DIRECTIVE`)** — not a pause lever, but the same transport: your lapsed claim was deposed and a salvage pointer recorded for the next claimant. **Stop working**; your token is revoked; do not re-claim the task yourself.

## Attribution (diagnosing an injection)

Every directive delivery writes a `board_events` row (action `directive`, id `directive:<task>:<auditID>`) carrying task, session, directive kind, and delivery outcome (`dispatched` or `failed` + error). To attribute a user-role message found in a worker transcript:

1. Does it start with the sentinel? Parse `taskId` + `auditId`.
2. Look up `board_events` for id `directive:<task id>:<auditId>` — its payload names the exact delivery.
3. No sentinel and no matching event? The message is not a board directive (candidate senders: `send_message` from a sibling agent — which carries its own `fromSession` sentinel — or a human chat message, which carries neither).

Delivery failures WARN in the API log (`board: worker directive delivery failed …`) and surface in the event payload; the pause/resume itself never fails on an unreachable worker (the flags are authoritative; a lost directive self-heals at lease lapse).

## Known incident notes

- **2026-11-15** (the #2040 wedge incident): directive-era messages were observed persisted with **zero parts** in the harness DB. The platform seam is pinned non-empty + sentinel (`TestBoardWorkerComms_Messenger_DirectiveSentinelAtAdapterBoundary`, `TestLoopbackL2_DirectiveDeliversAtBoundaryWithParts`); the residual loss class lives in the harness's wedged-admission window and is owned by opencode invariant issue #2048.
- An unattributed injection pair with **no** sentinel and **no** `directive` event is outside this path — see #2055 for the 23:46 pair investigation.

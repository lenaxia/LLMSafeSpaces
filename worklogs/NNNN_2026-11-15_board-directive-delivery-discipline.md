# Worklog — board directive delivery discipline (#2046)

**Date:** 2026-11-15
**Branch:** `fix/2040-directive-discipline`
**Issue:** [#2046](https://github.com/lenaxia/LLMSafeSpaces/issues/2046) (parent incident: #2040)

## What

Made board HALT/RESUME/DEPOSED directives first-class: sentinel-marked with a typed payload, audited per delivery in `board_events`, delivered with a bounded outcome verdict, and pinned at the adapter boundary and against the real opencode binary for turn-boundary delivery with parts intact. Documented the worker-facing contract (charter + runbook).

## Assumptions stated and validated (README rule 7)

1. **The messenger's text is non-empty at the platform seam today.** Validated: `newBoardMessenger` composes non-empty prose (`board_worker_comms_test.go` asserted `texts[0]` contains the directive header pre-change) and the adapter POSTs `parts:[{type:"text",text}]` (`pkg/agent/opencode/adapter.go` Send body). Conclusion: the incident's zero-parts rows are lost BELOW the platform seam — inside the harness's admission/persistence (opencode 1.18.15 `createUserMessage` persists message info and parts as SEPARATE durable events; a wedged/interrupted admission window can leave the shell without its parts).
2. **The V1 `/message` route queues server-side on busy.** Validated in opencode source (`SessionRunState.ensureRunning` → `Runner` queues the run effect; `createUserMessage` runs immediately) AND live by the pre-existing `TestLoopbackL2_BusyMessageDeliversAtBoundary`; extended with the directive-shaped `TestLoopbackL2_DirectiveDeliversAtBoundaryWithParts` (sentinel text persists with parts, answered as its own turn).
3. **The nudge audit pattern is the right idempotency shape.** Validated: `reconciler.go` `recordEventTx` with deterministic id + `ON CONFLICT DO NOTHING`; mirrored as `recordDirectiveEvent` (id `directive:<task>:<auditID>`).
4. **A fast-fail window on directive sends does not meaningfully slow pause/resume.** Validated: dead sessions 4xx in <1s (pre-existing #1959 pins document sub-second 4xx; 10s window there covered slow resolves — 3s is the directive bound); healthy/busy targets return `dispatched` at the window, and cascades fan out at concurrency 8 (the nudge sweep's shape). Worst case per pause request: ~3s with all workers unreachable-but-hanging.
5. **The depose path can reuse the cascade seam.** Validated: `DeposeTask` holds task + project in scope at the delivery point; the row's assignee is captured pre-write (the helper's documented contract).

## Investigation: the empty-parts wire shape and the 23:46 pair (R3)

- Platform seam pinned non-empty + sentinel (born-red pin, now green).
- opencode 1.18.15 source walk (vendored clone at tag): message row + part rows are separate durable events (`message.updated`, then `message.part.updated` per part) — the incident's zero-parts rows lost the part events inside the wedged-admission window (`ensureRunning` hang, "No user message found in stream" — the exact #2048 invariant class). Filed under #2048 (its R5 explicitly coordinates with this issue); not fixable from the platform side.
- The 23:46:18/23:46:23 pair into `ses_e2459678` (sushi, mid-turn, the wedged session): **filed as follow-up** with the evidence enumeration. Summary: no board event at 23:46 (cascades fired 23:13:25/23:18:19 only — and pre-#2046 the directive path wrote no per-delivery events and its failures never reached agentd logs, so "no agentd log line" excludes nothing about it); post-#2046 attribution is deterministic (sentinel + `directive:` event per delivery). Remaining candidate senders (all platform-external to the directive path): sibling `send_message` (silent on success, but carries `fromSession` sentinel), human chat via the UI (silent, no sentinel), and — timing-adjacent only — the workspace's `*/15` board-monitor cron (fired 23:45:00, 78s before the first injection; its routine is read-only in its own fresh session, so it requires an additional bug to be the sender — flagged, not claimed).

## Decisions

- **Structured seam over positional args**: `WorkerMessenger(ctx, sessionID, Directive{Kind, TaskShortID, AuditID})` — the audit id must travel with the kind to key the event and the sentinel payload; a struct keeps the seam poor while making an unaudited delivery unrepresentable.
- **3s fast-fail window (not 0, not 10s)**: 0 loses the dead-session 4xx class from the audit payload; 10s (the nudge's) needlessly holds pause/resume requests. 3s balances (assumption 4).
- **One composer, two callers**: `agentMessageSentinelLine(payload)` in `board_liveness.go`; `leaseNudgeText` refactored onto it byte-identically (the existing nudge grammar test stayed green without edits).
- **Charter + runbook for R5**: the charter trailer (both persona and worker arms) states the halt/resume contract; `docs/runbooks/board-worker-directives.md` carries the operator/worker surface including the injection-attribution procedure.

## Adversarial self-review (README rule 11) — findings and dispositions

- **Finding: parallel cascade introduces data races on test fakes.** REAL (the race detector caught `recordingDirectiveMessenger` and the legacy `fakeMessenger` appending from cascade goroutines). Fixed: both fakes are mutex-guarded now.
- **Finding: forgot `Mode` in the sentinel payload (test caught `mode:""`).** REAL — fixed by construction (`boardDirectiveSentinel` constructor is the only way to build the payload; an unstamped mode is unwritable).
- **Finding: `recordDirectiveEvent` writes an event per delivery — could a flood of pause/resumes spam `board_events`?** False alarm by scope: cascades fire only on lifecycle actions (a manager pressing pause), each writing one row per in-flight worker — the same order as the nudge ladder's audit rows, and idempotently keyed per delivery.
- **Finding: the deposed delivery's audit references a task whose assignee was already cleared.** False alarm: the event keys on task id (not the assignee) and the payload records the captured pre-write session — attribution is exact.
- **Finding: two full-suite runs failed once (suspend kill test, three app wiring tests).** Validated as self-inflicted: I ran both suites concurrently against one Postgres — the failures were cross-suite TRUNCATE contention and a PG deadlock, not code. Both suites pass run solo (board `-race` full suite green, app suite green) and the suspend test passes on pristine main too.

## Adversarial review rounds (github-actions reviewer, rounds 1+2 — CHANGES_REQUESTED, same head)

1. `runbook-attribution-id-mismatch` (docs-truth) — REAL: the sentinel carries the task SHORT id while the event id embeds the UUID; the runbook's literal recipe assembled the event id from the sentinel's `taskId` and found no row — the exact mis-attribution the runbook exists to prevent. Fixed: the lookup now keys on `payload->>'audit_id'` (unique per delivery; rides both the sentinel and the event payload), with a worked example and an explicit short-vs-UUID warning.
2. `directive-audit-cancelled-ctx-lost` (robustness) — REAL: the audit write rode the cancelable request ctx; a client disconnect after the pause commit silently dropped the row for a genuinely dispatched directive (the delivery itself survives via WithoutCancel — the audit now must too). Fixed red-first: `TestKill_2046_AuditSurvivesCanceledCtx` born red (0 rows), then green — `recordDirectiveEvent` runs on `context.WithoutCancel(ctx)` bounded at 10s, and the rollback rides the same detached ctx.

## Tests (TDD)

- Born-red: `board.Directive` undefined at both seams (compile-red), then the sentinel `mode` assertion caught the unstamped payload before any manual check.
- `pkg/board/directive_delivery_test.go` (integration): task audit trail, failure WARN+payload+never-fails, project cascade (2 workers × pause+resume = 4 events, unique ids), epic cascade + deposed, idempotent replay.
- `api/internal/app/board_directive_comms_test.go`: sentinel grammar + prose for all three kinds, shared-composer shape, adapter-boundary non-empty+sentinel (the R3 born-red pin), busy-target queue-at-boundary (8s hold vs 3s window), dead-session 4xx synchronous outcome.
- Extended: `#1747` kill tests (structured fake + sentinel text assertions), halt/resume/deposed/phrase comms pins (new signature + sentinel), charter contract pin (both arms).
- Loopback (real opencode 1.18.10 binary): `TestLoopbackL2_DirectiveDeliversAtBoundaryWithParts` — sentinel directive to a busy session delivers at the boundary, persists with parts, answered as its own turn.
- `-race` clean; suites green solo (see the finding above for the one parallel-run artifact).

## Follow-ups

- The 23:46 pair attribution issue (filed alongside this PR; see the runbook's incident notes).
- opencode #2048 owns the wedged-admission zero-parts reproduction and fix; this PR's loopback pin is the healthy-path evidence boundary.

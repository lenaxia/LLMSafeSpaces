# 0071 — Session idle-subscriptions: subscribe/unsubscribe/list + the notifier

**Status:** Approved & implemented (2026-10-26/27) — owner-settled in-session (rulings 1–4 below); the approved design document lives verbatim in the charter workspace (`/workspace/reports/session-subscription-design.md`); this file is the repo-conventions rendering.
**Date:** 2026-10-27
**Issues:** #1574 (derived busy — never key on raw status strings) · #1722 (the synthetic evidence settles a harness-SSE hook would miss) · #1756 §1 (phrase-first id emission) · design/0055 (the sealed sessionstate authority) · #1832/#1849 (emission-corruption and atomic-create disciplines the MCP surface follows)
**Affects:** `cmd/workspace-agentd/{session_notifier,session_notifier_store,session_notifier_delivery,session_notifier_metrics,mcp_server,mcp_tools,main}.go` + tests · `runtimes/opencode/plugins/llmsafespaces-origin.js` (identity supply) · `CHANGELOG.md`. No proto/ABI, controller, or frontend changes.

---

## 1. The problem

Nothing pushes "worker finished" into an idle session's thread: managers poll `board_list`/`board_health` because the platform has no wake-up primitive between sessions. `send_message` is pull-by-the-sender; a subscriber that wants to know when a broadcaster's turn ends has to keep asking.

## 2. The two assessments (settled pre-implementation)

- **Subscribe by ID or bulk by tags? → ID-only for v1.** Sessions carry no tag field on any surface (platform contract, opencode wire, MCP metadata); a session-tag store would be a feature of comparable size before the feature it serves. The natural fan-out is a manager watching a handful of named workers. Registry rows carry an `events` set keyed by canonical broadcaster id — a future tag index expands to ids at subscribe time (additive, not a schema change).
- **Event-broadcast mechanism? → an in-process consumer on `Authority.Stream`.** The session-state authority already is an ordered event bus: every state-changing path — harness SSE folds, the #1722 evidence settles, lease rederives, orphan aborts — crosses `applyLocked` → `fanoutLocked` and reaches `Stream` subscribers seq-assigned. The notifier is a sibling consumer (like the API usagestream consumer): `sessionstate` stays sealed, the 5-op ABI frozen, no 6th op. Hooking the harness SSE tracker instead would miss the synthetic settles — the exact #1722 bug class.

## 3. Architecture

```
opencode harness (SSE /event) → sessionstate.Authority (fold → applyLocked → fanoutLocked)
    │ Stream(ctx) — ordered frames + stamped snapshot
    ├─ API usagestream consumer (existing)
    └─ session_notifier (NEW, cmd/workspace-agentd):
         transition tracker (derived busy per #1574, saw-busy latch,
           pending-input gate, subtask filter)
         suppression tracker (last completed send_message seq per target
           vs last mutating edit/write/bash seq)
         debounce timer per broadcaster (5s, AGENTD_NOTIFY_DEBOUNCE)
         subscription registry (durable JSON + latches + outbox)
         delivery: detached V1 POST /session/{id}/message
           (SessionSend + agentmessage sentinel, mode "notify")
```

Placement: `cmd/workspace-agentd/session_notifier.go` (+ store/delivery/metrics files), wired in `main.go` beside the authority wiring. Files: `session_notifier.go` (consumer loop + trackers), `session_notifier_store.go` (registry JSON), `session_notifier_delivery.go` (seam-POST vehicle), `session_notifier_metrics.go` (counters).

## 4. Subscription registry (durable)

`SubscriptionRow{Broadcaster, Subscriber, Events, CreatedAt}` — canonical harness ids; phrases for humans. Store: ONE JSON file at `/sandbox-runtime/session-subscriptions.json` (the shortids replica's storage class — atomic write + fsync; survives restart and pod suspend/resume). Persisted alongside: per-broadcaster latch state and a pending-notification outbox.

Registration guards: broadcaster exists (`SessionExists` + did-you-mean), neither side is a subtask (`ParentID == ""` from the opencode session list — ParentID is absent from the authority projection), no self-subscription, no cycle (DFS over the tiny graph — mutual A↔B would ping-pong forever), subscriber cap 32 per broadcaster. Auto-removal: broadcaster archived/deleted → rows dropped lazily.

## 5. Transition tracker (v1 filter = idle)

Derived busy per #1574 (`streaming || activeParts > 0 || queueDepth > 0`) — never raw status strings (the store reader is idle-biased). **Emit** when derived busy flips `true → false` and: final status IDLE **or ERROR** (ruling 1 — error settles notify with distinct wording), no pending user inputs (a session blocked on a QUESTION is not "complete"), broadcaster not a subtask. **Unknown→idle**: the saw-busy latch — `busy → … → unknown → idle` notifies; a fresh projection's `unknown → idle` never does. `Reseeded` frames rebuild derived state from the stamped snapshot, emit nothing.

## 6. Debounce, suppression, delivery

- **Debounce** (ruling 3): 5s per broadcaster (`notifyDebounceDefault`), `AGENTD_NOTIFY_DEBOUNCE` Go duration string; malformed → warn + default. Re-busy inside the window cancels (idle→busy flutter); otherwise one notification per busy episode.
- **Suppression** (already-notified): per broadcaster B, track the last **completed** `send_message` ToolPart's target (parsed from tool input, resolved via the short-id replica) and the last `edit`/`write`/`bash` part end (**completed or errored** — an errored bash may still have mutated; false suppression is the costly case). Suppress iff `lastSend[B][S] > lastMut[B]`: B's last word to S postdates B's every mutation — S already holds the final state.
- **Delivery** (ruling 4, Option A): detached V1 `POST /session/{id}/message` via `context.WithoutCancel` — the `send_message`/board-directive pattern; a busy subscriber queues server-side and the wake-up lands at its turn boundary. Body stamped with the v1 provenance sentinel `<!-- lsp:agent-message-v1 {"fromSession":"<phrase>","mode":"notify"} -->`, additive mode. **At-least-once, duplicates tolerated**: intent is written to the outbox before the POST, cleared on success; on restart the outbox is retried once (after the first successful harness session-list pull — never spending the retry on a harness that is not up), then dropped loudly. Frame-loss resilience: if fanout drops the notifier as a slow consumer, the resubscribe path gets a fresh stamped snapshot and reconciles latch-vs-snapshot (latched busy + snapshot idle/error ⇒ the settle) — no SSE replay needed. Rate bound: per-subscriber token bucket (60/h storm backstop).

## 7. Subtask exclusion (both sides, automatic)

Signal: `ParentID != ""` (task-tool children), refreshed from the opencode session list on a 60s ticker (the only surface carrying it). As broadcaster: never notify (the parent gets subtask results synchronously). As subscriber: the subscribe tool refuses subagent callers loudly; `create_session`'s `subscribe` flag from a subagent caller is ignored with a warning.

## 8. MCP surface

| Tool | Change |
|---|---|
| `subscribe_session` | NEW — `session_id` (short/canonical), `events` ⊆ `{"idle","error"}` (closed vocabulary, default both). Guards §4; idempotent re-subscribe updates events. Phrase-first id emission (#1756 §1). |
| `unsubscribe_session` | NEW — idempotent; absent edge → `{"status":"not_subscribed"}`. |
| `list_subscriptions` | NEW (ruling 2) — the caller's outbound edges (or a named session's), phrases + event sets. |
| `create_session` | MODIFIED — `subscribe` bool (default false, schema keeps `prompt` LAST): the caller is subscribed to the new session; return gains `"subscribed": true`. |

Caller identity is `lsp_injected_session` ONLY (the #1849 send_message posture — the `resolveCallerSession` heuristic is deliberately unused: a wrong guess would register a wrong durable edge). The origin plugin's injection set supplies identity for all three new tools + `create_session` (pinned by `TestMCPBoard_ManagerInjectionSupply`).

## 9. Rulings (resolved 2026-10-26)

| Ruling | Call |
|---|---|
| 1. ERROR transitions | notify, distinct "ended its turn in error" wording; `events` accepts `"idle"`, `"error"` |
| 2. `list_subscriptions` | included |
| 3. Debounce | 5s default, `AGENTD_NOTIFY_DEBOUNCE` |
| 4. Delivery durability | detached POST + persisted outbox, duplicates tolerated; the ledgered `Deliver` op is the documented upgrade path |

## 10. What does NOT change

`sessionstate` sealed (zero edits — the notifier is a consumer). `send_message`/session/board tool semantics untouched. No proto/ABI/frontend changes. The board stays the source of truth for task state; subscriptions only wake managers — workers still self-report via `board_move_stage`.

## 11. Tests (repo discipline)

Unit: tracker truth table (flips, latch, pending-input gate, subtask filter, Reseeded resync), suppression matrix, debounce cancel, registry guards, store round-trip. MCP: 20 handler tests over the real `mcpHandler` HTTP surface (phrase parity, injection-only identity, guards, idempotency, `create_session` flag + subagent warning). Integration (`session_notifier_wiring_e2e_test.go`): the full production wiring (real authority via `newStateAuthority` against a fake harness HTTP server, `wireSessionNotifier`/`startSessionNotifierLoops` as `runMain` invokes them, real seam-POST vehicle; events via `Authority.IngestForTest` — the `reconcile_publish_1722_test.go` pattern): (a) harness idle settle AND #1722 evidence settle each ⇒ exactly one debounced POST; (b) ERROR wording; (c) suppression both directions; (d) debounce cancel; (e) drop + resubscribe ⇒ latch reconciliation, no loss/dup; (f) restart ⇒ outbox retried exactly once, latches survive.

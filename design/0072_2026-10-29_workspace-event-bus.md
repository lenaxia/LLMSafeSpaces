# 0072 — Workspace event bus: one Redis Stream, many consumer groups

**Status:** Implemented (2026-10-29).
**Date:** 2026-10-29
**Issues:** design/0069 (the authority frame stream the bus formalizes) · design/0071 (idle-subscriptions v1 — the delivery layer this evolves) · #1574 (derived busy) · #1970/#1977 (the SSE guarantees + seamless-lease lanes whose consumers join next)
**Affects:** `pkg/eventbus` (NEW) · `cmd/workspace-agentd/{workspace_eventbus.go,session_notifier.go,session_notifier_metrics.go,mcp_server.go,mcp_tools.go,main.go}` + tests · `CHANGELOG.md`. No proto/ABI, controller, or frontend changes.

---

## 1. The problem

Subscriptions v1 (design 0071) woke idle managers with a point-to-point
delivery lane: per-subscriber POSTs, a hand-rolled outbox
(write-intent → POST → clear → retry-once), and exactly ONE consumer —
the notifier. The lease-nudge lane (0.44.7) built the same shape again
(event → SessionSend translator) on the API side. Every future
observer (audit → board_events, SSE → UI live updates) would hand-roll
its own fan-out, its own retry, its own resume — the owner's "per
tenant MQTT almost?" question named the gap: many independent
consumers need ONE pipeline.

## 2. The design (owner direction, 2026-10-29)

Build a **bus, not a protocol**. The frame stream already generates
the events; formalize them into a **topic vocabulary** with **Redis
Streams** as the transport, and let consumers subscribe independently
via **consumer groups** — the fan-out is the group set, not a
per-consumer wire protocol.

```
                    ┌─→ notifier consumer → SessionSend (subscription notifications)
frame stream ──→    ├─→ lease consumer → nudge sentinel SessionSend (the NudgeSender generalization)
(session events)    ├─→ audit consumer → board_events      (API joins: roadmap §9)
                    └─→ SSE consumer → UI live updates     (API joins: roadmap §9)
```

### Topic vocabulary — a convention, not an enum

Topics are dot-separated segments; the launch set:

| Topic | Producer | Payload fields |
|---|---|---|
| `session.{id}.idle` / `session.{id}.error` | agentd (the notifier's settle fold) | `session`, `settle`, `title` |
| `task.{id}.stage_changed` | API board engine (future) | `task`, `from`, `to` |
| `lease.{task_id}.nudged` | API nudge ladder (future) | `task`, `task_short_id`, `nudge_id`, `assignee` |
| `lease.{task_id}.wedge_verdict` | API nudge ladder (future) | `task`, `assignee`, `verdict` |

New topics join by producer↔consumer agreement — never by editing a
switch in `pkg/eventbus`. Subscriptions are PATTERNS: `*` matches one
segment (`session.*.idle`), trailing `**` matches the rest; an empty
pattern list subscribes to everything.

### Stream: one per workspace

Key `workspace:{id}:events` (workspace-scoped by construction — a pod
can only append to and read its own workspace's log). `XADD MAXLEN ~
10_000` on every publish: the retained window AND the memory bound in
one knob; no trim side-loop.

### At-least-once: consumer groups, not outboxes

Each consumer type is one Redis consumer group (`notifier`, `lease`,
later `audit`, `sse`). XREADGROUP delivers, XACK retires; a handler
error leaves the entry PENDING — the reclaim sweep (XPENDING idle →
XCLAIM) redelivers it, with the PEL's durable delivery count feeding a
poison cap (5 attempts → loud drop). This is v1's hand-rolled outbox
semantics (intent-before-send, retry, duplicates tolerated) for free.
A consumer restarting under its own name first drains ITS pending
(XREADGROUP `0`) — no MinIdle wait.

### Replay: the retained-message equivalent

A NEW group created at `0-0` reads the retained window (bounded by
MAXLEN) before the live tail — a late subscriber sees what it missed.
New groups default to `$` (live only: a notifier woken by history
would re-notify settled episodes). Existing groups resume at their own
last-delivered id — Redis keeps it.

## 3. What changed in agentd

- **Producer leg**: the notifier's settle fold IS the frame→topic
  mapping for session topics — `debounceFired` publishes ONE
  `session.{id}.{idle|error}` event per settle when the bus is wired
  (payload: session, settle, title). Publish failure falls back to the
  direct v1 leg — a lost wake-up is the failure that matters.
- **notifier consumer** (`ConsumeSettleEvent`): the bus message becomes
  the SAME per-subscriber deliveries — identical suppression (§4.4),
  rate bound, compose, subtask exclusion; the ONLY difference is the
  transport. Hard failures return error → no ack → redelivery (the
  retry the v1 outbox owned). Rate refusals and suppressions are final
  outcomes (acked). The outbox is untouched on this path — the stream
  entry is the durable intent.
- **lease consumer**: `lease.{task_id}.nudged` → the sentinel nudge
  (the exact grammar the API's nudger composes — byte-parity pinned by
  test) via SessionSend. `wedge_verdict` → log only (the row is
  reclaimed at the engine; messaging a wedged worker is noise). The
  API's nudge ladder becomes a PUBLISHER of these topics next (§9);
  the consumer stands ready and pinned.
- **Env gate**: `AGENTD_EVENTBUS_ADDR` (redis:// URL or host:port) +
  `WORKSPACE_ID` wire the bus; unset, unscoped, malformed, or
  unreachable → nil bus → v1 point-to-point verbatim (the default, and
  the fallback — the delivery layer never depends on a bus it cannot
  trust). Consumer name: pod hostname (`AGENTD_EVENTBUS_CONSUMER`
  override).
- **MCP surface**: `subscribe_session` gains the optional `topics`
  alias — `session.<id>.idle|error|*` addressing the SAME broadcaster,
  unioned into the v1 `events` set. Foreign families (`lease.*`,
  `task.*`) refuse loudly: arbitrary pattern subscription is a future
  `subscribe_events` tool. The existing tools stay compatible
  (default behavior unchanged).

## 4. What is preserved (the charter's list)

- Suppression tracking, rate limiting, notification compose — all
  bus-agnostic, all in the consumer; the tests pin the same semantics
  through both delivery layers.
- The MCP tools' surface — extended with the topics alias, not
  replaced.
- The frame stream as the event source — the bus formalizes its
  derived events; sessionstate stays sealed (zero edits).
- SessionSend as the delivery mechanism — the bus routes to CONSUMERS;
  consumers deliver to sessions (the harness-specific edge the bus
  deliberately does not abstract).

## 5. Delivery guarantees, stated plainly

- **At-least-once** per consumer group; duplicates are possible after
  a redelivery and are tolerated by contract (advisory wake-ups,
  latest-only ordinal anchors).
- **Ordering** within a group follows stream order (seq-assigned by
  XADD); across groups there is no ordering guarantee (independence is
  the point).
- **Retention**: ~10k entries (MAXLEN approx). A group whose cursor
  was trimmed resumes at the stream head — the at-least-once contract
  is unaffected (an acked entry needs no redelivery; a pending one is
  delivered from the PEL's pointer if the entry survives, acked-blind
  if it does not — `dispatch`'s values-less shell arm).
- **Poison**: 5 delivery attempts, then a loud drop (an observability
  hook, not a dead-letter queue — documented as such).

## 6. Deployment posture (honest)

Redis is an API-side dependency today; workspace pods do not carry
Redis credentials or NetworkPolicy routes. The bus is therefore
OPT-IN at the agentd edge: `AGENTD_EVENTBUS_ADDR` unset (the default)
keeps v1 point-to-point. Turning it on in a cluster is (a) a helm
change routing Redis to workspace pods, (b) setting the env — no code
change. The transport (`pkg/eventbus`) and both consumers are fully
covered by miniredis tests regardless.

## 7. Tests (repo discipline)

- `pkg/eventbus` (miniredis): vocabulary + pattern matching units;
  round-trip; two-groups fan-out; pattern filter acks-unread; handler
  error → pending → reclaim redelivery (the at-least-once pin);
  crashed-consumer reclaim by an heir; same-name restart resumes own
  pending; replay-from-`0-0` for a new group (+ live tail after);
  `$` skips history; MAXLEN cap; poison drop; OnError/OnPoison hooks;
  options validation; dead-Redis construction refusal.
- `cmd/workspace-agentd`: env gate units (5); consumer name; topics
  alias units; lease sentinel byte-parity vs the API's grammar;
  unknown-arm ack; `TestBusE2E_*` — the FULL wiring battery over the
  real authority + real bus + real consumers: settle publishes once
  and the notifier consumer delivers (v1 wording, outbox untouched,
  ack drains); suppression through the consumer (same §4.4); POST
  failure → pending → redelivery after heal; lease nudge delivery;
  wedge verdict logs-not-messages. The v1 wiring battery
  (session_notifier_wiring_e2e_test.go) passes unchanged — Bus nil is
  the default and the fallback.

## 8. Rulings embedded

- **The producer rides the notifier's fold** (no duplicate
  transition-tracker): the settle topics ARE the frame events this
  pod derives; other frame-derived topics extend the same publisher.
- **Suppression stays in the consumer**: its ledger folds the frame
  stream in-process; the settle message only triggers the decision.
  This is what "the logic is bus-agnostic and survives unchanged"
  means operationally.
- **No agentmessage.Compose on the lease sentinel**: the sentinel
  carries its own marker grammar (mode `lease-nudge`, the ordinal
  anchor `nudgeId`); the v1 compose's fromSession provenance does not
  apply to a system-originated sentinel.

## 9. Roadmap (the next consumers, in order)

1. **API audit consumer**: all topics → board_events entries (a
   workspace-events variant keyed to the project; replaces per-write
   event calls only if it proves LESS indirect — the charter's own
   escape hatch).
2. **API SSE consumer**: all topics → UI live-update frames (the
   activity feed becomes a bus consumer).
3. **API nudge-ladder publisher**: `newBoardNudger` publishes
   `lease.{task}.nudged` instead of direct-sending; agentd's lease
   consumer (this design) is already running.
4. **`subscribe_events` MCP tool**: arbitrary topic patterns for
   sessions that want them (the v1 vocabulary's `topics` alias is the
   bridge).

# Worklog: bus health — metrics and visibility for the workspace event bus (S3)

Task `tsk-misty-keystone-rambles` (epic: Liveness and observation: every
claim observable, every lapse visible — the 2026-10-30 incident
remediation, S0-S7). The design-0072 workspace event bus is ARMED and
live in prod since v0.44.10, but it has no health surface: silence on
the bus is indistinguishable from "nothing happened"; a stalled group
or a dropped publisher would be invisible. Before anything else moves
onto the bus (S4/S6), its observability must exist. This is that
surface — build-only, no tool or behavior change on the delivery paths.

## The health model (stated once, pinned everywhere)

The design premise: **a quiet bus is healthy** — no workspace activity
means no publishes, empty groups, all-zero gauges. Silence alone is
not a fault; silence PLUS a growing backlog is. The surface therefore
separates the three failure shapes an operator must tell apart:

1. **Silent publisher** — settles stop reaching the stream. Signal:
   `workspace_agentd_eventbus_last_publish_timestamp_seconds` going
   stale (alert `time() - ts > quiet period` while sessions are busy)
   and `publishes_total{outcome="failed"}` rising (each failure falls
   back to the direct v1 delivery leg — wake-ups still flow; the
   counter is the degradation signal, not an outage).
2. **Dead consumer group** (nobody consuming) — `group_consumers == 0`
   with `group_lag > 0`: every publish grows the backlog forever.
3. **Wedged consumer group** (names alive, loop dead) —
   `group_oldest_pending_idle_seconds ≥ 300`: entries were delivered
   but sat un-acked through five reclaim sweeps (the healthy reclaim
   threshold is 60s). Consumer names persist after pod death, so the
   NAME count is a lie; the pending IDLE is the truth.

Plus the failing-but-alive shape (not a stall):
`deliveries_total{outcome="handler_error"|"poison"}` rising while
lag/pending cycle — the group consumes but its work fails; a
downstream fault, not a bus fault.

`group_stalled{group}` is the derived 0/1 (classifier thresholds:
no-consumers-with-backlog, pending idle ≥ 5m, lag ≥ 100), edge-logged
`event bus: consumer group STALLED` / `recovered` on transition so the
log line exists exactly when the state changes — never per tick.

## Delivered

### `pkg/eventbus` — the transport-level seams (metrics-agnostic)

- **`health.go` (NEW)**: `StreamHealth`/`GroupHealth` +
  `RedisBus.Health(ctx)` — one snapshot call answering "is the bus
  alive and is every group keeping up": stream length, newest entry's
  publish time, and per group the consumer count, pending count,
  last-delivered cursor, COMPUTED lag (entries after the cursor,
  exclusive-range XRANGE pages, capped at 10k), oldest pending idle,
  and max pending delivery attempts.
  - **Why generic `Do` parsing**: go-redis v8's structured
    `XInfoGroups` hard-requires the Redis 6 reply shape (exactly
    name/consumers/pending/last-delivered-id) and ERRORS on the
    `entries-read`/`lag` fields Redis 7+, Valkey, and miniredis all
    return — the structured helper is version-brittle by
    construction. Health parsing goes through the generic reply,
    ignores unknown fields, and therefore survives server drift.
  - **Why lag is computed, not trusted**: XINFO's `lag` field means
    different things across server versions and mocks (miniredis
    reports the whole stream length as lag); counting entries after
    the cursor is identical everywhere. A replay group at cursor
    `0-0` counts the whole stream — the true answer (every entry is
    undelivered for it).
  - A stream that does not exist yet is a ZERO snapshot, not an
    error: an empty bus is healthy until a backlog says otherwise.
- **`eventbus.go`**: the delivery-outcome vocabulary —
  `DeliveryOutcome` (delivered / filtered / poison / handler_error /
  ack_error) + `DeliveryEvent` (group, consumer, topic, durable
  attempts) — the consume-counter seam.
- **`redis.go`**: `ConsumeOptions.OnDelivery` (every dispatch outcome,
  after it settles) and `RedisBus.SetPublishObserver` (every publish
  outcome: success with id, validation and transport failures) — both
  plain function hooks, both documented observer discipline (tick a
  counter, never block). `dispatch` now resolves exactly one outcome
  per pass; `Publish` observes both failure arms.

### `cmd/workspace-agentd` — the Prometheus surface

- **`eventbus_metrics.go` (NEW)**: the bridge (the
  session_notifier_metrics plumbing class — promauto on the default
  registry, the admin port :4098/metrics the PodMonitor already
  scrapes; pkg stays prometheus-free):
  - Counters: `workspace_agentd_eventbus_publishes_total`
    {workspace_id, topic_family(session|task|lease), outcome},
    `…_deliveries_total` {workspace_id, group, outcome},
    `…_redeliveries_total` {workspace_id, group}.
  - Gauges: `…_stream_entries`, `…_last_publish_timestamp_seconds`,
    `…_group_lag`, `…_group_pending`, `…_group_consumers`,
    `…_group_oldest_pending_idle_seconds`, `…_group_stalled`.
  - `classifyGroupStall` (the health model, one switch) +
    `collectEventbusHealth` (one snapshot → all gauges + edge logs,
    probe failures logged at a bounded cadence: first, then every
    10th) + `runEventbusHealthLoop` (first snapshot at startup so
    gauges exist from the first scrape, then every 30s).
- **`workspace_eventbus.go`** wiring: `eventbusFromEnv` sets the
  publish observer at bus construction (both pod topologies get it —
  the sidecar wiring reuses the same function);
  `runEventbusConsumers` attaches the delivery observer to both
  groups (notifier + lease) and starts the health loop alongside
  them. No behavior change on any delivery path — the hooks observe,
  they never act.

## Tests (red-first where a behavior existed to bite)

- `pkg/eventbus/health_test.go`: the shapes the signals are built
  from — empty stream (zero, not error), last-publish timestamp from
  the newest entry's `at`, the dead-group backlog (lag 5, consumers
  0), the drained healthy group, the wedged group's pending idle
  growing with a deterministic miniredis clock advance (10-minute
  jump, no sleeps), the lag probe's 10k cap, the per-group fan-out
  view, and both observer seams (publish success/validation/transport;
  delivered/filtered/handler_error/poison + the Attempts>1
  redelivery).
- `cmd/workspace-agentd/eventbus_metrics_test.go`: the counter
  arithmetic (outcomes, topic families, redelivery ticks), every row
  of the stall-classifier table (healthy quiet, healthy transient
  backlog, healthy fresh pending — the false-positive guards — plus
  all three stall shapes), and the collector over a REAL bus:
  dead-group → stalled=1 + exactly one WARN (second collect does not
  repeat; draining recovers with the Info line), wedged-group →
  pending-idle crosses the horizon with a live consumer name still
  registered (the "names lie, idle tells the truth" shape) and the
  reason field names `pending_idle_past_reclaim`, silent-publisher
  timestamp (absent on empty, set from the entry), probe-failure log
  bounding (streak 1 and streak 10 fire, 12 probes → 2 logs).
- Existing suites re-run green: full `pkg/eventbus`, the agentd bus
  e2e family (`TestBusE2E_*`, `TestEventbus*`, sidecar wiring) — the
  OnDelivery/health-loop wiring changed their code path without
  changing their outcomes.

## Docs

- `docs/operator/monitoring.md`: the "Event bus (design 0072, S3)"
  metric table + the health-model triage section (what a healthy bus
  looks like, each signal's meaning, the three stall shapes) + four
  PromQL examples (stalled groups, silent publisher, publish
  failures, falling-behind groups).

## Honest notes

- The health loop has no interval env knob — 30s is a constant. If an
  operator needs faster gauges during an incident, the collector is
  one function call away from a knob; none was added because no
  consumer asked (the charter says surface, not tune).
- Lag/pending gauges are per workspace POD (each pod reports its own
  workspace's stream — one stream per workspace, one agentd per
  workspace, so the per-pod view IS the per-stream view; the
  `workspace_id` label is the join key).
- The classifier thresholds (5m pending idle, lag 100) are constants
  with the reasoning written next to them; they classify, they do not
  page — alerting rules are the operator's PromQL away (four starter
  queries shipped in the doc).

## The shared-tree incident (honest note, 2026-10-31)

This lane initially worked in the shared /workspace/LLMSafeSpaces
checkout (branch eventbus-bus-health off origin/main a345b8101). The
agentd-version-visibility lane (tsk-linear-vignette-dines) then
switched THAT checkout to their branch and staged everything — both
lanes' uncommitted work landed in one tree/index. Resolution: this
lane moved to an isolated worktree (/tmp/opencode/bus-health-wt, the
repo's lane convention visible in `git worktree list`), copied its
files out, and restored the shared tree to exactly the other lane's
commit 91daf475a (their CHANGELOG entry and COORDINATE row preserved;
this lane's block/row/files removed from their tree and index before
their commit landed — verified their commit contains zero eventbus
files and zero bus-health text). No work was lost on either side.
Lesson applied: lanes take their own worktree from the START.

## Review round 1 (AI reviewer, CHANGES_REQUESTED → remediated)

Two REAL bugs found, both mine, both reproduced by the reviewer before
I saw them:

1. **Pending idle inflated ×10⁶** (`health.go` probePending):
   go-redis v8's `XPendingExt.Idle` is already a `time.Duration`
   (converted from wire ms at the reply boundary); my
   `time.Duration(p.Idle) * time.Millisecond` re-multiplied it — true
   2m idle reported as ~3.8 years, the `oldest_pending_idle` gauge
   worthless, and `classifyGroupStall` tripped on any non-empty PEL
   (threshold 5m = 3e11ns ÷ 1e6 = 0.3ms true idle). The exact
   wedged-group signal this PR exists for was a false-positive
   generator. Fixed: `p.Idle` verbatim.
2. **MaxPendingAttempts off-by-one**: Redis's XPENDING delivery count
   is 1-based; my `RetryCount+1` made a first delivery report 2,
   contradicting the DeliveryEvent.Attempts vocabulary. Fixed: verbatim.

**Why my own suite missed both (the reviewer's meta-finding, accepted)**:
the idle pins asserted LOWER bounds only (trivially satisfied by
inflation) and the "healthy fresh pending" row was vacuous — frozen
clock made idle exactly 0 (0×10⁶ = 0). Remediation: the wedged pin is
now ABSOLUTE (2m clock jump → idle ∈ [2m, 2m+30s], upper bound catches
any unit inflation), the healthy row carries NONZERO idle (30s < 5m
horizon → unstalled, with a lower bound > 1s), attempts==1 is pinned
after exactly one raw delivery, and the two dispatch edges live
miniredis cannot reach deterministically are pinned directly
(DeliveryAckError via a forced command error; nil OnDelivery
behavior-preserving). Plus the reviewer's minor: the deliveries help
text now states outcomes count per dispatch pass (a redelivered
filtered/poison entry re-counts).

Reviewer's non-blocking notes, taken as documented: SetPublishObserver
is not called on the unreachable "bus unwired" branch; vanished groups
hold stale gauge series until restart (groups are never destroyed in
this system).

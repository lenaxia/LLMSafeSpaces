# NNNN — Workspace event bus on Redis Streams (design 0072)

Task `tsk-handsome-refectory-picnics` (epic: Session Subscription Feature).
Owner direction 2026-10-29: "For sessions sub would it make sense to basically
have a per tenant mqtt almost?" — YES conceptually, but build a bus, not a
protocol: the frame stream already generates the events; formalize them into a
topic vocabulary with Redis Streams as the transport, and let consumers
subscribe independently. Design: `design/0072_2026-10-29_workspace-event-bus.md`.

## Dependencies & branch construction (the honest part)

The two dependency tasks (`tsk-spherical-chalk-seizes` — notifier
suppression/delivery/metrics, `tsk-viridian-vehicle-folds` — MCP surface; board
stages done/in_review) had NOT landed on origin/main — their work lives
uncommitted in the charter workspace's shared tree, mixed with three other
lanes' edits to the same files. This PR carries the v1 lane forward VERBATIM
as its first commit (feat: session idle-subscriptions v1), extracted surgically:

- Wholesale (subscription-only diffs vs origin/main): the 9 new
  `session_notifier*`/`mcp_session_subscriptions_test.go` files, design/0071,
  worklog 1233, `main.go`, `mcp_tools.go`, `mcp_tools_test.go`.
- Hunk-by-hunk (files shared with other lanes): `mcp_server.go` (4 subscription
  hunks; 10 stale-base reversals dropped), `llmsafespaces-origin.js` (the
  injection-set hunk; the board_validate_file/get_config hunks were stale-base
  artifacts — origin/main already carries them), `session_ref_test.go` +
  `session_phrase_stability_1850_test.go` (call-site updates for
  mcpCreateSession's new subscribe/injected params only).
- The board-activity loop call site in main.go belonged to a foreign lane —
  removed; `boardWorkerSelfCertificationPin` routing hunks likewise.

v1's own tests pass unmodified on this base — the evolution claim starts from a
green baseline.

## Delivered

### `pkg/eventbus` (new package)

- **Vocabulary as convention** (`eventbus.go`): topic builders
  (`SessionSettleTopic`, `TaskStageTopic`, `LeaseNudgeTopic`, `LeaseWedgeTopic`),
  parsers, segment-glob matching (`*` one segment, trailing `**`, empty list =
  subscribe-all), `StreamKey(workspace)` = `workspace:{id}:events`.
- **Transport** (`redis.go`): `RedisBus` over go-redis v8 (already in go.mod —
  pkg/secrets + the API's Redis services). Publish = XADD with `MAXLEN ~
  10_000` on every append (retained window + memory bound, no trim loop).
  Consume = consumer groups: XGROUP CREATE MKSTREAM at `$` (live) or `0-0`
  (replay), same-name restart drains its OWN pending first (XREADGROUP `0`),
  steady loop = XPENDING(idle)+XCLAIM reclaim sweep + blocking XREADGROUP `>`;
  XACK retires. Handler error → stays pending → reclaim redelivers with the
  PEL's durable delivery count → poison cap (5) with loud drop. Handler errors
  and poison drops surface through OnError/OnPoison hooks (the debug session
  that found the lease-compose bug paid for this hook). Trimmed-shell pending
  entries ack blind (undeliverable is not retryable).
- **Tests** (miniredis, 15): round-trip, two-group fan-out, pattern-filter
  acks-unread, at-least-once redelivery, dead-consumer reclaim by an heir,
  same-name restart resume, replay-from-0-0 + live tail, `$` skips history,
  MAXLEN cap, poison drop, OnError, options validation, dead-Redis
  construction refusal, vocabulary/matching units.

### agentd (design 0072 §3)

- **Producer leg rides the notifier's settle fold** (`session_notifier.go`):
  `Bus` seam on the config; `debounceFired` publishes ONE settle topic event
  per settle (payload session/settle/title) instead of the direct fan-out;
  publish failure falls back to the direct v1 leg (§4.5: the lost wake-up is
  the failure that matters). New outcome `bus_published`.
- **`notifier` consumer group**: `ConsumeSettleEvent` — the bus message becomes
  the same per-subscriber deliveries via `selectSettleNoticesLocked`
  (collectSettles split; identical gates: event-set match, subscriber-side
  subtask, §4.4 suppression) + `deliverNotices` (now returns the aggregated
  hard error; rate refusals final). No outbox enqueue on this path — the
  stream entry is the durable intent; the consumer group's pending machinery
  IS the retry. v1 call sites unchanged (Bus nil is default + fallback) — the
  v1 wiring battery passes untouched.
- **`lease` consumer group** (`workspace_eventbus.go`): the NudgeSender
  generalization — `lease.{task}.nudged` → the sentinel nudge (marker line
  `lsp:agent-message-v1 {mode:lease-nudge, taskId, nudgeId,
  expects:respond|advance|fail}` + the directive prose) via detached
  SessionSend; byte-parity with the API's `leaseNudgeText` PINNED (one source
  of truth is impossible across the pkg/board pgx split — the test is the
  lockstep). `wedge_verdict` → log-only. No `agentmessage.Compose` wrapper
  (it requires fromSession provenance; the system-originated sentinel carries
  its own grammar — found via e2e, pinned by unit).
- **Env gate**: `AGENTD_EVENTBUS_ADDR` (redis:// URL or host:port) +
  `WORKSPACE_ID` → `eventbusFromEnv`; every failure mode → nil bus → v1
  point-to-point. Consumer name = pod hostname (`AGENTD_EVENTBUS_CONSUMER`
  override). `runEventbusConsumers` starts both groups on bgCtx/bgWg with
  OnError logging.
- **MCP topics alias**: `subscribe_session` accepts `topics`
  (`session.<id>.idle|error|*`, any id spelling, same broadcaster; foreign
  families refuse loudly), unioned into `events`. Schema + dispatch wired.

### e2e (`session_notifier_bus_e2e_test.go`)

The full wiringEnv battery over a real bus: settle publishes ONCE → consumer
delivers (v1 wording + sentinel; outbox untouched; ack drains); suppression
through the consumer (§4.4 same semantics); POST failure → pending →
redelivery after harness heal (at-least-once e2e); lease nudge delivery with
every sentinel field pinned; wedge verdict acks without messaging.

## Bugs found & fixed during the work (born-red, all pinned)

- XPENDING's range form answers `redis.Nil` on an empty PEL — the consumer
  loop treated it as fatal (Consume exited instantly on a fresh group).
- New-group start id `0` is accepted by Redis but not safely by miniredis's
  comparator — `StartReplay = "0-0"`.
- The lease sentinel's body went through `agentmessage.Compose`, which refuses
  a missing fromSession — the handler silently no-acked (OnError exists now
  because this was invisible).
- Test-harness races (cancel-inside-handler racing its own ack;
  publish-before-group-creation against `$` semantics) — consumeUntil now
  waits for the group PEL to drain before cancelling, and live tests prime
  the group first.

## Verification (2026-10-29, this worktree)

- `go build ./...` → exit 0.
- `go test -count=1 ./pkg/eventbus/` → ok (15 tests + race ×2).
- `go test -count=1 -run 'TestBusE2E|TestEventbus|TestLeaseBusConsumer|TestComposeLeaseNudge|TestSubscriptionEventsFromTopics|TestRedisOptions|TestRunEventbus' ./cmd/workspace-agentd/` → ok.
- Full suites + golangci-lint: see the PR checks (this worklog is updated with
  the final tallies at review time).

## Process (owner ruling, binding)

Iterate with the GitHub AI reviewer (pr-review.yml) until APPROVED. Never
merge without its APPROVE. Review rounds recorded in §Review rounds below.

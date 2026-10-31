# Worklog 1242 — S6: the unification (board events + stamps onto the bus, the SSE join, the v1 leg retired-when-armed)

- **Lane**: tsk-ethereal-piano-notices (epic "Liveness and observation", 2026-10-30 incident remediation S6), branch `s6-bus-unification`, isolated worktree `/tmp/opencode/s6-unification-wt` (the shared checkout belongs to the S2/S5 lanes).
- **Base**: sprint-board-story-machinery @ 2f7e6420e UNION-MERGED with `eventbus-bus-health` (S3) and `lease-events-publisher` (S4) — both dependencies done on the board but unmerged; conflicts were exactly the predicted four (CHANGELOG/COORDINATE stacks; `app.go` S2's `WithBusySource` + S4's 4-arg nudger/sink; `pkg/board/service.go` both struct fields). Resolution: keep both sides everywhere.

## What was built

1. **`pkg/eventbus` — the board-plane vocabulary as code** (the S4 constructor discipline): `BoardEventTopic`/`TaskStampedTopic` (+ the pre-existing `TaskStageTopic` now produced), `ParseBoardEvent`/`ParseTask`, the field-name constants, and `BoardEventPayload`/`TaskStagePayload`/`TaskStampPayload` builders. The package header's vocabulary table lists all nine topics.
2. **`pkg/board` — the stamp-event seam**: `StampEvent`/`StampEventSink`/`WithStampSink`/`SetStampSink` (the S4 lease-sink shape: post-write, fail-open, nil-inert) with `emitStamp` panic-contained (a broken projection never takes the stamp write down — pinned). Emissions: `writeAssigneeStamp` success (origin=spawn; signature now takes the `*Claim` so short-id/project ride) and `backfillAssigneeStamp` success (origin=backfill). The stamp semantics (retry/backoff/lag/guards) are byte-identical — the sink is additive.
3. **`api/internal/config`**: `board.eventbus.{addr,password}` (env `LLMSAFESPACES_BOARD_EVENTBUS_ADDR`/`_PASSWORD`) with a PER-FIELD fallback to S4's `board.leaseEventbus.*` (one Redis, one posture; the S4 knob keeps working unchanged — pinned).
4. **`api/internal/app/board_bus.go`** (the lane's core):
   - `boardBusClient` — S4's transport (client/lazy per-workspace buses/fail-fast breaker/2s budget) EXTRACTED and shared by both API legs, with per-leg counters injected at the publish call.
   - `boardBusPublisher` — the bus-first `board.EventPublisher` wrapper: single order-preserving drainer (v1 parity), scope resolved at publish through a 60s TTL-cached indexed read (`boardScopeCache`; failures NOT cached, fall back to v1 whose own lookup decides), `board.{project}.event` per committed row, `task.{id}.stage_changed` for move_stage (from/to off the row's own payload; source = user|engine|reconciler by event-id prefix), `StampEventSink` drainer for `task.{id}.stamped`. THE CONTRACT: publish success ⇒ the v1 leg is NOT called (retired-when-armed, pinned); any failure (scope, transport, overflow) ⇒ THAT event takes v1 verbatim.
   - `boardSSEJoin` — the consumer: per-replica group `sse-<hostname>` at `$`, patterns `board.*.event`+`task.*.stamped` (lease/session families excluded by construction — pinned), discovery loop (DISTINCT projects read every 30s) arming/retiring per-workspace Consume goroutines, teardown = cancel + best-effort `XGROUP DESTROY` on every touched stream.
   - `DiscoverBoardWorkspaces` production discovery; `boardBusPublisherOrV1` install choice.
5. **`api/internal/handlers/board_sse.go`**: the join's render — `BoardEventBusHandler` exported over a `boardFanoutBroker` interface (the real broker satisfies it; tests mount it directly) rendering through the SAME `boardSSEEvent` + `translateBoardEventIDs` + dual-channel fan-out as v1, scope from the payload (NO per-delivery project lookup); the stamp frame synthesizes `assignee_stamped` (actor `{"kind":"system","surface":"engine","session_id":assignee}` — phrase-translated by the same path; inner payload `assignee_session` so `PayloadAssigneeIDs` picks it up). Always-ACK: an advisory projection never wedges the group (the reseed-belt posture). `translateBoardEventIDs` is now a free function with a nil-pool degrade guard.
6. **`app.go` wiring**: the S6 block sits BEFORE the Service/Reconciler construction (the stamp sink rides `spawnOpts`; the first cut had it after — caught on re-read); unarmed ⇒ `boardBusPublisherOrV1(nil, v1)` installs v1 verbatim.
7. **Frontend**: one feed label — `assignee_stamped: "worker stamped"` in `EpicDrawer`'s ACTION_LABELS (the SSE feed is the stamp frame's only display surface; `TaskDrawer`'s timeline reads board_events rows which never contain it).
8. **Docs**: design 0072 amended (the three new vocabulary rows; §9.2 expanded to LANDED with the full join/retirement record); `docs/operator/monitoring.md` — the API-side expvar counters + the `sse-*` stalled-group triage note.

## The retirement (AC #3), stated precisely

Retired-when-armed: (a) the v1 point-to-point writer→broker hand-off for board events — NOT invoked when the bus publish succeeds (pinned by `TestBoardBusPublisher_BusFirstRetiresV1OnSuccess`: `spy.count()==0` while both entries land on the stream); (b) the per-delivery `SELECT user_id, target_workspace_id FROM projects` in `boardEventDeliver` — the join's delivery reads the scope off the payload (publish-time, TTL-cached). Parity pins: `TestBoardBusRenderParityWithV1` (handlers — one row renders BYTE-IDENTICAL frames through both legs over the real broker, both channels; the broker's own EventID is normalized — it identifies the publish, not the content) + `TestBoardSSEJoin_E2EFrameParity` (app — the full stack: publisher → real stream → join → real broker → subscribed user). Subscription/nudge behavior unchanged: the join's pattern exclusion is pinned (`TestBoardSSEJoin_StartLiveAndPatterns` covers the lease family too), and the 0071 notifier + S4 ladder batteries run green untouched.

## Notable decisions

- **Per-replica groups (`sse-<hostname>`), not one shared group**: a shared group would hand each entry to ONE replica's broker — the interactive echo (the user who just made the REST call) could silently move to another replica, a REGRESSION the parity pins must not allow. Per-replica groups deliver every event to every broker in stream order (strictly better than v1's writer-replica-only). Cost: crashed replicas leave groups — S3's detector flags them (that is its job); graceful shutdown destroys them.
- **The scope cache (60s TTL, failures uncached)**: retires the per-delivery read without lying about scope — a deleted project's cached entry publishes to a stream with no subscribers (a no-op), and a failed read falls back to v1's own lookup (the exact degrade today).
- **Always-ACK in the join handler**: the broker send is non-blocking and the reseed belt covers drops — retrying an advisory frame past five reclaim sweeps would wedge a group for nothing. The group's value here is cursor durability + per-replica fan-out, not redelivery.
- **No `Close()` on the publisher's drainers at shutdown**: writes are done and the pool is closing anyway; the join's teardown runs on the app context's cancel (group destroy), the publisher's goroutines die with the process (they hold no cross-process state).
- **go-redis v8's structured `XInfoGroups` errors on miniredis** (S3's health.go documented the trap; my first test probe walked straight into it — probes go through the generic `Do`).

## Verification

    go build ./... && go vet ./api/... ./pkg/...
    make fmt-check
    TEST_DATABASE_URL=postgres://postgres:testpass@127.0.0.1:5433/llmsafespaces_test_s6?sslmode=disable \
      go test -count=1 -tags integration -timeout 900s ./pkg/board/ ./api/internal/app/ ./api/internal/handlers/
    go test -count=1 -timeout 300s ./pkg/eventbus/ ./api/internal/config/ ./cmd/workspace-agentd/
    cd frontend && ./node_modules/.bin/tsc --noEmit && ./node_modules/.bin/vitest run   # 2590/2590

Private test DB `llmsafespaces_test_s6` (template copy of the shared `llmsafespaces_test` on :5433 — the S2 lane's shared-DB race lesson). Green: pkg/board integration 47s, app 33s, handlers 86s, agentd 374s, eventbus, config, vitest 2590, tsc, fmt-check.

## Review notes for the AI reviewer

- The union-merge commits (S3+S4) are the dependencies, not this lane's work; the four conflict resolutions are mechanical keeps-both.
- S4's `board_lease_events_test.go` needed two mechanical line updates (fields moved into `boardBusClient`); behavior is pinned unchanged by that suite running green.
- The parity family is the review's center of gravity: retirement (v1-not-called), fallback (four failure shapes), render parity (byte-equal frames), pattern exclusion (subscription/nudge untouched), StartLive, teardown.

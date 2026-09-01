# Worklog: US-69.11 (part 1) — the busy-gated usage stream foundation

**Date:** 2026-09-01
**Session:** Epic 69 (#1134) US-69.11 (#1145, design 0055 S3 + D1-B). Part 1: the NEW machinery that replaces the SSE tracker's derived duties — the busy-gated pod ABI consumer (billing, Epic 28 state bridge, persistence, death detection). The tracker deletion itself (emitters, v2Shadow/v2Pending, classifier wiring, MCP cutover, alerts) is part 2.
**Status:** In Progress — foundation complete and green; the deletion sweep remains (map below).

---

## Objective

Retire the API's SSE tracker (its per-workspace legacy-dialect pod streams and session-state derivation) with D1-B semantics: the API holds pod event streams only while activity justifies them, billing sourced from the contract ABI, Epic 28's user-stream surface preserved with its source swapped.

## Owner dispositions (recorded this session)

1. **Billing**: busy-gated contract-stream consumption (this work) — per-step MESSAGE_END tokens with deterministic idempotency keys.
2. **MCP**: cut over to the contract stream (part 2).
3. **Scope**: full US-69.11 in one PR — v2Shadow/v2Pending retirement included.

## Work Completed

### `pkg/abi/abiclient` — applied-events extension
- `WithAppliedEvents(fn)` Stream option: the fold invokes fn for every ACCEPTED event with its seq — exactly-once-per-seq across reconnects (discard rule unchanged, single-sourced). `AppliedEventsOf(opts)` exposes the composed callback for wrapping consumers. Test: applied events in seq order after the snapshot lands; distinct ingestions deliver distinct seqs.

### `api/internal/services/usagestream` (new) — the consumer
- One gate per workspace: `Open` (idempotent) starts a pod ABI subscription via the reference client; `Close`/idle-drop tears it down.
- **Idle rule**: the gate survives while the CURRENT fold has a BUSY/COMPACTING session; once the fold reports all-idle, a settle window (30s default) runs — quiet-but-busy streams (long turns, no events) stay connected. A watchdog ticker evaluates the rule on quiet streams.
- **Per-event policy**: MESSAGE_END (assistant, non-zero cost) → billing Usage{tokens, model, messageID, seq} + context numerator (input+cacheRead+cacheWrite); SESSION_STATUS BUSY/COMPACTING/IDLE → bridge; INPUT_REQUEST/RESOLVED → bridge; SESSION_UPDATED title → bridge; stream death after frames → AgentDied + retry backoff.
- Tests: gate lifecycle (open-once, busy-holds, idle-drops, close-cancels), billing extraction (zero-cost + user-message skips), bridge shapes, death detection. Race ×3.

### `api/internal/handlers/proxy_usagestream.go` (new) — production wiring
- `UsageStream()` lazy singleton; resolve seam = `agentdEndpoint` (resume-safe, A7); client = abiclient over a §D1 Basic-auth transport.
- `recordStepUsage`: deterministic keys `tokens:{ws}:{messageID}:{seq}:in|out` — replicas generate identical keys and `usage_events`' UNIQUE constraint enforces exactly-once (fixes the tracker's latent double-billing: its keys embedded `UnixNano` while the chart runs 2 API replicas). Inference metrics ride the same sink pair via `SetUsageBilling` (app.go).
- `usageBridge`: session.status → USER stream only (workspace-stream copies die with the tracker); agent.question/permission (+resolved) with client-side root resolution (`resolveRootSessionID`); QuestionRequest re-expanded from the ABI's flattened single-question shape; title/context persistence via sessionIndex; agent_died dual-published.

### `proxy_state_reconciler.go` (new)
- The tracker reconciler's surviving duties on their own ticker (60s): `escalateHungs` (D6 sweep — data source unchanged, scheduler moved), usage-gate arming for Active workspaces, teardown otherwise. Runs alongside sseWatchReconciler until part 2 deletes the latter.

### Outbox hook
- `outboxOnDelivered` arms the workspace's usage gate on confirmed delivery (a turn may start) under the agentd-terminus regime.

## Key Decisions

1. **Per-step billing, not cumulative deltas**: the tracker billed session.updated cumulative output-delta with Redis watermarks; the ABI's MESSAGE_END carries per-step tokens+model directly, so the DB constraint replaces the watermark machinery (Redis TokenSeenStore dies with the tracker). Input tokens now bill per-step (each step's own input) rather than once-per-session — more accurate; noted as a semantics change.
2. **Epic 28 surface preserved**: the bridge re-emits session.status/agent.*/agent_died to the user stream in the exact legacy wire shapes — the frontend provider needs NO changes and the dialect-gates exceptions stand.
3. **User-stream only**: the workspace-stream session.status/agent.* copies are NOT re-emitted (ChatPage is cut over; MCP cutover is part 2).

## Tests Run

- `go test ./api/internal/services/usagestream/ ./pkg/abi/...` — ok (abiclient 85s full suite).
- `go test ./api/internal/handlers/` — ok (115s, full suite).

## Next Steps — part 2 map (the deletion sweep)

1. **Delete the tracker**: `api/internal/services/sse/` package, `sseTracker` field + `newSSETracker` wiring (proxy_lifecycle.go:296-312), callbacks `onSessionIdle/onSessionActive/onRawEvent/onAgentDied/reconcileSessionState` (proxy_events.go), `GetSSETracker` consumers (app.go:1586-1631 tracker billing block — superseded by SetUsageBilling; agent_reload drain wiring needs `SubscribeDrain` re-sourced or reload rewired to statusz/fold idle), `sseWatchReconciler` + `RefreshLastEventGauges`, all re-arm call sites (EnsureWatching at proxy_events.go:135, proxy.go:456, proxy_adapter_crosscutting.go:150, proxy_stream.go:65 — the last one may keep a usage-gate Open instead).
2. **Delete emitters**: publishClientEvents (+ RetryFromEvent session.status retry), emitNormalizedInputEvent (agent.* dialect translation — the usage bridge replaces it), onSessionIdle/Active workspace+user session.status publishing, agent.event raw relay, persistContextFromEvent (usage bridge covers). KEEP: queue.update (outbox-owned), workspace.phase, workspace.alert, agent_died (bridge).
3. **v2 retirement**: proxy_v2_shadow.go delete (+ app.go:286 + ListQueue/Remove read paths proxy_handlers.go:1463,1509); v2Pending (proxy_v2.go) delete + wakeStrandedV2Sessions migration onto the state reconciler's tick; v2BusyReap review.
4. **Classifier/drift**: tracker SetEventMetrics/observeAgentEvent wiring, metrics.RecordAgentEvent + `llmsafespaces_agent_events_total`, adapter SetEventClassifier seam; ADD agentd Custom-valve counter (sessionstate metrics; count in the projection where Custom parts apply). Delete tracker Prometheus alerts (prometheus-rules.yaml:94,111,145) + promtool tests; ADD contractstream.Manager + usagestream gate gauges (the scale-to-zero AC's connection metrics).
5. **MCP cutover**: pkg/mcp/client.go idle-wait/questions/streaming → the API's /contract-events ABI frames; update mcp_router_integration_test.
6. **Tests to rewrite** (inventory): stream_events_test, proxy_test (session-limit/idle), proxy_broker_agentdied_test, context_usage_adapter_e2e_test, proxy_input_test, proxy_subtask_permission_test, proxy_v2*_test (5 files), opencode_upgrade_test:330, admin_token_bearer_test:112, agent_drain_test, agent_reload_e2e_test, sse_billing_e2e_test, proxy_902_e2e_test, proxy_d6_test, mcp_router_integration_test, tracker*.go, shadowconsumer scenarios (retire with S1 waiver). Add `no_api_session_derivation_remains` dead-code check.
7. **openapi.yaml**: drop session.event/session.status channel docs (sdks/openapi.yaml:1167,8295); canary d-prompt-async scenarios' idle wait path.
8. Worklog part 2 + PR (branch feat/epic-69-us6911-tracker-retirement).

## Files Modified

- `pkg/abi/abiclient/client.go`, `client_events_test.go` (new)
- `api/internal/services/usagestream/consumer.go` (new), `consumer_test.go` (new)
- `api/internal/handlers/proxy_usagestream.go` (new), `proxy_usagestream_test.go` (new), `proxy_state_reconciler.go` (new)
- `api/internal/handlers/proxy.go` (billing sink fields), `proxy_lifecycle.go` (reconciler start + outbox gate hook)
- `api/internal/app/app.go` (SetUsageBilling wiring)

# Worklog: observability gaps from the halting-sessions investigation (#901)

**Date:** 2026-08-16
**Session:** Close #901 — implement the remaining instrumentation gaps (G1/G2/G11 shipped in #903).
**Status:** Complete

---

## Work Completed

- **G3 — upstream liveness signal** (`sse/tracker.go`): per-workspace last-event timestamps recorded on every `processEvent`; `llmsafespaces_sse_tracker_last_event_age_seconds` gauge refreshed each reconciler tick (watched-but-never-received reports a large age — visible silence, not absent series). Kills the "client heartbeats prove the tracker is alive" trap from the incident.
- **G4 — delivered counter** (`eventbroker/user_broker.go`): `llmsafespaces_sse_broker_delivered_events_total{workspace_id,type}` — distinguishes zero-emitted / zero-delivered / zero-subscribed (the drop counter had no series while users received nothing).
- **G6 — pprof** (`server/router.go`): admin-auth-gated `/api/v1/admin/debug/pprof/*` (Index/Cmdline/Profile/Symbol/Trace behind AuthMiddleware+AdminGuard). Stacks no longer require SIGQUIT-ing a production replica. OpenAPI contract allowlisted with rationale (9 wildcard-method routes).
- **G7 — stream lifecycle logs** (`proxy_stream.go`): open (workspace, subscriber count) and close (duration, eventsSent) at Info.
- **G9 — alert rules** (`helm/templates/prometheus-rules.yaml`): SSETrackerWatchesZero (critical — the incident shape), SSETrackerUpstreamSilent (>10m), WatchdogSuppressing (D1 policy → notify), TrackerBusyResetRate (restart churn), RestartMarkerWriteFailed (any), RelayInjectorDegraded (G8 — free-model fetch exhausted; counter `llmsafespaces_relay_injector_total{outcome=fetch_failed}` already existed, it had no alert).
- **Reviewer-deferred from #903**: dead backoff reset fixed — `connectAndRead` always returns non-nil, so the `else { backoff = 2s }` branch was dead; a healthy long-lived connection that ended kept the maxed 30s backoff forever. Now resets when the connection lived >30s.
- Reconciler tick also refreshes G3 gauges (no second loop for the same data).

## Closed without code (documented in the issue close-out)

- G1/G2/G11: shipped in #903 (watched-workspaces gauge, Warn disconnects with error+backoff covering the empty-podIP case).
- G5: redundant with G1 — the API-side gauge answers "is this replica watching"; an agentd-side subscriber count would duplicate it.
- G10: descoped — the frontend has no telemetry sink; SSE drops already surface via the `resync` sentinel + streamTimedOut banner (#490 pattern). A client beacon is its own project.

## Assumptions (validated)

1. PodMonitor job label is `<fullname>-agentd` (podmonitor-agentd.yaml:56) — alert `job=~".*agentd.*"` matches.
2. `llmsafespaces_relay_injector_total` is the actual counter name (relay_injector.go:84).
3. Alert rules render: `helm template` with monitoring enabled produces all rules.

## Tests Run

- `go test ./api/internal/services/sse/ ./api/internal/services/eventbroker/ ./api/internal/server/ ./api/internal/handlers/ -count=1` — green (new: RefreshLastEventGauges, ProcessEvent_RecordsLastEvent, DeliveredCounter; OpenAPI contract with the allowlist)
- New tests `-race` — green; `go build ./... && go vet ./...` clean; helm template renders 25 alerts

## Files Modified

- api/internal/services/sse/tracker.go (+tracker_test.go)
- api/internal/services/eventbroker/user_broker.go (+user_broker_test.go)
- api/internal/server/router.go (+router_openapi_contract_test.go)
- api/internal/handlers/proxy_stream.go
- api/internal/handlers/proxy_lifecycle.go
- helm/templates/prometheus-rules.yaml

---

## Round 2 (reviews 1-3 on #906): all blockers

- **pprof mount actually fixed** (three rounds found two successive bugs):
  round-1's StripPrefix+custom-mux served the HTML index for every named
  profile; the round-2 DefaultServeMux rewrite silently 404'd everything
  because import hygiene had stripped the blank net/http/pprof import
  (verified: DefaultServeMux had no pprof routes). Final: self-contained
  mux with explicit handlers (Index subtree covers all named profiles;
  cmdline/profile/symbol/trace exact) — no import magic to strip.
- **Backoff reset pinned**: logic extracted to `backoffAfterConnect(cur,
  connDuration)` (threshold var for tests); `TestBackoffAfterConnect`
  covers reset/keep/boundary.
- **Delivered counter no longer counts drops**: `Subscriber.Send`
  returns delivered; counter increments only on true delivery.
  `TestPublishToWorkspace_FullBufferNotCountedDelivered` (fill buffer →
  publish → counter unchanged).
- **G1 per-workspace state** (the issue's actual ask): `llmsafespaces_sse_tracker_connected{workspace_id}` (1 while an /event stream is open — armed-but-failing reads 0) + `llmsafespaces_sse_tracker_reconnects_total{workspace_id}`. Stale series deleted on StopWatching
  (`TestTrackerConnectedGauge_SeriesDeletedOnStop`).
- **reconcileSessionState** all-Debug → Warn (4 sites).
- **G8 statusz**: `relay_free_models` field (0 unknown/1 ok/2 degraded)
  from the injector's terminal state; consequence-bearing Warn text.
- **Alert exprs hardened**: WatchesZero guarded `and sum(...) > 0` (no
  critical on empty fleets); UpstreamSilent joined `and on(workspace_id)
  connected == 1` (no firing on suspended workspaces).
- **subscribersIncludingSelf** off-by-one fixed (SubscribeWorkspace runs
  before the count).
- **pprof e2e tests**: unauthenticated 401; non-admin 404 (AdminGuard
  deliberately hides route existence — admin_guard.go:13; initial 403
  expectation was wrong); admin gets real goroutine/heap/cmdline data
  (asserts not-HTML + contains stack data).
- **chart_test pins** the six new alert names.

## Tests Run (round 2)

- Full touched suites: sse, eventbroker, server, handlers, agentd
  (readyz/statusz), helm TestMonitoring_PrometheusRule — all green
- New tests `-race` green

---

## Round 3 (review on #906): residuals

- G7 completed: the user stream (`/api/v1/events`) gets the same
  open/close lifecycle logs (duration + eventsSent) — both SSE
  endpoints instrumented.
- G1 paging signal: `LLMSafeSpacesSSETrackerWatchFailing` (watched ∧
  ¬connected, 10m, critical) — the armed-but-failing class that was the
  incident's invisible shape.
- G8: `no_free_models` outcome counter alongside `fetch_failed`;
  alert matches both; `relay_free_models` state machine tested
  (defaults/transitions/statusz field).
- G11: `sse_tracker_pod_ip_unavailable_total` counter on the
  resolver-empty resume race.
- G5/G10 dispositions corrected on the record (#901 comment): G5
  wontfix with honest rationale; G10 tracked on #907. The phantom
  close-out (signals claimed before any PR existed) corrected in the
  same comment — per-workspace state landed in #906, not #903.
- Chart test pins WatchFailing.

---

## Round 4 (review on #906): the four carried code blockers

- **F1 — dead WatchFailing expression**: the join aggregated the
  UNLABELED watched-count gauge by (workspace_id) — producing a single
  {workspace_id=""} series that can never match labeled RHS series; dead
  a second way via F2. Fixed per the reviewer's simpler construction:
  connected series is initialized AT ARM TIME (F2) and deleted on
  StopWatching, so a PRESENT series implies armed — the alert is now
  `connected == 0` guarded by unlabeled `and on() (max(watched) > 0)`.
- **F2 — never-connected emitted no series**: `setTrackerConnected(id,
  false)` at arm time in EnsureWatching; pinned by
  TestTrackerConnectedGauge_InitializedAtArmTime (series exists at 0
  immediately; deleted on stop).
- **F3 — outcome counter double-tick**: the fetch-deadline terminal
  incremented BOTH fetch_failed and no_free_models (one event, two
  ticks). The catalog-empty terminal now also guards on `fetchErr ==
  nil` — fetch ERRORS are fetch_failed; an empty catalog on a clean
  fetch is no_free_models. (The round-3 fix script for this aborted
  mid-file before writing — the duplicate survived two rounds; this
  round's scripts assert on write.)
- **F4 — statusz "unknown" on terminal paths**: all four additional
  terminal-failure sites (catalog-empty, unhealthy_timeout,
  config_write_failed, auth_write_failed) now Store(2).
- **Injector terminal-path tests** (real startRelayInjector against
  httptest /provider fakes): fetch-failed (exactly one tick, no
  conflation), catalog-empty (state 2, distinct outcome), success
  (state 1). Counter assertions are cross-test-safe deltas.
- **promtool rule tests** (the "would have caught it twice" ask):
  helm/tests/alerts_promtool_test.yaml + TestPromtoolRules renders the
  chart, extracts the PrometheusRule, and executes the REAL expressions
  — WatchFailing fires (ws-blind), connected==1 doesn't, absent series
  doesn't, WatchesZero empty-fleet guard, UpstreamSilent
  connected-guard both ways, RelayInjectorDegraded on both outcomes.
  CI installs promtool (pinned v3.4.1) in both test jobs. The harness
  proved itself during development (caught the job-label mismatch and
  the annotation-exactness requirement).

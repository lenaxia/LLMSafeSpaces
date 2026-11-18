# Worklog: Listing Latency — Count Walks Leave the Request Path

**Date:** 2026-11-18
**Session:** Frontend sluggishness triage (user-reported "long wait times to load data") → live-cluster diagnosis → three fixes (async count walks, walk-failure retirement, models cache TTL) + pre-existing board-test schema repair.
**Status:** Complete

---

## Objective

Quantify why the frontend waits long on data loads, fix the measured causes. User approved three suggestions: (A) stop blocking the listing on harness count walks, (B) stop re-walking permanently-dead rows, (C) cache the model catalog.

---

## Work Completed

### Diagnosis (live cluster, home-kubernetes, llmsafespaces ns @ 0.44.17)

- VictoriaMetrics histograms (6h, both API replicas): `/sessions` p50 120ms / p95 1.26s / p99 1.96s; `/models` p50 838ms / p95 1.83s; session detail ~0.85s p95. Overall API p95 630ms.
- Ruled out: Postgres (6ms p95), Redis (1.6ms p95), API CPU (~50m/pod), restarts.
- API logs: 663 `harness count walk failed` warnings/hour across ~8 workspaces (`workspace pod not running`, `session not found 404`, one 500). The walk re-arm contract ("no tracker entry on error") made ghost rows retry on EVERY listing, serially, on the request path.
- Corollary found but NOT in scope: `GetAuthoritativeActiveSessions` (proxy_connections.go:176) is also a pod round trip (3s budget) on the listing path — follow-up candidate.

### A — async count walks (api/internal/handlers/proxy_session_index.go)

`ServeHarnessMessageCounts` → `ScheduleHarnessMessageCounts`: selection stays synchronous (eligibility → retirement → cooldown → rotation → budget), walks dispatch detached (`runListingCountWalks`, `context.Background()` + `listingCountWalkTimeout`=10s, the `runParentBackfill` precedent). Persist semantics unchanged (tombstone consult → Register → RebuildMessageCount; outcomes preserved). Contract change: the wire serves the index value this pass; the count converges in the index for the next poll (~30s sidebar refresh). Inflight dedupe (`tryMarkInflight`) prevents concurrent passes double-walking a key. Dispatch seam `countWalkDispatch`/`SetCountWalkDispatchForTest` for deterministic wiring tests. Metric name `listing_count_served` kept for dashboard continuity; it now means "persisted (converged)".

### B — confirmed-miss retirement (same file)

`countServeRetireAfterMisses`=3 consecutive typed `errors.Is(err, agent.ErrSessionNotFound)` misses → `listing_count_retired` for `countServeRetireWindow`=1h, then re-arm. A successful walk resets the streak. Transient failures (ErrNoRunningPod, 5xx, timeouts) NEVER feed the streak. Rows are never deleted here — deletion stays with the operator-reviewed reap path (a 404 from a running pod says nothing about a suspended pod's PVC-held sessions; history-bearing rows are the reap guard's kept_for_operator population). Known limitation, conservative: the pi adapter's CountMessages may not render the 404 classifier, so pi-harness ghosts classify as transient (extra walks, never a wrong retirement).

### C — models cache TTL (api/internal/handlers/models.go)

`modelCacheTTL = 60s` (was hardcoded 5s). Invalidation seams cover catalog mutations (SetModel evict; credential pushes via `agentpush.WithModelCache`; #467 relay-transition live guard re-checks on every cache hit). TTL bounds the residual class — pod restart mid-window — self-heals in ≤60s. Pinned by `TestModelCache_TTLIsModelCacheTTL`.

### Pre-existing board-test schema repair (api/internal/app, 3 files)

While verifying, `go test ./internal/app/` failed on clean main: `column "wrap_up_review" of relation "epics" does not exist` — migration 000059 landed, but `board_liveness_test.go` / `board_bus_discovery_integration_test.go` / `board_phrase_bounce_integration_test.go` build bare `pgxpool` connections (bypassing testharness) and read whatever schema is on the shared test DB. Reproduced with my changes stashed (pre-existing). Fixed by routing the three pool helpers through `testharness.New` (skip-on-unreachable preserved; schema migrates when not current). Also reset the local shared test DB (was a stale schema dump with empty `schema_migrations`).

---

## Key Decisions

1. **Async, not parallelized walks.** Parallel walks still block the request; async converges within one poll cycle, which the original #1880 design already assumed ("the sidebar refreshes ~30s").
2. **Retire, not delete.** A harness 404 is definitive for the harness (#1340) but says nothing about the index row's history or a suspended pod's PVC. Walk budget is the only thing retirement spends.
3. **60s TTL, not 5m.** The residual staleness class (pod restart) heals faster; the picker's refresh cadence is fully absorbed either way. My plain-English proposal to the user said "say 5 minutes"; 60s is the conservative point on the same curve — documented here so the deviation is explicit.
4. **Dispatch seam as a func field, lazily resolved.** Struct-literal test constructions must not nil-panic (the countServeState precedent); server-package tests needed an exported `SetCountWalkDispatchForTest` (the SetParentBackfilledForTest precedent).
5. **Dropped the unused ctx param** from `ScheduleHarnessMessageCounts` — the pass builds its own detached context; keeping the param would lie about the dataflow.

## Assumptions (Rule 7) and validation

| Assumption | Validation |
|---|---|
| Latency is API-side, pod-proxy-bound | VM histograms + logs (this worklog §Diagnosis); DB/Redis/CPU ruled out numerically |
| `agent.ErrSessionNotFound` is only rendered by httpSessionError (404 on session routes) | `pkg/agent/opencode/adapter_helpers.go:76-82`; harness router delegates to dialect adapters |
| Harness count walks persisting via RebuildMessageCount makes the NEXT listing serve corrected counts | `recordingIndex.rebuilt` wiring pin `TestRouterSessionList_SchedulesHarnessCountWalkForDriftRow`; cooldown-skip path `TestListingCount_CooldownBacksOffConvergedRows` |
| errors.Is classifies wrapped sentinels | `TestListingCount_MissClassificationIsTyped` (double-%w wrap from adapter_helpers.go:79) |
| testharness.New skip semantics equal the old helpers' (skip when PG unreachable) | `harness.go:112,119` (t.Skipf on construct/ping failure) |
| App-package failures pre-existing on main | `git stash` → same 3 failures on clean 0aedbaf0a → unstashed |

## Blockers

None. Follow-up candidates (not done, deliberately): `GetAuthoritativeActiveSessions` pod round trip on the listing path; pi-adapter CountMessages 404 classification (retirement coverage for pi harnesses).

---

## Tests Run

- `go test ./internal/handlers/ ./internal/server/ ./internal/services/sessionindex/ ./internal/app/ -count=1` — PASS (incl. the 3 previously-failing app suites)
- `go test ./... -count=1` (api module) — 42 packages ok
- `golangci-lint run ./internal/...` — 0 issues; `gofmt -l` clean
- TDD: red confirmed first (undefined symbols / assertion failures), then green per unit

---

## Review Round 1 (PR #2106) — all four findings fixed

1. **robustness: unbounded `misses`/`retiredAt`** — `recordMiss` now carries its own `countServeCap` wholesale reset (ghost rows never reach `record()`'s lastAt-triggered reset, so the miss map needed its own trigger; retirement stamps clear with it); `record()`'s rotation reset also clears both new maps. Pins: `TestCountServeTracker_CapResetBoundsRetirementMaps` (cap+50 distinct keys → bounded) + `TestCountServeTracker_SuccessWalkResetAlsoBounds`.
2. **docs-truth: inflight-vs-budget comment** — the inflight claim moved INTO selection, before the budget cut, so an in-flight row never displaces a walkable row (cooldown-skipped and over-budget rows release their claim). Comment now states the real order. Pin: `TestListingCount_InflightRowDoesNotDisplaceBudget` (4 eligible rows, 1 pre-inflight → 3 walkable rows walk).
3. **docs-truth: stale README TTL passages** — README-LLM.md:592 (per-replica staleness) and the stale-window paragraph (:646 block) updated to `modelCacheTTL`/60s with 60s+15s window math.
4. **style: orphan comment fragment** — sessionindex/service.go outcome-registry comment re-attached cleanly (duplicate fragment dropped, #1889 clause rejoined).

Tests re-run: handlers/server/sessionindex/app all ok; golangci-lint 0 issues; gofmt clean.

---

## Next Steps

1. Push branch, open PR, run the AI-review cycle to APPROVE, squash-merge (repo workflow).
2. Deploy to the home cluster and re-read the VM histograms: expect `/sessions` p95 ≈ index-read latency (<50ms) and `/models` p95 ≈ cache-hit latency; watch `session_index_reconcile_outcomes_total{outcome="listing_count_retired"}` climb then flatten.
3. Follow-up: evaluate `GetAuthoritativeActiveSessions` on the listing path (same request-path pod round trip class).

---

## Files Modified

- api/internal/handlers/proxy_session_index.go (A+B core)
- api/internal/handlers/proxy.go (countWalkAsync field)
- api/internal/server/router.go (call site)
- api/internal/services/sessionindex/service.go (outcome registry docs)
- api/internal/handlers/proxy_session_listing_count_test.go (rewritten to async contract)
- api/internal/handlers/proxy_session_listing_count_async_test.go (new — A+B pins)
- api/internal/server/router_session_listing_count_test.go (wiring pins)
- api/internal/server/router_session_reconcile_test.go (recordingIndex.rebuilt)
- api/internal/handlers/models.go, models_handler.go (C)
- api/internal/handlers/models_cache_ttl_test.go (new — TTL pin)
- api/internal/app/board_liveness_test.go, board_bus_discovery_integration_test.go, board_phrase_bounce_integration_test.go (testharness conversion)
- worklogs/1302_2026-11-18_listing-latency-count-walks.md (this file)

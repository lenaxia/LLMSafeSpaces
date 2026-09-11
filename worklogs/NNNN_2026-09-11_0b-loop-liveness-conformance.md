# Worklog: 0b conformance follow-up — shared loop-liveness gauge + L9/lease-clock pin

**Date:** 2026-09-11
**Session:** Post-merge follow-up to 0b (PR #1318): conform its observability and timing constants to the epic-71 shared surfaces that landed while 0b was in review (#1319's loop-liveness gauge family and lease clock).
**Status:** Complete

---

## Objective

My landed-notes commitment from 0b: "reconcile ParkedSweepInterval/probeTimeout into the shared lease clock when it lands." #1317 (1b) and #1319 (0c) merged during 0b's review cycle, introducing `llmsafespaces_loop_last_run_timestamp_seconds{loop}` (whose Help text explicitly names the parked-error sweeper as an expected exporter) and `LeaseConvergenceBound`/`ReconcileCadence`.

## Work Completed

- **Gauge family conformance** (`parked_sweeper.go`): the API-side `llmsafespaces_outbox_parked_sweeper_last_run_timestamp_seconds` (own-name gauge) is replaced by the shared family — `llmsafespaces_loop_last_run_timestamp_seconds{loop="outbox_parked_sweeper"}` — registered in the API binary (separate process from agentd's registration; no duplicate-registration conflict). One query now covers every periodic loop across both binaries. The old metric name is removed outright (pre-deploy: nothing scraped it).
- **Constant documentation pin** (`ParkedSweepInterval`): now documents L9 (≤5min parked→disposition, 60s cadence = 4 retries of headroom) as an API-side recovery bound DISTINCT from the agentd lease clock, with the derivation rationale; coherence proposal posted on #1314 (comment 5629899572) for the #1312 budget-table owner.

## Key Decisions

1. **Families stay distinct, derived and documented** — not literally unified. L9's floor is the lease clock's own convergence (a LEDGERED row must flip via #1311's deadline before the sweeper can disposition it), so L9 ≥ admissionDeadline + sweep headroom; driving the sweep at ReconcileCadence (15s) would 4× probe load for zero L9 gain. Proposed to the epic; will re-time if the budget-table owner overrules.
2. **Own-name gauge deleted, not aliased** — Rule 5 (no backcompat shims); the metric family is pre-deploy.

## Tests Run

`go test -race ./api/internal/services/outbox/` green (metrics tests updated to the labeled family); golangci-lint 0 issues.

## Next Steps

- Monitor the budget-table proposal on #1314 for 1b/0c owner response.
- 1a remains stuck (dead session, unpushed branch) — reclaim window at 2026-09-12T22:39Z unless hand-off happens sooner.

## Files Modified

- `api/internal/services/outbox/parked_sweeper.go`
- `api/internal/services/outbox/parked_sweeper_test.go`
- `worklogs/NNNN_2026-09-11_0b-loop-liveness-conformance.md` (this file)

---

## Review round 1 (PR #1322)

- **Finding 1 (fixed):** the liveness gauge was stamped by BOTH callers (periodic loop + on-transition sweep) — transition churn would keep a dead loop looking fresh. The stamp is now loop-owned: `stampLoopLiveness` fires only in the Run loop's periodic pass; direct/transition sweeps never stamp (pinned by the updated metrics test).
- **Finding 2 (fixed):** the family Name was pinned by no API-side test. `TestLoopLiveness_NamePinnedOnScrapeSurface` asserts the exact pkg/obs name + loop label on the DefaultGatherer surface (self-sufficient: materializes the child first).
- **Style 1 (fixed):** Help drift and duplicated literals across binaries — new `pkg/obs` package holds the family constants; both agentd's and the API's registrations consume them.
- **Pre-existing main failure found and fixed:** `TestMetricsScrape_Completeness` (agentd) failed on pristine main — a *Vec with no children emits no series, so the completeness scrape was order/timing-dependent (loop_last_run depended on the watchdog goroutine having stamped). The test now materializes every vec family first; verified green with `-run` isolation and in the full package (228s).
- **Robustness note (recorded):** in adapter mode the API's loop gauge series never materializes (inert loop, honestly absent) — absent()-style alerting should account for regime; noted here for when alerting un-gates.

---

## Review round 2 corrections (PR #1322; fixes landed in commit 263bbad7, reviewed against f0188a61)

The round-1 "robustness note" above was **inverted for the shipped code** and is corrected here: after the loop-owned-stamp fix, the stamp is UNCONDITIONAL (outbox.go stamps on every completed periodic pass, and `Run` starts whenever the outbox exists — only the verifier/delivery hooks (`SetVerifier`/`SetOnDelivered`/`SetOnStaged`) are adapter-gated). So in adapter mode the series MATERIALIZES on the first tick: the gauge measures Run-loop goroutine liveness uniformly, and dead-loop detection works in both regimes. That is the chosen semantics (a stamp measuring loop liveness should not depend on which delivery regime the loop serves); pinned by `TestRun_LoopLivenessStampsInAdapterMode`. Operators building absent()-style alerting should treat a never-materialized `outbox_parked_sweeper` series as "outbox unwired", not "loop dead".

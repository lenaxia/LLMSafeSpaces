# Worklog: US-69.12 — session-state metrics & alerts (seq stall, ledger funnel, delivery latency, stalled/wake alerts)

**Date:** 2026-08-31
**Session:** Epic 69 (#1134) US-69.12 (#1146, design 0055 R5 + §Rollout S4): seq advance, ledger states, promotion stalls, and snapshot/delivery costs become first-class observables — exported from agentd (`:4098/metrics`, the existing PodMonitor scrape), four alerts with promtool-executed expressions, a dashboard row, and per-alert triage in the operator docs.
**Status:** Complete (in-repo). Staged-pool ACs (scrape completeness on the pool, the CPU-starvation soak with CFS correlation, the #1119 stranded replay) ride the pool per the epic's staging — the in-repo rehearsal is the promtool scenarios (fire + healthy for every alert).

---

## Objective

R5: in-pod starvation, ledger health, and delivery latency become visible from OUTSIDE. Seq advance is the progress signal 0050 D1 wanted.

## Work Completed

### The metrics bridge (sessionstate stays prometheus-free — the seal)
- `Authority.Metrics()` extended: `SecondsSinceSeqAdvance` (a `lastSeqAt` clock reset at both seq-advance sites + construction; a reseed counts as advance), `LedgerDepths` (per-state funnel), `StalledEntries`, `OldestPromotionStallSeconds`. New ledger methods `depths()`/`stalledCount()`/`oldestAdmittedAge()`.
- `Authority.CheckStalls(ctx) StallStats` exported: one stall pass — admitted rows past the deadline move to stalled (fsync'd) and the **Config.Wake** seam fires once per row; the pass reports `Stalled` + `WakeFailures`. **The stall detector is now production-driven** (a 1-minute watchdog goroutine in agentd) — before this story `checkStalls` was test-only; stalls never actually happened.
- **The wake**: a store reseed (`ReseedReasonStallWake`, wired in sessionstate_wiring via the authority-closure pattern) — events completing the stalled row's turn promote/turn-end it; a harness-specific queue-poke would require a probed route (never invented). Wake failure = reseed error.

### The wiring (`cmd/workspace-agentd/sessionstate_metrics.go`)
- promauto on the default registry (the `:4098` scrape): `llmsafespaces_seq_stall_seconds{workspace_id}`, `llmsafespaces_ledger_depth{workspace_id,state}`, `llmsafespaces_stalled_entries`, `llmsafespaces_promotion_stall_seconds{workspace_id}`, `llmsafespaces_delivery_202_latency_seconds` + `llmsafespaces_snapshot_size_bytes`/`_latency_seconds` (histograms), `llmsafespaces_wake_failures_total`, plus the containment gauges (`dropped_events`, `parser_failures`, `panics_contained`, `subscribers`) — the `Metrics()` bridge worklog 0861 asked for.
- `instrumentABISurface`: an http wrapper at the user-mux mount timing/sizing the two budget-carrying ops by procedure suffix (Deliver → 202-latency; GetSnapshot → size+latency). Reads unmeasured, harness untouched (M3.1).
- `runSessionStateWatchdog`: 1-minute ticker — CheckStalls → wake-failure counter + one gauge refresh. Started in main.go next to the boot reseed.

### Alerts (helm/prometheus-rules.yaml, `llmsafespaces.agentd` group)
- **`LLMSafeSpacesSeqStalled`** (critical): seq stall >5m **correlated with pending ledgered work** — an idle pod legitimately stalls (no events); the correlation is what makes the signal page-worthy (documented in the rule + docs).
- **`LLMSafeSpacesStalledEntries`** (warning, for 10m), **`LLMSafeSpacesWakeFailures`** (warning), **`LLMSafeSpacesDeliveryLatencyBudget`** (p99 > 1s, for 10m).
- **Promtool rehearsal** (`helm/tests/alerts_promtool_test.yaml`): fire + healthy-not-fire scenarios for every alert — including the join (idle-pod negative case) and the histogram_quantile breach. Iterated against real promtool 3.4.1 (installed via the CI curl path — `go install` is blocked by upstream replace directives); the expected annotations pin the rendered `humanizeDuration`/increase values exactly.

### Dashboard + docs
- `operational.json`: a "Session State — agentd (Epic 69)" row (seq stall by workspace, ledger funnel by state, delivery p50/p99, stalled + wake-failure stats, promotion stall + snapshot size). New `__LLMSAFESPACES_AGENTD_JOB__` placeholder substituted in dashboards-configmap.
- `docs/operator/monitoring.md`: the metric table (types + labels) + a per-alert triage section (starvation→CFS/watchdog correlation; stranded-entry→GetDeliveryStatus/replay; wake-failure→harness path; latency→fsync/WAL contention).

## Key Decisions

1. **Seq-stall alerting correlates pending work**: idle pods stall by design; the alert is `seq_stall > 300 AND ledger_depth{ledgered} > 0` — starvation WITH stranded work pages, quiet idles never do.
2. **The wake is a reseed, not an invented route**: a store refresh is I3-safe and resolves any turn that actually completed; a queue-poke needs a probed pinned route (the 1.18.10 discipline).
3. **Promtool-executed alerts, not name-only checks**: the #906 lesson (a dead join shipped twice behind name checks) — every new alert has fire + negative scenarios.

## Tests Run

- `go test ./cmd/workspace-agentd/sessionstate/` — green (incl. the new Metrics/CheckStalls suite: funnel, seq-stall reset, stall stats + wake-failure counts, wake-once idempotence, nil-wake/no-ledger no-ops)
- `go test ./cmd/workspace-agentd/ -run "TestRecordSessionState|TestInstrumentABISurface"` — green
- `go test ./helm/` — green (promtool rules + ContainsAllAlerts + dashboards render; promtool 3.4.1 on PATH)
- `golangci-lint --new-from-merge-base ./cmd/workspace-agentd/...` — 0 issues

## Files Modified

- `cmd/workspace-agentd/sessionstate/{authority.go,ledger.go,metrics_test.go(new)}`
- `cmd/workspace-agentd/{sessionstate_metrics.go(new),sessionstate_metrics_test.go(new),sessionstate_wiring.go,server.go,main.go}`
- `helm/{templates/prometheus-rules.yaml,templates/dashboards-configmap.yaml,dashboards/operational.json,tests/alerts_promtool_test.yaml,chart_test.go}`
- `docs/operator/monitoring.md`

# Worklog: epic-71 / 0c-alerts — canary alert rules + dashboard panels + loop-liveness consumers (#1312 alerts wave)

**Date:** 2026-09-15
**Session:** Implement the alerting layer for the 0c production canary (from the 2026-09-15 assessment issuecomment-5690026976 §3/§5.2): the chart had 24 rules with zero canary/S/L coverage and `llmsafespaces_loop_last_run_timestamp_seconds` had no consumer. Stream epic-71 / 0c-alerts; the prod knob flip (`api.canary.enabled=true`, talos-ops-prod) stays an owner action.
**Status:** Complete

---

## Objective

Make the canary flip safe by defining what pages when it arms:

1. **S-violation immediate** on the canary's failure-classification counter (`resolve/not_found_error` = S6 violated).
2. **L-breach burn-rate** (NOT per-sample, per #1312's rollout rule) on the probe-duration histogram against the L2 bound.
3. **Dead-loop detection** on the loop-liveness family for all three registered loops.
4. Dashboard panels (probe rate/classifications, L-bound latency, loop-liveness grid), promtool scenarios for every rule (firing / healthy / dark), an ops note on what flips.

## Metric contract (Rule 7 — read from the emitters, never invented)

| Metric | Emitter | Labels / values used |
|---|---|---|
| `llmsafespaces_canary_probe_outcomes_total` | `api/internal/services/canary/metrics.go:12` | `class,leg,outcome`; legs `snapshot\|resolve` (canary.go:70-71); S6 = `leg="resolve",outcome="not_found_error"` (canary.go:77-78,90); wedge = `outcome="timeout"` (canary.go:83); `no_target/pick_error/unresolved_target` = targeting failures not pod verdicts (canary.go:84-85); `canceled` = rolling-restart noise (canary.go:303-306) — never alerted |
| `llmsafespaces_canary_probe_duration_seconds` | `metrics.go:17` | histogram, buckets 0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5; resolve leg IS the L2 surface (`DefaultTimeout` 2s == the L2 bound, canary.go:63-66) |
| `llmsafespaces_loop_last_run_timestamp_seconds` | `pkg/obs/loopliveness.go:23` (single registration; stamped end-of-pass only) | `loop` ∈ {`reconcile_watchdog` (agentd, 15s `ReconcileCadence` = `LeaseConvergenceBound`/2, sessionstate/reconcile.go:44-48), `outbox_parked_sweeper` (API, 60s `ParkedSweepInterval`, parked_sweeper.go:78), `canary_probe` (API, 60s default interval)} |

Scrape jobs verified: API metrics via ServiceMonitor `<fullname>-api` (`$apiJob`); agentd via PodMonitor job relabel (`$agentdJob` = `.*agentd.*`). The parked sweeper rides the outbox `Run` loop started unconditionally (`app.go:299` SetOutbox → `proxy_lifecycle.go:99` `outbox.Run`), so its absent-series = dead-since-boot. The canary service is constructed nil when the knob is off (`app.go:1555`) → zero canary series in prod today.

## What landed

**`helm/templates/prometheus-rules.yaml`** — 7 alerts (24 → 31; api group +5, agentd group +1):

| Alert | Severity / hold | Expression shape |
|---|---|---|
| `LLMSafeSpacesCanaryS6Violation` | critical, `for: 0m` | `sum by (class) (increase(outcomes{leg="resolve",outcome="not_found_error"}[10m])) > 0` — page-on-any (#1312 "S- violation (immediate)") |
| `LLMSafeSpacesCanaryProbeTimeouts` | warning, `for: 10m` | `sum by (class, leg) (increase(outcomes{outcome="timeout"}[10m])) >= 3` — the wedge signal; 1 probe/class/leg/pass means a single blip cannot reach 3 |
| `LLMSafeSpacesCanaryResolveL2Burn` | warning, `for: 15m` | `histogram_quantile(0.99, sum by (class, le) (rate(duration_bucket{leg="resolve"}[10m]))) > 2` — burn not per-sample: **for > window** makes one slow probe structurally unable to fire; timeouts land in the (1,2.5] bucket → wedged class interpolates p99 ≈ 2.485 > 2 |
| `LLMSafeSpacesCanaryNoTarget` | info, `for: 1h` | sustained `no_target` per class = false coverage (knob on, class asserts nothing); window 30m < for 1h keeps transient mass-suspend silent |
| `LLMSafeSpacesLoopCanaryProbeStale` | warning, `for: 5m` | `(time() - loop_last_run{loop="canary_probe"}) > 300`; **NO absent() arm** — a disabled canary must look dark, not stale |
| `LLMSafeSpacesLoopParkedSweeperStale` | critical, `for: 5m` | staleness > 180 (3×60s) **or absent()** (dead since boot — the SecretsReconcileStalled pattern; loop is unconditional) |
| `LLMSafeSpacesLoopReconcileWatchdogStale` | critical, `for: 2m` | per-instance staleness > 90 (6×15s); **NO absent() arm** — agentd pods are ephemeral (suspend/resume), only live-but-not-stamping pages |

**`helm/tests/alerts_promtool_test.yaml`** — 21 new scenarios: firing rows for every alert (incl. per-instance watchdog with a healthy sibling, and the absent()-arm row whose exp_labels pin that `absent()` carries the equality matcher's `loop` label), silent rows for healthy/blip cases (single timeout, single slow probe checked at peak-pending 21m and post-window 25m, transient no-target, halted-gauge-style healthy siblings), and **dark-canary rows**: each canary alert stays silent with zero canary series (with sibling/loop series present so an added `absent()` arm or a loosened matcher would fire and fail the suite).

**`helm/chart_test.go`** — the 7 names appended to `TestMonitoring_PrometheusRule_ContainsAllAlerts`; new `TestMonitoring_DashboardCanaryLoopPanels` pinning the dashboard contract (row title, outcomes counter, `histogram_quantile` over `leg="resolve"` buckets, loop-liveness queries against BOTH job placeholders, no unpinned loop-liveness matcher).

**`helm/dashboards/operational.json`** — new "Canary & Loop Liveness (Epic 71)" row (y=94, ids 51-55): probe rate by class/leg/outcome (timeseries), S6 violations stat (red ≥1), resolve-leg p50/p99 vs the 2s L2 budget, loop-liveness grid (seconds since last completed pass, API + agentd queries, red ≥300s). Round-trip-verified JSON formatting (indent 2, ensure_ascii=False — byte-identical dump of untouched panels).

**`helm/values.yaml`** canary comment + **`helm/MONITORING-OPERATIONAL.md`** §"Canary alerts & loop liveness" — the ops note: what is armed today (the two unconditional loop alerts), what flips with `enabled=true` (the five canary arms), threshold-scaling guidance (LoopCanaryProbeStale 5× interval; sweeper/watchdog budgets are code-constant multiples), and why L3/L4 are not canary-measurable.

**`COORDINATE.md`** — active claim row (epic-71 / 0c-alerts).

## Key decisions

1. **L2 only on the duration histogram; L3/L4 documented as not canary-measurable.** #1312's budget table maps L2 (resolve ≤ 2s) to the resolve leg exactly (DefaultTimeout == the bound). L3 (30s convergence) requires a real divergence; synthetic probe ids never diverge (`SyntheticSessionID` cannot hold pending state), and the histogram tops out at 5s anyway. L3/L4 enforcement stays with the (green) fault-matrix harness — which was the gate for shipping this wave at all. Stated in the rules' comment block, the ops note, and here; the L1/real-ask probe remains owner-budget-gated.
2. **for > window as the per-sample guarantee.** The L2 burn holds 15m over a 10m window; a single blip can keep the condition true at most `window` minutes, so it can never satisfy `for`. Pinned by the blip scenario at peak-pending time.
3. **absent() asymmetry is deliberate and load-bearing.** Sweeper: unconditional loop → absent = dead since boot → arm. Canary: knob-gated → absent = disabled → NO arm (dark, not stale). Watchdog: ephemeral pods → absent = suspended fleet → NO arm (staleness markers make disappeared targets silent). Each no-arm case has a dedicated promtool row that fails if someone adds the arm.
4. **NoTarget at info tier, not warning.** A suspended-for-an-hour fleet legitimately no_targets; only the *sustained* (1h) case surfaces, and it surfaces as information (false coverage), not a page.
5. **S6 without `for` but with a 10m increase window** — immediate per #1312; the window only absorbs counter re-reads, not time.
6. **No chart-level gating of the canary rules** on `api.canary.enabled` — the rules ship armed-but-inert on absent series, which is exactly what the dark-canary scenarios prove; gating rules on the knob would silently DELETE the loop alerts' canary arm when an operator toggles the knob off (and the flip is a prod-values change, not a chart change).

## Rule 7 — Assumptions stated and validated

| # | Assumption | Validation |
|---|---|---|
| A1 | Metric names/labels exactly as above | Read from `metrics.go`, `canary.go`, `loopliveness.go` (cited inline in the rules' comment block) |
| A2 | Buckets 0.005..5 make p99 interpolation for a wedged class ≈ 2.485s (> 2) | Computed (1 + 0.99×(2.5−1)) and pinned by the firing scenario's exact annotation string "2.485s" |
| A3 | Cadences 60s/60s/15s | `DefaultInterval` canary.go:64, `ParkedSweepInterval` parked_sweeper.go:78, `ReconcileCadence` reconcile.go:46-48 |
| A4 | Parked sweeper unconditional in the API | `app.go:299` (SetOutbox always) → `proxy_lifecycle.go:82-104` (Run started in handler Start) |
| A5 | Canary nil-service when knob off → no series at all | `app.go:1555` `if a.canarySvc != nil`; `New` returns idle service only for empty Classes and the chart renders the env only when enabled (api-deployment.yaml:150) |
| A6 | `absent()` carries equality matchers (loop label) into the alert | promtool run initially failed with exactly that label mismatch; expectation updated and documented in the scenario |
| A7 | `histogram_quantile` silent on absent series (dark canary) | Dark scenario passes with the real rendered expression |
| A8 | Watchdog is per-agentd-pod (instance, not workspace, distinguishes series) | GaugeVec has only the `loop` label (loopliveness.go:38-41); scrape adds instance; runSessionStateWatchdog runs per pod (main.go:224) |

**Mutation (non-vacuousness) proofs — each observed to fail the suite, then restored:** (1) S6 matcher loosened to `outcome=~".+"` → healthy row fails; (2) absent() arm added to LoopCanaryProbeStale → dark-canary row fails; (3) L2 bound 2→3 → firing row fails; (4) watchdog wrapped in `max by (loop)` → per-instance row fails; (5) sweeper budget 180→600 → stale row fails.

## Blockers

None. Local env: promtool 3.4.1 + helm 3.22 installed to `/tmp/opencode/bin` (helm ≥3.19 needed for the chart's `kubeVersion >=1.35.0` — helm-render fails under the older default; CI's get-helm-3 installs current).

## Tests Run

- RED (rules/panels absent): `go test ./helm/ -run 'TestPromtoolRules|TestMonitoring_PrometheusRule_ContainsAllAlerts|TestMonitoring_DashboardCanaryLoopPanels'` → 3 FAILs (7 firing scenarios + inventory + dashboard row).
- GREEN: same command → `ok`; full `go test -count=1 ./helm/...` → `ok` (26s) with promtool+helm on PATH.
- `make repolint` → all checks passed. `make helm-render` → OK (helm 3.22). `helm lint` → 0 failed.
- Mutation drills 1-5 above → FAIL each, restored after.

## Next Steps

- Owner: flip `api.canary.enabled=true` + classes in talos-ops-prod (Post-deploy ops; needs ≥1 Active workspace per class) — the ops note tells them what arms.
- Owner decision pending: token-spend probe budget (real-ask L1, Deliver-path) — the only path to an automated L1 row.
- Possible follow-up (not taken): SLO-style multi-window burn-rate if probe volume ever justifies it; today's 1 probe/class/min makes single-window + hold the honest choice.

## Files Modified

- `helm/templates/prometheus-rules.yaml` (+7 alerts, 2 comment blocks)
- `helm/tests/alerts_promtool_test.yaml` (+21 scenarios)
- `helm/chart_test.go` (inventory + new TestMonitoring_DashboardCanaryLoopPanels)
- `helm/dashboards/operational.json` (+row +4 panels, version bump)
- `helm/values.yaml` (canary comment rewrite)
- `helm/MONITORING-OPERATIONAL.md` (new section)
- `COORDINATE.md` (claim row)
- `worklogs/NNNN_2026-09-15_canary-alert-rules.md` (new)

---

## Review round 1 (CHANGES_REQUESTED) — corrections

Three findings, all mutation-confirmed test gaps against the PR's own stated invariants; fixed as scenario additions, each proven red against its mutant BEFORE being trusted green:

1. **Per-instance anti-masking unpinned for both API loop alerts** → added two-instance scenarios (healthy sibling + frozen instance) for `LoopCanaryProbeStale` and `LoopParkedSweeperStale`. Mutation drills: `min by (job, loop)` fleet-masking on each expr → both rows FAIL (observed), restored.
2. **Any-leg semantics unpinned for `CanaryProbeTimeouts`** → added a sustained `snapshot/timeout` firing row (class node:22). Mutation drill: adding `leg="resolve"` to the matcher → row FAILS (observed), restored.
3. **No dark row for `CanaryResolveL2Burn`** → added the zero-buckets scenario with outcome series flowing (the real `no_target` shape — probes skipped before duration observation). Mutation drill: adding an `absent()` arm on the bucket selector → row FAILS (observed), restored.

Non-blocking corrections also taken:

- Sweeper comment overstatement fixed: the loop is wired wherever the Redis-backed cache service is active (`app.go` `SetOutbox` sits inside the `*cache.Service` assertion); the InMemoryStore dev fallback never wires the outbox and would trip the absent arm — dev-only false page, disclosed in the comment (verified at app.go:274-299).
- `CanaryProbeTimeouts` comment reworded: the blip-proofness claim now states the per-replica nuance (≥3 replicas, one fleet-wide bad pass reaches 3 — correct paging there).
- Count corrections (this worklog + PR body): the file goes **34 → 41 alerts** (api +6, agentd +1), not "24 → 31" — the assessment's "24 rules" figure undercounted the pre-existing set (14 api + 10 controller + 7 agentd + 3 billing = 34, verified by `grep -c "alert: LLMSafeSpaces"` on origin/main).
- COORDINATE.md claim no longer lists `helm/promtool_rules_test.go` (the runner needed no change — r1 nit).

# Monitoring & Observability — Operational Runbook

This document describes how this chart's monitoring surface (Prometheus ServiceMonitors, Grafana dashboards, alert rules) interacts with the cluster's observability stack, and what operational procedures are required to keep it healthy.

For chart upgrade procedures (UID changes, dashboard renames), see `CHART-UPGRADE.md`.

For KEK rotation procedures, see `KEK-ROTATION.md`.

---

## Overview

The chart ships three observability surfaces, all gated by `monitoring.enabled` (default: false; commonly true in production):

| Surface | Template | Picked up by |
|---|---|---|
| ServiceMonitor for the API + controller | `servicemonitor.yaml`, `controller-servicemonitor.yaml` | Prometheus Operator (kube-prometheus-stack) |
| Alert rules | `prometheus-rules.yaml` | Prometheus Operator |
| Grafana dashboards | `dashboards-configmap.yaml` | Grafana sidecar provisioner (`kiwigrid/k8s-sidecar` or equivalent), via the `grafana_dashboard: "1"` ConfigMap label |

This chart does NOT deploy Grafana or Prometheus itself. Operators must have a working kube-prometheus-stack (or compatible) installation that scrapes ServiceMonitors and a working Grafana with a dashboard sidecar that watches ConfigMaps with the `grafana_dashboard` label.

---

## Multi-replica Grafana sidecar provisioning race

### The problem

Grafana installations commonly run with `replicas >= 2` for HA. Each Grafana pod runs its own `kiwigrid/k8s-sidecar` (or equivalent) container that watches Kubernetes for ConfigMaps labeled `grafana_dashboard: "1"`, and writes their content to `/tmp/dashboards` for Grafana to provision.

In multi-replica deployments, **all sidecars receive the same ConfigMap update event simultaneously** and all attempt to upsert the dashboard into the shared Grafana database (Postgres in this cluster) at the same time. Grafana's optimistic-concurrency guard for dashboard upserts checks for existing dashboards by an internal hash-based ID; under simultaneous writes from multiple replicas, the check intermittently sees a transient "found 2 with same ID, desired 1" state and refuses to write, logging:

```
logger=provisioning.dashboard type=file name=sidecarProvider
  level=error msg="failed to save dashboard"
  file=/tmp/dashboards/operational.json
  error="unexpected number of dashboards for id 1029897025667072. found: 2. desired: 1"
```

When this happens:
- The dashboard JSON in `/tmp/dashboards/` matches the chart (sidecar successfully wrote the file)
- Grafana's database has the OLD content (sidecar provisioner refused to upsert)
- Operators see "stale dashboard" symptoms — bookmarked URLs return data from the old version

### Mitigations

**Recommended (Grafana-side fix, not in this chart's scope):**

Enable leader election on the dashboard sidecar so only one replica's sidecar provisions at a time. In the `grafana` Helm chart's values, this is:

```yaml
sidecar:
  dashboards:
    enabled: true
    label: grafana_dashboard
    leaderElection:
      enabled: true   # only one replica's sidecar provisions at a time
```

Operators of the Grafana installation should set this. This chart cannot enforce it because the Grafana deployment is a separate Helm release in a separate namespace (commonly `monitoring`).

**Manual recovery if the race triggers:**

If you observe the symptoms (stale dashboard content despite a recent helm upgrade, "found 2" in Grafana logs):

1. Run the manual cleanup script (see `CHART-UPGRADE.md` § "Manual cleanup procedure") to remove orphaned dashboard rows whose UIDs don't match the current chart.
2. Scale Grafana down and back up to clear in-memory replication state across the replicas:

   ```sh
   ORIGINAL=$(kubectl get deploy -n monitoring grafana -o jsonpath='{.spec.replicas}')
   kubectl scale -n monitoring deploy/grafana --replicas=0
   sleep 10
   kubectl scale -n monitoring deploy/grafana --replicas=${ORIGINAL}
   ```

3. Wait ~30 seconds for the sidecar to re-provision dashboards, then verify in Grafana.

### Why we don't auto-fix this from inside this chart

A pre-upgrade Helm hook Job that purges stale dashboards via Grafana's REST API would be possible, but:

- It requires the chart to know Grafana's URL + admin credentials, coupling this chart to the Grafana installation
- A failed hook (Grafana down during upgrade) blocks the chart upgrade for an unrelated reason
- Most installations won't hit the race; an automatic fix punishes everyone for an edge case

The manual procedure in `CHART-UPGRADE.md` is the right tool for the rare case when the race actually causes a problem.

---

## Job-label portability

The chart's Grafana dashboards use PromQL queries with `{job=~"<release>-api.*"}` and `{job=~"<release>-controller.*"}` matchers. The `<release>` prefix is rendered at chart-render time by `dashboards-configmap.yaml`'s Helm `replace` pipeline against placeholder strings (`__LLMSAFESPACES_API_JOB__`, `__LLMSAFESPACES_CTRL_JOB__`) in the dashboard JSON files.

This means the dashboards work in any Helm release, regardless of the release name. The `chart_test` `TestMonitoring_DashboardJobVariablesPortable` enforces the contract.

If you observe panels with "No data" despite Prometheus having metric series, check:

1. The deployed ConfigMap has the right job patterns (no leftover `__LLMSAFESPACES_*_JOB__` placeholders, and the substituted values match your release name's ServiceMonitor labels):

   ```sh
   kubectl get configmap -n <release-ns> <release>-grafana-dashboards \
       -o jsonpath='{.data.operational\.json}' | grep -oE 'job=~"[^"]+"' | sort -u
   ```

2. Prometheus actually has the data:

   ```sh
   kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090
   curl 'http://localhost:9090/api/v1/query?query=count(llmsafespaces_workspaces_running)'
   ```

3. The dashboards in Grafana have been re-provisioned after the ConfigMap update (sidecar logs in the Grafana pod, or check the file's `md5sum` against the chart's `dashboards/*.json`).

---

## Dependency-up + db-pool metrics

PR #356 wired six metric families that previously had no observation calls in production code:

- `llmsafespaces_db_query_duration_seconds` — pgx tracer attached to all `*sql.DB` and `*pgxpool.Pool` instances
- `llmsafespaces_db_errors_total` — same tracer
- `llmsafespaces_redis_command_duration_seconds` — go-redis hook
- `llmsafespaces_redis_errors_total` — same hook
- `llmsafespaces_dependency_up` — health-checker probes for postgres + redis
- `llmsafespaces_auth_attempts_total` — auth handler

Counters/histograms only emit a series after their first observation. On a healthy quiet system, panels for `_errors_total` and similar will correctly show "No data" — that's the expected behavior, not a bug. The error-side panels populate when something actually fails. To force-test, e.g. triggering an auth failure or running a synthetic load.

---

## Canary alerts & loop liveness (Epic 71 / 0c-alerts)

The #1312 production canary and the shared loop-liveness family (`pkg/obs`) got their alerting and dashboard layer in the 0c-alerts wave. What exists and what flips:

### What is live right now (canary knob OFF)

With `api.canary.enabled=false` (the default and current prod state) the API constructs no canary service, emits **zero** `llmsafespaces_canary_probe_*` series, and never stamps the `canary_probe` loop gauge. Every canary alert is **structurally silent on absent series** — no `absent()` arms exist on canary metrics — so the dark canary pages nobody. This is pinned per-alert in `helm/tests/alerts_promtool_test.yaml`.

Two loop-liveness alerts are armed **regardless of the canary knob**, because their loops run unconditionally:

| Alert | Loop | Sev | Meaning |
|---|---|---|---|
| `LLMSafeSpacesLoopParkedSweeperStale` | `outbox_parked_sweeper` (API, 60s cadence) | critical | Parked user-visible errors stop re-arming (S3-adjacent strand path silent). Includes the `absent()` arm: a fleet that never stamped is dead since boot. |
| `LLMSafeSpacesLoopReconcileWatchdogStale` | `reconcile_watchdog` (agentd, 15s cadence) | critical | S5/S7 convergence and the L4/L5 lease bounds are dark for one pod. No `absent()` arm — agentd pods are ephemeral (suspend/resume); only a live-but-not-stamping pod pages. |

### What flips when `api.canary.enabled=true`

The API starts probing each configured class once per interval (GetSnapshot round-trip + resolve-by-absence Act; zero model spend). Four alert arms light up on the new series, plus the loop-liveness arm for the runner itself:

| Alert | Sev | Trigger |
|---|---|---|
| `LLMSafeSpacesCanaryS6Violation` | critical, immediate | any `resolve/not_found_error` outcome in 10m — absence surfaced as an error instead of resolving (safety invariant; every occurrence is a bug) |
| `LLMSafeSpacesCanaryProbeTimeouts` | warning, 10m hold | ≥3 `timeout` outcomes per class/leg in 10m — the wedge signal |
| `LLMSafeSpacesCanaryResolveL2Burn` | warning, 15m hold | resolve-leg p99 > 2s (the L2 resolve bound) over 10m — burn-rate, not per-sample: the 15m hold exceeds the 10m window, so one slow probe structurally cannot fire |
| `LLMSafeSpacesCanaryNoTarget` | info, 1h hold | a configured class records `no_target` every pass — the knob is on but the class asserts nothing (false coverage; point `classes` at a runtime environment with an Active workspace) |
| `LLMSafeSpacesLoopCanaryProbeStale` | warning, 5m hold | the `canary_probe` gauge goes > 5m stale — the runner is wedged |

### Threshold scaling

- `LoopCanaryProbeStale` budget (5m) = 5× the default 60s `api.canary.interval` — scale it if you raise the interval. A pass is `classes × ~4s` worst-case sequential, so large class lists also warrant headroom.
- `LoopParkedSweeperStale` (3m) = 3× `ParkedSweepInterval` (60s, code constant). `LoopReconcileWatchdogStale` (90s) = 6× `ReconcileCadence` (15s, code constant).
- `CanaryResolveL2Burn` compares against the 2s L2 bound (#1312's budget table), which equals `DefaultTimeout`; a different `api.canary.timeout` changes what timeouts mean — keep them aligned.

L3/L4 (convergence bounds) are **not** canary-measurable — synthetic probe ids never diverge, so there is no convergence to time. Their enforcement lives in the fault-matrix harness (`cmd/workspace-agentd/faultmatrix`); the token-spend probes that could assert L1/the real-ask path remain gated on the owner budget decision.

The "Canary & Loop Liveness (Epic 71)" dashboard row (operational.json) carries the probe rate by class/leg/outcome, the S6 violation stat, the resolve-leg p50/p99 against the 2s budget, and the loop-liveness grid (seconds since last completed pass per loop/instance, red over 300s).

---

## Future improvements (not blocking)

- Helm hook for automatic stale-dashboard purge (declined for now — see "Why we don't auto-fix this" above)
- Dashboard JSON validation at build time (current chart_test covers structure; could add JSON-schema validation against Grafana's published schema)
- Per-tenant ServiceMonitor targets if/when multi-tenant scraping becomes a real requirement

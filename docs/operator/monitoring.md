# Monitoring

This page covers observability for an LLMSafeSpaces deployment: the Prometheus metrics exposed by the API, controller, and workspace-agentd sidecar; the Grafana dashboards shipped in the chart; PodMonitor and ServiceMonitor configuration; the `readyz` health contract; and the key alerts to set up. Monitoring is opt-in via `monitoring.enabled` (off by default) with independent sub-toggles.

## On this page

- [Enabling monitoring](#enabling-monitoring)
- [Prerequisites](#prerequisites)
- [Metrics exposed](#metrics-exposed)
- [Service and Pod monitors](#service-and-pod-monitors)
- [Grafana dashboards](#grafana-dashboards)
- [Prometheus alerting rules](#prometheus-alerting-rules)
- [The readyz contract](#the-readyz-contract)
- [Controller metrics endpoint](#controller-metrics-endpoint)
- [API metrics authentication](#api-metrics-authentication)
- [Dashboard UID stability](#dashboard-uid-stability)

---

## Enabling monitoring

```yaml
monitoring:
  enabled: false   # master toggle (off by default)

  dashboards:
    enabled: true
  datasources:
    enabled: true
  prometheusRules:
    enabled: true
  serviceMonitors:
    enabled: true
    agentdPodMonitor:
      enabled: true
```

Set `monitoring.enabled=true` to deploy Grafana dashboard ConfigMaps, `PrometheusRule` resources, and `ServiceMonitor`/`PodMonitor` resources. Each sub-section can be enabled/disabled independently.

```bash
helm upgrade llmsafespaces ./helm \
    --set monitoring.enabled=true \
    -n llmsafespaces
```

---

## Prerequisites

| Requirement | Why |
|---|---|
| **Prometheus Operator** | For `PrometheusRule` and `ServiceMonitor`/`PodMonitor` CRDs. |
| **Grafana with sidecar dashboard importer** | Dashboard ConfigMaps use the `grafana_dashboard: "1"` label. |
| **Grafana with sidecar datasource importer** | Datasource ConfigMaps use the `grafana_datasource: "1"` label. The billing dashboard queries Postgres directly. |
| **A Postgres password Secret** | The datasource references `${PG_PASSWORD}`; configure Grafana to inject this from the credentials Secret. |

---

## Metrics exposed

### API service

The API exposes Prometheus metrics on `/metrics` (port 8080). When `LLMSAFESPACES_METRICS_TOKEN` is set, the endpoint requires `Authorization: Bearer <token>`. Key metric families:

| Metric | Type | Description |
|---|---|---|
| `http_requests_total` | Counter | Request count by method, path, status |
| `http_request_duration_seconds` | Histogram | Latency by method, path |
| API error rate | (derived) | `rate(http_requests_total{status=~"5.."}[5m]) / rate(http_requests_total[5m])` |
| Active DB connections | Gauge | `postgresql.maxOpenConns` pool utilization |
| Redis operations | Counter | Cache hit/miss, rate-limit decisions |

### Controller

The controller exposes controller-runtime metrics. By default bound to `127.0.0.1:8080` (loopback only — same-pod sidecars). When `monitoring.serviceMonitors.enabled=true`, the chart overrides `controller.metricsAddr` to `0.0.0.0:8080` so the ServiceMonitor can reach it.

| Metric | Type | Description |
|---|---|---|
| `controller_runtime_reconcile_total` | Counter | Reconcile calls by controller, result |
| `controller_runtime_reconcile_errors_total` | Counter | Reconcile errors |
| `controller_runtime_reconcile_time_seconds` | Histogram | Reconcile duration |
| Workspace phase transitions | Counter | By from/to phase |
| `workqueue_depth` | Gauge | Per-controller work queue depth |

### Workspace-agentd sidecar

Workspace pods are dynamic (one per `Workspace` CRD) and addressed by PodIP, so a **PodMonitor** (not ServiceMonitor) is the correct resource. When `monitoring.serviceMonitors.agentdPodMonitor.enabled=true`, the chart also extends the workspace NetworkPolicy to permit Prometheus ingress on port 4098.

| Metric | Type | Description |
|---|---|---|
| `workspace_restarts_total` | Counter | Pod restart count |
| `workspace_memory_bytes` | Gauge | Memory usage from cgroup v2 |
| `workspace_active_sessions` | Gauge | Active agent sessions |
| `workspace_context_tokens` | Gauge | Context window utilization |
| `workspace_oom_kills_total` | Counter | OOM kill attribution |

#### Session state (Epic 69, US-69.12)

The delivery-ledger projection exports its own health surface (design 0055 R5):

| Metric | Type | Labels | Description |
|---|---|---|---|
| `llmsafespaces_seq_stall_seconds` | Gauge | `workspace_id` | Seconds since the projection last advanced — the starvation signal. Idle pods legitimately stall (no events); correlate with `ledger_depth` before acting. |
| `llmsafespaces_ledger_depth` | Gauge | `workspace_id`, `state` | Row count per ledger state (the funnel: `ledgered` → `admitted` → `promoted`/`turn_ended`, with `stalled` and `failed` visible). |
| `llmsafespaces_stalled_entries` | Gauge | — | Rows in `stalled` (admitted past the promotion deadline) — the #1119 silent-strand class, visible. |
| `llmsafespaces_promotion_stall_seconds` | Gauge | `workspace_id` | Age of the oldest admitted-unpromoted row (0 when none) — the stall precursor. |
| `llmsafespaces_delivery_202_latency_seconds` | Histogram | — | Deliver-ack latency (the 202 equivalent: idempotent fsync'd ledger accept). |
| `llmsafespaces_snapshot_size_bytes` / `llmsafespaces_snapshot_latency_seconds` | Histogram | — | GetSnapshot cost (R9: O(in-flight state), never O(history)). |
| `llmsafespaces_wake_failures_total` | Counter | — | Errored stall-wake attempts (the wake is a store reseed; failure = harness/store unreachable). |
| `llmsafespaces_sessionstate_dropped_events` / `_parser_failures` / `_panics_contained` | Gauge | — | Cumulative projection containment counters. |
| `llmsafespaces_sessionstate_subscribers` | Gauge | — | Active ABI stream subscribers (on-demand consumption health). |

##### Session-state alert triage

- **`LLMSafeSpacesSeqStalled`** (critical): seq stalled >5m **with ledgered work pending** — starvation or a wedged harness, not an idle pod. Triage: check CFS throttling (`container_cpu_cfs_throttled_periods_total` for the pod) and the watchdog suppression counter; if starved, the watchdog is deliberately suppressing restarts — free CPU or evict the noisy neighbor. If not starved, exec into the pod and `curl :4097` GetSnapshot — a wedged opencode with a live agentd needs the stranded-entry path.
- **`LLMSafespacesStalledEntries`** (warning): an admitted entry crossed the promotion deadline and the wake (store reseed) did not resolve it. The user-visible turn is stranded: `GetDeliveryStatus(entryId, attempt)` on the pod's ABI to read the row; if the transcript never landed, replay the entry (the outbox re-arms at attempt+1 only on `failed` — a stalled row needs manual re-drive or the [authority flip/park runbook](../runbooks/authority-flip.md)).
- **`LLMSafespacesWakeFailures`** (warning): the reseed behind the wake is failing — opencode unreachable or store read errors. The stalled entries will not self-resolve; fix the harness path first (agentd logs `stall wake failed`).
- **`LLMSafeSpacesDeliveryLatencyBudget`** (warning): Deliver-ack p99 > 1s sustained — fsync latency on the platform volume or WAL contention. Every send on the pod slows; check the volume's write latency and ledger WAL compaction (`ledger.compact` runs at retention boundaries).

#### Event bus (design 0072, S3)

The workspace event bus (one Redis/Valkey Stream per workspace, `workspace:{id}:events`) exports its health surface when `AGENTD_EVENTBUS_ADDR` arms the bus. Collected every 30s by agentd's health loop (first snapshot at startup) and scraped on the same admin port.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `workspace_agentd_eventbus_publishes_total` | Counter | `workspace_id`, `topic_family`, `outcome` | Publish outcomes: `published` / `failed` (validation or transport). A failed publish falls back to the direct v1 delivery leg — wake-ups still flow; the counter rise is the degradation signal. |
| `workspace_agentd_eventbus_last_publish_timestamp_seconds` | Gauge | `workspace_id` | Unix timestamp of the newest entry's publish time. **The silent-publisher signal**: alert on `time() -` this while sessions are busy. No series = the stream has no entries. |
| `workspace_agentd_eventbus_deliveries_total` | Counter | `workspace_id`, `group`, `outcome` | Dispatch outcomes per consumer group: `delivered`, `filtered` (pattern-skipped, acked unread), `handler_error` (left pending — the reclaim is the retry), `poison` (dropped past the attempt cap), `ack_error`. |
| `workspace_agentd_eventbus_redeliveries_total` | Counter | `workspace_id`, `group` | Dispatches that were redeliveries (delivery count > 1) — the at-least-once machinery working; rising without `delivered` rising means work is being retried, not completed. |
| `workspace_agentd_eventbus_stream_entries` | Gauge | `workspace_id` | Stream entry count (bounded ~10k by the publisher's MAXLEN — the retained-replay window). |
| `workspace_agentd_eventbus_group_lag` | Gauge | `workspace_id`, `group` | Entries never delivered to the group (capped at 10000). Consumers dead or falling behind. |
| `workspace_agentd_eventbus_group_pending` | Gauge | `workspace_id`, `group` | Delivered-but-un-acked entries: failing handlers, or consumers that died mid-work (the reclaim sweep is the retry). |
| `workspace_agentd_eventbus_group_consumers` | Gauge | `workspace_id`, `group` | Registered consumer names. 0 with lag/pending > 0 = the dead-group shape (names persist until `XGROUP DELCONSUMER`, so 0 means nobody ever joined or all were removed). |
| `workspace_agentd_eventbus_group_oldest_pending_idle_seconds` | Gauge | `workspace_id`, `group` | How long the longest-idle pending entry has sat un-acked (0 = empty). Healthy consumers reclaim past 60s; far past that, nobody is reclaiming — the wedged-group signal. |
| `workspace_agentd_eventbus_group_stalled` | Gauge | `workspace_id`, `group` | Derived 0/1: no consumers with a backlog, oldest pending idle ≥ 5m, or lag ≥ 100. Edge-logged (`event bus: consumer group STALLED` / `recovered`) on transition. |

##### Event bus health model — what a healthy bus looks like

- **A quiet bus is healthy**: no workspace activity means no publishes, empty groups, all-zero gauges. Silence alone is NOT a fault — silence *plus* a growing backlog is.
- **Healthy publish leg**: `publishes_total{outcome="published"}` rises when sessions settle (or the lease ladder nudges); `failed` stays flat. A silent publisher is caught by `time() - last_publish_timestamp_seconds` exceeding any sane quiet period *while sessions are visibly busy* — that combination means settles stopped reaching the bus (agentd also logs each fallback to the direct leg at publish failure).
- **Healthy consumer group** (`notifier`, `lease`): `lag` returns to 0 shortly after publishes, `pending` returns to 0, `oldest_pending_idle` stays near 0. Transient blips (a burst, one handler error) are the at-least-once machinery working — `redeliveries_total` may tick.
- **Stalled group, dead shape**: `group_consumers == 0` with `lag > 0` — nobody is consuming; every publish grows the backlog forever. Usually the agentd process died or never started its groups (check the pod and the `event bus: transport wired` boot log).
- **Stalled group, wedged shape**: `group_oldest_pending_idle_seconds` ≥ 300 — entries were delivered but no consumer reclaimed them through five reclaim sweeps (healthy reclaim is 60s). The consumer names may still be registered (dead pods leave stale names); the idle time is the truth.
- **Failing-but-alive group**: `deliveries_total{outcome="handler_error"}` (or `poison`) rising while `lag`/`pending` cycle — the group consumes but its work fails; that is a downstream fault (delivery seam), not a bus fault.
- **Probe failure**: agentd logs `event bus: health probe failed` (first failure, then every 10th) — Redis/Valkey unreachable from the pod; gauges go stale at their last values, which is itself the signal.

!!! warning "cgroup v2 required"
    `workspace_memory_bytes` and `workspace_oom_kills_total` require cgroup v2. On cgroup v1 hosts these silently produce nothing — agentd logs a single Warn per pod boot. See [Storage](storage.md#cgroup-v2-requirement).

Without the agentd PodMonitor + NetworkPolicy rule, these metrics are never scraped and do not appear in production dashboards.

---

## Service and Pod monitors

### API + controller (ServiceMonitor)

```yaml
monitoring:
  serviceMonitors:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
    labels: {}        # add labels to match your Prometheus Operator selector
    api:
      bearerTokenSecret: {}   # set when LLMSAFESPACES_METRICS_TOKEN is used
```

### Workspace agentd (PodMonitor)

```yaml
monitoring:
  serviceMonitors:
    agentdPodMonitor:
      enabled: true

networkPolicy:
  prometheusPodLabelSelector:
    app.kubernetes.io/name: prometheus
  prometheusNamespace: ""   # set to where Prometheus runs, e.g. "monitoring"
```

The `prometheusPodLabelSelector` defaults to the kube-prometheus-stack chart's labels. Override to match a custom Prometheus deployment. Without the NetworkPolicy rule, Prometheus cannot reach the workspace admin port (4098).

---

## Grafana dashboards

The chart ships two dashboards via ConfigMaps, picked up by the Grafana sidecar:

| Dashboard | UID | Content |
|---|---|---|
| **LLMSafeSpaces - Operational** | `llmsafespaces-operational` | Request overview, connections, workspace lifecycle, reconciliation, recovery, agent operations, SSE/relay, billing at a glance |
| **LLMSafeSpaces - Billing & Metering** | `llmsafespaces-billing` | Inference cost/token breakdown, per-user metering (active seconds, CPU, LLM calls), per-workspace resource consumption (storage, memory, CPU, proxy bytes) |

### Datasource

The billing dashboard queries Postgres directly for per-user/per-workspace attribution against the `usage_events` table. A datasource ConfigMap is deployed:

```yaml
monitoring:
  datasources:
    enabled: true
    postgres:
      uid: "llmsafespaces-postgres"   # must match the dashboard's datasource variable
```

The datasource references `${PG_PASSWORD}` — configure the Grafana chart to inject this env var from the credentials Secret.

---

## Prometheus alerting rules

The chart ships 19 alert rules across 4 groups (`llmsafespaces.api`, `llmsafespaces.controller`, `llmsafespaces.agentd`, `llmsafespaces.billing`). Key alerts:

| Alert | Severity | Condition |
|---|---|---|
| API error rate | warning / critical | >5% / >15% |
| API p99 latency | warning | >5s |
| Workspace creation p99 | warning | >120s |
| Consecutive workspace failures | critical | >3 |
| Agentd startup p95 | warning | >60s |
| Inference cost rate | warning | >$10/hour |
| Workspace disk usage | warning | >90% |

```yaml
monitoring:
  prometheusRules:
    enabled: true
    namespace: ""    # defaults to release ns; set to where Prometheus watches
    labels: {}       # e.g. { role: alert-rules } to match your selector
```

### Recommended additional alerts

Set these up based on your SLOs:

- **Postgres connection saturation** — `pg_stat_activity` near `maxOpenConns`.
- **Redis memory** — DEK cache eviction; high memory indicates cache churn.
- **PVC usage per StorageClass** — aggregate across workspaces.
- **NetworkPolicy denials** — if your CNI exports denial metrics (Cilium, Calico), alert on spikes indicating misconfigured egress.

---

## The readyz contract

| Endpoint | Port | Returns | Used for |
|---|---|---|---|
| `/livez` | 8080 (API) | `200` if the process is responsive | Liveness probe |
| `/readyz` | 8080 (API) | `200` only when Postgres **and** Redis are reachable (2s timeout each); `503` otherwise | Readiness probe |
| `/health` | 8080 (API) | Legacy alias for `/livez` | Backward compat |
| `/healthz` | 8081 (controller) | controller-runtime healthz | Liveness probe |
| `/readyz` | 8081 (controller) | controller-runtime readyz | Readiness probe |
| `/v1/readyz` | 4098 (agentd) | Workspace pod health (healthCache snapshot + providerCache) | Controller health polling, Prometheus |

The probe paths are excluded from auth, logging, and metrics middleware so kubelet probes don't generate auth errors or pollute Prometheus cardinality.

A pod that fails readiness is removed from the Service endpoints but **not** restarted. A pod that fails liveness **is** restarted. This is why `readyz` (dependency checks) is readiness, not liveness — a Postgres blip should not trigger a restart loop.

---

## Controller metrics endpoint

By default the controller binds `/metrics` to `127.0.0.1:8080` (loopback). When `monitoring.serviceMonitors.enabled=true`, the chart overrides this to `0.0.0.0:8080` so the ServiceMonitor can scrape through the Kubernetes Service.

!!! security "Unauthenticated metrics exposure"
    This exposes controller metrics (including reconciliation details and workspace phase transitions) to any pod that can reach the controller Service. For production, consider:

    - A `kube-rbac-proxy` sidecar that authenticates incoming scrapes and forwards to `127.0.0.1:8080`.
    - A NetworkPolicy restricting access to the controller metrics port.
    - Override `controller.metricsAddr` back to `127.0.0.0:8080` and run a scraping sidecar.

---

## API metrics authentication

The API `/metrics` endpoint requires `Authorization: Bearer <token>` when `LLMSAFESPACES_METRICS_TOKEN` is set. If you use this env var, configure the ServiceMonitor's bearer token:

```yaml
monitoring:
  enabled: true
  serviceMonitors:
    api:
      bearerTokenSecret:
        name: llmsafespaces-metrics-token
        key: token
```

If `LLMSAFESPACES_METRICS_TOKEN` is not set, the endpoint is unauthenticated and no `bearerTokenSecret` is needed.

---

## Dashboard UID stability

The dashboards are identified by their top-level `uid` field, not by filename or Grafana's internal numeric ID:

- `llmsafespaces-operational`
- `llmsafespaces-billing`

The chart test `TestMonitoring_DashboardUIDsAreStable` pins these values and prevents accidental changes. **Operators bookmark URLs of the form:**

```
https://grafana.example.com/d/llmsafespaces-operational/llmsafespaces-operational
```

If a chart upgrade renames the UID, the bookmark breaks. Worse, the **old dashboard with the old UID stays in Grafana's database** — the sidecar provisioner does not garbage-collect rows whose source files vanished. Operators end up looking at a stale dashboard with stale PromQL queries.

This happened during the `llmsafespace` (singular) → `llmsafespaces` (plural) chart rename (see `CHART-UPGRADE.md`). The fix required SQL-deleting orphan rows and recycling Grafana pods. To avoid recurrence:

- **Never change a dashboard UID** unless there's a deliberate, operator-coordinated migration.
- If you must change a UID, follow the procedure in `helm/CHART-UPGRADE.md` (update the JSON, the chart test pin, the purge script, notify operators, run the purge after upgrade).

---

## Useful PromQL queries

These queries power ad-hoc investigation beyond the shipped dashboards:

```promql
# API error rate (5xx) over 5 minutes
sum(rate(http_requests_total{status=~"5.."}[5m]))
  /
sum(rate(http_requests_total[5m]))

# API p99 latency by path
histogram_quantile(0.99,
  sum by (le, path) (rate(http_request_duration_seconds_bucket[5m])))

# Workspace creation duration p99
histogram_quantile(0.99,
  rate(workspace_creation_duration_seconds_bucket[5m]))

# Reconcile errors by controller
rate(controller_runtime_reconcile_errors_total[5m])

# Memory usage by workspace (cgroup v2)
topk(10, workspace_memory_bytes)

# Active sessions by workspace
topk(10, workspace_active_sessions)

# OOM kills in the last hour
increase(workspace_oom_kills_total[1h])

# Event bus: stalled consumer groups (design 0072 S3)
max by (workspace_id, group) (workspace_agentd_eventbus_group_stalled) == 1

# Event bus: silent publisher — newest entry older than 30m
time() - workspace_agentd_eventbus_last_publish_timestamp_seconds > 1800

# Event bus: publish failures (each fell back to the direct v1 leg)
sum by (workspace_id) (rate(workspace_agentd_eventbus_publishes_total{outcome="failed"}[5m]))

# Event bus: consumer groups falling behind (undelivered backlog)
max by (workspace_id, group) (workspace_agentd_eventbus_group_lag) > 100
```

---

## Log aggregation

Metrics tell you *what* is happening; logs tell you *why*. Ship logs to an aggregator (Loki, Elasticsearch, Datadog).

| Source | What to capture |
|---|---|
| API pods | Auth events, proxy decisions, settings changes, errors. Structured JSON (`logging.encoding: json`). |
| Controller pods | Reconcile decisions, phase transitions, webhook validations. |
| Workspace pods (agentd) | Health probes, credential reload, OOM warnings. |
| Workspace pods (main, opencode) | Agent behavior — high volume; consider sampling. |

!!! info "Secret logging (G25 fixed)"
    The secret `value` field is masked in all logged JSON bodies (added to `SensitiveFields`), and body logging is entirely skipped on credential-bearing paths (`/api/v1/secrets/*`, `/api/v1/account/*`, `/api/v1/auth/*`) via `SkipPathPrefixes`. Two-layer defense — either alone prevents the leak.

### Useful log queries

- `level=error` across all LLMSafeSpaces pods — surface failures.
- `event=sso` — SSO login flow tracing.
- `workspace_id=<id>` — single-workspace investigation across API + controller + pod.
- `msg="cgroup v2 memory.current unreadable"` — workspace scheduled on a cgroup v1 node.

---

## Alert routing

The shipped `PrometheusRule` resources define alerts; routing to receivers (Slack, PagerDuty, email) is configured in Alertmanager, which is **not** shipped by this chart. Example Alertmanager route:

```yaml
route:
  receiver: default
  group_by: ["alertname", "workspace_id"]
  routes:
    - matchers: ["severity=critical"]
      receiver: pagerduty
    - matchers: ["severity=warning"]
      receiver: slack
      group_wait: 5m
```

Critical alerts (workspace failures >3, API error rate >15%) should page on-call. Warnings (latency, disk usage) can route to Slack with a delay to avoid noise.

---

## Related

- [Installation](installation.md) — enabling monitoring at install time.
- [Troubleshooting](troubleshooting.md) — using metrics and `readyz` to diagnose failures.
- [Upgrading](upgrading.md#grafana-dashboard-uids) — the dashboard UID migration procedure.
- [Helm Values Reference](../reference/helm-values.md) — `monitoring.*`.

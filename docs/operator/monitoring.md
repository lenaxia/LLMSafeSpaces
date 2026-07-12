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

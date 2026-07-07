# LLMSafeSpaces Helm chart

Kubernetes-first deployment for the LLMSafeSpaces control plane: API service,
controller, CRDs, ValidatingWebhookConfiguration, and database migrations.

## Status

- Chart version: 0.2.2
- App version: 0.2.2
- Kubernetes: >= 1.27
- Helm: >= 3.13 (also tested with Helm 4)
- Tested locally with `helm lint` and `helm template`

This chart deploys the API, controller, frontend, and MCP Deployments, three
CRDs (Workspace, RuntimeEnvironment, InferenceRelay), validating webhooks
(Workspace, RuntimeEnvironment, optional per-tenant quota), RBAC, a
ConfigMap-driven config, and a pre-install migration Job. The relay-router
Deployment and optional gVisor RuntimeClass are gated by feature flags.

It does **not** deploy Postgres, Redis, or cert-manager. See "Prerequisites"
below.

## ⚠ GitOps deployment (FluxCD / Argo CD) — read before deploying via Git

> If you consume this chart from a Git source (FluxCD `GitRepository` or Argo CD
> Application pointing at this repo), you **must** set `reconcileStrategy:
> Revision` on your Flux `HelmRelease`. Otherwise the chart is packaged exactly
> once and **never re-packaged**, and every `helm upgrade` after the first will
> silently render against a stale snapshot of the chart.

**The trap.** The chart version is bumped per release, but intermediate
commits between releases (sha-/, ts-/, dev-tagged image builds for fast
iteration) do **not** bump `Chart.yaml`. FluxCD's `source-controller`
packages a `GitRepository`-sourced chart and caches the packaged artifact
keyed on the chart version. With the **default** `reconcileStrategy:
ChartVersion`, the artifact is built once on first reconcile and re-used
until the Chart.yaml version changes — so every intermediate commit (new
templates, new `ConfigMap` keys like new SQL migrations bundled via
`(.Files.Glob "migrations/*.sql")`, new RBAC) is invisible to the cluster
until the next release tag. Even at release cadence, the cache makes
upgrades unreliable.

**Symptom.** `kubectl get configmap <release>-migrations -o jsonpath='{.data}'`
shows only the original migration files after you've added new ones; new chart
templates don't appear; hook args never update. The migration Job from issue
[#455](https://github.com/lenaxia/llmsafespaces/issues/455) was masked by this
for 2.5 days because the stale packaged chart kept running the old (already
applied) baseline migration. This caused the 2026-06-29 production incident.

**Fix — set `reconcileStrategy: Revision` so source-controller re-packages on
every git revision:**

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: llmsafespaces
  namespace: flux-system
spec:
  chart:
    spec:
      chart: charts/llmsafespaces
      sourceRef:
        kind: GitRepository
        name: llmsafespaces
        namespace: flux-system
      reconcileStrategy: Revision   # <-- required: intermediate commits don't bump Chart.yaml
  interval: 5m
```

Argo CD users: the equivalent is ensuring your Application tracks `targetRevision`
of a branch/tag (not a chart version) and uses `helm` with `passCredentials` off;
Argo re-renders the chart from the synced Git revision on every sync, so it does
not hit the ChartVersion cache — but verify your `syncPolicy` re-renders rather
than re-applying a cached manifest set.

**Long-term alternative.** Publishing the chart to an OCI registry (ghcr.io)
on each commit would make `reconcileStrategy` irrelevant and is the most robust
fix. That is tracked as a follow-up to [#456](https://github.com/lenaxia/llmsafespaces/issues/456).

## Prerequisites

### Kubernetes

A cluster running Kubernetes 1.27 or later. The webhook configuration uses
`admissionregistration.k8s.io/v1` and `cert-manager.io/v1`.

### cert-manager

If `webhooks.enabled=true` (the default), [cert-manager](https://cert-manager.io)
must be installed in the cluster. The chart uses `cert-manager.io/v1`
`Issuer` and `Certificate` resources, plus the
`cert-manager.io/inject-ca-from` annotation read by `cainjector`.

Install cert-manager:

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
kubectl wait --for=condition=Available -n cert-manager deployment/cert-manager-webhook --timeout=120s
```

If you cannot install cert-manager, set `webhooks.enabled=false`. Admission
validation will only be enforced client-side by the API service. Operators
using `kubectl` directly will bypass validation.

### Postgres

The chart does NOT bundle Postgres. Provide an existing Postgres instance
reachable from the cluster. Configure via `postgresql.host`, `postgresql.port`,
`postgresql.database`, `postgresql.user`, plus the password in
`externalSecret.postgresPassword` (or via an existing Secret pointed at by
`externalSecret.existingSecret`).

The migration Job uses `migrate/migrate:v4.17.1` and expects the database
named in `postgresql.database` to **already exist**. The migrations create
schema objects, not the database itself.

For a quick local Postgres in kind:

```sh
helm install pg oci://registry-1.docker.io/bitnamicharts/postgresql \
    --version 13.4.4 \
    --set auth.username=llmsafespaces \
    --set auth.password=changeme \
    --set auth.database=llmsafespaces \
    -n llmsafespaces --create-namespace
# (Or use any other Postgres chart / raw manifests / cloud DB.)
```

### Redis

Same story — provide an existing Redis. Configure `redis.host`, `redis.port`,
and optionally `externalSecret.redisPassword`.

## Quick install

```sh
# 1. Install cert-manager (see above)

# 2. Install LLMSafeSpaces
helm install llmsafespaces ./charts/llmsafespaces \
    -n llmsafespaces --create-namespace \
    --set postgresql.host=pg-postgresql \
    --set redis.host=redis-master \
    --set externalSecret.postgresPassword=changeme

# 3. Watch the migration Job complete
kubectl -n llmsafespaces get jobs -w

# 4. Wait for both Deployments
kubectl -n llmsafespaces rollout status deployment llmsafespaces-api
kubectl -n llmsafespaces rollout status deployment llmsafespaces-controller

# 5. Smoke-test the API
kubectl -n llmsafespaces port-forward svc/llmsafespaces-api 8080:8080 &
curl http://localhost:8080/livez   # 200 OK
curl http://localhost:8080/readyz  # 200 if DB+Redis healthy, 503 otherwise
```

> **Image tags:** The chart defaults each image tag to `.Chart.AppVersion`
> (`0.2.2`), which resolves to the published GHCR image for the latest release
> tag (`v0.2.2`). For intermediate (non-release) builds, supply a tag
> explicitly via `--set api.image.tag=<tag>` (and the same for
> `controller`, `frontend`, `runtimeEnvironments.base`). See [Image tags](#image-tags) below.

## Image tags

CI (`ci.yml`) publishes four tag types on every main-branch build:

| Tag | Format | Purpose |
|-----|--------|---------|
| `sha-<commit>` | `sha-ac861c3` | Immutable, content-addressable. **Preferred for pinned deployments.** |
| `ts-<unix>` | `ts-1782762331` | Sortable by time. Useful for chronological queries. |
| `dev` | `dev` | Moving pointer to the latest main build. **Avoid in production** — kubelet caching of moving tags is unreliable. |
| semver | `0.2.2` | Emitted on `v*.*.*` git tags (`type=semver,pattern={{version}}`). The chart default targets this. |

**For production / pinned deployments:**

```sh
helm install llmsafespaces ./charts/llmsafespaces \
    --set api.image.tag=sha-ac861c3 \
    --set controller.image.tag=sha-ac861c3 \
    --set frontend.image.tag=sha-ac861c3 \
    --set runtimeEnvironments.base.image.tag=sha-ac861c3 \
    # ... other values
```

**For fast intermediate deploys** (no release needed):

```sh
helm upgrade llmsafespaces ./charts/llmsafespaces \
    --set api.image.tag=dev \
    --set controller.image.tag=dev \
    # ... or pin to a specific ts-/sha- build
```

**Cutting a release** makes the chart default (`appVersion`) resolve without
overrides:

```sh
git tag v0.2.2
git push origin v0.2.2   # CI publishes 0.2.2 + latest to GHCR
```

After the release, `helm install` with no tag override resolves every image to
`ghcr.io/lenaxia/llmsafespaces/{api,...}:0.2.2`.

> **GHCR retention:** GitHub's native package-version retention prunes old
> versions. `sha-` and `ts-` tags label the same manifest version and are pruned
> together — pinning to `sha-` does not prevent recurrence on its own. Configure
> retention to keep the latest semver-tagged version, or cut releases
> frequently enough that the current `appVersion` is always within the retention
> window. See issue #454.

## Values reference

The chart exposes ~150 documented values. Highlights:

| Key | Default | Purpose |
|-----|---------|---------|
| `api.replicaCount` | `2` | Number of API pods |
| `api.image.repository` | `llmsafespaces/api` | API container image |
| `api.config.rateLimiting.enabled` | `true` | API per-user rate limiting |
| `controller.replicaCount` | `1` | Number of controller pods |
| `controller.watchNamespaces` | `""` | Comma-separated list of namespaces to watch (empty = all) |
| `controller.leaderElection.enabled` | `true` | Use leader election for HA controller |
| `crds.install` | `true` | Install CRDs from `crds/` |
| `rbac.create` | `true` | Create (Cluster)Role and (Cluster)RoleBinding |
| `rbac.scope` | `"namespace"` | `"cluster"` or `"namespace"` (defense-in-depth) |
| `webhooks.enabled` | `true` | Deploy ValidatingWebhookConfiguration (requires cert-manager) |
| `webhooks.failurePolicy` | `"Fail"` | Admission failure policy |
| `migrations.enabled` | `true` | Run migrations as pre-install/upgrade Helm hook |
| `externalSecret.create` | `true` | Create the credentials Secret from chart values |
| `externalSecret.existingSecret` | `""` | Reference an existing Secret instead |
| `postgresql.host` / `port` / `database` / `user` / `sslMode` | — | Postgres connection |
| `redis.host` / `port` / `db` | — | Redis connection |

See [`values.yaml`](./values.yaml) for the full list with comments.

## Operational concerns

### Probes

- API `/livez` returns 200 if the process is responding (used for liveness)
- API `/readyz` returns 200 only when both Postgres and Redis are reachable
  (used for readiness; pings have a 2s timeout)
- API `/health` is preserved as a legacy alias for `/livez`
- Controller exposes controller-runtime's `/healthz` and `/readyz`

The probe paths are excluded from auth, logging, and metrics middleware so
kubelet probes don't generate auth errors or pollute Prometheus cardinality.

### CRD upgrades

Helm 3 installs CRDs from `crds/` on first install but **does not upgrade**
them on `helm upgrade`. To pick up CRD changes:

```sh
kubectl apply -f charts/llmsafespaces/crds/
helm upgrade llmsafespaces ./charts/llmsafespaces -n llmsafespaces
```

For production safety, set `crds.install=false` and manage CRDs out-of-band.

### Webhook failure mode

`webhooks.failurePolicy` defaults to `Fail`: if the controller webhook is
unavailable, kube-apiserver rejects all CREATE/UPDATE on RuntimeEnvironment.
This is the secure default but means controller downtime blocks runtime
submissions. Set to `Ignore` for availability over security, or `Fail` for
security over availability.

### RBAC scope

`rbac.scope=namespace` (default) gives the controller only namespace-scoped Role on
the release namespace. Combine with `controller.watchNamespaces=<release-ns>`
for tightest isolation. Resources in other namespaces will not be reconciled.

`rbac.scope=cluster` gives the controller cluster-wide permissions.
This is required when `controller.watchNamespaces` is empty (cluster-wide
mode).

### Workspace namespace

By default, workspace CRDs are created in `.Release.Namespace`. Override with
`api.config.kubernetes.namespace` to deploy workspaces elsewhere. RBAC for
the API ServiceAccount is created in that namespace.

## Validating the chart

```sh
make helm-lint                  # syntax check
make helm-template               # render with defaults
make helm-template-debug         # render with full debug output
make helm-install-dry-run        # validate against live cluster
make helm-package                # produce dist/llmsafespaces-0.2.2.tgz
```

## Uninstalling

```sh
helm uninstall llmsafespaces -n llmsafespaces
kubectl -n llmsafespaces delete pvc --all   # PVCs are not deleted by Helm
kubectl delete crd workspaces.llmsafespaces.dev runtimeenvironments.llmsafespaces.dev inferencerelays.llmsafespaces.dev
```

CRDs are intentionally not deleted by `helm uninstall` (Helm 3 default
behavior) to avoid accidental data loss. Delete them manually if intended.

## Limitations

- No Kyverno policy templates yet (deferred per EVOLUTION-V2.md §9.6)
- No bundled Postgres or Redis sub-charts
- Migrations run with the official `migrate/migrate` image; the database
  must exist before the chart is installed (`CREATE DATABASE` is not run)
- The API service does not yet support TLS at the API level (use an Ingress
  with TLS termination)

## Monitoring

The chart optionally deploys Grafana dashboards, Prometheus alerting rules,
and ServiceMonitor resources. All are gated by `monitoring.enabled` (off by
default) with independent sub-toggles.

### Prerequisites

- **Grafana** with the [sidecar dashboard
  importer](https://github.com/grafana/helm-charts/tree/main/charts/grafana#sidecar-dashboard-provider)
  (the dashboard ConfigMap uses the `grafana_dashboard: "1"` label)
- **Prometheus Operator** (for `PrometheusRule` and `ServiceMonitor` CRDs)

### Enabling

```sh
helm install llmsafespaces ./charts/llmsafespaces \
    --set monitoring.enabled=true \
    -n llmsafespaces
```

### What is deployed

| Resource | Count | Toggle |
|----------|-------|--------|
| Grafana dashboard ConfigMap | 1 (2 dashboards) | `monitoring.dashboards.enabled` |
| PrometheusRule | 1 (19 alert rules) | `monitoring.prometheusRules.enabled` |
| ServiceMonitor | 2 (API + controller) | `monitoring.serviceMonitors.enabled` |

### Dashboards

- **LLMSafeSpaces - Operational**: request overview, connections, workspace
  lifecycle, reconciliation, recovery, agent operations, SSE/relay, billing
  at a glance
- **LLMSafeSpaces - Billing & Metering**: inference cost/token breakdown,
  per-user metering (active seconds, CPU, LLM calls), per-workspace resource
  consumption (storage, memory, CPU, proxy bytes)

### Alerting rules

19 alert rules across 4 groups (`llmsafespaces.api`,
`llmsafespaces.controller`, `llmsafespaces.agentd`, `llmsafespaces.billing`).
Key alerts:

- API error rate >5% (warning) / >15% (critical)
- API p99 latency >5s
- Workspace creation >120s at p99
- Consecutive workspace failures >3 (critical)
- Agentd startup >60s at p95
- Inference cost rate >$10/hour
- Workspace disk usage >90%

### Controller metrics endpoint

When `monitoring.serviceMonitors.enabled=true`, the controller deployment
automatically overrides `controller.metricsAddr` to `0.0.0.0:8080` so the
ServiceMonitor can reach the metrics endpoint through the Kubernetes
Service. Without this, the default loopback binding (`127.0.0.1:8080`)
rejects connections from other pods.

**Security note:** This exposes controller metrics (including reconciliation
details and workspace phase transitions) to any pod that can reach the
controller Service. For production, consider deploying a `kube-rbac-proxy`
sidecar to authenticate scrapes, or use NetworkPolicy to restrict access to
the controller metrics port.

### API metrics authentication

The API `/metrics` endpoint requires `Authorization: Bearer <token>` when
the `LLMSAFESPACES_METRICS_TOKEN` env var is set. If you use this env var,
configure the ServiceMonitor's bearer token:

```yaml
monitoring:
  enabled: true
  serviceMonitors:
    api:
      bearerTokenSecret:
        name: llmsafespaces-metrics-token
        key: token
```

If `LLMSAFESPACES_METRICS_TOKEN` is not set, the endpoint is unauthenticated
and no `bearerTokenSecret` configuration is needed.

### Configuration reference

| Key | Default | Purpose |
|-----|---------|---------|
| `monitoring.enabled` | `false` | Master toggle for all monitoring resources |
| `monitoring.dashboards.enabled` | `true` | Deploy Grafana dashboard ConfigMap |
| `monitoring.dashboards.namespace` | `""` | Override namespace (defaults to release namespace) |
| `monitoring.dashboards.labels` | `{grafana_dashboard: "1"}` | Labels for dashboard ConfigMap |
| `monitoring.prometheusRules.enabled` | `true` | Deploy PrometheusRule alerts |
| `monitoring.prometheusRules.namespace` | `""` | Override namespace |
| `monitoring.prometheusRules.labels` | `{}` | Additional labels (e.g. `role: alert-rules`) |
| `monitoring.serviceMonitors.enabled` | `true` | Deploy ServiceMonitor for API + controller |
| `monitoring.serviceMonitors.namespace` | `""` | Override namespace |
| `monitoring.serviceMonitors.interval` | `30s` | Scrape interval |
| `monitoring.serviceMonitors.scrapeTimeout` | `10s` | Scrape timeout |
| `monitoring.serviceMonitors.api.bearerTokenSecret` | `{}` | Optional bearer token for API metrics auth |

# Helm Values Reference

This is a reference for the most commonly-configured values in `helm/values.yaml`. Grouped by section. For each value: type, default, what it does, and when to change it. For values not documented here (some `*.image.*` sub-fields, `*.resources`, `migrations.*`, `dbInit.*`, `monitoring.*` sub-fields), consult `helm/values.yaml` directly — comments there are extensive.

For installation steps, see the [operator installation guide](../operator/installation.md). For the security-relevant values, cross-reference the [threat model](../architecture/threat-model.md).

## Top-level

| Key | Type | Default | Description |
|---|---|---|---|
| `nameOverride` | string | `""` | Override the chart name (standard Helm). |
| `fullnameOverride` | string | `""` | Override the full resource name prefix. |

## `namespace`

Controls the namespace the platform's own components run in. Workspaces are created in the same namespace by default.

| Key | Type | Default | Description |
|---|---|---|---|
| `namespace.create` | bool | `false` | If `true`, the chart creates the release namespace. Set `false` if installing with `--namespace --create-namespace`. |
| `namespace.podSecurityEnforce` | string | `"restricted"` | Pod Security Admission label applied at enforce/audit/warn levels. Empty string opts out (G11). All chart pods comply with `restricted`. |
| `namespace.podSecurityVersion` | string | `"latest"` | PSA policy version. |

## `api`

The Gin API service. Stateless, horizontally scalable.

| Key | Type | Default | Description |
|---|---|---|---|
| `api.enabled` | bool | `true` | Deploy the API service. |
| `api.replicaCount` | int | `2` | Number of API replicas. Stateless — no sticky sessions required. Bump to scale. |
| `api.image.repository` | string | `ghcr.io/lenaxia/llmsafespaces/api` | API image repository. |
| `api.image.tag` | string | `""` | Image tag. Falls back to `Chart.AppVersion`. **Pin to immutable `sha-<7char>` or `ts-<unix>` tags in production** — moving tags (`dev`, `latest`) interact poorly with kubelet image caching. |
| `api.image.pullPolicy` | string | `IfNotPresent` | Standard Kubernetes pull policy. |
| `api.imagePullSecrets` | list | `[]` | Private registry credentials. |
| `api.service.type` | string | `ClusterIP` | Service type. |
| `api.service.port` | int | `8080` | Service port. |

### `api.resources`

| Key | Default | Description |
|---|---|---|
| `api.resources.requests.cpu` | `100m` | |
| `api.resources.requests.memory` | `128Mi` | |
| `api.resources.limits.cpu` | `1000m` | |
| `api.resources.limits.memory` | `1Gi` | |

### `api.podSecurityContext` / `api.containerSecurityContext`

Both default to restricted-profile compliance: `runAsNonRoot: true`, `runAsUser/Group: 65532`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`. Override only if you have a specific reason.

### `api.config`

Rendered into a ConfigMap. Sensitive fields (passwords, JWT secret) come from the `externalSecret` block or an existing secret. See `api/config/config.yaml` for the full schema.

| Key | Type | Default | Description |
|---|---|---|---|
| `api.config.server.host` | string | `"0.0.0.0"` | Bind address. |
| `api.config.server.port` | int | `8080` | Listen port. |
| `api.config.server.shutdownTimeout` | duration | `30s` | Graceful shutdown deadline. |
| `api.config.kubernetes.configPath` | string | `""` | Kubeconfig path (dev only; in-cluster uses the SA token). |
| `api.config.kubernetes.inCluster` | bool | `true` | Use the in-cluster config. |
| `api.config.kubernetes.namespace` | string | `""` | Namespace for workspaces. Defaults to the release namespace. |
| `api.config.kubernetes.leaderElection.enabled` | bool | `true` | API leader election. |
| `api.config.kubernetes.leaderElection.leaseDuration` | duration | `15s` | |
| `api.config.kubernetes.leaderElection.renewDeadline` | duration | `10s` | |
| `api.config.kubernetes.leaderElection.retryPeriod` | duration | `2s` | |
| `api.config.logging.level` | string | `"info"` | Log level (`debug`, `info`, `warn`, `error`). |
| `api.config.logging.development` | bool | `false` | Zap dev mode (console encoder, stacktraces). |
| `api.config.logging.encoding` | string | `"json"` | `json` or `console`. |

#### `api.config.auth`

| Key | Type | Default | Description |
|---|---|---|---|
| `api.config.auth.tokenDuration` | duration | `24h` | JWT lifetime. |
| `api.config.auth.apiKeyPrefix` | string | `"lsp_"` | API key prefix. |
| `api.config.auth.jwtIssuer` | string | `""` | `iss` claim. Default `"llmsafespaces"` applied at boot if empty. **Set when deploying multiple instances** that should not accept each other's tokens. |
| `api.config.auth.jwtAudience` | string | `""` | `aud` claim. Same default behavior as `jwtIssuer`. |

!!! note "JWT iss/aud (v0.3.0)"
    JWTs now carry explicit `iss` and `aud` claims, minted from these values and validated on every parse. Pre-fix tokens carried only `sub/jti/exp/iat`, so any service sharing the same HMAC secret could mint accepted tokens. Pre-fix tokens are rejected after upgrade; rotation is fast (24h default lifetime).

#### `api.config.rateLimiting`

| Key | Type | Default | Description |
|---|---|---|---|
| `api.config.rateLimiting.enabled` | bool | `true` | Enable Redis-backed rate limiting. |
| `api.config.rateLimiting.limits.default` | object | `{requests: 1000, window: 1h}` | Global default. |
| `api.config.rateLimiting.limits.create_workspace` | object | `{requests: 100, window: 1h}` | Per-user workspace creation. |
| `api.config.rateLimiting.limits.execute_code` | object | `{requests: 500, window: 1h}` | Code execution endpoint. |

#### `api.config.security`

| Key | Type | Default | Description |
|---|---|---|---|
| `api.config.security.allowedOrigins` | list | `["https://safespace.thekao.cloud"]` | CORS allow-list. **Replace with your own origin in production.** |
| `api.config.security.allowCredentials` | bool | `false` | Set `Access-Control-Allow-Credentials: true`. **Cannot be `true` with `allowedOrigins=["*"]`** — the API refuses to start in that state (CORS spec violation, fail-closed guard). |

### `api.livenessProbe` / `api.readinessProbe`

`/livez` always returns 200 if the process is responsive. `/readyz` pings Postgres and Redis; returns 503 if either is down. `/health` is a legacy alias for `/livez`. Defaults: liveness initial 10s/period 10s; readiness initial 5s/period 5s.

### `api.ingress`

| Key | Type | Default | Description |
|---|---|---|---|
| `api.ingress.enabled` | bool | `false` | Create an Ingress for the API. Most deployments front the API via the frontend ingress instead. |
| `api.ingress.className` | string | `""` | IngressClass. |
| `api.ingress.annotations` | object | `{}` | Ingress annotations. |
| `api.ingress.hosts` | list | see values | Host/path rules. |
| `api.ingress.tls` | list | `[]` | TLS configuration. |

## `controller`

The controller-runtime operator. Single leader-elected replica by default.

| Key | Type | Default | Description |
|---|---|---|---|
| `controller.enabled` | bool | `true` | Deploy the controller. |
| `controller.replicaCount` | int | `1` | Replicas. Leader election means only one is active; extras are standby. |
| `controller.image.repository` | string | `ghcr.io/lenaxia/llmsafespaces/controller` | |
| `controller.image.tag` | string | `""` | Same pinning recommendation as `api.image.tag`. |
| `controller.watchNamespaces` | string | `""` | Comma-separated namespaces to watch. `""` or `"*"` = cluster-wide. Combine with namespace-scoped RBAC for defense-in-depth. |
| `controller.leaderElection.enabled` | bool | `true` | |
| `controller.apiServiceURL` | string | `""` | In-cluster API URL the controller polls (30s, cached) for org-level suspension (D20). Empty derives from release name + namespace. |
| `controller.metricsAddr` | string | `"127.0.0.1:8080"` | Metrics bind address. Loopback by default (F1.4.3) — run a `kube-rbac-proxy` sidecar for Prometheus. Override to `0.0.0.0:8080` only if you accept the unauthenticated-metrics trade-off. |
| `controller.probeAddr` | string | `":8081"` | Health probe port. |
| `controller.webhookPort` | int | `9443` | Webhook server port. |

### `controller.inferenceRelay` (opt-in, Epic 42)

The self-hosted multi-cloud relay fleet. Disabled by default. **Requires `rbac.scope=cluster`** (InferenceRelay is cluster-scoped).

| Key | Type | Default | Description |
|---|---|---|---|
| `controller.inferenceRelay.enabled` | bool | `false` | Feature gate. |
| `controller.inferenceRelay.routerURL` | string | `"http://relay-router:8080"` | Router `/metrics` scrape URL (controller → router, in-cluster). |
| `controller.inferenceRelay.workspaceRouterURL` | string | `""` | URL workspace pods use to reach the router. Empty → derived cross-namespace FQDN. |
| `controller.inferenceRelay.artifact.urls` | list | `[github.com/.../v0.1.0-relay]` | Mirror URLs for the relay-proxy binary (cloud-init downloads + verifies). At least one required. |
| `controller.inferenceRelay.artifact.sha256Arm64` | string | `671c46...` | Hex SHA-256 of the arm64 binary. **Required** when enabled. |
| `controller.inferenceRelay.artifact.sha256Amd64` | string | `ac12e2...` | Hex SHA-256 of the amd64 binary. **Required** when enabled. |
| `controller.inferenceRelay.upstreamURL` | string | `"https://opencode.ai/zen/v1"` | Upstream the fleet proxies to. Default uses the anonymous `public` key for free Zen models. |
| `controller.inferenceRelay.upstreamAuth.keySecret.name` | string | `""` | Optional K8s Secret with a real upstream key (paid gateway). Empty = forward `Bearer public` unchanged. |
| `controller.inferenceRelay.upstreamAuth.keySecret.key` | string | `"key"` | Key within the Secret. |
| `controller.inferenceRelay.upstreamAuth.header` | string | `""` | Header name to set. Empty = `Authorization`. Use `x-api-key` for Anthropic-native upstreams. |

The `router` sub-block configures the in-cluster relay-router Deployment (image, service, resources, scheduling).

### `controller.freeModelsRefresher`

| Key | Type | Default | Description |
|---|---|---|---|
| `controller.freeModelsRefresher.enabled` | bool | `true` | Periodically fetch the opencode free-tier model catalog from models.dev and publish as a ConfigMap. Eliminates the in-pod relay-injector restart cycle (~6-8s saved per cold start and resume). Disable to fall back to the legacy in-pod injector. |
| `controller.freeModelsRefresher.refreshInterval` | duration | `6h` | Refetch cadence. Catalog changes ~weekly. |
| `controller.freeModelsRefresher.apiURL` | string | `""` | Override catalog URL. Empty defaults to `https://models.dev/api.json`. Useful for air-gapped clusters. |

### `controller.resources`

Defaults: requests `100m` / `256Mi`, limits `500m` / `512Mi`.

## `workspace`

Platform requirements for workspace pods.

| Key | Type | Default | Description |
|---|---|---|---|
| `workspace.cgroupV2Required` | bool | `true` | cgroup v2 is a **hard requirement** for memory-pressure warnings, `workspace_memory_bytes`, and OOM attribution. On cgroup v1 these features silently produce nothing (agentd logs a single Warn per boot). |
| `workspace.defaultStorageClass` | string | `""` | When non-empty, pins the `workspace.defaultStorageClass` instance setting at API boot (read-only in admin UI; PUTs return 409). When empty, the setting stays admin-mutable. |

## `terminal`

| Key | Type | Default | Description |
|---|---|---|---|
| `terminal.allowedOrigins` | list | `[]` | Additional origins accepted by the WebSocket terminal proxy. Default is same-origin only (G39 fix). Non-browser clients (no Origin) authenticate via the single-use ticket. `"*"` disables Origin checking entirely — use only if you understand the cross-site WebSocket hijacking risk. |

## `crds`

| Key | Type | Default | Description |
|---|---|---|---|
| `crds.install` | bool | `true` | Install CRDs from the chart's `crds/` directory. **Set `false` and manage CRDs out-of-band in production** — Helm 3 does not upgrade CRDs in `crds/`. |

## `externalSecret`

The chart creates a Secret with DB/Redis/JWT/master credentials. All auto-generated as 32-char random strings on first install (G26 fix) when left empty.

| Key | Type | Default | Description |
|---|---|---|---|
| `externalSecret.create` | bool | `true` | Create the Secret. `false` = use `existingSecret`. |
| `externalSecret.existingSecret` | string | `""` | Existing Secret name (keys: `postgres-password`, `redis-password`, `jwt-secret`, `master-secret`). |
| `externalSecret.postgresPassword` | string | `""` | Postgres password. Empty = auto-generate. **Pinning a value rotates it on upgrade;** the Secret is `helm.sh/resource-policy: keep` so the random value is stable across upgrades. |
| `externalSecret.redisPassword` | string | `""` | Redis password. Empty = auto-generate. |
| `externalSecret.jwtSecret` | string | `""` | JWT HMAC signing key. Empty = auto-generate. |
| `externalSecret.masterSecret` | string | `""` | Root KEK. Auto-generated as 64-char random. **Rotating invalidates all admin-encrypted credentials.** |

!!! warning "Rotation out of vulnerable state"
    If a previous chart version left `postgres-password="changeme"` or `redis-password=""`, the next `helm upgrade` re-randomizes both. You **must** then run the `ALTER USER` procedure in `NOTES.txt` to sync the Postgres role.

## `masterSecret` (KEK delivery — US-50.1)

How the master KEK reaches the API container. Distinct from `externalSecret.masterSecret` (which is the *value*).

| Key | Type | Default | Description |
|---|---|---|---|
| `masterSecret.deliveryMethod` | string | `"file"` | `file` (default) projects the KEK as a read-only file mount at `fileMountPath` (mode 0440). `env` is the legacy opt-in (delivers as `LLMSAFESPACES_MASTER_SECRET` env var) — retained for non-Helm deploys, logs a deprecation warning. |
| `masterSecret.fileMountPath` | string | `/var/run/secrets/llmsafespaces/master-secret` | File mount path. **Must NOT be inside `/etc/llmsafespaces`** (the config volume mountPath) — nested subPath mounts fail on some kernel/runtime combinations. |

## `kms` (Cloud KMS — Epic 57 US-57.1)

Opt-in. When enabled, the master KEK is backed by cloud KMS instead of the local static/sealed file. The key never leaves the cloud service.

| Key | Type | Default | Description |
|---|---|---|---|
| `kms.aws.enabled` | bool | `false` | Enable AWS KMS provider. |
| `kms.aws.region` | string | `""` | AWS region. |
| `kms.aws.credentialsSecret` | string | `""` | Existing K8s Secret with a credentials-file key at `credentials` (AWS shared-credentials INI format). File-mounted, not IRSA — narrower trust surface per US-50.1's pattern. |
| `kms.aws.keyArns.providerCredentials` | string | `""` | KMS key ARN for admin/org LLM API keys. |
| `kms.aws.keyArns.orgCredentials` | string | `""` | KMS key ARN for org-level credentials. |
| `kms.aws.keyArns.masterKek` | string | `""` | KMS key ARN for API keys + org SSO client secrets. |

Three keys required (D4 per-purpose domain separation). The master-secret file mount is **retained under KMS** to protect the Redis DEK cache.

## `datastore` (NetworkPolicies)

| Key | Type | Default | Description |
|---|---|---|---|
| `datastore.networkPolicy.enabled` | bool | `true` | Block ingress to Postgres/Valkey except from the API and migration Job. Workspace pods cannot reach datastores. Gated by the master `networkPolicy.enabled`. |
| `datastore.networkPolicy.postgresPodSelectorLabels` | object | `{app: postgres}` | Pod selector for the postgres pods. Override for Bitnami (`{app.kubernetes.io/name: postgresql}`). |
| `datastore.networkPolicy.valkeyPodSelectorLabels` | object | `{app: valkey}` | Pod selector for the valkey pods. |

## `postgresql`

Connection details consumed by the API config. The chart does **not** bundle Postgres.

| Key | Type | Default | Description |
|---|---|---|---|
| `postgresql.host` | string | `postgres` | |
| `postgresql.port` | int | `5432` | |
| `postgresql.user` | string | `llmsafespaces` | |
| `postgresql.database` | string | `llmsafespaces` | |
| `postgresql.sslMode` | string | `disable` | `disable`/`require`/`verify-ca`/`verify-full`. **Use `verify-full` in production.** |
| `postgresql.maxOpenConns` | int | `25` | |
| `postgresql.maxIdleConns` | int | `10` | |
| `postgresql.connMaxLifetime` | duration | `5m` | |

## `redis`

The chart does **not** bundle Redis.

| Key | Type | Default | Description |
|---|---|---|---|
| `redis.host` | string | `redis-master` | |
| `redis.port` | int | `6379` | |
| `redis.db` | int | `0` | |
| `redis.poolSize` | int | `20` | |

!!! warning "H3 production requirement"
    The API caches per-workspace `opencode` passwords in Redis in **plaintext**. Production Redis **must** provide TLS in-transit, at-rest encryption, and a NetworkPolicy restricting ingress to API pods.

## `migrations`

| Key | Type | Default | Description |
|---|---|---|---|
| `migrations.enabled` | bool | `true` | Run migrations as a Helm pre-install/pre-upgrade Job (`migrate/migrate` image). |
| `migrations.image.repository` | string | `""` | Empty falls back to `migrate/migrate:v4.17.1`. |
| `migrations.image.tag` | string | `""` | |
| `migrations.backoffLimit` | int | `3` | Job backoff limit. |
| `migrations.ttlSecondsAfterFinished` | int | `600` | TTL for completed Job. |

## `dbInit` (optional role/database bootstrap)

Disabled by default. Enable on green-field Postgres where the role+DB don't exist yet.

| Key | Type | Default | Description |
|---|---|---|---|
| `dbInit.enabled` | bool | `false` | Render a pre-install Job that runs idempotent `CREATE ROLE`/`CREATE DATABASE` as a superuser. |
| `dbInit.image.repository` / `tag` | string | `postgres` / `16-alpine` | Image with the `psql` client. |
| `dbInit.superuserSecret.name` | string | `""` | **Required** when enabled. Secret holding Postgres superuser credentials. |
| `dbInit.superuserSecret.userKey` | string | `username` | |
| `dbInit.superuserSecret.passwordKey` | string | `password` | |

## `networkPolicy` (Epic 17 G16)

| Key | Type | Default | Description |
|---|---|---|---|
| `networkPolicy.enabled` | bool | `true` | Master toggle. Ships `workspace-default-deny-ingress` and `workspace-egress`. **Threat model A2 requires SOME default-deny** — if you disable this, document the equivalent control. |
| `networkPolicy.workspaceEgress.enabled` | bool | `true` | Workspace egress NetworkPolicy. Disable **only** when a CNI-native policy (Cilium CNP, Calico GNP) enforces a strict FQDN allowlist — otherwise pods have no egress restriction. The K8s NP allows `0.0.0.0/0` which would union-defeat a Cilium CNP. |
| `networkPolicy.apiPodLabelSelector` | object | `{app.kubernetes.io/name: llmsafespaces, app.kubernetes.io/component: api}` | Selector for the API pods allowed to reach workspaces on agentd port 4097. |
| `networkPolicy.controllerPodLabelSelector` | object | `{app.kubernetes.io/name: llmsafespaces, app.kubernetes.io/component: controller}` | Selector for controller pods (health polling on admin port 4098). |
| `networkPolicy.prometheusPodLabelSelector` | object | `{app.kubernetes.io/name: prometheus}` | Selector for Prometheus (scraping :4098/metrics). Only effective when `monitoring.serviceMonitors.agentdPodMonitor.enabled` is true. |
| `networkPolicy.prometheusNamespace` | string | `""` | Namespace where Prometheus runs. Defaults to release namespace; typically override to e.g. `monitoring`. |
| `networkPolicy.apiIngressRestricted` | bool | `false` | Opt-in default-deny ingress for the API pod. Admits only controller, kube-system (kubelet probes), and `apiIngressSourcePodSelector`. Default `false` because the user-traffic source is deployment-specific. |
| `networkPolicy.apiIngressSourcePodSelector` | object | `{app.kubernetes.io/name: llmsafespaces, app.kubernetes.io/component: frontend}` | Pod selector allowed to send user traffic to the API when `apiIngressRestricted=true`. Override to your ingress controller. |
| `networkPolicy.dnsNamespace` | string | `kube-system` | Namespace for the DNS service. |
| `networkPolicy.dnsPodLabelSelector` | object | `{k8s-app: kube-dns}` | DNS pod selector. |
| `networkPolicy.allowedEgressCIDRs` | list | `[0.0.0.0/0]` | CIDR allowlist for sandbox egress. Default allows all public internet. |
| `networkPolicy.blockedEgressCIDRs` | list | see below | Block egress to RFC1918 + CGNAT + cloud-metadata. **Keep in sync with `controller/internal/workspace/network_policy.go`** — the chart-test `TestG16_DefaultRender_BlockedEgressIncludesAllControllerSideCIDRs` pins parity. |

Default `blockedEgressCIDRs`:

```yaml
blockedEgressCIDRs:
  - 10.0.0.0/8        # RFC1918
  - 172.16.0.0/12     # RFC1918
  - 192.168.0.0/16    # RFC1918
  - 169.254.0.0/16    # link-local + cloud metadata (169.254.169.254)
  - 100.64.0.0/10     # CGNAT — managed K8s pod CIDRs (AKS, some EKS, k3s)
  - 127.0.0.0/8       # loopback
  - 224.0.0.0/4       # multicast
```

## `kyverno`

| Key | Type | Default | Description |
|---|---|---|---|
| `kyverno.enabled` | bool | `false` | Deploy Kyverno admission policies (deferred). Requires Kyverno installed in the cluster. |

## `serviceAccount`

| Key | Type | Default | Description |
|---|---|---|---|
| `serviceAccount.api.create` | bool | `true` | Create the API SA. |
| `serviceAccount.api.annotations` | object | `{}` | E.g. `eks.amazonaws.com/role-arn` for SES IRSA. |
| `serviceAccount.api.name` | string | `""` | Override SA name. |
| `serviceAccount.controller.create` | bool | `true` | |
| `serviceAccount.controller.annotations` | object | `{}` | |
| `serviceAccount.controller.name` | string | `""` | |

## `rbac`

| Key | Type | Default | Description |
|---|---|---|---|
| `rbac.create` | bool | `true` | Create (Cluster)Role + (Cluster)RoleBinding. |
| `rbac.scope` | string | `"namespace"` | **`namespace`** (default, G5): Role + RoleBinding scoped to the release namespace. **`cluster`**: adds ClusterRole + ClusterRoleBinding for cluster-wide CRD watch. Pods/Secrets/PVCs/NetworkPolicies remain namespace-scoped regardless. **Required `cluster`** for `controller.inferenceRelay.enabled`. Even in cluster mode, no mutating verbs on secrets/pods (chart_test.go:1411). |

## `webhooks`

Validating webhooks for Workspace and RuntimeEnvironment. **Requires cert-manager.**

| Key | Type | Default | Description |
|---|---|---|---|
| `webhooks.enabled` | bool | `true` | Deploy webhook configuration. Set `false` if you lack cert-manager; validation still happens server-side in the API. |
| `webhooks.issuerKind` | string | `"Issuer"` | `Issuer` (namespace-scoped) or `ClusterIssuer`. |
| `webhooks.existingIssuer` | string | `""` | Use an existing Issuer. Empty = create self-signed. |
| `webhooks.failurePolicy` | string | `"Fail"` | `Fail` (recommended) or `Ignore`. |
| `webhooks.timeoutSeconds` | int | `10` | Max 30s. |
| `webhooks.allowedImageRegistries` | list | `["ghcr.io/lenaxia/"]` | Registry prefixes a Workspace runtime image reference must match. Empty = only RuntimeEnvironment-name references allowed. |
| `webhooks.allowedStorageClassNames` | list | `[]` | If non-empty, `spec.storage.storageClassName` must be one of these. Empty = any. |
| `webhooks.maxWorkspaceStorageGi` | int | `1024` | Max workspace storage in GiB. 0 disables. |
| `webhooks.maxWorkspaceCPUMillicores` | int | `16000` | Max CPU per workspace. 0 disables. |
| `webhooks.maxWorkspaceMemoryMi` | int | `65536` | Max memory per workspace. 0 disables. |
| `webhooks.tenantQuota.maxWorkspacesPerTenant` | int | `0` | Max concurrent workspace pods per tenant (Epic 51 S51.2). 0 = disabled. Recommended: 10-20. |
| `webhooks.tenantQuota.maxCPUMillisPerTenant` | int | `0` | Max aggregate CPU per tenant. Recommended: 8000. |
| `webhooks.tenantQuota.maxMemoryMiPerTenant` | int | `0` | Max aggregate memory per tenant. Recommended: 16384. |

## `gvisor` (Epic 51)

| Key | Type | Default | Description |
|---|---|---|---|
| `gvisor.enabled` | bool | `false` | Create a `gvisor` RuntimeClass (handler: `runsc`) and set `--default-runtime-class=gvisor` on the controller. **Prerequisites:** runsc installed on all workspace nodes, container runtime configured with the runsc handler. |
| `gvisor.defaultRuntimeClass` | string | `"gvisor"` | RuntimeClass name. Change if you have a custom one. |

Individual workspaces opt out via `spec.runtimeClass: "runc"` — admin-gated by the validating webhook (requires annotation `llmsafespaces.dev/allow-runtime-class-override: "true"`).

## `runtimeEnvironments`

Seeded `RuntimeEnvironment` CRDs.

| Key | Type | Default | Description |
|---|---|---|---|
| `runtimeEnvironments.base.image.repository` | string | `ghcr.io/lenaxia/llmsafespaces/base` | Base runtime image. |
| `runtimeEnvironments.base.image.tag` | string | `""` | Falls back to `Chart.AppVersion`. |

## `frontend`

Optional web UI. React 19 + TypeScript + Vite.

| Key | Type | Default | Description |
|---|---|---|---|
| `frontend.enabled` | bool | `false` | Deploy the frontend. |
| `frontend.replicaCount` | int | `1` | |
| `frontend.image.repository` / `tag` / `pullPolicy` | | `ghcr.io/lenaxia/llmsafespaces/frontend` / `""` / `IfNotPresent` | |
| `frontend.apiBaseUrl` | string | `"/api/v1"` | API base URL injected at container start. |
| `frontend.ingress.enabled` | bool | `false` | Create a frontend Ingress. |
| `frontend.ingress.className` | string | `""` | |
| `frontend.ingress.host` | string | `safespace.example.com` | Primary hostname. |
| `frontend.ingress.tls` | bool | `true` | **Default flipped `false → true` (RT-6.14).** Pre-fix exposed the frontend over plain HTTP, leaking JWT cookies. |
| `frontend.ingress.tlsSecret` | string | `""` | Existing TLS Secret name. |
| `frontend.ingress.annotations` | object | see values | Security headers for ingress-nginx: CSP (`frame-ancestors 'none'`), `X-Frame-Options: DENY`, HSTS, `X-Content-Type-Options`, `Referrer-Policy`. Override for Traefik/HAProxy. |
| `frontend.ingress.additionalHosts` | list | `[]` | Additional hostnames. Each: `{host, [tlsSecret]}`. |

## `mcp`

The MCP server (stdio/SSE). Delegates auth to the API.

| Key | Type | Default | Description |
|---|---|---|---|
| `mcp.enabled` | bool | `true` | Deploy the MCP server. |
| `mcp.replicaCount` | int | `1` | |
| `mcp.transport` | string | `"sse"` | `sse` (HTTP SSE) or `stdio` (sidecar/subprocess). |
| `mcp.service.type` / `port` | | `ClusterIP` / `3001` | |
| `mcp.apiBaseUrl` | string | `"http://{{ .Release.Name }}-api:8080"` | In-cluster API URL. |
| `mcp.timeout` | duration | `300s` | `session_message` timeout. |

| `internalToken` | string | `""` | Shared secret for controller↔API internal org-status calls (D20). Empty = auto-generate. The internal org-status endpoint **fails closed (403)** when unset. |

## `email` (Epic 49)

| Key | Type | Default | Description |
|---|---|---|---|
| `email.enabled` | bool | `false` | Enable transactional email (org invitations, password reset, verification). Disabled = NoopProvider logs to stderr. |
| `email.provider` | string | `""` | `""` (noop) or `"ses"`. `"smtp"` is future. |
| `email.sesRegion` | string | `""` | AWS region. |
| `email.fromAddress` | string | `""` | Verified SES sender. |
| `email.baseUrl` | string | `""` | Public origin for email body links. Must match the frontend ingress host. |

For SES, configure IRSA via `serviceAccount.api.annotations` (`eks.amazonaws.com/role-arn`). No static AWS credentials stored.

## `oidc` (per-org SSO plumbing — Epic 43)

Instance-level wiring for the per-org OIDC flow. Does **not** configure an IdP — each org does that via `/api/v1/orgs/:id/sso`.

| Key | Type | Default | Description |
|---|---|---|---|
| `oidc.redirectBaseUrl` | string | `""` | Absolute origin the IdP redirects back to. Full callback: `{redirectBaseUrl}/api/v1/auth/sso/:orgSlug/callback`. **Set in production.** Empty derives from `X-Forwarded-*` headers (F11 gap documented). |
| `oidc.frontendRedirectUrl` | string | `""` | Where the browser lands after SSO callback. Empty = `/`. |
| `oidc.stateCookieName` | string | `""` | Signed PKCE/state cookie name. Empty = `"lsp_sso_state"`. |

## `orgSubdomainRouting` (Epic 54, US-54.3)

Org-scoped login via wildcard subdomain routing. Disabled by default.

| Key | Type | Default | Description |
|---|---|---|---|
| `orgSubdomainRouting.enabled` | bool | `false` | Enable wildcard subdomain routing. |
| `orgSubdomainRouting.baseDomain` | string | `""` | Parent domain. E.g. `app.example.com` → org `acme` → `acme.app.example.com`. **Required** when enabled. |
| `orgSubdomainRouting.cookieDomain` | string | `""` | Cookie Domain attribute. Must start with `.`. **Required** when enabled. |
| `orgSubdomainRouting.wildcardCert.tlsSecret` | string | `""` | Existing wildcard TLS Secret. |
| `orgSubdomainRouting.wildcardCert.issuerRef.name` / `kind` | string | `""` / `ClusterIssuer` | cert-manager Issuer for the wildcard cert. |

**Prerequisites:** wildcard DNS, cert-manager, an ingress controller supporting wildcard host rules.

## `monitoring`

Grafana dashboards, Prometheus alerts, ServiceMonitors. Requires Grafana sidecar + Prometheus Operator.

| Key | Type | Default | Description |
|---|---|---|---|
| `monitoring.enabled` | bool | `false` | Master toggle. |
| `monitoring.dashboards.enabled` | bool | `true` | Deploy dashboard ConfigMaps (`grafana_dashboard: "1"` label). |
| `monitoring.dashboards.namespace` | string | `""` | Override namespace. |
| `monitoring.datasources.enabled` | bool | `true` | Deploy Postgres datasource for the billing dashboard. Requires `${PG_PASSWORD}` env in the Grafana pod. |
| `monitoring.datasources.postgres.uid` | string | `"llmsafespaces-postgres"` | Datasource UID referenced by dashboard panels. |
| `monitoring.prometheusRules.enabled` | bool | `true` | Deploy PrometheusRule resources. |
| `monitoring.serviceMonitors.enabled` | bool | `true` | Deploy ServiceMonitors for API + controller. |
| `monitoring.serviceMonitors.api.bearerTokenSecret` | object | `{}` | Optional bearer token Secret for the API `/metrics` endpoint (required when `LLMSAFESPACES_METRICS_TOKEN` is set). |
| `monitoring.serviceMonitors.agentdPodMonitor.enabled` | bool | `true` | PodMonitor for workspace pods' agentd admin port (`:4098/metrics`). Also extends the workspace NetworkPolicy to permit Prometheus ingress on 4098. |

## `turnstile` (Cloudflare CAPTCHA — US-63.x)

| Key | Type | Default | Description |
|---|---|---|---|
| `turnstile.enabled` | bool | `false` | Enable Turnstile on `/register`. |
| `turnstile.siteKey` | string | `""` | Public site key (baked into frontend HTML). |
| `turnstile.secretKey.existingSecret` | string | `"llmsafespaces-credentials"` | K8s Secret holding the secret key. |
| `turnstile.secretKey.key` | string | `"turnstile-secret"` | Key within the Secret. |
| `turnstile.verifyURL` | string | `"https://challenges.cloudflare.com/turnstile/v0/siteverify"` | Cloudflare verification endpoint. |

# Installation

This page covers deploying LLMSafeSpaces into a production Kubernetes cluster with Helm: the prerequisites you must provision first, the `helm install` flow, how generated credentials and the master KEK are handled, TLS/ingress setup, and how to verify a healthy install. For the full list of chart values, see the [Helm Values Reference](../reference/helm-values.md); this page focuses on the decisions and steps an operator makes at install time.

## On this page

- [Prerequisites](#prerequisites)
- [Install the chart](#install-the-chart)
- [Generated credentials and the master KEK](#generated-credentials-and-the-master-kek)
- [TLS and ingress](#tls-and-ingress)
- [Verify the install](#verify-the-install)
- [Production checklist](#production-checklist)
- [Next steps](#next-steps)

---

## Prerequisites

### Kubernetes

A cluster running **Kubernetes 1.27 or later**. The chart uses `admissionregistration.k8s.io/v1` (validating webhooks) and `cert-manager.io/v1` (webhook TLS). Node operating systems must run **cgroup v2** — the workspace-agentd sidecar reads `/sys/fs/cgroup/memory.current` and `/sys/fs/cgroup/memory.max` to surface memory-pressure warnings and OOM attribution (see [Storage](storage.md#cgroup-v2) for why this matters). All modern node OSes (Debian 11+, Ubuntu 22.04+, RHEL 9, Flatcar, Bottlerocket, Talos) default to cgroup v2.

### Helm

**Helm 3.13+** (also validated against Helm 4). The chart ships ~150 documented values and a pre-install/pre-upgrade migration Job.

### cert-manager

Required for the validating webhooks (`Workspace`, `RuntimeEnvironment`, optional per-tenant quota). The chart uses `cert-manager.io/v1` `Issuer` + `Certificate` resources plus the `cert-manager.io/inject-ca-from` annotation read by `cainjector`.

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
kubectl wait --for=condition=Available -n cert-manager deployment/cert-manager-webhook --timeout=120s
```

!!! warning "Without cert-manager"
    If you cannot install cert-manager, set `webhooks.enabled=false`. Admission validation will then only be enforced client-side by the API service. **Operators using `kubectl` directly will bypass validation entirely** — including the storage-size ceiling, the registry allow-list, and the resource caps. This is not recommended for production.

### Postgres

The chart does **not** bundle Postgres. Provide an existing instance reachable from the cluster. The migration Job expects the database named in `postgresql.database` to **already exist** — the migrations create schema objects (tables, indexes), not the database itself.

```bash
helm install pg oci://registry-1.docker.io/bitnamicharts/postgresql \
    --version 13.4.4 \
    --set auth.username=llmsafespaces \
    --set auth.password=<strong-password> \
    --set auth.database=llmsafespaces \
    -n llmsafespaces --create-namespace
```

??? tip "Green-field Postgres without a pre-created role"
    If you are deploying against a stock Postgres where the `llmsafespaces` role and database do not exist yet, the migration Job will fail with `FATAL: role "llmsafespaces" does not exist`. Enable the optional bootstrap Job:

    ```yaml
    dbInit:
      enabled: true
      superuserSecret:
        name: <postgres-superuser-secret>
    ```

    This renders a pre-install hook Job that connects as a superuser and runs idempotent `CREATE ROLE` / `CREATE DATABASE` statements before the migration Job runs.

Production Postgres requirements:

- Connection pooling configured to match `postgresql.maxOpenConns` (default 25) and `postgresql.maxIdleConns` (default 10).
- TLS to the database if traffic crosses an untrusted network (`postgresql.sslMode`).
- Regular backups. Postgres holds user accounts, API keys, encrypted secrets, org SSO configs, and settings.

### Redis

Same model — provide an existing instance. Configure `redis.host`, `redis.port`, and the password via the external Secret.

!!! danger "Redis stores workspace passwords in plaintext"
    The API caches per-workspace opencode passwords in Redis in **plaintext** (`wsstate.SetCachedPassword`). This is necessary because the proxy needs the plaintext for HTTP Basic-Auth on every forwarded request. Production Redis **must** provide:

    - TLS in transit (`rediss://`, or a TLS sidecar).
    - At-rest encryption (Redis 7 ACL + encryption, or PVC-level encryption).
    - A [NetworkPolicy](networking.md#datastore-network-policies) restricting ingress to API pods only.

    Without these, workspace passwords are exposed in RDB/AOF dumps, memory, and backups. The passwords are per-workspace generated credentials (not user passwords), bounded by a 1h TTL, and the source of truth is the K8s Secret (encrypted at rest by the cluster).

### A StorageClass

Workspaces are PVC-backed. Pick (or provision) a StorageClass appropriate for your durability and performance needs. See [Storage](storage.md#storageclass-selection) for Longhorn vs cloud-provider CSI guidance.

---

## Install the chart

=== "From a local chart checkout"

    ```bash
    helm install llmsafespaces ./helm \
        -n llmsafespaces --create-namespace \
        --set postgresql.host=pg-postgresql \
        --set redis.host=redis-master \
        --set externalSecret.postgresPassword=<strong-password>
    ```

=== "From a values file (recommended for production)"

    Create `llmsafespaces.yaml`:

    ```yaml
    api:
      replicaCount: 2
      config:
        server:
          host: "0.0.0.0"
        auth:
          jwtIssuer: "llmsafespaces-prod"
          jwtAudience: "llmsafespaces-prod"
        security:
          allowedOrigins:
            - "https://app.example.com"

    postgresql:
      host: pg-postgresql
      port: 5432
      user: llmsafespaces
      database: llmsafespaces
      sslMode: require

    redis:
      host: redis-master
      port: 6379

    # Pin to immutable image tags for reproducibility
    api:
      image:
        tag: sha-ac861c3
    controller:
      image:
        tag: sha-ac861c3

    workspace:
      defaultStorageClass: longhorn-2r

    frontend:
      enabled: true
      ingress:
        enabled: true
        host: app.example.com
        tls: true
    ```

    ```bash
    helm install llmsafespaces ./helm \
        -n llmsafespaces --create-namespace \
        -f llmsafespaces.yaml
    ```

### Watch the migration Job

```bash
kubectl -n llmsafespaces get jobs -w
```

The migration Job runs the `migrate/migrate:v4.17.1` image against the SQL files bundled in the chart's `migrations/` ConfigMap. It has `backoffLimit: 3` and `ttlSecondsAfterFinished: 600`. If it fails, inspect the Job logs:

```bash
kubectl -n llmsafespaces logs job/llmsafespaces-migrations
```

### Wait for the Deployments

```bash
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-controller
```

---

## Generated credentials and the master KEK

The chart auto-generates three critical credentials on first install when you leave them empty, and preserves them across upgrades via the `helm.sh/resource-policy: keep` annotation on the Secret:

| Credential | Default behavior | Helm value |
|---|---|---|
| Postgres password | 32-char random alphanumeric | `externalSecret.postgresPassword` |
| Redis password | 32-char random alphanumeric | `externalSecret.redisPassword` |
| JWT signing secret | 32-char random alphanumeric | `externalSecret.jwtSecret` |
| **Master KEK** | **64-char random** (root of trust for at-rest credential encryption) | `externalSecret.masterSecret` |
| Internal token | random (controller↔API org-status auth) | `internalToken` |

### The master KEK

The master KEK (Key Encryption Key) is the root of trust for at-rest encryption. It wraps API-key DEKs, org SSO client secrets, and every admin/org LLM provider credential. Compromise of the KEK decrypts every row it wraps — treat it as the most sensitive secret in the deployment.

**Default delivery: read-only file mount.** The KEK is projected as a file at `/var/run/secrets/llmsafespaces/master-secret` (mode `0440`), read via `LLMSAFESPACES_MASTER_SECRET_FILE`. This eliminates `/proc/1/environ` exposure — the previous env-var delivery path is a deprecated opt-in (`masterSecret.deliveryMethod=env`) for non-Helm deploys and logs a startup warning when used.

```yaml
masterSecret:
  deliveryMethod: file   # default; the only recommended value for production
  fileMountPath: /var/run/secrets/llmsafespaces/master-secret
```

!!! note "Rotating the KEK"
    Rotating the master KEK re-wraps every encrypted credential in Postgres. The `rotate-kek` CLI (`cmd/rotate-kek/main.go`) supports dry-run, resume-from, and multi-table rotation. See the [Runbook](runbook.md#rotating-the-master-kek) for the full procedure.

### Bring-your-own credentials

If you point at an external Postgres/Redis whose password is already known, pin it explicitly:

```bash
helm install llmsafespaces ./helm \
    --set externalSecret.create=false \
    --set externalSecret.existingSecret=my-creds-secret \
    ...
```

The existing Secret must contain the keys `postgres-password`, `redis-password`, `jwt-secret`, and (if not in `masterSecret`-managed mode) `master-secret`.

!!! warning "Rotation out of a vulnerable state"
    If a previous chart version left `postgres-password="changeme"` or `redis-password=""` in the live Secret, the next `helm upgrade` will re-randomize both values. You must then run the `ALTER USER` procedure documented in the chart's `NOTES.txt` to bring the running Postgres role in sync.

---

## TLS and ingress

The API service does **not** terminate TLS itself — it listens on plain HTTP (`0.0.0.0:8080`). Terminate TLS at an ingress controller in front of it. The frontend ingress defaults to TLS on:

```yaml
frontend:
  enabled: true
  ingress:
    enabled: true
    host: app.example.com
    tls: true              # default; must explicitly set false to disable
    tlsSecret: ""          # provide a name, or let cert-manager issue one
    annotations:
      nginx.ingress.kubernetes.io/configuration-snippet: |
        more_set_headers "Content-Security-Policy: default-src 'self'; ...";
        more_set_headers "Strict-Transport-Security: max-age=31536000; includeSubDomains";
```

### Trusting `X-Forwarded-Proto`

The API relies on the ingress to set correct forwarded headers. For per-org OIDC SSO, the platform **requires** `oidc.redirectBaseUrl` to be set explicitly — it no longer derives the callback URL from `X-Forwarded-*` headers (a fail-loud hardening change). See [OIDC SSO](oidc-sso.md#redirect-base-url) for the rationale.

### CORS

Configure the origins permitted to make credentialed cross-origin requests:

```yaml
api:
  config:
    security:
      allowedOrigins:
        - "https://app.example.com"
      allowCredentials: false
```

The API refuses to start if `allowedOrigins=["*"]` is combined with `allowCredentials=true` — this violates the CORS spec (Fetch §3.2.1) and the fail-closed guard in `config.validateSecurity` enforces it.

---

## Verify the install

### Verify image signatures (recommended)

Before trusting the running pods, verify the images haven't been tampered with:

```bash
# Install cosign if you don't have it: https://github.com/sigstore/cosign
for img in api controller base frontend relay-router relay-proxy; do
  cosign verify "ghcr.io/lenaxia/llmsafespaces/${img}:0.4.2" \
    --certificate-identity-regexp "https://github.com/lenaxia/LLMSafeSpaces" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
done
```

If verification passes, the images were built by the project's release workflow. See [Security Hardening](security.md#supply-chain-security) for the full supply chain story.

### Health endpoints

```bash
kubectl -n llmsafespaces port-forward svc/llmsafespaces-api 8080:8080 &
curl http://localhost:8080/livez    # 200 — process is responsive
curl http://localhost:8080/readyz   # 200 only if Postgres AND Redis are reachable
curl http://localhost:8080/health   # legacy alias for /livez
```

The `readyz` contract: it pings Postgres and Redis with a 2s timeout each and returns `503` if either is down. This is the signal kubelet uses for readiness — a pod that fails readiness is removed from the Service endpoints but not restarted.

### Controller health

```bash
kubectl -n llmsafespaces port-forward svc/llmsafespaces-controller 8081:8081 &
curl http://localhost:8081/healthz
curl http://localhost:8081/readyz
```

### Smoke test: create a workspace

```bash
API=http://localhost:8080

TOKEN=$(curl -sX POST "$API/api/v1/auth/register" \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"hunter2hunter2","username":"alice"}' \
  | jq -r '.token')

WS=$(curl -sX POST "$API/api/v1/workspaces" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"smoke","runtime":"base","storageSize":"1Gi"}' \
  | jq -r '.id')

curl -X POST "$API/api/v1/workspaces/$WS/activate" \
  -H "Authorization: Bearer $TOKEN"

# Wait for Active
while [ "$(curl -s -H "Authorization: Bearer $TOKEN" \
    "$API/api/v1/workspaces/$WS/status" | jq -r .phase)" != "Active" ]; do
  sleep 2
done
echo "workspace $WS is Active"
```

### Verify the workspace pod

```bash
kubectl -n llmsafespaces get pods -l llmsafespaces.dev/workspace
kubectl -n llmsafespaces get workspace "$WS" -o yaml | grep -A5 conditions
```

---

## Production checklist

Before declaring the install production-ready:

- [ ] **Kubernetes 1.27+** with cgroup v2 nodes.
- [ ] **cert-manager** installed (or `webhooks.enabled=false` with the trade-off understood).
- [ ] **Postgres** reachable, database + role created, TLS configured, backups scheduled.
- [ ] **Redis** reachable, TLS in transit, at-rest encryption, NetworkPolicy restricting ingress.
- [ ] **StorageClass** chosen and pinned via `workspace.defaultStorageClass` (see [Storage](storage.md)).
- [ ] **Master KEK** delivery is `file` (the default), not `env`.
- [ ] **Generated credentials** recorded in your secrets manager (extract from the Secret once).
- [ ] **TLS ingress** configured; `frontend.ingress.tls=true` (the default).
- [ ] **CORS** `allowedOrigins` set to your actual frontend origin(s).
- [ ] **JWT iss/aud** set (`auth.jwtIssuer` / `auth.jwtAudience`) if running multiple instances.
- [ ] **Image tags pinned** to immutable `sha-<commit>` tags (avoid `dev` / `latest` in production).
- [ ] **RBAC scope** understood (`namespace` default vs `cluster` for multi-namespace).
- [ ] **NetworkPolicies** enabled (`networkPolicy.enabled=true`, the default) and a CNI that enforces them.
- [ ] **gVisor** evaluated for multi-tenant deployments (see [Security](security.md#gvisor)).
- [ ] **Monitoring** enabled (`monitoring.enabled=true`) with Prometheus Operator + Grafana sidecar.
- [ ] **etcd encryption at rest** enabled at the cluster level (operator responsibility — the chart cannot enforce this).

---

## Next steps

- [Configuration](configuration.md) — the ConfigMap, env var overrides, and the settings system.
- [Storage](storage.md) — PVC layout, StorageClass selection, sizing.
- [Networking](networking.md) — ingress, NetworkPolicies, egress allowlists.
- [Security Hardening](security.md) — the threat model in operator-actionable terms.
- [Helm Values Reference](../reference/helm-values.md) — every chart value documented.

# Multi-tenant Isolation

This page covers how LLMSafeSpaces isolates tenants who share a cluster: the shared-namespace model and why per-tenant namespaces are not the primary isolation boundary, the tenant identity label, gVisor as the primary kernel-isolation control, the opt-in per-tenant resource quota webhook, org-scoped access, and when per-tenant namespaces do make sense. Multi-tenant isolation is a layered defense — no single control is sufficient.

## On this page

- [The isolation model](#the-isolation-model)
- [Tenant identity](#tenant-identity)
- [Layered controls](#layered-controls)
- [gVisor: the primary kernel-isolation control](#gvisor-the-primary-kernel-isolation-control)
- [Per-tenant resource quotas](#per-tenant-resource-quotas)
- [Org-scoped access](#org-scoped-access)
- [RBAC scope](#rbac-scope)
- [A multi-tenant deployment example](#a-multi-tenant-deployment-example)
- [Observability per tenant](#observability-per-tenant)
- [When to use per-tenant namespaces](#when-to-use-per-tenant-namespaces)

---

## The isolation model

LLMSafeSpaces uses a **shared-namespace** model for multi-tenancy. All workspaces — across all tenants — run as pods in a single namespace (the release namespace, or a configured workspace namespace). Isolation is achieved through layered controls on top of the shared namespace, not through namespace boundaries.

### Why not per-tenant namespaces

| Argument for per-tenant namespaces | Why it doesn't hold here |
|---|---|
| "Namespaces isolate tenants" | Namespaces do not stop container escape. A kernel exploit in a tenant pod reaches the host node regardless of namespace. |
| "Namespaces enable per-tenant RBAC" | The platform's RBAC is for the controller/API ServiceAccounts, not for tenants. Tenants never get K8s RBAC — they interact via the API. |
| "Namespaces scale" | They don't scale to 1,000+ tenants. Namespace count has real costs (controller cache, apiserver RBAC evaluation, etcd). |

The platform's threat model (see [Security Hardening](security.md)) assumes tenants can run arbitrary (potentially malicious) code in their workspace pods. The controls that matter against that threat — gVisor, NetworkPolicies, pod security context, secret scoping — all work in a shared namespace. Per-tenant namespaces add operational cost without addressing the kernel-escape threat.

---

## Tenant identity

Every workspace pod carries a tenant identity via the `llmsafespaces.dev/tenant` label. The value is derived from the workspace owner:

- For personal workspaces: the user ID.
- For org workspaces: the org ID.

```yaml
metadata:
  labels:
    llmsafespaces.dev/tenant: "org-abc123"
```

This label is the key for:

- **Per-tenant resource quotas** — the `PodTenantQuotaValidator` webhook counts pods by this label.
- **Observability** — metrics and dashboards can break down by tenant.
- **Future billing** — usage attribution (see [Monitoring](monitoring.md#billing--metering)).

The CRD also carries structured ownership:

```yaml
spec:
  owner:
    userID: "user-xyz"
    orgID: "org-abc123"   # present for org-owned workspaces
```

This is authoritative for API-level ownership checks (`WorkspaceAccessMiddleware`).

---

## Layered controls

Multi-tenant isolation rests on controls in multiple layers. Each is independently shippable and configurable:

| Control | Status | Mechanism | See |
|---|---|---|---|
| Network isolation | Shipped | Default-deny ingress + RFC1918/CGNAT-filtered egress NetworkPolicies | [Networking](networking.md) |
| Secret scoping | Shipped | `rbac.scope=namespace` default; namespace-scoped Secrets Role | [Security](security.md#rbac-scope) |
| Tenant identity | Shipped | `WorkspaceOwner{UserID, OrgID}` on the CRD; `llmsafespaces.dev/tenant` pod label | [Tenant identity](#tenant-identity) |
| Container-runtime isolation | Opt-in | gVisor (`runsc`) RuntimeClass | [gVisor](#gvisor-the-primary-kernel-isolation-control) |
| Per-tenant resource quotas | Opt-in | `PodTenantQuotaValidator` admission webhook | [Per-tenant quotas](#per-tenant-resource-quotas) |

Org-specific quota overrides and billing-tier→quota mapping are deferred to Epic 43.

---

## gVisor: the primary kernel-isolation control

[gVisor](https://gvisor.dev/) is the **primary control against kernel-exploitation container escape** for multi-tenant deployments. Without it, a kernel CVE in a tenant pod can reach the host node and access other tenants' data. seccomp + cap-drop reduce the syscall surface but do not proxy syscalls in userspace — gVisor does.

### Enabling

```yaml
gvisor:
  enabled: true
  defaultRuntimeClass: "gvisor"
```

This creates a `RuntimeClass` named `gvisor` and sets `--default-runtime-class=gvisor` on the controller. All workspace pods then run under gVisor by default. See [Security Hardening](security.md#gvisor-kernel-isolation) for prerequisites and the admin-gated opt-out.

### When gVisor is essential

| Deployment | gVisor |
|---|---|
| Single-user / homelab | Optional — you trust your own code. |
| Small team, trusted members | Optional — social trust covers kernel risk. |
| Multi-tenant SaaS, untrusted users | **Required** — arbitrary code from tenants. |
| Compliance-sensitive (SOC2, HIPAA) | **Required** — defense-in-depth for audits. |

---

## Per-tenant resource quotas

The `PodTenantQuotaValidator` is an opt-in admission webhook (Epic 51 S51.2) that caps aggregate resource usage per tenant. It runs on Pod create, counts existing workspace pods per tenant (by the `llmsafespaces.dev/tenant` label), and rejects creation when the aggregate would exceed the limits.

```yaml
webhooks:
  tenantQuota:
    maxWorkspacesPerTenant: 0      # disabled by default
    maxCPUMillisPerTenant: 0
    maxMemoryMiPerTenant: 0
```

### Recommended values

| Setting | Recommended | Rationale |
|---|---|---|
| `maxWorkspacesPerTenant` | 10–20 | Prevents noisy-neighbor pod sprawl. |
| `maxCPUMillisPerTenant` | 8000 (8 cores) | Bounds aggregate compute. |
| `maxMemoryMiPerTenant` | 16384 (16 GiB) | Bounds aggregate memory. |

All values default to `0` (disabled). Enable for multi-tenant deployments. Set any individual value to `0` to disable that dimension while keeping the others.

### Behavior

- **Disabled when all limits are 0** — the webhook is inert, no admission overhead.
- **Fails open on transient errors** — if the webhook can't reach the apiserver to count pods, it admits the request (availability over strictness). This is deliberate: a webhook outage should not lock out all workspace creation.
- **Counts only workspace pods** — identified by the tenant label; non-workspace pods are ignored.
- **Org-specific overrides** — deferred to Epic 43. Currently, quotas are cluster-wide; every tenant gets the same limits.

### Example: enabling quotas

```yaml
webhooks:
  tenantQuota:
    maxWorkspacesPerTenant: 15
    maxCPUMillisPerTenant: 8000
    maxMemoryMiPerTenant: 16384
```

After `helm upgrade`, the webhook rejects any Pod create that would push a tenant over these limits.

---

## Org-scoped access

Organizations (orgs) are the unit of multi-tenant grouping above individual users. An org owns workspaces, credentials, SSO config, and policies. Org membership has roles (`admin`, `member`).

### Org workspaces

A workspace can be owned by a user or an org:

```yaml
spec:
  owner:
    userID: "user-xyz"          # personal workspace
    # OR
    orgID: "org-abc123"          # org workspace
```

Org workspaces are accessible to org members per their role. The `WorkspaceAccessMiddleware` checks `WorkspaceOwner{UserID, OrgID}` against the caller's identity on every `:id` route.

### Org isolation within a shared namespace

Orgs share the same namespace but are isolated by:

- **Ownership labels** — org workspaces carry the org's tenant label.
- **NetworkPolicies** — default-deny ingress means org A's pods cannot reach org B's pods.
- **Secret scoping** — credentials are per-workspace K8s Secrets; RBAC restricts access to the controller/API ServiceAccounts.
- **Per-org SSO** — each org configures its own OIDC IdP. See [OIDC SSO](oidc-sso.md).

### Org policies

Org-level configuration lives in dedicated normalized tables (`org_policies`, `org_sso_configs`, `org_credentials`) — not a generic key-value `org_settings` table. This keeps the schema typed and auditable.

---

## RBAC scope

The chart's `rbac.scope` controls how the controller's Kubernetes RBAC is granted. This is a blast-radius decision, not a tenant-isolation decision — but it matters for multi-tenant deployments.

| Scope | RBAC granted | Use when |
|---|---|---|
| **`namespace`** (default, G5) | Role + RoleBinding scoped to the release namespace | Single-namespace deployments. Tightest least-privilege. |
| **`cluster`** | Adds ClusterRole + ClusterRoleBinding for `llmsafespaces.dev/*` + `storageclasses` | Multi-namespace deployments (controller watches multiple namespaces) or the self-hosted relay fleet (cluster-scoped `InferenceRelay` CRD). |

```yaml
rbac:
  scope: "namespace"   # default
```

Even in `cluster` mode, Pods, Secrets, PVCs, and NetworkPolicies remain namespace-scoped. Operators running multi-namespace deployments must per-namespace-bind the workspace Role themselves. The default flipped from `cluster` to `namespace` in worklog 0107 — pre-flip, the chart-default install gave the controller cluster-wide secrets+pods access, which was a blast-radius hazard for a single-namespace deployment.

### Combining with `watchNamespaces`

For tightest isolation in a multi-namespace deployment, combine `rbac.scope=namespace` with explicit `controller.watchNamespaces`:

```yaml
controller:
  watchNamespaces: "tenant-acme,tenant-globex"
rbac:
  scope: "namespace"
```

The controller's `--watch-namespaces` flag narrows what it actually reconciles, and `cache.DefaultNamespaces` further narrows the informer cache. This is defense-in-depth: even if the controller is granted cluster-wide RBAC elsewhere, it only watches the listed namespaces.

---

## A multi-tenant deployment example

A reference values file for a multi-tenant SaaS deployment:

```yaml
# Multi-tenant hardening
namespace:
  podSecurityEnforce: "restricted"

rbac:
  scope: "namespace"   # single namespace; flip to cluster for multi-namespace

gvisor:
  enabled: true                 # REQUIRED for untrusted tenants
  defaultRuntimeClass: "gvisor"

networkPolicy:
  enabled: true
  workspaceEgress: true
  apiIngressRestricted: false   # enable after confirming ingress selector

webhooks:
  enabled: true
  failurePolicy: "Fail"
  tenantQuota:
    maxWorkspacesPerTenant: 15
    maxCPUMillisPerTenant: 8000
    maxMemoryMiPerTenant: 16384
  allowedImageRegistries:
    - "ghcr.io/lenaxia/"
  maxWorkspaceStorageGi: 50      # tighten from 1024 default

masterSecret:
  deliveryMethod: "file"         # default; never use env in multi-tenant

api:
  config:
    auth:
      jwtIssuer: "llmsafespaces-prod"
      jwtAudience: "llmsafespaces-prod"
    security:
      allowedOrigins:
        - "https://app.example.com"
      allowCredentials: true

oidc:
  redirectBaseUrl: "https://app.example.com"
  frontendRedirectUrl: "https://app.example.com"
```

### What this configuration enforces

- Every workspace pod runs under gVisor (kernel isolation).
- Each tenant is capped at 15 workspaces, 8 cores, 16 GiB aggregate.
- Workspace storage capped at 50 GiB (tighter than the 1024 default).
- Egress blocks RFC1918/CGNAT/metadata.
- The KEK is file-mounted (not in `/proc/1/environ`).
- JWT iss/aud are set (tokens don't cross instances).
- CORS is explicit (no wildcards).

### What this configuration does NOT enforce

- IPv6 egress is denied by NetworkPolicy default-deny (G43 resolved).
- DNS exfiltration is not blocked (G30, accepted — use Cilium FQDN or Calico GlobalNetworkPolicy).
- No mTLS on the pod network (G4, accepted — add a service mesh if needed).
- Decrypt operations are audited (G50 fixed — AuditedProvider wired).

---

## Observability per tenant

The `llmsafespaces.dev/tenant` label enables per-tenant breakdowns:

- **Metrics** — workspace metrics (`workspace_memory_bytes`, `workspace_active_sessions`, etc.) carry the tenant label via the pod. Build Grafana panels grouped by tenant.
- **Billing** — the billing dashboard (`llmsafespaces-billing` UID) queries Postgres `usage_events` for per-user/per-workspace attribution. Org-owned workspaces attribute to the org.
- **Audit logs** — secret operations are logged with the user ID; org operations with the org ID.

```promql
# Aggregate memory by tenant
sum by (tenant) (workspace_memory_bytes)

# Workspaces per tenant
count by (tenant) (workspace_active_sessions)
```

Org-specific quota overrides and billing-tier→quota mapping are deferred to Epic 43.

---

## When to use per-tenant namespaces

The shared-namespace model is the default and recommended approach. Per-tenant namespaces make sense in specific cases:

| Scenario | Use per-tenant namespaces? | Why |
|---|---|---|
| < 50 tenants, shared cluster | No | Shared-namespace with layered controls is simpler and scales. |
| 50–500 tenants | No | Still manageable in one namespace with quotas. |
| 500+ tenants | Maybe | Consider namespace sharding (e.g. `tenants-a-f`, `tenants-g-m`) for controller-cache and apiserver-load reasons — not for security. |
| Hard regulatory isolation | Yes | If a regulator requires physical/namespace separation between tenants (rare), use per-tenant namespaces with dedicated controller instances. |
| Per-tenant cluster-admin access | Yes | If tenants legitimately need `kubectl` access to their own resources (unusual for this platform). |

### How to deploy per-tenant namespaces

If you do need per-tenant namespaces, the controller supports watching specific namespaces:

```yaml
controller:
  watchNamespaces: "tenant-acme,tenant-globex"

rbac:
  scope: "namespace"
```

The controller's `--watch-namespaces` flag narrows what it reconciles. Combine with namespace-scoped RoleBindings for least-privilege. Resources in unwatched namespaces will not be reconciled.

!!! note "Per-tenant namespaces do not replace gVisor"
    Even with per-tenant namespaces, a kernel exploit still crosses the namespace boundary via the host node. gVisor remains the primary kernel-isolation control regardless of namespace topology. Namespaces are a bulkhead, not a sandbox.

---

## Related

- [Security Hardening](security.md) — the threat model and gVisor opt-out gate.
- [Networking](networking.md) — the NetworkPolicies that enforce tenant network isolation.
- [OIDC SSO](oidc-sso.md) — per-org identity providers.
- [Helm Values Reference](../reference/helm-values.md) — `gvisor.*`, `webhooks.tenantQuota.*`, `controller.watchNamespaces`.

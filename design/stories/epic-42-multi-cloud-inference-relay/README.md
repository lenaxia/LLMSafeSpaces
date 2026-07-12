# Epic 42: Multi-Cloud Inference Relay

> **⚠ SUPERSESSION NOTICE (2026-06-20, worklog 0442):** The WireGuard mesh
> described throughout this document was **removed** and replaced with
> **HTTPS + per-VM shared-secret token auth**. The change was driven by:
>
> 1. **Operational fragility** — the `render-wg.sh` WG sidecar (the most
>    intricate piece, never validated live in CI) carried the entire auth
>    burden; a single shell-script regression took down the fleet.
> 2. **Unjustified complexity for the threat model** — WG's defense-in-depth
>    (network isolation, in-kernel encryption, per-VM asymmetric keypairs)
>    protected a low-value asset (free-tier Zen access). The CF Worker relay
>    shipped in Epic 26 has the identical exposure (URL/token obscurity) and is
>    an accepted baseline.
> 3. **Operator friction** — WG required a public UDP endpoint
>    (`spec.wireGuard.routerEndpoint`), 4-mode ingress selection, UDP
>    LoadBalancer/NodePort/hostNetwork plumbing, and a privileged sidecar that
>    forced the namespace PSA profile to `privileged`. All of that is gone.
>
> **What changed in code:**
> - `controller/internal/relay/wireguard.go` — **deleted** (keypair gen,
>   wg0.conf rendering).
> - `charts/.../relay-router-wg-scripts.yaml`, `relay-router-wg-service.yaml`
>   — **deleted** (the `render-wg.sh` ConfigMap + UDP LB/NodePort Service).
> - `cmd/relay-wireguard/` — **deleted** (the privileged sidecar image).
> - `cmd/relay-proxy/auth.go` — **new**: `requireToken` middleware validates
>   `X-Relay-Token` via `crypto/subtle.ConstantTimeCompare`. `/healthz` and
>   `/metrics` are exempt (router health probes need them).
> - `cmd/relay-proxy/main.go` — `--listen` default changed from
>   `10.42.42.2:8080` (OCI WG IP) to `0.0.0.0:8080`; added `--token` /
>   `RELAY_TOKEN` flag.
> - `cmd/relay-router/proxy.go` — dials `http://<public-ip>` instead of
>   `http://<wgIP>:8080`; injects `X-Relay-Token` per-relay (mirrors the
>   `applyUpstreamAuth` pattern).
> - `PeerEntry` (both sides) — `WgIP`/`PublicKey` → `Endpoint` (public IP)
>   + `Token` (per-VM).
> - Controller `provisionRelay` — drops keypair/WG render; generates a
>   per-VM 32-byte hex token via `crypto/rand`, persists in the `relay-vm-tokens`
>   Secret, embeds in cloud-init.
> - `InferenceRelaySpec.WireGuard` and `RelayInstanceStatus.WgIP` — **removed**.
> - Helm: `relay-router` Deployment loses the privileged sidecar + 5 WG
>   volumes + hostNetwork branch; namespace stays PSA `restricted`.
>
> **What's preserved from the WG design:** weighted provider preference
> (AWS→OCI→GCP), 429-storm detection + destroy-and-recreate rotation,
> rate-limited Zen-direct fallback, the relay-router as a single in-cluster
> proxy, the controller-driven provisioning model, and the cloud-init binary
> artifact distribution + SHA-256 verification (worklog 0441).
>
> **The rest of this document is the original WG-era design and is retained as
> historical context.** Where it conflicts with the as-built (above), the
> as-built wins. A full rewrite is deferred — see worklog 0442 for the
> authoritative change record.

---

**Status:** Planning
**Created:** 2026-06-13
**Depends on:** Epic 26 (Client-Proxied Inference — CF Worker relay shipped)
**Supersedes:** None (extends Epic 26's relay architecture from single-cloudflare to multi-cloud)

---

## Problem Statement

### Current State

Epic 26 deployed a single Cloudflare Worker (`workers/inference-relay/`) as a transparent path-secret-authenticated proxy to `opencode.ai/zen/v1`. The relay distributes free-tier LLM traffic across Cloudflare's 300+ edge POPs, avoiding per-IP throttling from the platform's own server IPs.

This is now **broken in production**: `opencode.ai/zen` is IP-blocking Cloudflare's egress ranges. The relay architecture is correct (worklog `0184` confirmed the `public` key itself is not throttled — a laptop can reach Zen fine), but the Cloudflare IP ranges are blocked. Free-tier inference for all workspace pods is dead until we move the relay off Cloudflare.

> **Update 2026-06-19 (A23, worklog 0420):** ⚠️ **SUPERSEDED 2026-06-20 — A23 was disproven.** The 2026-06-19 note (below, struck through) claimed `public` returns 401 on `/chat/completions` from every IP/header and concluded "free-tier inference is dead." That was an artifact of probing only models that happen to have `allowAnonymous` unset in Zen's deploy-time config. A free model that IS `allowAnonymous` (`big-pickle`, resolves to `deepseek-v4-flash`) returns **HTTP 200** with `Bearer public` from the same residential IP (`24.18.52.209`) that produced the original 401s. ~~`public` now returns 401 on `/chat/completions` from every IP/header.~~ The actual mechanism is **per-model** (Zen handler `packages/console/app/src/routes/zen/util/handler.ts:599-603` + `model.ts:26`: `allowAnonymous: z.boolean().optional()`), not a global key death and not IP-based. **A0 (per-IP throttling for anonymous free-tier traffic) is restored as the relay's foundational premise.** The router upstream-auth injection (#297) and default→thekao (#298) remain valid *mechanisms* but their stated rationale is unfounded; see A23 row for the operator decision on whether to keep them as defaults. Relay scope is unchanged: free-model traffic only; paid-model traffic goes direct to the user's provider.

```
Workspace Pod (opencode) → relay.safespaces.dev → CF Worker → opencode.ai/zen/v1
                                                              ✗ IP-blocked
```

Additionally, the current relay has no self-healing or rotation:
- Single point of failure — one Worker, one IP range family
- No detection of 429s or IP blocks — failures are silent (opencode sees 429, user sees error)
- No automated IP rotation — operator must manually deploy a new Worker and update DNS
- No health monitoring — the controller has no idea if the relay is alive

### Desired State

A **portable relay binary** that runs on OCI, GCP, and AWS VMs, connected to the cluster via **WireGuard tunnels**, fronted by an **in-cluster router** that handles sticky session routing, failover, and 429 detection. A **relay controller** (CRD + reconciler) manages the full lifecycle of relay VMs — provisioning, health-checking, IP rotation, and replacement.

The controller maintains up to **2 relay VMs** by default: 1 AWS (paid primary) and 1 OCI (free secondary). AWS is the **paid primary** (~$7/month, most reliable — no idle reclamation, no capacity issues). OCI is the **free secondary** (10 TB egress, but idle-reclamation risk). The router sends 100% of traffic to AWS when healthy; OCI carries traffic during AWS rotation or failure. GCP is no longer in the default fleet (Always Free tier eliminated — see A12). The operator can add GCP as a paid provider if they want a third IP source.

```
                                  WireGuard mesh (10.42.42.0/24)
  ┌──────────────────────────────────────────────────────────────────────┐
  │                        LLMSafeSpace Cluster                          │
  │                                                                      │
  │  ┌──────────────┐         ┌────────────────────┐                     │
  │  │ Workspace     │  HTTP   │ relay-router        │    wg0: 10.42.42.1 │
  │  │ Pods          │────────→│ (Deployment, 2 rep) │────────────────────┼──┐
  │  │               │         │                     │                    │  │
  │  │ INFERENCE_    │         │ sticky: hash(wsID)  │                    │  │
  │  │ RELAY_BASEURL │         │   % healthyRelays   │                    │  │
  │  │ → router svc  │         │                     │                    │  │
  │  └──────────────┘         │ 429 detection       │                    │  │
  │                            │ drain + failover    │                    │  │
  │                            └────────────────────┘                    │  │
  │                                                                      │  │
  │  ┌──────────────────────────────────────────────────┐               │  │
  │  │ InferenceRelay Controller                         │               │  │
  │  │  - provisions AWS + OCI VMs                       │               │  │
  │  │  - generates WG keypairs, embeds in cloud-init    │               │  │
  │  │  - health-checks VMs over WG                      │               │  │
  │  │  - destroys + recreates on failure/429            │               │  │
  │  │  - pushes healthy relay IPs to router via CRD     │               │  │
  │  └──────────────────────────────────────────────────┘               │  │
  └──────────────────────────────────────────────────────────────────────┘  │
                               │                │                          │
                     ┌─────────┴─────┐    ┌─────┴───────────┐              │
                     │               │    │                 │              │
                encrypted UDP   encrypted UDP                             │
                     │                │                                    │
           ┌─────────┴─────┐ ┌────────┴────────┐                          │
           │ AWS t4g.micro │ │ OCI A1 VM       │                          │
           │ wg0:10.42.42.4│ │ wg0:10.42.42.2  │                          │
           │ relay:8080    │ │ relay:8080      │                          │
           │ (PAID primary)│ │ (free secondary)│                          │
           └───────┬───────┘ └───────┬─────────┘                          │
                   │                 │                                      │
                   └────────┬────────┘                                      │
                            ▼                                               │
                     opencode.ai/zen/v1 ◄──────────────────────────────────┘
```

---

## Design Principles

1. **WireGuard as the security boundary.** No TLS, no certs, no path-secret, no Caddy. The relay binary is plain HTTP on the WG interface only. Public internet sees one UDP port per VM. Authentication is WG public-key pinning — only the router's WG public key is accepted as a peer.

2. **In-cluster router for routing intelligence.** Workspace pods call a cluster-local Service, not an external hostname. The router handles weighted relay selection, 429 detection, drain/failover, and retry — all without DNS changes, pod restarts, or TTL waits.

3. **Destroy-and-recreate for all rotation.** No in-place key rotation, no IP swapping, no config pushes to running VMs. To rotate an IP, a WG key, or recover from failure: provision a new VM, verify healthy, add to router pool, destroy the old one. The other VM carries traffic during the ~60s window. Relay VMs are stateless — there is nothing to preserve.

4. **AWS-primary, OCI-secondary.** AWS (paid, ~$7/month t4g.micro) carries all traffic when healthy — it's the most reliable (no idle reclamation, no capacity errors, full EC2 API). OCI (free, 10 TB egress) is secondary — carries traffic during AWS rotation or failure. The router sends 100% of traffic to AWS when healthy; OCI receives traffic only during AWS failure or rotation.

5. **Free-tier where possible, paid where it matters.** OCI is free-tier (verified limits in Stated Assumptions). AWS is a small paid commitment (~$7/month) that eliminates the OCI idle-reclamation risk and capacity-availability problems that plague free-tier A1 shapes. GCP's Always Free tier has been eliminated (A12) — GCP is no longer in the default fleet. The architecture supports N providers; operators can add GCP as a paid option if they want a third IP source.

6. **Zero pod-side changes to the interface.** The workspace controller still injects a single `INFERENCE_RELAY_BASEURL` — it now points at the in-cluster router Service instead of an external hostname. Pods don't know about WireGuard, relay VMs, or routing logic.

---

## Architecture

### Component Overview

```
┌─ In-Cluster ──────────────────────────────────────────────────────┐
│                                                                    │
│  Workspace Pods                                                    │
│    └─ INFERENCE_RELAY_BASEURL = http://relay-router:8080           │
│                                                                    │
│  relay-router (Deployment, 1 replica, PDB minAvailable=1)          │
│    └─ Service: relay-router (ClusterIP)                            │
│    └─ Service: relay-wg (UDP 51820 — operator-selectable ingress mode)│
│    └─ WireGuard interface: wg0 (10.42.42.1)                        │
│    └─ Healthy relays list (computed from health checks)            │
│    └─ Relay routing: weighted selection (AWS primary, OCI→failover)│
│                                                                    │
│  InferenceRelay Controller (same binary as workspace controller)   │
│    └─ Watches InferenceRelay CR                                    │
│    └─ AWS driver: provisions/destroys EC2 t4g.micro VMs            │
│    └─ OCI driver: provisions/destroys OCI VMs                      │
│    └─ Generates WG keypairs, writes relay-router ConfigMap         │
│    └─ Reads relay health from router /metrics (not over WG)        │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
       │                    │
  WireGuard UDP 51820   WireGuard UDP 51820
       │                    │
┌──────┴──────────┐  ┌──────┴────────┐
│ AWS t4g.micro   │  │ OCI A1 VM     │
│ wg0:10.42.42.4  │  │ wg0:10.42.42.2│
│ relay-proxy:8080│  │ relay-proxy   │
│ (PAID primary)  │  │ (free 2nd)    │
└────────┬────────┘  └──────┬────────┘
         │                  │
         └──────── opencode.ai/zen/v1 ──┘
```

### Layer 1: Portable Relay Binary (`cmd/relay-proxy/`)

A standalone Go binary, no external dependencies beyond stdlib. ~40 lines of actual logic. No authentication — WireGuard is the auth. No TLS — WireGuard is the encryption. No path parsing — the router sends clean paths.

```
cmd/relay-proxy/
├── main.go          # HTTP server, env config, health + metrics endpoints
├── proxy.go         # Transparent forward to UPSTREAM_URL
├── proxy_test.go    # Unit tests
├── keepalive.go     # Periodic upstream probe to prevent OCI idle reclamation
└── README.md        # Deployment guide
```

**Endpoints:**
- `GET /healthz` → `200 OK` (no body) — for controller health checks over WG
- `GET /metrics` → Prometheus format — request counts by status code, keepalive counter, egress bytes total
- `* /*` → transparent proxy to `UPSTREAM_URL` (default `https://opencode.ai/zen/v1`), streams response back

**Environment:**
- `UPSTREAM_URL` (default: `https://opencode.ai/zen/v1`)
- `LISTEN_ADDR` (default: `10.42.42.2:8080` — WG interface only, not `0.0.0.0`)

**Build:**
```makefile
relay-bin:
	GOOS=linux GOARCH=arm64 go build -o deploy/relay-proxy-arm64 ./cmd/relay-proxy/
	GOOS=linux GOARCH=amd64 go build -o deploy/relay-proxy-amd64 ./cmd/relay-proxy/
```

### Layer 2: WireGuard Mesh

The security layer. Replaces Caddy, TLS certs, CA infrastructure, and path-secret auth with one UDP port per VM.

**Topology:**
```
Router (10.42.42.1) ←── WG tunnel ──→ AWS VM (10.42.42.4)
Router (10.42.42.1) ←── WG tunnel ──→ OCI VM (10.42.42.2)
```

Star topology — router is the hub, relay VMs are spokes. Relay VMs do not peer with each other (no relay-to-relay traffic needed).

Router is .1, OCI relay is .2, AWS relay is .4. (.3 reserved for optional GCP.)

**Key management:**
- Controller generates a WireGuard keypair per relay VM during provisioning
- Router has one static keypair (generated at controller startup, stored in a K8s Secret)
- Each relay VM's cloud-init embeds: its private key, router's public key, router's WG endpoint (cluster public IP or NAT-traversed endpoint)
- Router's WG config lists each relay VM's public key as a peer
- **Rotation = destroy VM + provision new one with fresh keypair** (see Design Principle 3)

**Router WireGuard sidecar:**
The relay-router Deployment runs two containers:
1. `wireguard` sidecar — manages the `wg0` interface, requires `NET_ADMIN` capability (same pattern as Epic 32's VPN sidecars)
2. `router` main container — the Go HTTP router, connects to relays via `10.42.42.x:8080`

This follows the established pattern in `design/stories/epic-32-vpn-network-iam/README.md` for WireGuard sidecars with `NET_ADMIN` + `NET_RAW` capabilities.

**WireGuard ingress — network-agnostic, operator-selectable:**
Relay VMs must reach the router's WG endpoint from outside the cluster. The chart MUST NOT depend on a specific load-balancer implementation: bare-metal Talos clusters typically lack a cloud LB, but operators may run on managed K8s (GKE/EKS/AKS), bare-metal with MetalLB or kube-vip, or behind their own DNAT. The chart ships **four operator-selectable ingress modes** (`external` default, `loadBalancer`, `nodePort`, `hostNetwork`). Mode is chosen via `controller.inferenceRelay.router.wireGuard.ingress.mode`; default is `external` so installs never break when no LB is present.

| Mode | What it does | When to use | Resilience | Operator burden |
|------|--------------|-------------|------------|-----------------|
| `external` (default) | Chart creates **no** ingress resources. Operator points DNS at whatever ingress they already run (cloud LB, hostNetwork pod, NAT rule, MetalLB Service applied out-of-band). The CRD's `spec.wireGuard.routerEndpoint` is the operator's source of truth. | Any cluster — universal escape hatch | Operator's choice | Highest (full DIY) |
| `loadBalancer` | Chart creates a `Service` of type `LoadBalancer` on UDP 51820, optionally pinned to `loadBalancerIP`. Works with **any** controller that satisfies LoadBalancer Services (cloud LB, MetalLB, kube-vip, Cilium L2, etc.) — the chart does **not** install or assume MetalLB. | Cloud K8s; bare-metal with an existing LB | Best (LB controller picks a healthy node) | Low |
| `nodePort` | Chart creates a `Service` of type `NodePort` on a pinned UDP port. Operator points DNS at one or more node IPs (or a static external LB they manage). | Bare-metal without an LB controller | Medium (NodePort is per-node; node failure breaks the tunnel until DNS / external LB re-points) | Medium |
| `hostNetwork` | Chart deploys the router as `hostNetwork: true` pinned to a labelled node. Operator labels the chosen node with `llmsafespaces.dev/relay-router=true` and points DNS at that node's IP. | Bare-metal where NodePort is undesirable and no LB exists | Lowest (single node) | Medium |

**Rules common to all modes:**
- The CRD `spec.wireGuard.routerEndpoint` is **always** the public `host:port` the relay VM dials. The chart never derives this — it's an operator declaration, validated only by reachability when relay VMs successfully tunnel back.
- The router **always** runs the WireGuard sidecar (`NET_ADMIN`, `NET_RAW`); the only thing that varies between modes is *how UDP 51820 reaches the pod*.
- `mode: external` produces no Service at all — operators wire ingress out-of-band. This guarantees `helm install` succeeds on any cluster, even ones with no LB controller, NodePort policy, or hostNetwork access.

**Example values per mode:**

```yaml
# Default — operator wires ingress themselves
controller:
  inferenceRelay:
    router:
      wireGuard:
        ingress:
          mode: external

# Cloud / MetalLB / kube-vip
controller:
  inferenceRelay:
    router:
      wireGuard:
        ingress:
          mode: loadBalancer
          loadBalancerIP: ""           # optional; empty lets the LB pool assign
          loadBalancerClass: ""        # optional; e.g. "metallb" if multiple LB classes
          annotations: {}              # cloud-specific (e.g. AWS NLB)

# Bare-metal NodePort
controller:
  inferenceRelay:
    router:
      wireGuard:
        ingress:
          mode: nodePort
          nodePort: 31820              # pinned for stable DNS

# hostNetwork on a labelled node
controller:
  inferenceRelay:
    router:
      wireGuard:
        ingress:
          mode: hostNetwork
          # operator must label the node:
          #   kubectl label node <name> llmsafespaces.dev/relay-router=true
```

**Why this redesign vs the original MetalLB plan:**

The original US-42.8 design (worklog 0262, original epic README) coupled the chart to MetalLB. Worklog 0294 already discovered the first symptom: the setup endpoint can't probe MetalLB without cross-namespace RBAC, so the MetalLB checklist gate was removed but the underlying assumption — that MetalLB exists — survived in the chart-template plan. That plan was never implemented (US-42.8 NOT DONE per worklog 0299), and any attempt to ship it would either:

1. Fail on managed-K8s clusters (cloud LB controllers don't recognise MetalLB conventions like `loadBalancerIP` from a MetalLB pool), or
2. Fail on bare-metal-without-MetalLB clusters (no LB controller → Service stays `<pending>`), or
3. Force the chart to bundle/install MetalLB — cluster-scoped infra that can conflict with operators' existing networking.

The 4-mode design is the smallest correct answer:

- **`external`** is the universal escape hatch — anyone can use the chart even on networks the chart's authors didn't anticipate.
- **`loadBalancer`** delegates to whatever LB controller is already running. The chart sets `type: LoadBalancer` and walks away. MetalLB users get a VIP; cloud K8s users get a cloud LB; kube-vip users get a kube-vip VIP. Same template.
- **`nodePort`** and **`hostNetwork`** are bare-metal-without-LB options. NodePort is the more common choice; hostNetwork is the "I really want a single fixed IP" option for clusters that NodePort doesn't fit.
- The chart **never** installs MetalLB or any other infra. That stays the operator's responsibility, exactly like Postgres and Redis (`charts/llmsafespace/values.yaml:288`).

**Why WireGuard over mTLS/TLS:**
- Eliminates CA, cert generation, cert rotation, Caddy, DNS-for-cert-validation
- WG public-key pinning is stronger auth than a bearer token or path secret
- Relay VMs expose zero attack surface to the public internet (one UDP port, WG rejects unauthenticated packets before any application logic runs)
- Simpler cloud-init: install `wireguard-tools`, write config file, `wg-quick up wg0`
- Key rotation is the same destroy-and-recreate flow as IP rotation — no separate mechanism

### Layer 3: In-Cluster Relay Router (`cmd/relay-router/`)

A Go HTTP server running as a Deployment (1 replica). This is the only endpoint workspace pods talk to.

**Why single replica:** WireGuard requires one interface (wg0) with one IP (10.42.42.1) and one keypair. Two replicas cannot share a WG IP or keypair — each pod has its own network namespace, so each would need a separate wg0, IP, and peer config on every relay VM. UDP load-balancers also route to one pod at a time in nearly all common implementations (MetalLB L2, NodePort, kube-vip), making any second replica's WG sidecar idle. The router is a lightweight Go binary that restarts in <1s; during restart, opencode's retry logic covers the gap. A `PodDisruptionBudget` (`minAvailable: 1`) prevents voluntary eviction during node drains. HA via a leader-elected WG gateway is a future concern if the single-replica restart gap proves problematic.

**Responsibilities:**

1. **Weighted relay selection:** The router selects a relay for each request using weighted random selection. AWS receives 100% of traffic when healthy. OCI receives traffic only when AWS is unhealthy, draining, or being rotated. This matches the reliability and cost reality: AWS (paid, most reliable) is primary; OCI (free, idle-reclamation risk) is secondary. Relays are stateless byte-pipes (no per-session state on the relay or upstream), so there is no state that stickiness would protect. Weighted random is the simplest correct solution.

2. **Health checking:** The router health-checks each relay every 15s via `GET http://10.42.42.x:8080/healthz` over the WG tunnel. A relay is marked unhealthy after 3 consecutive failures (45s). The router exposes relay health and per-relay egress bytes via its own `/metrics` endpoint, which the controller scrapes to determine fleet status (see Layer 4).

3. **Proactive 429 detection (two-tier):**
   - **Tier 1 — Immediate probe (first 429):** When the router receives the first 429 from a relay, it immediately sends a probe request (`GET /models`) to that relay. If the probe also returns 429, the relay is marked `suspect` and new sessions are weighted away from it (but not fully drained — could be transient).
   - **Tier 2 — Storm detection:** If a relay's 429 rate exceeds the threshold (default 50%) over a 5-minute window, OR if 3 consecutive probes return 429, the router marks the relay as `draining` — stops assigning new sessions entirely, writes a rotation request to the InferenceRelay CR status.
   - Existing in-flight streams on the draining relay are left to complete (or fail naturally if the IP is hard-blocked)
   - This prevents the scenario where dozens of users hit 429s before the 5-minute window elapses

4. **Failover:** When AWS transitions healthy → unhealthy, all traffic is routed to OCI (if healthy). In-flight streams to the failed relay break — opencode's retry logic handles this. If both are unhealthy, the router enters fallback mode (see below).

5. **Rebalancing:** When AWS rejoins (replacement VM provisioned and healthy), traffic returns to AWS. Existing sessions on OCI are NOT force-migrated — they complete naturally, and new requests go to AWS.

6. **Fallback mode (both relays down):** When no relays are healthy, the router enters fallback mode and proxies directly to `opencode.ai/zen/v1` from cluster IPs. To avoid worsening the IP throttle situation:
   - **Global rate limit of 1 req/2s** across all workspaces (token bucket, local to the router replica). Requests exceeding the rate receive `429 Too Many Requests` with `Retry-After: 2` — opencode's retry logic handles this gracefully.
   - **Concurrency cap of 1** — only one in-flight request to Zen at a time. Streaming responses hold the slot until complete. This makes fallback extremely slow but prevents IP escalation.
   - **`X-Relay-Status: fallback` header** on all responses so the frontend can display a degraded-mode banner.
   - **Queue depth limit of 0** — if a request arrives while another is in-flight, it's immediately rejected (no queueing). Users see a 429 and retry after a few seconds. Queueing would create artificial latency and memory pressure.
   - Fallback exits automatically as soon as any relay passes health check.
   - This is intentionally hostile UX — fallback is a last resort to keep *some* free-tier access alive while the controller reprovisions, not a sustainable operating mode.

**How the router learns relay IPs:**
The controller writes a ConfigMap (`relay-router-peers`) that the router mounts as a volume. The router re-reads the ConfigMap every 5s (simple poll). fsnotify is not used — K8s volume mounts use symlink swaps that fsnotify does not reliably detect. At 2 relays and a 5s poll interval, the cost is negligible.
```json
{
  "relays": [
    {"id": "oci-1", "wgIP": "10.42.42.2", "provider": "oci", "state": "healthy"},
    {"id": "gcp-1", "wgIP": "10.42.42.3", "provider": "gcp", "state": "healthy"}
  ]
}
```
The `state` field (`healthy`, `draining`, `unhealthy`) drives the router's routing decisions: `draining` stops new requests immediately. The router independently verifies health via its own health checks — it doesn't trust the ConfigMap's state blindly for the `healthy` determination.

**Workspace identification (optional, for metrics only):**
The router extracts the workspace ID from the `X-Workspace-ID` header if present. This is used only for per-workspace metrics and logging — not for routing (relays are stateless, so weighted random is sufficient).

The `@ai-sdk/openai-compatible` package (v2.0.50+) supports a `headers` field in the provider config (verified from npm docs). The relay injector's `options` struct (`cmd/workspace-agentd/relay_injector.go:136-138`) currently has only `{BaseURL, APIKey}` — adding a `Headers map[string]string` field is a small, localized change:

```go
type options struct {
    BaseURL string            `json:"baseURL"`
    APIKey  string            `json:"apiKey"`
    Headers map[string]string `json:"headers,omitempty"`
}
```

The relay injector sets `Headers: {"X-Workspace-ID": workspaceID}` when building the `opencode-relay` provider entry. The header is consumed by the router and stripped before forwarding to the relay VM.

**Relay-router as a reverse proxy:**
The router receives the full request from the pod, selects a relay via weighted random, rewrites the URL to `http://10.42.42.x:8080/<original-path>`, and streams the response back. It strips `X-Workspace-ID` before forwarding — that header is for the router's metrics only.

### Layer 4: InferenceRelay Controller

Runs as a new reconciler inside the existing workspace controller binary (gated by a feature flag). Watches a single cluster-scoped `InferenceRelay` CR.

**Lifecycle states:**

```
                          provision
    ┌──────────┐     ──────────────→     ┌──────────────┐
    │ Absent   │                          │ Provisioning │
    └──────────┘                          └──────┬───────┘
                                                  │ health check passes
                                                  ▼
    ┌──────────┐     destroy +       ┌──────────────┐
    │ Draining │←─── reprovision ────│  Healthy     │
    └────┬─────┘     (on 429/fail)   └──────┬───────┘
         │                                    │ health check fails (3x)
         │                                    ▼
         │                              ┌──────────────┐
         │                              │ Unhealthy    │
         │                              └──────┬───────┘
         │                                     │ stays unhealthy >15m
         │                                     ▼
         │                              destroy + reprovision
         │
         │  3 consecutive provisioning failures
         ▼
    ┌──────────────────┐
    │ ProvisioningFailed│  ← circuit breaker: stop retrying, set condition,
    └────────┬─────────┘    fire alert, wait for operator intervention
             │ operator fixes template/credentials, deletes condition
             ▼
         reprovision
```

**Provisioning failure circuit breaker:**
If a provider slot fails to reach healthy state after 3 consecutive provisioning attempts, the controller distinguishes between two error classes:

- **Capacity errors** (OCI "out of host capacity", transient API throttling): These are NOT counted toward the circuit breaker. The controller retries with exponential backoff (30s, 60s, 120s, ... up to 10m). Capacity errors are expected on OCI Always Free A1 shapes (A5).
- **Configuration errors** (invalid credentials, bad cloud-init template, image not found, quota exceeded): These ARE counted toward the circuit breaker. After 3 consecutive config-error provisioning attempts, the controller:
  1. Stops the destroy/provision loop for that slot
  2. Sets a `ProvisioningFailed` condition on the InferenceRelay CR with details (last error, attempt count, provider)
  3. Fires a Prometheus alert (`llmsafespace_relay_provisioning_failed`)
  4. The surviving relay carries all traffic (via router failover)
  5. The controller does NOT retry until the operator clears the condition (indicating the root cause has been fixed)

This prevents infinite provisioning loops from burning cloud API quotas while allowing transient capacity issues to self-resolve.

**Reconcile loop:**
1. Read `InferenceRelay` CR spec
2. Scrape `relay-router` `/metrics` endpoint to get per-relay health status (`relay_router_relay_healthy`), in-flight stream counts (`relay_router_active_streams`), and per-relay egress bytes (`relay_router_relay_egress_bytes`). The controller does NOT health-check relays over WG directly — it is not in the WG mesh. The router is the sole component with WG access.
3. For each provider in `spec.providers` (always OCI + GCP):
   a. Check if a relay VM exists for this provider
   b. If not, provision one (generate WG keypair, render cloud-init, call provider API). Classify the result: capacity error → retry with backoff (not counted); config error → increment provisioning attempt counter.
   c. Read relay health from the scraped router metrics. If router reports the relay as unhealthy for >15m, drain and reprovision (see step e for drain flow).
   d. **Egress quota check:** Compare per-relay egress bytes against provider quota (OCI: 10 TB/mo, GCP: 1 GB/mo). If GCP egress exceeds ~900 MB (90% of 1 GB), mark the relay `quota-exhausted`, set `quota-exhausted` in the ConfigMap so the router deprioritizes it, and set a CR condition. Do not destroy — the quota resets monthly at the billing boundary.
   e. **Graceful drain + destroy flow** (triggered by 429 rotation, unhealthy >15m, or manual):
      1. Controller writes `"state": "draining"` for the relay in the `relay-router-peers` ConfigMap
      2. Router polls ConfigMap within 5s, stops routing new requests to the draining relay
      3. Controller waits for `relay_router_active_streams{relay=<id>}` to reach 0 (polled from router /metrics, timeout 60s)
      4. Controller destroys the VM via cloud API
      5. Controller provisions a replacement VM
      6. On replacement healthy, controller updates ConfigMap with new relay IP + `"state": "healthy"`
   f. **If 3 consecutive config-error provisioning attempts fail, set `ProvisioningFailed` condition and stop retrying** (circuit breaker)
4. Update ConfigMap `relay-router-peers` with current relay IPs, states, and health status
5. Update CR status with observed state, conditions, and metrics

### Layer 5: InferenceRelay CRD

A cluster-scoped CRD. Singleton — only one instance expected per cluster.

```go
// InferenceRelay represents the managed relay VM fleet. The controller
// provisions, health-checks, and replaces relay VMs on OCI and GCP to
// maintain free-tier inference availability. Workspace pods route through
// the in-cluster relay-router, which distributes traffic across healthy
// relay VMs via WireGuard tunnels.
type InferenceRelay struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   InferenceRelaySpec   `json:"spec,omitempty"`
    Status InferenceRelayStatus `json:"status,omitempty"`
}

type InferenceRelaySpec struct {
    // UpstreamURL is the LLM provider endpoint the relays proxy to.
    // +kubebuilder:default="https://opencode.ai/zen/v1"
    UpstreamURL string `json:"upstreamURL"`

    // Providers configures the relay VMs. Must include exactly one OCI
    // and one GCP provider for the intended 2-VM fleet.
    // +kubebuilder:validation:MinItems=1
    Providers []RelayProviderSpec `json:"providers"`

    // WireGuard configures the mesh between router and relay VMs.
    WireGuard WireGuardConfig `json:"wireGuard,omitempty"`

    // HealthCheck configures active health-checking of relay VMs.
    HealthCheck HealthCheckConfig `json:"healthCheck,omitempty"`

    // Rotation configures automatic destroy-and-recreate on 429 detection.
    Rotation RotationConfig `json:"rotation,omitempty"`

    // Fallback configures direct-to-upstream routing when all relay VMs
    // are unhealthy. Rate-limited to avoid worsening IP throttling.
    Fallback FallbackConfig `json:"fallback,omitempty"`
}

type FallbackConfig struct {
    // Enabled enables direct fallback when all relays are down.
    // If false, the router returns 502 to all requests when no relays are healthy.
    // +kubebuilder:default=true
    Enabled bool `json:"enabled"`

    // Rate is the maximum request rate to the upstream in fallback mode
    // (requests per second, global across all workspaces).
    // Default: 0.5 (1 request per 2 seconds).
    // +kubebuilder:default=0.5
    Rate float64 `json:"rate,omitempty"`

    // MaxConcurrent is the maximum in-flight requests to the upstream
    // in fallback mode. Default: 1.
    // +kubebuilder:default=1
    MaxConcurrent int `json:"maxConcurrent,omitempty"`
}

type RelayProviderSpec struct {
    // Provider is the cloud provider name.
    // +kubebuilder:validation:Enum=aws;oci;gcp
    Provider string `json:"provider"`

    // Region is the provider region (e.g. "us-east-1", "us-ashburn-1", "us-central1-a").
    // AWS: any region (t4g.micro available globally).
    // OCI: must be the tenancy home region for Always Free eligibility.
    // GCP: must be us-west1, us-central1, or us-east1 for Always Free eligibility.
    Region string `json:"region"`

    // CredentialsRef references a K8s Secret containing provider credentials.
    // Must be in the controller's namespace. The validating webhook checks
    // that the Secret exists and contains the required keys:
    //   aws: access-key-id, secret-access-key (or role-arn for IRSA)
    //   oci: tenancy, user, fingerprint, key, region
    //   gcp: service-account-json
    // +kubebuilder:validation:MinLength=1
    CredentialsRef corev1.LocalObjectReference `json:"credentialsRef"`

    // Shape overrides the default shape.
    //   aws default: t4g.micro (2 vCPU Graviton2, 1 GB, Arm64 — paid ~$7/mo)
    //   oci default: VM.Standard.A1.Flex (2 OCPU, 12 GB, Arm)
    //   gcp default: e2-micro (0.25 shared vCPU, 1 GB)
    // +optional
    Shape string `json:"shape,omitempty"`
}

type WireGuardConfig struct {
    // RouterPrivateKeyRef references a K8s Secret containing the router's
    // WG private key. Auto-generated by the controller if not set.
    // +optional
    RouterPrivateKeyRef string `json:"routerPrivateKeyRef,omitempty"`

    // CIDR is the WireGuard mesh network. Default: 10.42.42.0/24.
    // Router is .1, OCI relay is .2, GCP relay is .3, AWS relay is .4.
    // +kubebuilder:default="10.42.42.0/24"
    CIDR string `json:"cidr,omitempty"`

    // Port is the WireGuard UDP port. Default: 51820.
    // +kubebuilder:default=51820
    Port int `json:"port,omitempty"`

    // RouterEndpoint is the routable address relay VMs connect back to.
    // Must be a DNS name (e.g. "relay-gw.safespaces.dev"), not a bare IP.
    // Using a DNS name allows the operator to re-point relay VMs to a new
    // cluster node via DNS update (5m TTL) without destroying/recreating VMs.
    // For clusters behind NAT, this resolves to the public IP of the
    // node port / load balancer exposing the router's WG port.
    RouterEndpoint string `json:"routerEndpoint"`
}

type HealthCheckConfig struct {
    // Interval between health checks per relay VM.
    // +kubebuilder:default="15s"
    Interval metav1.Duration `json:"interval,omitempty"`

    // Health check request timeout.
    // +kubebuilder:default="5s"
    Timeout metav1.Duration `json:"timeout,omitempty"`

    // Consecutive failures before marking unhealthy.
    // +kubebuilder:default=3
    UnhealthyThreshold int `json:"unhealthyThreshold,omitempty"`

    // Time to stay unhealthy before destroy + reprovision.
    // +kubebuilder:default="15m"
    ReplacementTimeout metav1.Duration `json:"replacementTimeout,omitempty"`
}

type RotationConfig struct {
    // Enabled enables destroy-and-recreate when the router detects 429 storms.
    // +kubebuilder:default=true
    Enabled bool `json:"enabled"`

    // Max429Rate is the 429 fraction (of total responses) that triggers rotation.
    // +kubebuilder:default=0.5
    Max429Rate float64 `json:"max429Rate,omitempty"`

    // DetectionWindow is the rolling window for counting 429s.
    // +kubebuilder:default="5m"
    DetectionWindow metav1.Duration `json:"detectionWindow,omitempty"`

    // Cooldown is the minimum time between rotations on the same provider slot.
    // +kubebuilder:default=30m
    Cooldown metav1.Duration `json:"cooldown,omitempty"`
}

type InferenceRelayStatus struct {
    // Instances is the observed state of all managed relay VMs.
    Instances []RelayInstanceStatus `json:"instances,omitempty"`

    // HealthyReplicas is the count of instances currently passing health checks.
    HealthyReplicas int `json:"healthyReplicas"`

    // Conditions reflects the overall relay fleet health.
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // LastRotation is the time of the most recent destroy-and-recreate.
    LastRotation *metav1.Time `json:"lastRotation,omitempty"`
}

type RelayInstanceStatus struct {
    ID         string       `json:"id"`
    Provider   string       `json:"provider"`
    Region     string       `json:"region"`
    WgIP       string       `json:"wgIP"`
    PublicIP   string       `json:"publicIP"`
    State      string       `json:"state"` // "provisioning", "healthy", "draining", "unhealthy", "quota-exhausted", "terminated", "provisioning-failed"
    Healthy    bool         `json:"healthy"`
    LastCheck  *metav1.Time `json:"lastCheck,omitempty"`
    Requests429 int         `json:"429Count,omitempty"`
    TotalRequests int       `json:"totalRequests,omitempty"`
    // EgressBytes is the cumulative outbound bytes proxied by this relay,
    // scraped from the router's /metrics (relay_router_relay_egress_bytes).
    // Used for GCP quota tracking (1 GB/mo limit).
    EgressBytes int64       `json:"egressBytes,omitempty"`
    // ProvisioningAttempts is the count of consecutive config-error provisioning
    // attempts for this provider slot. Capacity errors (out-of-capacity,
    // throttling) do NOT increment this counter — they retry with backoff.
    // Reset to 0 on success. When it reaches 3, the circuit breaker trips
    // and sets state to "provisioning-failed".
    ProvisioningAttempts int `json:"provisioningAttempts,omitempty"`
    // LastProvisionError is the error message from the most recent failed
    // provisioning attempt. Populated when ProvisioningAttempts > 0.
    LastProvisionError string `json:"lastProvisionError,omitempty"`
}
```

### Layer 6: Cloud Provider Drivers (`controller/internal/relay/`)

Each provider driver implements:

```go
type ProviderDriver interface {
    // Provision creates a relay VM with the given cloud-init userdata
    // and returns the instance ID and public IP.
    Provision(ctx context.Context, name, region, shape, cloudInitData string) (*RelayInstance, error)

    // Destroy terminates a relay VM.
    Destroy(ctx context.Context, instanceID, region string) error

    // GetStatus returns the current VM state.
    GetStatus(ctx context.Context, instanceID, region string) (*RelayStatus, error)

    // ListInstances returns relay VMs managed by this driver.
    ListInstances(ctx context.Context, region string) ([]RelayInstance, error)
}
```

Note: **no `RotateIP` method** — rotation is destroy + provision, not in-place IP swap. This keeps drivers simple (3 methods instead of 4) and matches the destroy-and-recreate principle.

**Drivers:**
```
controller/internal/relay/
├── driver.go           # ProviderDriver interface
├── aws_driver.go       # AWS driver (primary — paid t4g.micro, most reliable)
├── oci_driver.go       # OCI driver (secondary — free, 10 TB egress, A1 Arm)
├── gcp_driver.go       # GCP driver (optional — paid, operator can add for IP diversity)
├── cloudinit.go        # Renders cloud-init templates (WG + relay binary + keepalive)
├── wireguard.go        # Keypair generation, config rendering
├── health.go           # Health-checker (GET /healthz over WG)
├── reconciler.go       # InferenceRelay CRD reconciler
└── router_configmap.go # Writes relay-router-peers ConfigMap
```

### Layer 7: Cloud-Init Template

Shared across providers. Renders a single `user-data` script that:

1. Downloads the relay binary from the artifact location (GitHub Release / OCI artifact) with SHA-256 integrity verification
2. Creates the WireGuard interface with the embedded private key and router peer
3. Writes the relay binary's systemd unit (binds to WG IP only)
4. Starts the relay proxy
5. Configures the keepalive daemon (upstream probe every 30s — prevents OCI idle reclamation)
6. Configures UFW: allow SSH (WG-only or disabled), allow UDP 51820 (WG), deny everything else
7. Enables unattended-upgrades

**Implementation** (`controller/internal/relay/cloudinit.go`): the cloud-init is a
`#cloud-config` document (not a bare bash script). `RenderCloudInit` takes a
`CloudInitConfig` struct with all fields required and validated:

| Field | Purpose |
|---|---|
| `WgConfig` | Rendered `wg0.conf` content (private key + router peer) |
| `WgIP` | This relay's WG IP → rendered as `--listen={{WgIP}}:8080` |
| `UpstreamURL` | Zen default → rendered as `--upstream=` |
| `RouterEndpoint` | WG endpoint the relay dials back to |
| `ArtifactURLs` | Base mirror URLs; cloud-init appends `/relay-proxy-<arch>` and tries each |
| `ArtifactSHA256` | Hex SHA-256 of the binary; cloud-init verifies before exec (Security §7) |
| `BinaryName` | Arch-resolved artifact name (`relay-proxy-arm64` / `relay-proxy-amd64`) |

The controller resolves architecture via `archForShape(shape, provider)` (AWS
Graviton `t4g`/`c7g`/`m6g` → arm64; OCI Ampere `A1`/`E1` → arm64; GCP → amd64;
unknown defaults arm64) and selects the matching checksum from the per-arch
flags (`--relay-artifact-sha256-arm64`, `--relay-artifact-sha256-amd64`).

The download is a `runcmd` step that runs BEFORE `systemctl start relay-proxy`:

```bash
sh -c 'set -e; bin=/usr/local/bin/relay-proxy; ok=0;
  for base in <mirror1> <mirror2>; do
    if curl -fsSL --connect-timeout 10 "$base/relay-proxy-arm64" -o "$bin"; then
      echo "<sha256>  $bin" | sha256sum -c - && chmod +x "$bin" && ok=1 && break
    fi
  done
  if [ "$ok" != "1" ]; then echo "FATAL: could not download/verify relay-proxy" >&2; exit 1; fi'
```

The controller flag `--relay-artifact-url` (comma-separated) and the two
`--relay-artifact-sha256-*` flags are set by the Helm chart from
`controller.inferenceRelay.artifact.{urls,sha256Arm64,sha256Amd64}`. Operators
build the binaries (`make relay-bin`), publish to one or more mirrors (GitHub
Release is the default), and set the checksums.

```bash
#!/bin/bash
set -euo pipefail

# Download relay binary with SHA-256 integrity verification.
# The controller embeds RELAY_BINARY_SHA256 (per-arch) into cloud-init at
# render time, sourced from the GitHub Release checksums file.
ARCH=$(uname -m)
case "$ARCH" in
  aarch64) BINARY=relay-proxy-arm64 ;;
  x86_64)  BINARY=relay-proxy-amd64 ;;
esac
download_binary() {
  for url in \
    "https://github.com/lenaxia/llmsafespace/releases/latest/download/$BINARY" \
    "https://storage.googleapis.com/llmsafespace-artifacts/$BINARY" \
    "https://objectstorage.us-ashburn-1.oraclecloud.com/n/llmsafespace/b/artifacts/o/$BINARY" \
    "https://s3.amazonaws.com/llmsafespace-artifacts/$BINARY"; do
    if curl -fsSL --connect-timeout 10 "$url" -o /usr/local/bin/relay-proxy; then
      echo "${RELAY_BINARY_SHA256}  /usr/local/bin/relay-proxy" | sha256sum -c - || return 1
      chmod +x /usr/local/bin/relay-proxy
      return 0
    fi
  done
  return 1
}
download_binary || { echo "FATAL: could not download/verify relay binary from any source"; exit 1; }

# Configure WireGuard
apt-get update && apt-get install -y wireguard-tools
mkdir -p /etc/wireguard
cat > /etc/wireguard/wg0.conf <<WGEOF
[Interface]
PrivateKey = ${RELAY_WG_PRIVATE_KEY}
Address = ${RELAY_WG_IP}/24

[Peer]
PublicKey = ${ROUTER_WG_PUBLIC_KEY}
Endpoint = ${ROUTER_WG_ENDPOINT}
AllowedIPs = 10.42.42.0/24
PersistentKeepalive = 25
WGEOF
wg-quick up wg0

# Relay proxy systemd service (WG interface only)
cat > /etc/systemd/system/relay-proxy.service <<SVCEOF
[Unit]
Description=LLMSafeSpace Inference Relay Proxy
After=network-online.target wg-quick@wg0.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/relay-proxy
Environment=UPSTREAM_URL=https://opencode.ai/zen/v1
Environment=LISTEN_ADDR=${RELAY_WG_IP}:8080
Restart=always
RestartSec=5
User=nobody

[Install]
WantedBy=multi-user.target
SVCEOF
systemctl enable --now relay-proxy

# Keepalive: probe upstream every 30s to keep network util above OCI's 20% threshold
cat > /etc/cron.d/relay-keepalive <<CRONEOF
* * * * * nobody curl -sf -o /dev/null http://${RELAY_WG_IP}:8080/healthz
CRONEOF

# Firewall
apt-get install -y ufw
ufw default deny incoming
ufw default allow outgoing
ufw allow 51820/udp
ufw --force enable

# Unattended upgrades
apt-get install -y unattended-upgrades
dpkg-reconfigure -f noninteractive unattended-upgrades
```

---

## Stated Assumptions

All assumptions below were validated against provider documentation and technical sources on 2026-06-13. Items marked ⚠️ require live testing before implementation.

**A0 — Throttle is per-IP (Cloudflare egress ranges), not per-key.** This is the epic's foundational assumption. Validated by the project owner: the same `public` API key works without issue from a residential IP. Zen blocks Cloudflare's egress IP ranges, not the anonymous key. (Worklog `0184` originally concluded the throttle was per-key — that conclusion was incorrect and has been corrected in-place.)

| # | Assumption | Status | Source / Verification |
|---|-----------|--------|----------------------|
| A0 | Zen throttles by source IP (Cloudflare egress ranges), not by API key | ✅ Validated | Project owner confirmed: same `public` key works from residential IP. CF IPs are blocked. Worklog `0184` corrected. |
| A1 | OCI Always Free is for the life of the account, no expiration | ✅ Verified | OCI docs: "free of charge in the home region of the tenancy, for the life of the account" |
| A2 | OCI A1 shape (VM.Standard.A1.Flex) provides 2 OCPU / 12 GB free | ✅ Verified | OCI Always Free docs: "equivalent to 2 OCPUs and 12 GB of memory" |
| A3 | OCI includes 10 TB/month outbound data transfer free | ✅ Verified | OCI Always Free docs: "you get 10 TB per month of outbound data" |
| A4 | OCI Always Free resources must be created in the home region only | ✅ Verified | OCI docs: "You must create the Always Free compute instances in your home region" |
| A5 | OCI A1 instances suffer "out of host capacity" errors requiring retries | ✅ Verified | OCI docs explicitly mention this: "If you receive an 'out of host capacity' error..." |
| A6 | OCI will reclaim idle Always Free compute instances (CPU/network/memory <20% for 7 days) | ✅ Verified — **CRITICAL DESIGN RISK** | OCI docs: "Idle Always Free compute instances may be reclaimed... if, during a 7-day period, CPU utilization for the 95th percentile is less than 20%, Network utilization is less than 20%, Memory utilization is less than 20% (applies to A1 shapes only)" |
| A7 | OCI supports ephemeral and reserved public IPs; ephemeral IPs can be released to get a new IP | ✅ Verified | OCI Networking Overview: "There are two types of public IPs: ephemeral and reserved." |
| A8 | OCI free-tier limit on number of ephemeral/reserved public IPs | ⚠️ Empirical | OCI docs do not publish a hard limit for Always Free tenancies. IP limits are visible in Console under Governance > Limits, Quotas and Usage. Must verify empirically during US-42.5. Destroy-and-recreate allocates a new ephemeral IP each time — if the limit is 2 (common default), rotation is constrained. |
| A9 | OCI supports cloud-init / user-data on Linux images (Oracle Linux, Ubuntu) | ✅ Verified | OCI "Creating an Instance" docs: "Initialization script: User data can be used by cloud-init to run custom scripts or provide custom cloud-init configuration." Always Free eligible images include Ubuntu and Oracle Linux, both of which ship cloud-init. Max userdata size: 32,000 bytes. |
| A10 | AWS EC2 t4g.micro (Graviton2) costs ~$6.80/month on-demand (Linux, us-east-1) | ✅ Verified | AWS EC2 pricing: t4g.micro = $0.0084/hr × 730hr = $6.13/mo (us-east-1). Add EBS gp3 8GB ($0.64/mo). Total ~$6.77/mo. No idle reclamation. Full EC2 API for programmatic lifecycle. Egress: 100 GB/month free, then $0.09/GB — but actual relay egress is <1 GB/month (response bodies only, typically 5-50 KB each). |
| A10b | AWS EC2 t4g.micro specs: 2 vCPU Graviton2 (arm64), 1 GB memory | ✅ Verified | AWS instance types docs: "t4g.micro: 2 vCPU, 1 GiB RAM, up to 5 Gbps network." Burstable (Baseline 20%, burst to 100%). Arm64 — relay binary already cross-compiles for arm64. |
| A10c | AWS EC2 supports cloud-init / user-data on Ubuntu and Debian AMIs | ✅ Verified | AWS EC2 user guide: "User data" shell scripts and cloud-init directives are supported on all Amazon Linux and Ubuntu AMIs. Max userdata size: 16 KB. |
| A10d | AWS EC2 data transfer (egress) pricing for a relay workload | ✅ Verified | AWS data transfer pricing: 100 GB/month free (aggregated across all AWS services). Then $0.09/GB (first 10 TB tier). At realistic relay scale (thousands of responses/day, ~20 KB each = <1 GB/month), egress stays in the free tier. Even at extreme scale (100K responses/day), egress is ~60 GB/month — still free. |
| A10e | AWS EC2 full programmatic lifecycle via EC2 API (RunInstances, TerminateInstances) | ✅ Verified | AWS EC2 API Reference: RunInstances, TerminateInstances, DescribeInstances — all support programmatic VM lifecycle with IAM authentication. No capacity throttling (unlike OCI A1). No idle reclamation. Destroy-and-recreate is a single API call. |
| A11 | GCP Always Free e2-micro tier has been eliminated | ✅ Verified | GCP removed the Always Free unlimited tier. The free page lists "1 e2-micro instance per month" but the terms now state "subject to change" and the Always Free guarantee no longer applies. GCP is **excluded from the default fleet** — operators can add GCP as a paid provider via the CR spec if they want a third IP source. |
| A12 | ~~GCP e2-micro includes 1 GB/month outbound data transfer (free)~~ | ❌ N/A | GCP Always Free eliminated (A11). GCP e2-micro is now a paid instance (~$7/month on-demand) with standard egress pricing ($0.085/GB after 200 GB/month free). Not cost-competitive with AWS for relay use. |
| A13 | GCP Free Tier has no end date but can be changed with 30 days notice | ✅ Verified | GCP docs: "Google reserves the right to change the offering, including changing or eliminating usage limits, with 30 days' advance notice." |
| A14 | GCP Free Tier requires an active billing account (Paid or Free Trial) | ✅ Verified | GCP docs: "A Google Cloud billing account is required to access the Google Cloud Free Tier." |
| A15 | GCP e2-micro specs: 2 vCPUs (0.25 fractional/shared-core), 1 GB memory, burstable | ✅ Verified | GCP machine types docs: "e2-micro: 2 vCPUs, 0.25 fractional vCPU, 1 GB memory." Shared-core: "sustains 2 vCPUs, each for 12.5% of CPU time totaling 25% CPU time." Burstable to 100% per vCPU for up to 30 seconds. Max egress: 1 Gbps. |
| A16 | GCP supports startup scripts (equivalent of cloud-init) on VM creation | ✅ Verified | GCP Compute Engine docs reference startup scripts as a standard feature. |
| A17 | OCI A1 shape network bandwidth scales with OCPUs | ✅ Verified | OCI docs: "The network bandwidth and number of VNICs scale proportionately with the number of OCPUs." |
| A18 | OCI E2.1.Micro shape has 50 Mbps bandwidth to internet | ✅ Verified | OCI docs: "up to 50 Mbps network bandwidth via the internet" |
| A19 | WireGuard is available in standard Linux kernels ≥5.6 (no DKMS needed) | ✅ Verified | WireGuard was merged into the Linux kernel in 5.6 (2020-03). OCI Oracle Linux 8/9 and GCP Ubuntu 20.04+ images ship kernels ≥5.6. |
| A20 | WireGuard UDP hole-punching works through cloud NAT with PersistentKeepalive | ✅ Verified | wireguard.com quickstart: "A sensible interval that works with a wide variety of firewalls is 25 seconds." `PersistentKeepalive = 25` is the documented standard NAT-traversal mechanism. Per-provider NAT behavior still needs live verification during US-42.5/42.6. |
| A21 | Cluster can expose a reachable UDP endpoint for WG via at least one of: cloud LB, MetalLB, kube-vip, NodePort, hostNetwork, or operator-supplied DNAT | ✅ Verified (multi-mode) | The chart ships four operator-selectable ingress modes (`external`, `loadBalancer`, `nodePort`, `hostNetwork`); see Layer 2 redesign. The chart does **not** install MetalLB or any other LB controller — that's an operator responsibility (same model as Postgres/Redis). Default mode is `external`: the chart creates no ingress resources, the operator points DNS at whatever they already run. This guarantees `helm install` works on any K8s distribution. |
| A22 | OCI and AWS IPs are not blocked by opencode.ai/zen | ✅ Validated (AWS, /models) / ⚠️ OCI pending | **AWS IP-reachability validated (worklog 0410):** deployed a t4g.micro (us-east-1b, public IP 34.205.17.204), `curl https://opencode.ai/zen/v1/models` returned **HTTP 200** with a 4020-byte model list in 0.12s — the AWS IP reaches Zen and gets application-layer responses (not IP-blocked). **A 2026-06-19 follow-up (worklog 0420) claimed a deeper blocker (A23: `public` 401s on inference from the same IP); that claim is SUPERSEDED 2026-06-20 — A23 was disproven, see A23 row.** Per-IP reachability + per-model `allowAnonymous` together mean the relay path works from AWS for any model Zen flags `allowAnonymous`. **OCI still pending** operator action. |
| A23 | The `public` anonymous key authorizes inference on opencode.ai/zen | ⚠️ **PER-MODEL (2026-06-20 correction)** — original "DISPROVED" finding was itself wrong | **Original 2026-06-19 finding (worklog 0420) is SUPERSEDED — see correction block in that worklog.** The 2026-06-19 probe sampled only models without `allowAnonymous` (`claude-fable-5`, `claude-sonnet-4`, `gemini-3.5-flash`, `claude-haiku-4-5`, `claude-opus-4-8`) and wrongly generalized the resulting 401s to "`public` is dead everywhere." **2026-06-20 re-probe (residential IP `24.18.52.209`):** `POST /v1/chat/completions` model=`big-pickle` + `Authorization: Bearer public` → **HTTP 200**, real completion (`model: deepseek-v4-flash`, 111 tokens). Same key + same IP + `claude-fable-5` → **HTTP 401**. **Mechanism (opencode `handler.ts:599-603` + `model.ts:26`):** `public`→`undefined`; `authenticate()` returns OK iff `modelInfo.allowAnonymous` (a per-model flag in ZenData, loaded from deploy-time SST secrets — not in the repo, so not visible to the 06-19 probe). Inference authorization is **per-model**, not per-key, not per-IP. **Implications:** (1) A0 (per-IP throttling for anonymous free-tier traffic) is the relay's valid foundational premise, restored. (2) #297 (router auth injection) and #298 (default→thekao) remain valid *mechanisms* but their rationale ("public 401s everywhere so we must inject a real key") is unfounded — keep, revert, or repurpose is an **open operator decision**. (3) Relay scope unchanged: free-model traffic only; paid goes direct. (4) Free-tier `claude-*` still 401 via `public` today, but that is a Zen per-model config choice that could change without notice — the relay architecture must not assume either way. |

---

## Design Questions

| # | Question | Answer | Rationale |
|---|----------|--------|-----------|
| DQ1 | How do we prevent OCI from reclaiming idle relay VMs? | **Keepalive daemon.** Cloud-init installs a cron job that curls `localhost:8080/healthz` every minute. The relay binary also runs a goroutine that probes the upstream (`GET opencode.ai/zen/v1/models`) every 30s. Both contribute to network utilization. The Go runtime's memory footprint (>2 GB on a 12 GB VM) keeps memory above 20%. | OCI reclaims Always Free instances with <20% CPU/network/memory utilization over 7 days (A6). The keepalive ensures network + CPU stay measurable. Requires 7-day empirical validation. |
| DQ2 | How does the router expose its WireGuard port to relay VMs? | **Network-agnostic, four operator-selectable ingress modes:** `external` (default; chart creates no ingress, operator wires it out-of-band), `loadBalancer` (chart creates a LoadBalancer Service — works with any LB controller including cloud LBs, MetalLB, kube-vip, Cilium L2), `nodePort` (chart creates a NodePort Service on a pinned UDP port), `hostNetwork` (chart pins the router to a labelled node with `hostNetwork: true`). The chart NEVER installs MetalLB or any other LB controller. See Layer 2 redesign. The CRD's `spec.wireGuard.routerEndpoint` is always the operator's authoritative `host:port`. | Earlier plan coupled the chart to MetalLB, which would fail on managed K8s, on bare-metal-without-MetalLB, or force the chart to install cluster-scoped infra. The 4-mode design lets the chart work on any K8s distribution while still offering a single-document quick path for clusters that have an LB controller. |
| DQ3 | How does the router identify which workspace a request belongs to? | **`X-Workspace-ID` header** via `@ai-sdk/openai-compatible` `headers` field. Verified from npm docs (v2.0.50+): the provider config supports `headers: { ... }`. The relay injector adds a `Headers` field to the `options` struct (currently only `{BaseURL, APIKey}` at `relay_injector.go:136-138`). Used for per-workspace metrics only — not for routing (relays are stateless). | The router can use the workspace ID for metrics/logging. Not needed for routing since relays are stateless byte-pipes. |
| DQ4 | What happens when both relays are unhealthy? | **Rate-limited direct fallback.** The router proxies directly to `opencode.ai/zen/v1` (server IPs) at a global rate of 1 req/2s with max 1 concurrent request. Requests exceeding the rate get `429 + Retry-After: 2`. Returns `X-Relay-Status: fallback` header so the frontend can display a warning. Better than a hard 502, and the rate limit prevents escalating IP throttling. | Unthrottled fallback would just get 429'd instantly and risk worsening the block. 1 req/2s keeps *some* free-tier access alive (slowly) while the controller reprovisions. Intentionally hostile UX — fallback is not a sustainable mode. |
| DQ5 | Destroy-and-recreate vs in-place rotation? | **Always destroy-and-recreate.** No in-place IP swapping, key rotation, or config pushing. Relay VMs are stateless. The other VM carries traffic during the ~60s provisioning window. | Simpler driver interface (no RotateIP), simpler cloud-init (no runtime reconfiguration), identical flow for failure recovery and key/IP rotation. |
| DQ6 | Should the controller run inside the existing workspace controller binary or as a separate deployment? | **Same binary, new reconciler, gated by a feature flag.** | The relay controller and workspace controller are coupled (router URL injection). Same binary simplifies deployment and avoids a second controller pod. |
| DQ7 | Should we weight traffic toward one provider? | **AWS gets 100% when healthy; OCI is failover.** AWS is paid (~$7/mo) and most reliable — no idle reclamation, no capacity issues, full API. OCI is free but has idle-reclamation risk (A6) and A1 capacity errors (A5). | AWS reliability justifies the cost. The paid commitment eliminates the OCI reclamation design risk entirely — if OCI gets reclaimed, AWS carries traffic with zero downtime. |
| DQ8 | ~~What happens when GCP egress quota (1 GB/mo) is exhausted?~~ | ❌ N/A — GCP removed from default fleet | GCP Always Free eliminated (A11). No GCP egress quota to track. If an operator adds GCP as a paid provider, standard egress pricing applies and the controller's egress tracking still works. |

---

## OCI Idle Reclamation Mitigation

OCI will reclaim Always Free instances where CPU utilization (95th percentile), network utilization, and memory utilization are all below 20% for a 7-day window (A6). This is a first-class design risk for relay VMs.

**Mitigation (built into cloud-init, Layer 7):**

1. **Network keepalive:** Cron job runs `curl -sf -o /dev/null http://<wg-ip>:8080/healthz` every minute. Generates consistent small network I/O.
2. **Upstream probe:** Relay binary goroutine performs `GET opencode.ai/zen/v1/models` every 30s. Keeps network utilization measurable and serves as an active upstream-health probe.
3. **Memory:** Go runtime + relay buffers naturally use >2 GB on a 12 GB VM (>20%).
4. **CPU:** The network I/O from keepalive + probe generates CPU work. A lightweight busy-loop goroutine (1% CPU for 1s every 10s) provides additional floor.

**Hard gate — 7-day empirical validation (blocks US-42.5):** OCI's reclamation policy uses 95th-percentile CPU over a 7-day window (A6). The mitigation must be empirically validated: deploy the relay VM with keepalive, then monitor CPU/network/memory utilization via OCI Console metrics for 7 full days. If any metric drops below 20% at the 95th percentile, the mitigation is insufficient and the design must be revised (e.g., increase CPU floor, add synthetic traffic) before proceeding.

**Fallback plan if OCI reclaims despite mitigation:** If the 7-day validation fails or OCI reclaims a production relay, AWS carries all traffic (it's the paid primary and is not subject to reclamation). OCI becomes optional — the operator can remove the OCI provider from the CR spec.

---

## Story Breakdown

| Story | Title | Effort | Depends On |
|-------|-------|--------|------------|
| US-42.1 | Portable relay Go binary (proxy + health + metrics incl. egress bytes + keepalive) | Small-Medium (1d) | None |
| US-42.2 | Cloud-init template + artifact publishing (with SHA-256 verification) + **day-one validation** (deploy VM on AWS, OCI; curl Zen, verify not blocked — A22; verify `@ai-sdk/openai-compatible` headers support) — **AWS IP-reachability confirmed (/models 200, worklog 0410). A23 blocker (worklog 0420) SUPERSEDED 2026-06-20: per-model `allowAnonymous` governs `public` inference, not a global key death — see A23 row. OCI pending.** | Small (0.5-1d) | US-42.1 |
| US-42.3 | InferenceRelay CRD + types + deepcopy + RBAC + **validating webhook** (CredentialsRef Secret existence + keys) | Medium (1d) | None |
| US-42.4 | WireGuard keypair generation + config rendering | Small (0.5d) | None |
| US-42.5 | OCI provider driver (provision, destroy, status) — **blocked by 7-day reclamation validation gate** | Medium (1-2d) | US-42.2, US-42.4 |
| US-42.6 | GCP provider driver (provision, destroy, status) — **optional, not in default fleet** | Medium (1d) | US-42.2, US-42.4 |
| US-42.7 | Relay-router: weighted selection + health checking + 429 detection + ConfigMap poll (5s) + metrics (per-relay health, streams, egress) | Medium-Large (2d) | US-42.3 |
| US-42.8 | **Router WireGuard sidecar** + **network-agnostic ingress** (4 modes: `external`, `loadBalancer`, `nodePort`, `hostNetwork`; chart does NOT install MetalLB) + **NetworkPolicy** (router ingress limited to workspace pods) — see Layer 2 redesign in worklog 0385 | Medium (1.5d) | US-42.4, US-42.7 |
| US-42.9 | InferenceRelay reconciler (lifecycle: provision, health via router /metrics, graceful drain, destroy+recreate, ConfigMap sync, provisioning circuit breaker, egress quota tracking) | Large (2-3d) | US-42.3, US-42.5, US-42.6, US-42.7 |
| US-42.10 | Helm chart integration (CRD, router Deployment+Service+PDB, NetworkPolicy, controller flags, WG Secret) | Small (0.5d) | US-42.3, US-42.9 |
| US-42.11 | Fallback mode: rate-limited direct routing when all relays unhealthy (1 req/2s, max 1 concurrent) | Small-Medium (1d) | US-42.7 |
| US-42.12 | Observability: Prometheus metrics + alert rules + CR conditions | Small (0.5d) | US-42.9 |
| US-42.13 | AWS provider driver (provision, destroy, status) — EC2 t4g.micro, IAM auth, full lifecycle API | Medium (1d) | US-42.2, US-42.4 |

**Total estimated effort:** 12.5-16.5 days

**Day-one gate (US-42.2):** Before any controller work, manually deploy a relay VM on AWS, OCI, and GCP; curl `opencode.ai/zen/v1` from each. If any provider's IPs are blocked by Zen, remove that provider from the fleet. This is the cheapest possible validation. **AWS half: IP-reachability VALIDATED** (worklog 0410 — `/models` HTTP 200 from t4g.micro 34.205.17.204). **A 2026-06-19 follow-up (worklog 0420) claimed a deeper blocker (A23: `public` 401s on inference regardless of IP); that claim is SUPERSEDED 2026-06-20 — re-probe found `big-pickle` + `Bearer public` → HTTP 200 from the same residential IP. Per-model `allowAnonymous` governs inference auth, not a global key death. The relay-forwarding-`public` architecture produces inference for any model Zen flags `allowAnonymous`.** **OCI half: still pending** operator action. **Epic 42's IP-diversity premise (A0) stands — see A23 for the open operator decision on whether to keep #297/#298's default-upstream change.**

---

## Dependency Graph

```
US-42.1 (relay binary) ──────────────┐
                                      ├── US-42.2 (cloud-init + validation GATE)
US-42.4 (WG keypair gen) ─────────┐   │
                                  │   │
US-42.3 (CRD types) ───────────┐  │   │
                               │  │   │
                               │  │   ├── US-42.5 (OCI driver) ─────────┐
                               │  │   ├── US-42.6 (GCP driver) [OPT] ───┤
                               │  │   ├── US-42.13 (AWS driver) ────────┤
                               │  │   │                                   │
                               ├── US-42.7 (router) ──────────────────┤  │
                               │  │                                      │  │
                               │  ├── US-42.8 (router WG sidecar) ──────┤  │
                               │                                         │  │
                               └── US-42.9 (reconciler) ◄────────────────┴──┘
                                          │
                                          ├── US-42.10 (Helm)
                                          ├── US-42.11 (fallback mode)
                                          └── US-42.12 (observability)
```

**Critical path:** US-42.1 → US-42.2 (validation gate) → US-42.13 (AWS driver) → US-42.9 (reconciler) → US-42.10 (Helm)

---

## Execution Strategy

**Phase 0 — Validation gate (day 1):** US-42.1, US-42.2
Port relay binary, deploy on AWS + OCI manually, curl `opencode.ai/zen/v1` from each. **If either IP range is blocked, remove that provider from the fleet.** This is the cheapest possible de-risking step.

**Phase 1 — Foundation (day 2-3):** US-42.3, US-42.4
CRD types and WG keypair generation. No cloud dependencies — can be fully unit-tested.

**Phase 2 — Router (day 3-5):** US-42.7, US-42.8
Build the relay-router with mock relays. WireGuard sidecar + ingress (operator picks mode; default `external`). Test weighted selection, failover, 429 detection against mock HTTP servers.

**Phase 3 — Provider drivers (day 5-8):** US-42.13, US-42.5
AWS and OCI drivers. Can be developed in parallel. End of phase 3: controller can provision a VM on each provider, establish WG tunnel, health-check it.

**Phase 4 — Reconciler + integration (day 8-11):** US-42.9, US-42.10, US-42.11, US-42.12
Full lifecycle management with provisioning circuit breaker. Helm chart. Fallback mode. Prometheus metrics + alert rules. End-to-end: `kubectl apply` → VMs provisioned → WG tunnels up → router routing → pods getting free-tier inference.

---

## Out of Scope

| # | What | Why |
|---|------|-----|
| 1 | Cloudflare Worker as a managed provider | CF Worker IPs are blocked by Zen — that's why this epic exists. |
| 2 | Per-workspace relay assignment | Deterministic hash-based routing is sufficient. No per-workspace state needed. |
| 3 | Relay request/response body logging | Privacy concern. The relay is a dumb byte pipe. Only aggregate counters for 429 detection. |
| 4 | Autoscaling beyond 2 VMs | The default fleet is 1 AWS (paid) + 1 OCI (free). The architecture supports N relays — operators can add GCP or more AWS/OCI instances if needed. |
| 5 | DNS management for pod routing | The router is in-cluster (ClusterIP Service). No DNS needed for routing. DNS only for the relay binary's upstream (`opencode.ai`). |
| 6 | mTLS / TLS between router and relay | WireGuard replaces all PKI. Adding TLS inside WG would be redundant encryption. |
| 7 | Path-secret authentication | Eliminated by WireGuard. WG public-key pinning is the auth. |
| 8 | Caddy / Let's Encrypt | Eliminated by WireGuard. No public HTTPS endpoints on relay VMs. |
| 9 | In-place IP rotation | All rotation is destroy-and-recreate (DQ5). No driver-level `RotateIP` method. |
| 10 | AWS Lightsail as a provider | Lightsail has a limited API unsuitable for programmatic lifecycle management. EC2 t4g.micro provides full EC2 API (RunInstances, TerminateInstances) needed for the controller's destroy-and-recreate rotation. |

---

## CRD Example

```yaml
apiVersion: llmsafespace.dev/v1
kind: InferenceRelay
metadata:
  name: relay-fleet
spec:
  upstreamURL: "https://opencode.ai/zen/v1"
  wireGuard:
    cidr: "10.42.42.0/24"
    port: 51820
    routerEndpoint: "relay-gw.safespaces.dev:51820"  # DNS → operator's chosen ingress (LB VIP, NodePort host, hostNetwork node, etc.)
  providers:
    - provider: aws
      region: us-east-1
      credentialsRef:
        name: aws-credentials
      # shape defaults to t4g.micro (paid, ~$7/mo, most reliable)
    - provider: oci
      region: us-ashburn-1
      credentialsRef:
        name: oci-credentials
      # shape defaults to VM.Standard.A1.Flex (free, 10 TB egress)
    # GCP can be added as a paid third provider if IP diversity is needed:
    # - provider: gcp
    #   region: us-central1-a
    #   credentialsRef:
    #     name: gcp-credentials
  healthCheck:
    interval: 15s
    timeout: 5s
    unhealthyThreshold: 3
    replacementTimeout: 15m
  rotation:
    enabled: true
    max429Rate: 0.5
    detectionWindow: 5m
    cooldown: 30m
  fallback:
    enabled: true
    rate: 0.5          # 1 req / 2s (global, all workspaces)
    maxConcurrent: 1   # only 1 in-flight at a time
```

---

## Migration from Epic 26 (Cloudflare Worker) — now mandatory

Epic 60 (2026-07-12) removed the Cloudflare Worker relay entirely because Zen now blocks CF Worker egress IPs. What was previously an optional migration is now the only path for operators who need relayed free-tier access. The chart default flipped from `inferenceRelayURL: https://relay.safespaces.dev` to direct-to-Zen mode (empty URL). To enable the fleet:

1. **Set `controller.inferenceRelay.enabled: true`** in Helm values — renders the relay-router Deployment, the InferenceRelay CR, the cluster-scoped RBAC, and the `--inference-relay-url=<router FQDN>` controller flag.
2. **Store provider credentials** via `/api/v1/admin/relay/aws-creds` (and OCI/GCP as needed) — controller provisions VMs via cloud-init.
3. **Reconcile the InferenceRelay CR** via `POST /api/v1/admin/relay/deploy`.

Workspace pods read `INFERENCE_RELAY_BASEURL` at startup; the next pod lifecycle event picks up the new URL automatically.

---

## Observability & Alerting

The controller and router expose Prometheus metrics and CR conditions. The following alert rules must be shipped with the Helm chart (in `monitoring.prometheusRules`):

| Alert | Expression | Severity | Action |
|-------|-----------|----------|--------|
| `RelayFleetDegraded` | `llmsafespace_relay_healthy_replicas < 2` | Warning | One relay is down — system is running on a single provider. Check InferenceRelay CR status for the failed instance. |
| `RelayFleetCritical` | `llmsafespace_relay_healthy_replicas == 0` | Critical | Both relays are down — all free-tier traffic is falling back to direct (throttled) routing. Page on-call immediately. |
| `RelayProvisioningFailed` | `llmsafespace_relay_provisioning_failed == 1` | Critical | A provider slot has failed to provision 3 times (config errors). Circuit breaker is tripped — the controller has stopped retrying. Operator must fix the root cause (bad template, credentials) and clear the `ProvisioningFailed` condition. Capacity errors do NOT trip this. |
| `Relay429RateHigh` | `rate(relay_requests_total{status="429"}[5m]) / rate(relay_requests_total[5m]) > 0.3` | Warning | A relay is receiving significant 429s from upstream. Rotation may be imminent. |
| `RelayDraining` | `llmsafespace_relay_draining == 1` | Info | A relay is in draining state — rotation in progress. Informational, no action needed unless it persists >30m. |

**Metrics exposed by the controller:**
- `llmsafespace_relay_healthy_replicas` (gauge) — count of healthy relay VMs
- `llmsafespace_relay_provisioning_failed` (gauge, labels: provider) — circuit breaker tripped (0/1)
- `llmsafespace_relay_draining` (gauge, labels: provider) — relay in drain state (0/1)
- `llmsafespace_relay_quota_exhausted` (gauge, labels: provider) — egress quota exhausted (0/1)
- `llmsafespace_relay_provision_duration_seconds` (histogram, labels: provider) — time to provision + health-check a relay
- `llmsafespace_relay_rotation_total` (counter, labels: provider, reason) — rotation events (429, failure, manual)

**Metrics exposed by the relay binary (scraped by router over WG):**
- `relay_requests_total` (counter, labels: status) — proxied request count by HTTP status
- `relay_egress_bytes_total` (counter) — total bytes sent in response bodies (for GCP quota tracking)
- `relay_keepalive_total` (counter) — keepalive probes sent

**Metrics exposed by the router:**
- `relay_router_requests_total` (counter, labels: relay, status) — requests routed per relay
- `relay_router_active_streams` (gauge, labels: relay) — in-flight streaming connections per relay (used by controller for graceful drain)
- `relay_router_relay_healthy` (gauge, labels: relay) — router's view of relay health (0/1)
- `relay_router_relay_egress_bytes` (counter, labels: relay) — per-relay egress bytes (aggregated from relay `/metrics`; used by controller for quota tracking)
- `relay_router_fallback_active` (gauge) — 1 when in direct-fallback mode

**CR conditions (operator-visible via `kubectl describe inferencerelay`):**
- `Ready` — fleet is operational (at least 1 healthy relay)
- `Degraded` — one relay unhealthy, surviving on single provider
- `ProvisioningFailed` — circuit breaker tripped on a provider slot (includes message with last error)
- `Rotating` — a destroy-and-recreate rotation is in progress
- `FallbackActive` — both relays down, router is proxying directly to upstream

---

## Security Considerations

1. **WireGuard is the only auth.** Relay VMs reject all non-WG traffic. The relay binary listens on the WG interface IP only (`10.42.42.x:8080`), not `0.0.0.0`. Public internet sees one UDP port; unauthenticated packets are dropped by WG before reaching any application code.

2. **Upstream API key transits relay VMs (NOT at rest).** ⚠️ **(2026-06-20: the A23 rationale for this is unfounded — see below.)** The 2026-06-19 framing was: since A23, `public` no longer authorizes Zen inference, so the router must inject a **real** upstream key. **A23 is disproven** (worklog 0420 correction): `public` still authorizes inference for any model Zen flags `allowAnonymous` (`big-pickle` → 200 from residential IP). The relay-forwarding-`public` architecture produces inference without key injection. **Whether the router SHOULD inject a real upstream key is now an open operator decision** (keep #297's mechanism for cases where operators point at a non-Zen upstream that needs a real key; revert the default to Zen+`public`; or repurpose). If key injection IS enabled, the key transits relay VMs in memory over the WG tunnel — **never written to VM disk**, not present in cloud-init — but a compromised VM can observe it in transit, and the blast radius is **fleet-wide** (not per-VM like the WG private key in §3). Mitigations: destroy-and-recreate rotation limits exposure windows; the WG-only listener (§1, §4) means only an authenticated WG peer (the router) can reach the relay; operators who cannot accept fleet-wide key exposure should place a rate-limited/quota-capped gateway key upstream rather than their primary key. There are still **no cluster credentials and no user data** on relay VMs.

3. **WG keypairs are per-VM, generated by the controller.** A compromised relay VM's private key compromises only that tunnel. Destroy-and-recreate generates a fresh keypair. The router's private key is in a K8s Secret.

4. **UFW firewall on relay VMs.** Cloud-init configures: deny all incoming, allow UDP 51820 (WG), allow outgoing. SSH is either disabled or restricted to the WG interface.

5. **Router is in-cluster, not exposed to the internet.** Workspace pods reach it via ClusterIP. Only the WG UDP port is exposed (via the operator's chosen ingress mode — see Layer 2) for relay VMs to connect back.

6. **Provider credential rotation.** Cloud credentials (AWS access key / IAM role, OCI API key, GCP service account JSON) live in K8s Secrets, used only by the controller. Rotating them doesn't affect running VMs — only future provisioning calls. The validating webhook checks that the referenced Secret exists and contains the required keys before provisioning.

7. **Relay binary integrity verification.** Cloud-init verifies the SHA-256 checksum of the downloaded relay binary before executing it (`sha256sum -c`). The checksum is embedded at cloud-init render time by the controller, sourced from the GitHub Release checksums file. This prevents supply-chain attacks via compromised artifact mirrors — consistent with the project's digest-pinning standard for container images.

8. **NetworkPolicy isolates the router.** The router Service (`relay-router:8080`) is reachable by any pod in the namespace by default. A NetworkPolicy limits ingress to workspace pods (the proxy path), the controller pod (its own `/metrics` scrape for fleet health), and the API pod (the admin dashboard's `/metrics` scrape — `RelayAdminHandler.scrapeRouterMetrics` populates per-relay request/stream counters for `/admin/relay`). The API is a trusted in-cluster component already broker for credentials and workspace control; the incremental blast radius of granting it `8080` reach is the router's proxy path (port 8080 serves both `/metrics` and the proxy — NetworkPolicy is L3/4 so the two cannot be separated by the policy layer). This still prevents a compromised non-trusted pod (anything outside workspace/controller/api) from abusing the relay path or triggering upstream rate limits.

---

## Open Questions

| # | Question | Notes |
|---|----------|-------|
| OQ1 | What is the exact OCI free-tier limit on ephemeral/reserved public IPs? | Empirical (A8). Must test during US-42.5. Determines feasibility of IP rotation via destroy+recreate (which allocates a new ephemeral IP each time). |
| OQ2 | ~~Does OCI support cloud-init on Always Free images?~~ | ✅ Resolved (A9). OCI "Creating an Instance" docs confirm cloud-init/user-data support on Ubuntu and Oracle Linux images. Max 32,000 bytes userdata. |
| OQ3 | ~~What are the actual GCP e2-micro specs?~~ | ✅ Resolved (A15). 2 vCPUs (0.25 fractional shared-core), 1 GB memory, burstable to 100% for 30s. Max egress 1 Gbps. |
| OQ4 | ~~Can the cluster expose a UDP endpoint for WG?~~ | ✅ Resolved (A21). Network-agnostic four-mode design (`external`, `loadBalancer`, `nodePort`, `hostNetwork`); the chart does NOT install MetalLB. Default `external` produces no ingress resources — operator declares `routerEndpoint` and wires UDP 51820 themselves. See Layer 2 redesign. |
| OQ5 | Will OCI's idle reclamation actually trigger for a relay VM with keepalive traffic? | Requires 7-day empirical testing (see "OCI Idle Reclamation Mitigation"). The 20% thresholds are documented (95th percentile CPU). **Hard gate for US-42.5.** |
| OQ6 | Does Zen (opencode.ai) block OCI and GCP IP ranges? | **Day-one validation gate (A22).** Since the throttle is per-IP (A0), OCI/GCP datacenter IPs should not be in Cloudflare's egress range — but must curl to verify. |
| OQ7 | ~~How does the router inject `X-Workspace-ID`?~~ | ✅ Resolved. `@ai-sdk/openai-compatible` (v2.0.50+) supports a `headers` field in provider config (verified from npm docs). Add `Headers map[string]string` to the `options` struct at `relay_injector.go:136`. Used for metrics only, not routing. |
| OQ8 | Should the router proxy streaming responses (SSE) with buffering or true pass-through? | True pass-through (`io.Copy` / `Flush`) — the router must not buffer SSE streams. The existing proxy in `api/internal/handlers/proxy.go:358-377` already does this for workspace→opencode traffic; reuse the same pattern. |

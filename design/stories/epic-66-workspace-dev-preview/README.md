# Epic 66: Workspace Dev Preview — Authenticated HTTP/WS Tunnel to In-Workspace Dev Servers

**Status:** Definition (not yet in implementation)
**Created:** 2026-08-10
**Priority:** Medium — closes the "I can't see my React app" gap that today sends users to `kubectl port-forward` (worklogs 0069, 0705) or causes incidents when users improvised (worklog 0678 — Vite + Playwright inside a pod). Unblocks web/framework/preview-driven development as a first-class workspace use case alongside the existing conversational agent.
**Depends On:** Epic 03 (proxy pattern), Epic 14 US-14.2 (terminal WebSocket ticket pattern — the architectural template), Epic 22 US-22.8 (agentd user mux on port 4097 — the designated "future proxy" landing zone).
**Soft-depends on:** Epic 54 (org-scoped login / wildcard cookie domain — only relevant if per-workspace subdomain preview is later layered on; v1 is path-based and needs none of it).
**Authoritative for:** How an authenticated workspace owner views HTTP applications (Vite, Next, webpack-dev-server, Playwright, ad-hoc HTTP servers) running inside their own workspace pod from a browser, including WebSocket-based HMR, without exposing any port publicly or modifying the workspace NetworkPolicy.

---

## Problem Statement

### Current State

A workspace pod can run any process the user's runtime supports — including a React/Vue/Svelte dev server, a Next.js app, a Playwright browser test target, or an ad-hoc `python -m http.server`. There is no platform mechanism to view these from a browser:

- **Workspace pods have a default-deny ingress NetworkPolicy** (`helm/templates/workspace-network-policy.yaml:1-83`). Only the API pod (labels configurable via `.Values.networkPolicy.apiPodLabelSelector`) may open TCP to a workspace, and only on ports 4096/4097/4098. Every other source — including the ingress controller, peer workspace pods, and the user's browser — is dropped. This is intentional and load-bearing for tenant isolation (Epic 17, Epic 51).
- **There is no per-workspace Service or Ingress.** The API reaches opencode via direct `podIP:4096` (`api/internal/handlers/proxy.go:487`). Pod IPs are not externally routable — confirmed by multiple worklogs where operators resorted to `kubectl port-forward` to reach a workspace pod at all (worklog 0069:20 — "Pod IP `10.69.6.128:4096` — unreachable from bastion directly; used `kubectl port-forward` on port 24096"; worklog 0705:34-35 — "pod IPs are NOT routable from the host on this cluster (Cilium). Reached opencode via `kubectl port-forward pod/<p> 18096:4096`").
- **The platform provides no user-facing port-forward mechanism.** A PATH-shadow wrapper that would have blocked `kubectl port-forward` as a subcommand was *specified* in the security design (`design/0021:1444-1445`, tier 2 hardened mode) but was **never implemented** — Epic 07 US-7.1/7.2 closed it as architecturally incompatible with `ReadOnlyRootFilesystem: true` and mise shims, and `runtimes/base/tools/wrappers/` does not exist. The audit policy (`design/stories/epic-17-security-review/phase-0/audit-policy.yaml:28`) lists `pods/portforward` at `RequestResponse` level — for audit *logging*, not authorization blocking. There is no K8s RBAC rule today that blocks the portforward subresource.
- **Users improvise today.** Worklog 0678 records a production incident (line 33): *"Per user: 'I was running playwright tests in pod'. Pod process list confirmed `npm run dev` (vite) for `frontend/` and a tree of chrome-headless + pkill processes consistent with a Playwright e2e teardown"* — the user was doing web development inside a workspace with no way to view their dev server from a browser, and the memory pressure from running Vite + Playwright + opencode simultaneously caused a 34-minute opencode deadlock (liveness-probe incident). The use case is real, unserved, and already causing operational pain.

The platform has the assets to solve this cleanly — an authenticated reverse-proxy seam (`proxy.go`), an agentd user mux explicitly reserved for "future proxy endpoints" (`cmd/workspace-agentd/server.go:190`, `pkg/agentd/types.go:53`), and a WebSocket-upgrade-with-ticket precedent (Epic 14 US-14.2 terminal proxy). They have not been connected.

### Desired State

An authenticated workspace owner can point their browser at an API route that tunnels HTTP and WebSocket traffic to a port inside their own workspace pod — loading a Vite/Next/any HTTP app, with hot-module-replacement working — without any change to the workspace NetworkPolicy, without any per-workspace Service/Ingress, and without exposing any URL reachable by an unauthenticated viewer.

Concretely, a user should be able to:

1. Start a dev server inside their workspace (e.g. `npm run dev` on port 5173, or `python -m http.server 8080`).
2. Toggle "Dev Preview" on for that workspace (off by default).
3. Open `https://<api-origin>/api/v1/workspaces/:id/dev-preview/5173/` in a browser tab and see their app, with HMR live-reloading on file change.
4. Have the same auth that protects every other `/api/v1/workspaces/:id/*` route protect this one — no new credential, no shareable unauthenticated URL.

---

## Relationship to Existing Subsystems (do not confuse)

| | opencode reverse proxy (Epic 03, shipped) | Terminal WebSocket proxy (Epic 14 US-14.2, shipped) | Dev Preview (this epic) |
|---|---|---|---|
| Forwards to | opencode `:4096` (agent API) | `pods/exec` SPDY into the pod shell | Arbitrary user-started port on `localhost` inside the pod |
| Protocols | HTTP + SSE (agent streaming) | WebSocket ↔ SPDY | HTTP + WebSocket (for HMR) |
| Auth | `WorkspaceAccessMiddleware` + HTTP Basic to pod | Ticket-based WS upgrade + `WorkspaceAccessMiddleware` | `WorkspaceAccessMiddleware` (JWT cookie or Bearer header) |
| Touches `proxy.go`? | **Is** `proxy.go` — must not be modified (SSE-tuned) | No | No — uses `httputil.ReverseProxy` on a separate route |
| NetworkPolicy change? | n/a (existing) | No | **No** — reuses the existing API→pod:4097 allowance |

**Scope guardrail (load-bearing):** *This is an authenticated developer tool, not a public hosting feature.* The URL is reachable only by the workspace owner (or an attacker who has already stolen their JWT — at which point every other workspace route is equally compromised). Public/shareable preview URLs are a categorically different product (requires abuse-report intake, content scanning, rate limiting, takedown workflow, ToS — none of which the platform staffs today) and are explicitly out of scope for v1 (see "Out of Scope"). This guardrail is enforced by route placement and middleware inheritance, not by review alone.

---

## Scope

### In scope

- **One HTTP route:** `GET /api/v1/workspaces/:id/dev-preview/:port/*` — path-based port discovery (no CRD port list, no webhook for ports).
- **HTTP + WebSocket proxying** via `httputil.ReverseProxy` (not `proxy.go`). WS support is mandatory for HMR (Vite, Next, webpack-dev-server all use it); shipping HTTP-only defeats the purpose.
- **agentd-mediated in-pod forward** on port 4097 (the existing user mux). API proxies to `podIP:4097/v1/dev-preview/:port/...`; agentd forwards to `localhost:<port>`. **Zero NetworkPolicy changes** — 4097 is already in the API→pod allowlist (`helm/templates/workspace-network-policy.yaml:33-48`).
- **Authenticated owner-only.** The route inherits `AuthMiddleware` (JWT cookie or Bearer header — see D2) and `WorkspaceAccessMiddleware` (ownership check) from `idGroup`. No new auth surface, no capability token, no shareable URL.
- **Opt-in per workspace** via a CRD field on `WorkspaceNetworkAccess` (D3). Off by default; the API handler returns 503 for workspaces that haven't enabled it.
- **Port guardrail:** denylist the agent's own ports (4096/4097/4098) and privileged ports (<1024) at both the API and agentd layers. Belt-and-suspenders; see Threat Model for why the blocklist is not the primary boundary.
- **Connection-limit isolation:** the dev-preview path uses its own connection pool, NOT the existing `maxConnectionsPerWorkspace=10` budget (`proxy.go:36`) which is sized for agent sessions and would be exhausted by a browser's parallel asset/WS/fetch fan-out.
- **Response size cap** to bound bandwidth abuse (operator-configurable, default 50 MiB per response).
- **Frontend:** a "Dev Preview" toggle in the workspace settings drawer + an "Open preview" affordance that opens the proxied URL in a new tab, optionally with a port prompt.

### Out of scope (with rationale — see "Out of Scope" section)

Public/shareable preview URLs (D7); per-workspace Ingress or Service objects (D4); per-workspace subdomain routing via Epic 54 machinery (D7); CRD port allowlist (`spec.devPorts`) — denylist is sufficient for v1; reverse-tunnel / agentd-dials-out model (D5); raw-TCP port forwarding (D6 — HTTP/WS only in v1); cross-workspace or org-member-shared previews; in-workspace-agent-driven preview discovery (the agent cannot open a browser; the human can); embedding the preview in an iframe inside the SPA (SameSite=Lax cookie default blocks this — deferred to v2 if demanded).

---

## Alternatives Considered

Five architectural shapes were assessed before landing on the agentd-mediated HTTP/WS proxy. This is a conscious decision, not a default.

| Option | Assessment | Verdict |
|---|---|---|
| **A. API-mediated proxy to arbitrary pod port** (`podIP:<port>` directly) | Requires widening the workspace NetworkPolicy to allow API→pod on the dev port range — an auditable but real change to the cluster's primary tenant-isolation boundary. Smaller in-pod SSRF surface than B (only 0.0.0.0-bound services reachable), but adds a new ingress rule every workspace inherits. | **Rejected for v1** — defensible alternative; the trade is "trust NP review" vs "trust agentd code review." B keeps the NP untouched, which is the lower-risk choice for a security-sensitive codebase. |
| **B. agentd-mediated proxy on port 4097 (this epic)** | API → `podIP:4097/v1/dev-preview/:port/*` → agentd → `localhost:<port>`. Zero NetworkPolicy changes (4097 already allowed). Lands on the codebase's designated "future proxy" seam (`server.go:190`). One extra in-pod hop, but the path is short (same pod). | **Adopt** — smallest blast radius, lands where the codebase already expects proxy endpoints. |
| **C. Reverse tunnel (agentd dials out to API over WebSocket)** | The removed Epic 26 pattern. Solves NAT-traversal problems that don't exist in-cluster. Same in-pod SSRF surface as B with strictly worse properties (persistent token in agentd env, observability loss, reconnection thundering herd). | **Rejected** — pure cost, no benefit in this environment. |
| **D. Per-workspace Ingress + wildcard DNS (Epic 54 machinery)** | Clean shareable URLs, real browser experience, but: needs a NetworkPolicy widening for the ingress controller, creates an unauthenticated-by-default endpoint on a trusted domain (phishing surface against non-users), one Ingress+Service per workspace (heavy at 1000+ tenants), and the platform's own `WorkspaceAccessMiddleware` is bypassed (Ingress routes direct to podIP). | **Rejected for v1** — the right mechanism for a *public-shareable* v2 layered on top of B, the wrong mechanism for *authenticated-owner* v1. See D7. |
| **E. Don't build it — document `cloudflared` / `ngrok` / `tailscale serve`** | Users run their own tunnel provider; the platform egress NP already allows public HTTPS. Zero platform risk (abuse is the tunnel provider's problem). | **Rejected as primary, recommended as companion doc** — ships *alongside* B as an operator-recommended escape hatch for users who want stable shareable URLs today without the platform taking on the abuse surface. |

---

## Actors & Roles

| Actor | Auth guard | Can do |
|---|---|---|
| Workspace owner | authenticated + `WorkspaceAccessMiddleware` ownership | Toggle dev-preview on/off for own workspace; open the preview URL in a browser; use the proxied HTTP/WS path |
| Org admin (`org_memberships.role='admin'`) | `OrgAdminGuard` (if workspace is org-scoped) | Same as owner for org workspaces (existing ownership semantics) |
| Other tenant | n/a | **Cannot** reach another workspace's preview — `WorkspaceAccessMiddleware` returns 403 before any proxying |
| Unauthenticated viewer | n/a | **Cannot** reach any preview URL — the route is behind `AuthMiddleware`; the `lsp_session` cookie or Bearer header is required |
| In-workspace agent | n/a | No platform API to toggle preview or open a browser — preview is a human-driven affordance. (Agent can still run dev servers in the workspace; that's just process execution, not this feature.) |

---

## Architecture

```
┌── Browser (owner) ───────────────────────────────────────────────────┐
│  GET  https://<api-origin>/api/v1/workspaces/:id/dev-preview/5173/    │
│  WebSocket upgrade for HMR                                            │
│  Cookie: lsp_session=<JWT>  (sent automatically on navigation)       │
└──────────────────────────────────────────────────────────────────────┘
                  │
                  │ inherits AuthMiddleware (JWT cookie OR Bearer header)
                  │ inherits WorkspaceAccessMiddleware (ownership check)
                  ▼
┌── API server (stateless) ────────────────────────────────────────────┐
│  api/internal/handlers/dev_preview.go                                │
│  • parse :port as int; reject non-numeric / out of range             │
│  • denylist: 4096/4097/4098, <1024                                    │
│  • check workspace.Status.Phase == Active + PodIP present            │
│  • check CRD spec.networkAccess.devPreview == true (D3)               │
│  • httputil.ReverseProxy → http://<podIP>:4097/v1/dev-preview/5173/… │
│  • rewrite Host → localhost (dev-server origin checks — D8)          │
│  • separate connection pool (NOT maxConnectionsPerWorkspace)         │
│  • response size cap (default 50 MiB)                                │
└──────────────────────────────────────────────────────────────────────┘
                  │ existing NetworkPolicy allowance (4097 in allowlist)
                  ▼
┌── Workspace pod — agentd user mux (port 4097) ───────────────────────┐
│  cmd/workspace-agentd/dev_preview.go                                 │
│  • Basic auth with workspace password (same pattern as every other   │
│    user-mux handler — workflow_execute.go:130, mcp_server.go)        │
│  • API injects SetBasicAuth("opencode", password) at the proxy layer │
│  • parse :port; denylist: 4096/4097/4098, <1024 (re-check in-pod)    │
│  • httputil.ReverseProxy → http://localhost:<port>/…                 │
│  • supports HTTP + WebSocket upgrade (stdlib Hijacker)               │
└──────────────────────────────────────────────────────────────────────┘
                  │ localhost (no NP applies inside the pod netns)
                  ▼
┌── User-started dev server (e.g. Vite on :5173) ──────────────────────┐
│  listens on 0.0.0.0:5173 (or 127.0.0.1:5173 — agentd reaches both)   │
└──────────────────────────────────────────────────────────────────────┘
```

**Why agentd (4097) and not direct API → podIP:<dev-port> (Option A).** The workspace NetworkPolicy is the platform's primary tenant-isolation boundary — it is the one control that makes "workspace A cannot reach workspace B" true at the network layer. Widening it to allow API → arbitrary pod ports, even in a bounded range, mutates that boundary for every workspace in the cluster. Port 4097 is *already* in the allowlist for the API pod (it serves `/v1/reload-secrets` and Epic 64 workflow endpoints). Reusing it costs zero new network policy. The price is the localhost-SSRF surface inside the pod (agentd can reach 127.0.0.1:anything) — but the workspace owner already has equivalent reachability via the terminal proxy (Epic 14 US-14.2: they can `curl localhost:anything` from a shell). So B grants no new capability to the owner; it just makes 0.0.0.0-bound and localhost-bound ports reachable from their browser instead of their shell, for the same authenticated user. See Threat Model.

**Why `httputil.ReverseProxy`, not a hand-rolled forwarder.** `proxy.go`'s `doProxy` is hand-tuned for SSE agent streaming (chunk-by-chunk flush, basic-auth injection, quota gating, stale-IP retry — `proxy.go:486-620`). It does not correctly handle a WebSocket `Upgrade`. Touching it risks regressions on the most important request path in the platform. `httputil.ReverseProxy` forwards both HTTP and WebSocket natively (WS via `http.Hijacker`), is stdlib, and is purpose-built for this. Rule 12: don't entangle two concerns. The dev-preview handler is a separate file with a separate proxy instance.

---

## Threat Model (the dominant concern)

This feature was threat-modeled against four actors: (1) external attacker with no creds; (2) honest workspace owner running untrusted npm code; (3) honest-but-curious tenant wanting to reach a neighbor's pod; (4) malicious workspace owner weaponizing the platform. The platform's product shape — authenticated, metered, transient-workspace SaaS with Cloudflare Turnstile on signup (README-LLM.md §16), usage metering (Epic 12), auto-suspend/idle reaping, and per-tenant active-workspace quotas (`PodTenantQuotaValidator`, Epic 51) — bounds the abuse surface at the account level rather than the architecture level.

| Threat | Control |
|---|---|
| External attacker reaches workspace contents via preview URL | Route is behind `AuthMiddleware` (JWT cookie or Bearer header) + `WorkspaceAccessMiddleware` (ownership). No credential → 401. Valid credential but not the owner → 403. **There is no unauthenticated surface.** This is the core difference from Option D and the reason D is out of scope for v1. |
| Cross-tenant access (tenant A views tenant B's preview) | `WorkspaceAccessMiddleware` (`workspace_access.go:49`) runs before any proxying. Resolves the workspace, runs `CheckOwnership`, returns 403 on mismatch. Same gate every other `/api/v1/workspaces/:id/*` route already uses. |
| Stale pod IP cross-workspace (pod IP reused after resume) | Handler re-checks `workspace.Status.Phase == Active` and re-Gets the CRD on the request path (mirror `proxy.go:245` stale-IP retry). Pod IP is never cached across requests on the dev-preview path. |
| **Localhost SSRF via agentd** (owner reaches agentd admin :4098, opencode :4096, future sidecar admin ports) | **Port denylist at both API and agentd layers** (block 4096/4097/4098 + <1024). However: the workspace owner already has equivalent localhost reachability via the terminal proxy (Epic 14 — they can `curl localhost:4098/v1/statusz` from a shell today). The dev-preview grants **no new capability** to the owner; it changes the *transport* (browser-via-agentd vs shell), not the *principal* or the *surface*. The denylist is belt-and-suspenders, not the primary boundary. Additionally: agentd's admin mux on 4098 is Bearer-gated via `AGENTD_ADMIN_TOKEN` (`server.go:163-176`) — but the dev-preview handler lives on the **user mux (4097)**, which authenticates via **Basic auth with the workspace password** (same pattern as `workflow_execute.go:130`, `mcp_server.go`). The API injects the password; the browser never sees it. |
| Path traversal (`/dev-preview/5173/../../4098/`) | Parse `:port` as an integer at the route boundary; reject non-numeric or out-of-range. The `:port` parameter cannot escape its integer type. |
| Recursion (`/dev-preview/4097/v1/dev-preview/…`) | 4097 is in the denylist. |
| Phishing against non-users (the Option D residual risk) | **N/A for v1.** The preview URL requires the viewer to hold a valid workspace-scoped JWT. A non-user clicking a leaked URL gets 401. There is no unauthenticated HTML surface on the platform domain that an attacker could use for phishing. This threat is the entire reason Option D is out of scope. |
| Malicious owner uses platform as free TLS-terminated hosting | Bounded by: (a) URL is owner-authenticated — sharing it leaks their own JWT, so distribution requires the attacker to extract and republish their token or build a re-signing shim; (b) Cloudflare Turnstile on signup blocks automated account creation; (c) usage metering (Epic 12) makes active compute billable; (d) auto-suspend + idle reaping + per-tenant active-workspace quotas bound how many active preview endpoints one account can keep hot; (e) the dev server dies when the workspace suspends. The abuse control is account-level, not architectural. |
| Bandwidth / response-size abuse (proxied dev server serves 10GB files) | Response size cap (operator-configurable, default 50 MiB). Per-route rate limit (existing `middleware/per_route_rate_limit.go`). |
| WebSocket upgrade auth ordering (CSRF / header smuggling on the WS handshake) | `AuthMiddleware` + `WorkspaceAccessMiddleware` run **before** the proxy hands off to the reverse proxy. The upgrade reaches `httputil.ReverseProxy` only after both gates pass. Same pattern as the terminal WS proxy (US-14.2). |
| Cookie scope attacks (preview origin shares registrable domain with platform auth) | v1 serves the preview from the **API origin** (path-based, same host as every other `/api/v1/workspaces/:id/*` route). The `lsp_session` cookie is already scoped to that origin. No new cookie-scope concern is introduced — it's the same trust boundary the existing workspace routes already live in. (This is also why v1 is path-based, not subdomain-based: a `ws-xyz.app.<domain>` preview would share the registrable domain with `app.<domain>` auth and introduce a cookie-scope surface that path-based routing avoids.) |
| Untrusted JS in the owner's browser (the dev server serves attacker-controlled HTML that runs in a browser holding the owner's session) | Inherent to any "view a web app in a browser" feature — equally true of opening a localhost dev server without the platform involved. The preview app's content is the owner's own dev server output. Same-origin discipline (CSP, cookie attributes) is the dev server's responsibility, same as any deployment. Not a platform-introduced risk. |
| SSRF via the dev server's outbound calls (dev server receives request → makes outbound call the user wants hidden → response proxied back) | The workspace egress NetworkPolicy already governs outbound traffic from the pod. The dev server's outbound behavior is bounded by the same policy that bounds the opencode agent's outbound behavior. No new egress path is opened. |

### What is NOT a threat under this model (and why)

- **Public hosting abuse (the "spin up N links, push to Cloudflare" scenario).** Bounded by Turnstile + metering + auto-suspend + per-tenant quotas (the controls the platform already operates for every other surface). The preview URL is not shareable without leaking the owner's JWT, which is a much smaller distribution surface than a public URL. This is a product-shape-driven conclusion: an authenticated, metered, transient-workspace SaaS is not a hosting provider, and adding an authenticated preview route does not make it one.
- **Trust-and-safety infrastructure (abuse-report intake, content scanning, takedown workflow).** Required for *public* preview URLs (Option D / v2). Not required for *authenticated-owner* preview (v1), because there is no unauthenticated viewer to protect.

---

## Design Decisions

### D1 — agentd-mediated proxy (Option B), not API-direct (Option A) or per-workspace Ingress (Option D)

See "Alternatives Considered." B reuses the existing API→pod:4097 NetworkPolicy allowance, lands on the codebase's designated "future proxy" seam, and grants the owner no capability they don't already have via the terminal proxy. A is a defensible alternative (smaller in-pod SSRF surface, auditable NP change) — the trade is "which review surface do you trust more: NP review or agentd code review." This codebase reviews agentd code more often than it reviews cluster-wide NP changes, so B is the lower-risk choice here. D is out of scope for v1 (see D7).

### D2 — Authentication rides the existing session model (JWT cookie + Bearer header)

The deployed `AuthMiddleware` (`api/internal/services/auth/auth.go:1474-1484`) extracts the token from the `Authorization: Bearer` header **OR** the `lsp_session` HttpOnly cookie (set at login by `setSessionCookie`, `api/internal/server/router.go:790-791`). Both feed the same `ValidateToken` path. This means:

- **Browser navigation works with zero additional auth plumbing.** "Open preview in new tab" sends the cookie automatically; the middleware validates the JWT inside it. No capability token in the URL is needed (the Gitpod pattern is not needed here).
- **API-key auth (`lsp_...`) is header-only** — programmatic clients (CI, MCP) use the Bearer path. Document this: the dev-preview is the first route where "browser navigation vs API client" meaningfully differs in transport.
- **SameSite is not explicitly set** in `setSessionCookie` (Gin's `SetCookie` doesn't take a SameSite arg; browser default is Lax). Fine for same-site navigation. If iframe embedding is ever demanded (v2+), this needs `SameSite=None; Secure` — deferred.

This decision is the reason "authenticated preview URL" is not a feature that needs building — it's the model the platform already has.

### D3 — Opt-in via CRD field `spec.networkAccess.devPreview` (boolean, default false)

`WorkspaceNetworkAccess` already exists on the CRD (`workspace_types.go:51-56`) with an `Ingress bool` field (currently vestigial for inbound). Adding `DevPreview bool` alongside it is the natural home, gives webhook validation, makes the opt-in state observable in `kubectl get workspace -o yaml`. The API handler reads it from the CRD on the request path via the K8s client — the same path it reads `Status.Phase` and `Status.PodIP` (`proxy.go:237-245`). NetworkAccess is **not** mirrored into PostgreSQL today (no SQL migration references it — confirmed by grep), so the handler must read from the CRD directly.

**The opt-in is enforced by the API handler alone.** agentd does **not** independently re-check the flag — it has no mechanism to read CRD fields (agentd has no K8s client; the materialize subcommand reads `/sandbox-cfg/` files, not agentd proper). This is consistent with the terminal proxy precedent (US-14.2): the terminal handler does not re-check any CRD field inside the pod; the API ownership check is the sole gate. Two independent gates already protect this path: the NetworkPolicy (network layer — only the API pod can reach 4097) and `WorkspaceAccessMiddleware` (application layer — only the owner gets a 200). The opt-in flag is a configuration value, not a security boundary.

**Why not a runtime flag in `workspace-config.json`.** That file is read by the materialize subcommand (a separate process that runs before agentd starts), not by agentd itself. Wiring agentd to read a new flag from it would require a new read path, a new materialization step, and a new reload behavior on toggle. The CRD field is already watched by the controller and already read by the API handler via the existing K8s client path — zero new plumbing.

**Why opt-in at all** (vs always-on). Always-on adds the proxy surface to every pod. Given the threat model is bounded (authenticated owner-only), always-on is defensible — but opt-in is safer and lets the agentd forwarder be a no-op in the common case where the user isn't doing web development. Belt-and-suspenders matches the codebase's posture on every other network-adjacent feature.

### D4 — Path-based port discovery (`/dev-preview/:port/*`), not a CRD port allowlist

The owner already has shell access (Epic 14 terminal proxy). They can already `curl localhost:anything`. The dev-preview does not grant new localhost reachability — it changes the transport, not the principal. Therefore a server-side port allowlist (`spec.devPorts: [5173]`) adds validation cost (CRD field + webhook + deepcopy regen + reconciler) for no security benefit the owner doesn't already have. A hard-coded denylist at the handler (block 4096/4097/4098 + <1024) is sufficient. Path-based parsing (`:port` as a typed integer) is trivially safe against traversal.

**Operator allowlist ranges (e.g. "only ports 3000-9999") are deferred to v2** as an instance setting. v1 ships with the denylist; if abuse patterns emerge, the operator can tighten without a schema change.

### D5 — Not a reverse tunnel; agentd does not dial out

The removed Epic 26 pattern (agentd dials the API over a persistent WebSocket, the API multiplexes inbound traffic down it) is strictly worse than B in this environment: same in-pod SSRF surface, plus a persistent token in agentd env (new leak/rotation problem — the class of problem Epic 50 just fought to eliminate for the master KEK), observability loss (Cilium flow logs would show pod→api:443, not api→pod:5173), and reconnection thundering herd at scale. Epic 26 was built for client-proxied inference, not for in-cluster preview. The API→pod:4097 path is already allowed and observable; reuse it.

### D6 — HTTP + WebSocket only; no raw-TCP port forwarding

Browser-viewable HTTP apps are the use case. Raw-TCP forwarding (database clients, SSH, custom binary protocols) is a different feature with different failure modes: no Origin/CSRF model, opaque to L7 inspection, harder to size-cap, and the security reasoning is categorically different (a raw-TCP tunnel to `localhost:5432` exposes the database with no HTTP-level auth gate). The platform does not provide raw-TCP port forwarding today — pod IPs are not externally routable (worklogs 0069, 0705), and no user-facing port-forward mechanism exists. The security design *specified* a PATH-shadow wrapper that would have blocked `kubectl port-forward` (design/0021:1444-1445, tier 2), but it was never implemented (Epic 07 closed US-7.1/7.2 as architecturally incompatible; `runtimes/base/tools/wrappers/` does not exist). v1 is HTTP/WS only; v2 raw-TCP is a separate decision that should evaluate whether the same security concerns that motivated the (never-implemented) wrapper design apply to a platform-provided tunnel.

### D7 — Public/shareable preview URLs are out of scope for v1

A public preview URL (`ws-xyz.<domain>`, reachable without a platform account) is a categorically different product. The architecture for it (Option D — per-workspace Ingress + Epic 54 wildcard DNS + a forward-auth gate) is sound and well-precedented (Codespaces, Gitpod, Replit, Glitch all do it). But the *operational* surface is the cost: abuse-report intake, automated content scanning, rate limiting at the edge, takedown workflow, ToS enforcement, domain-reputation monitoring. None of that exists on the platform today, and adding the feature without it would make the platform a hosting provider for attacker-controlled HTML on a trusted domain. That is a product decision (do we want to staff trust-and-safety?), not a security decision (the architecture is safe at the level our peers ship). v2 builds D on top of B's in-pod forwarder; v1 does not.

---

## Validated Assumptions

Closed by the US-66.1 spike (2026-08-10; see `PREVIEW-CONTRACT.md` for captured evidence). Recording per README-LLM.md Rule 7.

| ID | Assumption | Status | Evidence |
|---|---|---|---|
| A1 | `httputil.ReverseProxy` correctly forwards a WebSocket `Upgrade` end-to-end through two hops (API → agentd → dev server) without custom Hijack code | ✅ **CLOSED** | Spike proved the inner hop (`wscat` → `:5174 httputil.NewSingleHostReverseProxy` (zero custom code) → `:5173 Vite`). Both `full-reload` and partial `js-update` HMR events traversed the proxy. The outer hop (API → agentd:4097) is the same stdlib code path. |
| A2 | The `lsp_session` cookie is sent on a cross-tab navigational GET to a path under the same origin (no SameSite barrier) | ⚠️ Codebase-verified, not in-pod validated | `auth.go:1474-1484` confirms the deployed AuthMiddleware reads the `lsp_session` cookie. Browser-side cross-tab behavior is a manual confirmation during US-66.4 implementation review (open the preview URL in a fresh tab; expect 200). |
| A3 | agentd can bind a new handler on the existing user mux (port 4097) without conflicting with the existing `/v1/reload-secrets`, `/v1/agent/reload`, `/v1/workflow/*`, `/v1/mcp` routes | ⚠️ Codebase-verified, not in-pod validated | `server.go:233-249` confirms the existing handlers; `/v1/dev-preview/` prefix does not collide with any of them. Spike ran on a non-workspace VM (no agentd listening); live 401/405/404 probes deferred but not blocking — route collision is a static property of the code, verified by reading it. |
| A4 | Dev servers accept a request whose `Host` header is the API origin, not `localhost` — or whether `Host` must be rewritten to `localhost` for the dev server to respond 200 | ✅ **CLOSED — Host rewrite is MANDATORY** | Spike confirmed: Vite ≥5.4.13 (CVE-2025-30208 patch) returns **403** for any foreign `Host`; `python -m http.server` and `npx serve` ignore `Host` entirely. Universal rule: **always rewrite `Host` → `localhost:<port>`** — required for Vite, harmless for others. The proxy Director must set `req.Host = "localhost:" + port` (not just `req.URL.Host`). |
| A5 | The existing `idGroup` middleware chain (`AuthMiddleware` + `WorkspaceAccessMiddleware`) runs to completion before a WebSocket upgrade reaches `httputil.ReverseProxy` — no upgrade-shortcut bypass | ⚠️ Codebase-verified, not in-pod validated | `router.go:393,399` confirms `idGroup` inherits both middlewares before any handler runs. The WS upgrade reaches `httputil.ReverseProxy` only after both gates pass. |
| A6 | The response-size cap can be enforced without breaking streaming responses | ✅ **CLOSED — both response shapes are normal** | Spike found dev servers produce **both** `Content-Length`-sized responses (Vite's small module/HTML responses) AND `Transfer-Encoding: chunked` streams (large/streaming responses). The cap MUST handle both shapes: reject oversized `Content-Length` upfront, AND count bytes through a `ResponseWriter` wrapper for chunked streams (tear down on overflow, never panic). The HMR WS itself is a long-lived upgrade (not a sized response) and is exempt from the per-response body cap. |

---

## Edge Cases

1. **Workspace suspended mid-request** → handler's `Status.Phase == Active` check returns 503 with a clear message before any proxying. (Mirror `proxy.go:245`.)
2. **Workspace transitions Active → Suspending while a long-lived WebSocket (HMR) is open** → the underlying TCP connection drops when the pod terminates; the browser sees a disconnect. The user reconnects after resume. v1 does not transparently migrate the WS across pod restarts; document this.
3. **Port not listening** (user typed `/dev-preview/5173/` but hasn't started the dev server) → agentd's connection to `localhost:5173` is refused → `httputil.ReverseProxy` returns 502. Surface a helpful error page ("is your dev server running on port 5173?") rather than a raw proxy error.
4. **Port in denylist** (`/dev-preview/4096/`, `/dev-preview/4098/`, `/dev-preview/80/`) → 400 with a clear message. Blocked at both API and agentd layers.
5. **Dev server binds to 127.0.0.1 only** → agentd (same pod) still reaches it via `localhost`. Works. (If it bound to a pod-internal interface only, it would not — but that's not how dev servers default.)
6. **Dev server's HMR WebSocket uses a different port than the HTTP server** (Vite does this — HTTP on 5173, WS on a separate path/port) → the dev server advertises the WS URL relative to the page; the browser follows it through the same `/dev-preview/:port/` prefix. A4 validates this end-to-end.
7. **Browser opens many parallel connections** (assets, fetch, WS) → the dev-preview uses its own connection pool, NOT the `maxConnectionsPerWorkspace=10` agent-session budget. Still bounded by a separate per-workspace dev-preview connection cap (operator-configurable, default 50) to prevent fan-out abuse.
8. **User toggles dev-preview off while a WS is open** → the CRD field change does not actively tear down existing connections in v1 (agentd has no watch on the CRD and doesn't re-check the flag per request — D3). The next HTTP request/WS reconnect fails at the API handler's opt-in check. Documented; v2 could add an active drain via a controller-triggered agentd reload.
9. **Cookie not sent** (user cleared cookies, browser blocked third-party cookies in some edge configuration) → 401; the frontend surfaces a re-login prompt. Same as any other authenticated route.
10. **Programmatic client (curl, MCP) tries the preview URL** → works via Bearer header. Documented. The dev-preview is not browser-only; it's just HTTP+WS.
11. **Dev server streams a response larger than the size cap** (e.g. a 1GB video file, or a large chunked stream) → connection torn down when the cap is exceeded. The cap MUST handle both response shapes: reject oversized `Content-Length` upfront, and count bytes through a `ResponseWriter` wrapper for `Transfer-Encoding: chunked` streams (A6 — Vite's small responses are sized, large/streaming responses are chunked; both are normal). The HMR WS itself is a long-lived upgrade and is exempt from the per-response body cap. Logged + metric'd.
12. **Recursion attempt** (`/dev-preview/4097/v1/dev-preview/5173/`) → 4097 is in the denylist; rejected.
13. **IPv6/localhost resolution mismatch** → browsers and Node resolve `localhost` → `::1` first (per RFC 6724 precedence); if a listener binds IPv4-only (`0.0.0.0`), the WS upgrade fails with `ECONNREFUSED ::1`. Discovered in the US-66.1 spike. **Mitigation:** agentd's user mux already binds `0.0.0.0:4097` (`pkg/agentd/types.go:54`), so the inner hop is dual-stack-safe. The outer hop (browser → API origin) depends on the operator's ingress binding — document that the API ingress must serve both A and AAAA, or the preview must be advertised under a hostname that resolves to the address the ingress binds. Not a code change in this epic; an operator-guide note (US-66.8).

---

## Non-Functional Requirements

### Security
- **Zero NetworkPolicy changes.** v1 reuses the existing API→pod:4097 allowance (`helm/templates/workspace-network-policy.yaml:33-48`). No new ingress rule for the workspace namespace.
- **No per-workspace Service or Ingress objects.** v1 is path-based on the existing API origin.
- **Auth inheritance.** Route lives on `idGroup`; inherits `AuthMiddleware` + `WorkspaceAccessMiddleware` with no overrides. No new credential type.
- **Port denylist at both layers.** API handler (rejects before proxying) and agentd forwarder (rejects independently). 4096/4097/4098 + <1024.
- **Response size cap.** Operator-configurable (`devPreview.maxResponseBytes`, default 50 MiB).
- **Connection pool isolation.** Dev-preview has its own pool; does not consume agent-session slots.
- **Opt-in.** CRD field `spec.networkAccess.devPreview` defaults to false. Enforced by the API handler on the request path (reads from CRD via K8s client). agentd does not re-check — the NetworkPolicy (only API pod reaches 4097) and `WorkspaceAccessMiddleware` (only the owner gets 200) are the two independent security gates. The opt-in flag is a configuration value, not a third security boundary.

### Scalability & Performance
- **Per-workspace dev-preview connection cap** (operator-configurable, default 50) — separate from the agent-session budget.
- **Per-route rate limit** via existing `middleware/per_route_rate_limit.go`.
- **Stateless API.** Each API replica can proxy to any workspace pod; no sticky-session problem.
- **ReverseProxy tuning.** Per-handler `Transport` with sensible timeouts (idle, read, write) — separate from the agent-proxy transport.

### Robustness
- **Stale-IP retry** on the dev-preview path (mirror `proxy.go:337-349`): on connection error, re-Get the CRD, retry once with the fresh `Status.PodIP`.
- **Graceful 502** on agentd unreachable or dev-server-not-listening — surfacing a helpful error rather than a raw proxy panic.
- **No goroutine leaks** on WS disconnect — `httputil.ReverseProxy` handles hijack cleanup; test explicitly.

---

## Observability

Prometheus metrics (added per-component):
- `devpreview_requests_total{workspace_id, status}` counter
- `devpreview_websocket_connections_active` gauge
- `devpreview_response_bytes{workspace_id}` histogram (with the size cap as a documented upper bound)
- `devpreview_connection_refusals_total{reason}` counter — `port_denied`, `not_opted_in`, `workspace_not_active`, `size_cap_exceeded`

Audit log entries:
- Dev-preview toggle on/off (actor, workspace, before/after) — reuse the existing audit-log pattern.

SSE events: none. The dev-preview is a pass-through HTTP/WS path; no platform-level events are produced.

---

## Stories

| Story | Title | Effort | Depends On |
|---|---|---|---|
| US-66.1 | Validation spike: httputil.ReverseProxy end-to-end WS forwarding + Host-rewrite + cookie transport | ✅ Closed | None — A1/A4/A6 validated 2026-08-10 |
| US-66.2 | CRD field + webhook + deepcopy: `WorkspaceNetworkAccess.DevPreview bool` | 0.5d | None |
| US-66.3 | agentd in-pod forwarder: `/v1/dev-preview/:port/*` handler on user mux, port denylist, `httputil.ReverseProxy` to localhost | 1.5d | US-66.1 |
| US-66.4 | API dev-preview handler: route on `idGroup`, port denylist, opt-in check, stale-IP retry, separate connection pool + transport, response size cap, rate limit | 2d | US-66.2, US-66.3 |
| US-66.5 | Governance: instance settings (`devPreview.enabled` kill-switch, `maxResponseBytes`, per-workspace connection cap), audit log of toggle | 0.5d | US-66.2 |
| US-66.6 | Frontend: dev-preview toggle in workspace settings drawer + "Open preview" affordance (port prompt) | 1.5d | US-66.4 |
| US-66.7 | E2E integration: full wired path (toggle on → start Vite → load preview → HMR works → toggle off → 503) + adversarial cases (wrong owner, port in denylist, size cap, recursion) | 1.5d | US-66.4, US-66.6 |
| US-66.8 | Operator documentation: dev-preview user guide + recommended `cloudflared`/`ngrok` companion pattern for users who want stable shareable URLs | 0.5d | US-66.7 |

Total estimated effort: ~9 days.

---

## Dependency Graph

```
US-66.1 (spike: WS + cookie transport) ✅ **CLOSED 2026-08-10** — A1/A4/A6 validated, A2/A3/A5 codebase-verified
   │
   ├──> US-66.3 (agentd in-pod forwarder)   ─── unblocked
   │
   └──> US-66.4 (API dev-preview handler)   ─── unblocked
              ▲
              │
US-66.2 (CRD field + webhook) ───────────┘
              │
              └──> US-66.5 (governance: kill-switch + caps + audit)

US-66.4 ──> US-66.6 (frontend) ──> US-66.7 (e2e)
US-66.7 ──> US-66.8 (operator doc)
```

US-66.2 (CRD field — pure schema, no behavior) can start immediately. US-66.3 (agentd forwarder) and US-66.4 (API handler) are unblocked now that the spike closed A1/A4.

---

## Execution Strategy

**Phase 0 — De-risk:** ✅ **CLOSED** (US-66.1 spike, 2026-08-10). Closed A1 (HMR through ReverseProxy proven — both `full-reload` and `js-update`), A4 (Host rewrite mandatory — Vite ≥5.4.13/CVE-2025-30208), A6 (both Content-Length and chunked shapes normal). A2/A3/A5 remain codebase-verified (the spike ran on a non-workspace VM; these are static code properties, not blocking). See `PREVIEW-CONTRACT.md`.

**Phase 1 — Foundation:** US-66.2 (CRD field + webhook + deepcopy) and US-66.3 (agentd in-pod forwarder). Backend-only, fully tested. End of Phase 1: an authenticated curl to `podIP:4097/v1/dev-preview/5173/` returns the dev server's response.

**Phase 2 — The API wire:** US-66.4 (API handler on `idGroup`) + US-66.5 (governance). End of Phase 2: a browser hitting the API route sees the proxied dev server, HMR works.

**Phase 3 — UX + closure:** US-66.6 (frontend toggle + open-preview affordance), US-66.7 (e2e), US-66.8 (operator doc). End of Phase 3: the full human-workable, end-to-end-verified feature.

Each phase ends with `make test && make build && make lint` green and a worklog entry. No phase skips the validator loop (README-LLM.md Multi-Agent Workflow).

---

## Per-Story Detail

### US-66.1: Validation spike — WS forwarding + Host rewrite + cookie transport ✅ CLOSED

**Status:** Closed 2026-08-10. Artifact: `PREVIEW-CONTRACT.md` (captured in this epic's folder). Closed A1 (HMR through ReverseProxy proven), A4 (Host rewrite mandatory — Vite ≥5.4.13/CVE-2025-30208 returns 403 otherwise), A6 (both Content-Length and chunked response shapes are normal; cap must handle both). A2/A3/A5 remain codebase-verified, not in-pod validated — the spike ran on a non-workspace VM (no agentd listening); these are static properties of code already read and do not block implementation.

**Original goal:** Convert A1–A6 from "believed" to "validated with evidence." Produce the exact proxy rewrite US-66.4 implements, the cookie-transport confirmation, and the WS-end-to-end HMR demonstration.

**Original deliverables (for reference):**
1. `PREVIEW-CONTRACT.md` — the exact `httputil.ReverseProxy` `Rewrite` function (specifically: the `r.Out.Host` rewrite to `localhost:<port>`, the `r.SetURL(target)` for scheme+host), with captured request artifacts.
2. Confirmation that `httputil.ReverseProxy` forwards a WS `Upgrade` through API → agentd → dev server with no custom `Hijacker` code (A1). ✅ Both `full-reload` and `js-update` HMR events traversed.
3. The Vite HMR round-trip: file edit in workspace → browser updates without manual reload (A1). ✅
4. Cookie transport: open the preview URL in a fresh tab; confirm 200 (not 401) when the `lsp_session` cookie is present (A2). ⚠️ Codebase-verified only.
5. The `Host` rewrite determination (A4). ✅ **Mandatory** — Vite ≥5.4.13 returns 403 for any foreign Host.
6. Auth-gate-before-upgrade confirmation (A5). ⚠️ Codebase-verified only.
7. Size-cap-on-streams behavior (A6). ✅ Both shapes normal; cap must handle both.

**Open issue surfaced (not blocking):** browsers resolve `localhost` → `::1` first; agentd already binds `0.0.0.0` (inner hop safe), but the operator's API ingress binding matters for the outer hop. Captured as Edge Case 13 + US-66.8 operator note.

### US-66.2: CRD field + webhook + deepcopy

**Goal:** Land the schema. No behaviour yet.

**Deliverables:**
- Add `DevPreview bool` to `WorkspaceNetworkAccess` in `pkg/apis/llmsafespaces/v1/workspace_types.go:51-56`. `+kubebuilder:default=false`.
- `make deepcopy` regenerates `zz_generated.deepcopy.go`.
- Validating webhook: if `spec.networkAccess.devPreview` is true, no additional constraints in v1 (operator kill-switch in US-66.5 is the gate). Confirm the CRD schema is regenerated and the controller picks up the new field.
- **No PostgreSQL migration.** NetworkAccess is CRD-only today — confirmed by grep across all SQL migrations (no `network_access` column exists). The API handler reads `devPreview` from the CRD via the K8s client on the request path, same as `proxy.go` reads `Status.Phase` and `Status.PodIP` (`proxy.go:237-245`).

**Acceptance:** `kubectl apply` of a workspace with `devPreview: true` succeeds; `kubectl get workspace -o yaml` shows the field; deepcopy tests pass.

### US-66.3: agentd in-pod forwarder

**Goal:** `GET /v1/dev-preview/:port/*` handler on agentd's user mux (port 4097), forwarding to `localhost:<port>`.

**Deliverables:** `cmd/workspace-agentd/dev_preview.go`. Register on `userMux` in `server.go:wireHTTPServers` alongside the existing `/v1/reload-secrets`, `/v1/workflow/*`, `/v1/mcp` routes (no collision — A3 codebase-verified). **Auth pattern: Basic auth with the workspace password** — identical to every other user-mux handler. The handler takes `deps.password` (same as `workflowExecuteHandler(deps.password)` at `server.go:246`) and validates `r.Header.Get("Authorization") != "Basic "+basicAuth(password)` (the exact pattern at `workflow_execute.go:130`). The API proxy injects the password via `req.SetBasicAuth("opencode", password)` (the exact pattern at `proxy.go:508`). **NOT** `requireBearerToken` — that wrapper is admin-mux-only (`server.go:198, 208`); the user mux has never used it. Parse `:port` as int; reject non-numeric, out of range, in denylist (4096/4097/4098 + <1024). `httputil.ReverseProxy` with a `Rewrite` function (Go 1.21+) that sets `r.SetURL(target)` and **`r.Out.Host = "localhost:" + port`** (A4 closed — Host rewrite is mandatory; Vite ≥5.4.13 returns 403 otherwise). **No custom Hijack code** — A1 proved stdlib ReverseProxy forwards WS upgrades (both `full-reload` and `js-update` HMR events traverse cleanly). **No opt-in re-check in agentd** — agentd has no K8s client and no mechanism to read CRD fields; the API handler is the sole enforcer of the opt-in flag (D3). This is consistent with the terminal proxy precedent (US-14.2 does not re-check CRD fields in-pod either).

**Tests:** round-trip HTTP, round-trip WS upgrade, port-denied (each denylist entry), port-out-of-range, non-numeric port, dev-server-not-listening (502), wrong-password (401), recursion-attempt (4097 denied). Integration test against a real workspace with a real dev server (per US-66.1 contract).

### US-66.4: API dev-preview handler

**Goal:** `GET /api/v1/workspaces/:id/dev-preview/:port/*` on `idGroup`, proxying to `podIP:4097/v1/dev-preview/:port/*`.

**Deliverables:** `api/internal/handlers/dev_preview.go`. Register on `idGroup` (inherits `AuthMiddleware` + `WorkspaceAccessMiddleware`). Parse `:port`; reject denylist + out of range. Check `workspace.Status.Phase == Active` and `Status.PodIP != ""` (mirror `proxy.go:245`). Check `spec.networkAccess.devPreview == true` (D3) — 503 with clear message if false. Read this from the CRD via the K8s client (same Get call that resolves PodIP — no separate fetch). `httputil.ReverseProxy` to `http://<podIP>:4097/v1/dev-preview/<port>/<path>`; **rewrite `Host` to `localhost:<port>` in the `Rewrite` function** (A4 closed — mandatory for Vite ≥5.4.13/CVE-2025-30208, harmless for python/serve; `r.Out.Host = "localhost:" + port`, not just `r.SetURL`); **inject Basic auth** `req.SetBasicAuth("opencode", password)` (the password comes from the K8s Secret `workspace-pw-<id>`, same cache the existing proxy uses — `proxy_connections.go:20-42`). Stale-IP retry: on connection error, re-Get CRD, retry once with fresh `Status.PodIP` (mirror `proxy.go:337-349`). **Extract the stale-IP retry logic into a shared helper** (e.g. `api/internal/handlers/proxy_helpers.go` — `RetryOnceOnStalePodIP(k8sClient, namespace, name, fn)`) and consume it from both `proxy.go` and the dev-preview handler. Two concrete consumers now validates the extraction (Rule 4 — the abstraction earns its cost). **Separate `http.Transport`** with its own connection pool — NOT the agent-proxy transport. Per-workspace dev-preview connection cap (default 50, operator-configurable via US-66.5). Response size cap (default 50 MiB) enforced on **both** response shapes (A6 closed): reject oversized `Content-Length` upfront, AND wrap the `ResponseWriter` to count bytes on `Transfer-Encoding: chunked` streams (tear down on overflow, never panic). The HMR WS itself is exempt from the per-response body cap. Per-route rate limit. Helpful 502 page on dev-server-not-listening.

**Tests:** happy HTTP, happy WS (HMR round-trip), stale-IP retry, workspace-not-active (503), opt-in-false (503), port-denied (400), wrong-owner (403 via inherited middleware), no-cookie (401 via inherited middleware), size-cap-exceeded (connection tear-down), connection-cap-exceeded (429 or 503), recursion-port (400).

### US-66.5: Governance — kill-switch + caps + audit

**Goal:** Operator controls + audit of toggle.

**Deliverables:** Register three new `instance_settings` keys in `pkg/settings/registry.go`:
- `devPreview.enabled` (default true) — global kill-switch. When false, the API handler returns 503 for all workspaces regardless of CRD field.
- `devPreview.maxResponseBytes` (default 52428800 = 50 MiB).
- `devPreview.maxConnectionsPerWorkspace` (default 50).

Audit-log the toggle (actor, workspace, before/after) — reuse the existing audit-log pattern (`org_sso.go` is the precedent). The toggle is a workspace-spec mutation, so it already flows through the existing update path; this story adds the audit entry.

### US-66.6: Frontend — toggle + open-preview affordance

**Goal:** A user-facing surface for the feature.

**Deliverables:** React components under `frontend/src/components/workspace/`. A "Dev Preview" toggle in the workspace settings drawer (calls the existing workspace-update endpoint to flip `spec.networkAccess.devPreview`). An "Open preview" affordance in the workspace header that prompts for a port (default 5173) and opens `https://<api-origin>/api/v1/workspaces/:id/dev-preview/<port>/` in a new tab. Surface a "kill-switch disabled by operator" state when `devPreview.enabled` is false (read from a platform-info endpoint or instance-settings surface). Follow existing frontend conventions.

### US-66.7: E2E integration

**Goal:** Prove the full wired path end-to-end, including adversarial cases.

**Deliverables:** integration tests under `tests/`. Scenarios: (1) toggle on → start Vite in workspace → load preview URL → page returns 200 → edit file → HMR pushes update (browser-side assertion); (2) toggle off → same URL → 503; (3) wrong owner → 403; (4) no cookie → 401; (5) port in denylist (4096, 4098, 80) → 400; (6) size cap exceeded → connection tear-down; (7) recursion port (4097) → 400; (8) dev-server-not-listening → 502 with helpful message; (9) stale-IP retry on pod restart. No unwired code (per PR Review Guide E2E Wiring Verification).

### US-66.8: Operator documentation + cloudflared/ngrok companion note

**Goal:** A user-facing guide + a companion note for the shareable-URL escape hatch.

**Deliverables:**
- `docs/user/dev-preview.md` — what the feature does, how to enable it, common dev server configs (the per-framework table below), the HMR support note, the suspend/resume caveat (Edge Case 2), and the IPv6/ingress-binding note (Edge Case 13).
- **Per-framework user config table** (from A4 spike evidence):
  | Framework | Bind | `allowedHosts`/`allowedDevOrigins` needed? | HMR config |
  |---|---|---|---|
  | **Vite ≥5.4.13 / 6.0.12 / 7.x** | `--host 0.0.0.0` or `server.host: true` | **No** — proxy rewrites `Host` to `localhost:<port>` | None — HMR WS is same-origin via the proxy |
  | **Vite <5.4.13** | same | No (no Host check) | None |
  | **Next.js** | `--hostname 0.0.0.0` | No — proxy Host rewrite covers it | None |
  | **python `-m http.server`** | `--bind 0.0.0.0` or `127.0.0.1` | No (ignores Host) | n/a |
  | **`npx serve`** | default | No (ignores Host) | n/a |
- A section (in the same doc or a linked operator guide) documenting the **recommended companion pattern** for users who want stable, shareable URLs: running `cloudflared tunnel --url localhost:5173` or `ngrok http 5173` inside the workspace (workspace egress already allows public HTTPS). Explicitly states the platform does not provide public URLs in v1 and why (D7).
- **Operator note on ingress binding** (Edge Case 13): the API ingress must serve both A and AAAA records (or bind dual-stack), because browsers resolve `localhost`/hostnames to `::1` first and a v4-only listener will refuse the WS upgrade.

---

## Out of Scope (deferred with rationale)

| Item | Reason |
|---|---|
| Public / shareable preview URLs (Option D — per-workspace Ingress + wildcard DNS) | D7 — categorically different product; requires trust-and-safety infrastructure the platform does not staff (abuse intake, content scanning, takedown workflow, ToS). v2 layers this on top of B's in-pod forwarder if/when the product decision is made. |
| Per-workspace subdomain routing via Epic 54 machinery | D7 — same rationale as above; only relevant for public/shareable URLs. |
| Raw-TCP port forwarding (databases, SSH, binary protocols) | D6 — different feature, different failure modes; the platform already blocks `pods/portforward` in hardened RBAC, and re-introducing it via the API should re-examine that reasoning. |
| CRD port allowlist (`spec.devPorts: [5173]`) | D4 — owner already has equivalent localhost reachability via terminal proxy; allowlist adds cost for no security benefit. Operator-wide port-range allowlist is a v2 instance setting if abuse patterns emerge. |
| Reverse-tunnel model (agentd dials out) | D5 — strictly worse than B in this environment. |
| Iframe embedding of the preview in the SPA | SameSite=Lax cookie default blocks cross-origin iframe auth; reworking cookie scope is its own decision. Defer until embedding is demanded. |
| Active connection drain on opt-out toggle | Edge Case 8 — v1 lets in-flight connections finish; next reconnect fails. v2 could actively tear down. |
| WS migration across pod suspend/resume | Edge Case 2 — v1 lets the WS drop; user reconnects after resume. v2 could provide a transparent reconnect layer (significant complexity for low value). |
| Preview of in-workspace-agent-authored content (agent opens a browser) | The agent cannot open a browser; the human can. Preview is a human-driven affordance. |
| org-member-shared previews (preview visible to other org members, not just the owner) | v1 is owner-only (inherits `WorkspaceAccessMiddleware`). Org-member sharing is a separate authorization decision and depends on Epic 43 org-access design. |
| Per-framework dev-server auto-configuration (auto-detect Vite/Next and set `server.host`) | v1 documents the required config; auto-detection is speculative until usage patterns are observed. |

---

## Open Questions

1. **Default per-workspace dev-preview connection cap.** 50 is a placeholder; tune after load testing. Browser asset fan-out for a typical React app is ~20-30 parallel connections; HMR adds 1 WS.
2. **Should the response size cap apply to streaming responses (chunked transfer-encoding)?** A6 validates behavior. v1 likely tears down the connection on cap exceedance for chunked streams; document this as a known limitation for users serving large media from a dev server.
3. **Can the dev-preview handler share the existing workspace-password cache** (`proxy_connections.go:20-42`), or does it need its own fetch? The password is the same K8s Secret (`workspace-pw-<id>`); the cache is per-ProxyHandler. If dev-preview is a separate handler struct, it either shares the cache via injection or does its own Get. Resolve during US-66.4 — prefer cache injection to avoid redundant K8s API calls.

---

## Reference

This design was developed through a multi-step threat-modeling and option-assessment conversation (2026-08-10) covering: the four-actor threat model (external attacker, honest owner, curious tenant, malicious owner); five architectural options (API-direct, agentd-mediated, reverse-tunnel, per-workspace Ingress, do-nothing); the product-shape-driven conclusion that an authenticated metered transient-workspace SaaS is not a hosting provider; and the JWT-cookie realization that collapsed both the security concern (no unauthenticated surface) and the implementation concern (browser navigation works without capability tokens). The "future proxy" comments at `pkg/agentd/types.go:53` and `cmd/workspace-agentd/server.go:190` — written long before this epic — name port 4097's user mux as the designated home for exactly this kind of endpoint.

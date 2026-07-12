# Epic 26: Client-Proxied Inference for Free Models

**Status:** ⛔ Superseded by Epic 60 (2026-07-12)
**Priority:** Medium
**Depends On:** Epic 10 (Multi-Tenant Trust & Secret Management)
**Motivation:** Enable free-tier LLM models at scale without platform IP throttling/banning

> **Superseded by [Epic 60](../epic-60-remove-cf-worker-relay/README.md) (2026-07-12).** Zen (opencode.ai/zen/v1) now blocks all Cloudflare Worker egress IPs. The CF Worker relay at `relay.safespaces.dev` is unreachable from any workspace pod, and the chart default `inferenceRelayURL: https://relay.safespaces.dev` actively breaks fresh installs (#474: every free-tier request returns 403, opencode interprets that as a credential failure, the agent restart-loops). Epic 60 removed the Worker, the `relay-secret-sync` Helm Job, the `inferenceRelayURL`/`inferenceRelaySecret`/`cloudflare.*` chart values, the `--inference-relay-secret` controller flag, and the `INFERENCE_RELAY_SECRET` env var. The surviving relay path is the self-hosted InferenceRelay fleet (Epic 42); the new default is direct-to-Zen.

> **Historical — Architecture Pivot (2026-06-05, worklog 0155):** The original WebSocket relay design was replaced by a 37-line Cloudflare Worker. US-26.1–26.6 (WebSocket relay infrastructure) were built and then deleted in PR #35. US-26.7 is superseded — its Tasks A-E targeted the deleted relay code and no longer apply. The implementation shipped at the time: controller injected `OPENCODE_AUTH_CONTENT` with `metadata.baseURL: "https://relay.safespaces.dev"` at pod creation; the CF Worker proxied requests from Cloudflare's edge POPs. Epic goal achieved at the time; the architecture became unreachable 5 weeks later when Zen started blocking CF Worker IPs.

---

## Problem Statement

### Current State

opencode ships with a built-in `opencode` provider that offers free models (zero-cost models from `models.dev` catalog). When no API key is configured, the opencode plugin sets `apiKey: "public"` and disables paid models, leaving only free ones available.

In the current architecture, ALL LLM API calls originate from the LLMSafeSpace server cluster:

```
User Browser → LLMSafeSpace API → Workspace Pod (opencode) → Provider API (opencode.ai)
                                                                    ↑
                                                            All traffic from
                                                            our server IPs
```

At scale this means:
- Every free-tier user's requests come from the same set of server IPs
- Rate limits and abuse detection are per-IP, not per-user
- 100 concurrent free-tier users all sharing the same IP pool = throttled/banned for everyone
- The platform bears the compute and bandwidth cost of proxying all LLM traffic

### Desired State

Free-model traffic is proxied through each user's own client (browser or SDK), so:
- Each user's requests appear to come from their own IP
- Rate limits apply per-user naturally (different source IPs)
- Platform server doesn't carry the bandwidth/compute for free model streaming
- Paid providers (user-supplied API keys) continue to go direct from server (lower latency, key security)

---

## Key Insight: opencode's Free Models

opencode's model catalog comes from `https://models.dev/api.json`. The `opencode` provider plugin (`packages/core/src/plugin/provider/opencode.ts`):

1. If no API key/env/account is set → sets `apiKey: "public"`
2. Disables all models with `cost.input > 0` (paid models hidden)
3. Free models (cost.input === 0) remain enabled

These free models route through opencode's inference gateway (`opencode.ai`). The `opencode` provider uses an `aisdk` endpoint type with an opencode-specific SDK package.

**Examples of free models** (from models.dev, cost=0):
- Models offered by the opencode provider at zero cost
- Typically community/open models proxied through opencode's gateway

---

## Architecture: Client-Proxied Inference

### Core Concept

Instead of the server making the HTTP call to the LLM provider, the server delegates the HTTP call to the client. The client makes the actual network request and streams the response back to the server, which feeds it to opencode.

### Protocol

```
┌─────────┐         ┌──────────────┐         ┌───────────┐
│  Client  │         │ LLMSafeSpace │         │  opencode  │
│(Browser/ │         │   Server     │         │  (in pod)  │
│   SDK)   │         │              │         │            │
└────┬─────┘         └──────┬───────┘         └─────┬──────┘
     │                      │                       │
     │  1. User sends prompt│                       │
     │─────────────────────>│  2. Forward to agent  │
     │                      │──────────────────────>│
     │                      │                       │
     │                      │  3. Agent needs to    │
     │                      │     call LLM API      │
     │                      │<──────────────────────│
     │                      │                       │
     │  4. Proxy request    │                       │
     │     (method, url,    │                       │
     │      headers, body)  │                       │
     │<─────────────────────│                       │
     │                      │                       │
     │  5. Client makes     │                       │
     │     HTTP request to  │                       │
     │     provider API     │                       │
     │─────────[network]────────────────────────────────> Provider API
     │                      │                       │
     │  6. Stream response  │                       │
     │     chunks back      │                       │
     │─────────────────────>│  7. Feed to agent     │
     │                      │──────────────────────>│
     │                      │                       │
     │  8. Agent processes, │                       │
     │     emits events     │                       │
     │<─────────────────────│<──────────────────────│
     │                      │                       │
```

### Transport: WebSocket Relay Channel

A dedicated WebSocket connection between client and server carries proxy requests:

```
Client ←──WebSocket──→ Server (relay endpoint)
```

Messages on this channel:

**Server → Client (proxy request):**
```json
{
  "type": "proxy_request",
  "id": "req_abc123",
  "method": "POST",
  "url": "https://opencode.ai/v1/chat/completions",
  "headers": {
    "content-type": "application/json",
    "authorization": "Bearer public"
  },
  "body": "{\"model\":\"...\",\"messages\":[...]}"
}
```

**Client → Server (proxy response, streamed):**
```json
{"type": "proxy_response_start", "id": "req_abc123", "status": 200, "headers": {"content-type": "text/event-stream"}}
{"type": "proxy_response_chunk", "id": "req_abc123", "data": "data: {\"choices\":[...]}\n\n"}
{"type": "proxy_response_chunk", "id": "req_abc123", "data": "data: {\"choices\":[...]}\n\n"}
{"type": "proxy_response_end", "id": "req_abc123"}
```

**Client → Server (proxy error):**
```json
{"type": "proxy_error", "id": "req_abc123", "error": "CORS blocked", "status": 0}
```

### Decision: Which Traffic Gets Proxied?

| Provider | API Key | Proxy Through Client? | Rationale |
|----------|---------|----------------------|-----------|
| `opencode` | `"public"` (free tier) | **Yes** | Avoid platform IP throttling |
| Any provider | User-supplied key | **No** (server-direct) | Lower latency, key never sent to client |
| `opencode` | User-supplied paid key | **No** (server-direct) | Paid user, no throttle risk |

The proxy decision is made by the server when it intercepts the outgoing HTTP request from opencode. If the target is the opencode provider with `apiKey: "public"`, route through client. Otherwise, make the call directly.

---

## Implementation Layers

### Layer 1: Custom HTTP Transport for opencode (in-pod)

A custom transport layer that intercepts opencode's outgoing HTTP calls and routes them to the relay channel instead of making them directly.

**Location:** `cmd/workspace-agentd/` or a new `pkg/agentd/proxy/` package

**Mechanism:** opencode uses the `ai-sdk` which uses Node.js `fetch`. We can intercept at the environment level:
- Option A: Custom `HTTP_PROXY`/`HTTPS_PROXY` env var pointing to a local proxy in the pod
- Option B: Patch opencode's fetch via `--experimental-fetch` or custom global
- Option C: Add a transport plugin to opencode (upstream contribution)

**Recommended: Option A (local proxy in pod).** The agentd already runs alongside opencode. Add an HTTP proxy mode that:
1. Receives the outgoing request from opencode (via standard HTTPS_PROXY)
2. Checks if it should be client-proxied (free tier detection)
3. If yes: holds the connection open, sends the request over the relay WebSocket to the client
4. Streams the response back from the WebSocket into the HTTP response to opencode
5. If no: makes the request directly (pass-through)

### Layer 2: WebSocket Relay Channel (API server)

A new WebSocket endpoint on the API server:

```
GET /api/v1/workspaces/:id/relay
Upgrade: websocket
```

This maintains a bidirectional channel between:
- The workspace pod's agentd (connects as "provider")
- The user's client (connects as "consumer")

Messages from agentd (proxy requests) are forwarded to the client. Messages from the client (proxy responses) are forwarded back to agentd.

### Layer 3: Client SDK / Browser Implementation

**Browser:**
- JavaScript/TypeScript SDK that connects to the relay WebSocket
- Receives `proxy_request` messages
- Uses browser `fetch()` to make the actual HTTP call to the provider
- Streams the response back as `proxy_response_chunk` messages

**SDK (Python/Node/Go):**
- Same protocol, just using the SDK's HTTP client instead of browser fetch

### Layer 4: Free Model UX Annotation

When `ListModels` returns the model catalog, annotate models from the `opencode` provider with free-tier:

```json
{
  "id": "opencode/some-model",
  "providerID": "opencode",
  "name": "Some Model",
  "tier": "free",
  "proxyRequired": true,
  "note": "Free model — requests proxied through your browser"
}
```

The frontend uses `proxyRequired: true` to:
1. Ensure the relay WebSocket is connected before allowing selection
2. Show a UI indicator that this model routes through the client
3. Warn if the client goes offline mid-conversation

---

## User Stories

### US-26.1: Local HTTP Proxy in agentd

**Goal:** Intercept outgoing HTTP from opencode and route free-tier requests to the relay channel.

**Scope:**
- HTTP CONNECT proxy running on localhost in the pod
- `HTTPS_PROXY=http://localhost:{port}` env var set for opencode
- Detection logic: if target host is `opencode.ai` (or the opencode provider gateway) AND no paid API key → proxy
- Otherwise: CONNECT pass-through (direct)
- Buffer: hold requests pending client connection for up to 5s, then fail with 503

### US-26.2: WebSocket Relay Endpoint (API server)

**Goal:** Bidirectional WebSocket relay between agentd and client.

**Scope:**
- `GET /api/v1/workspaces/:id/relay` endpoint
- Auth: same JWT/API key as other endpoints
- Two participants per workspace: agentd (pod) and client (browser/SDK)
- Message routing: agentd→client for requests, client→agentd for responses
- Heartbeat/keepalive (30s ping)
- Graceful degradation: if client disconnects, pending requests get 503
- Multiple concurrent requests supported (multiplexed by request ID)

### US-26.3: Browser Relay Client

**Goal:** JavaScript/TypeScript library that handles the client side of the relay.

**Scope:**
- Connect to relay WebSocket
- Receive proxy requests, execute via `fetch()`
- Stream responses back (support SSE/chunked transfer)
- Handle CORS: if provider blocks browser requests, report error
- Reconnection logic with exponential backoff
- npm package: `@llmsafespace/relay-client`

### US-26.4: SDK Relay Client

**Goal:** Go/Python/Node SDK support for relay proxying.

**Scope:**
- Same protocol as browser
- SDK makes HTTP calls using native HTTP client (no CORS issues)
- Integrated into existing LLMSafeSpace SDK
- Automatic detection: if workspace has free models, connect relay

### US-26.5: Model Tier Annotation

**Goal:** API returns tier/proxy metadata with model list.

**Scope:**
- `GET /api/v1/workspaces/:id/models` includes `tier` and `proxyRequired` fields
- Detection: models from `opencode` provider with public key → `tier: "free", proxyRequired: true`
- Frontend shows indicator for free/proxied models

### US-26.6: CORS Fallback

**Goal:** Handle the case where provider APIs block browser requests.

**Scope:**
- If browser `fetch()` fails due to CORS, report `proxy_error` with CORS reason
- Server falls back to making the request directly (accepting the rate-limit risk)
- Rate-limit the fallback per-user (e.g., 10 requests/minute server-side for free tier)
- UI shows: "Your browser couldn't reach the model provider directly. Using server proxy (rate limited)."

---

## Trade-offs

| Dimension | Client-Proxied | Server-Direct (current) |
|-----------|---------------|------------------------|
| Latency | +50-200ms (extra WebSocket hop) | Lowest |
| Server bandwidth | Zero (client carries LLM traffic) | Full streaming through server |
| Rate limiting | Per-user IP (natural) | Per-platform IP (throttled) |
| Scalability | Linear with users | Bottlenecked on server IPs |
| Client offline | Requests fail | Always works |
| CORS issues | Possible (fallback exists) | None |
| Complexity | High (relay protocol) | Low (direct HTTP) |

**Acceptable because:** This only applies to free models where users accept lower reliability for zero cost.

---

## Security Considerations

1. **No secrets in proxy requests.** Free-tier uses `apiKey: "public"` — nothing sensitive is sent to the client. If a user has a paid key, that traffic never touches the relay.

2. **Request validation.** The server must validate that proxy requests only target allowed hosts (the opencode provider gateway). A malicious agentd must not be able to use the client as an open proxy.

3. **Response integrity.** The server should validate that proxy responses are well-formed HTTP (status code, headers, body). A malicious client must not be able to inject arbitrary data into the opencode response stream.

4. **DoS protection.** Rate-limit the number of proxy requests per workspace (e.g., 60/minute). A runaway agent must not flood the client with proxy requests.

---

## Non-Requirements (Out of Scope)

1. **Paid provider proxying** — Never. Paid API keys stay server-side.
2. **Provider API key rotation** — Separate concern (Epic 10).
3. **Multiple simultaneous clients** — One relay client per workspace for now.
4. **Offline queue** — If client disconnects, requests fail immediately. No store-and-forward.
5. **WebRTC** — WebSocket is sufficient; no need for peer-to-peer.

---

## Dependency Graph

```
US-26.1 (Local Proxy in agentd) ──────┐
                                        │
US-26.2 (WebSocket Relay Endpoint) ────┼──→ US-26.3 (Browser Client)
                                        │         │
                                        │         ▼
                                        ├──→ US-26.4 (SDK Client)
                                        │
                                        ▼
                                  US-26.5 (Model Tier Annotation)
                                        │
                                        ▼
                                  US-26.6 (CORS Fallback)
```

**Critical path:** US-26.1 + US-26.2 + US-26.3 (minimum for browser-based free models)

---

## Open Questions

1. **Does opencode.ai's free API support CORS?** If yes, browser `fetch()` works directly. If no, the CORS fallback (US-26.6) becomes critical from day one rather than an edge case.

2. **Can we use `HTTPS_PROXY` with opencode?** opencode uses Node.js `fetch` via the AI SDK. Node's `fetch` respects `HTTPS_PROXY` via `node --experimental-global-fetch` but behavior varies. Needs validation.

3. **Alternative: opencode transport plugin.** Instead of an HTTP proxy, could we contribute a custom AI SDK transport to opencode that delegates to a Unix socket? This would be cleaner but requires upstream acceptance.

4. **Rate limit signals.** When the free provider rate-limits the client, how does that signal propagate back to opencode? The proxy response will be a 429 — opencode's retry logic should handle this, but needs testing.


---

## US-26.7: Relay Activation — Critical Fixes for Production Functionality

**Status:** ~~Open~~ **SUPERSEDED** — Architecture pivot to Cloudflare Worker (worklog 0155, 2026-06-05)
**Priority:** N/A
**Depends On:** N/A

### Why Superseded

After this story was written (worklog 0153), a full architectural pivot occurred (worklog 0155, PR #35). The entire WebSocket relay system (~4000 lines: `relay_proxy.go`, `relay_handler.go`, `relay_config.go`, `useRelayClient.ts`, etc.) was **deleted** and replaced with a 37-line Cloudflare Worker.

**New architecture:**
```
Workspace Pod (opencode) → https://relay.safespaces.dev → CF Worker → opencode.ai/zen/v1
```

The controller injects `OPENCODE_AUTH_CONTENT` with `metadata.baseURL: "https://relay.safespaces.dev"` at pod creation time (no runtime push needed). The CF Worker is deployed separately via Wrangler and routes from Cloudflare's 300+ edge POPs, providing natural IP diversity.

**Why the pivot resolved all 5 issues in this story:**
1. Basic Auth → N/A (no relay push calls exist; `pushRelayBaseURL`/`isFreeTierModel` deleted)
2. DisposeInstance after push → N/A (no runtime push; `baseURL` injected at pod creation)
3. No boot-time activation → N/A (controller injects at pod creation, not at model selection)
4. CORS blocks browser relay → N/A (CF Worker is server-side; browser never makes direct opencode.ai calls)
5. No CORS fallback → N/A (same as #4)

**Tasks A-E below are preserved for historical context only. All have been rendered moot by the pivot.**

---

### [HISTORICAL] Original Problem (superseded)

The relay system was structurally complete but operationally non-functional in production. Five validated issues prevented it from working:

1. **Basic Auth missing on relay push** — `isFreeTierModel` and `pushRelayBaseURL` call opencode without Basic Auth → 401 → relay never activates via model selection.

2. **No DisposeInstance after baseURL push** — `PUT /auth/opencode` writes to auth.json but opencode's in-memory provider state is NOT refreshed without a subsequent `POST /instance/dispose` (validated: `PushCredentials` docstring in `pkg/agent/opencode/client.go` explicitly states this).

3. **No boot-time activation** — The relay baseURL is only pushed from `SetModel`. Fresh workspaces with free-tier default never push the baseURL → opencode routes directly to opencode.ai.

4. **CORS blocks browser relay** — `api.opencode.ai` does NOT return `Access-Control-Allow-Origin` headers (validated: `curl -sI` returns no CORS headers). Browser `fetch()` will ALWAYS fail with TypeError for opencode.ai requests. The relay's browser execution path is fundamentally broken.

5. **No automatic CORS fallback in agentd** — When the browser reports `proxy_error` (CORS), agentd returns 502 to opencode. There is no automatic fallback to server-side proxying. The existing fallback handler (`POST /relay/fallback`) is a browser-initiated endpoint, not an agentd failover.

### Validated Assumptions

| # | Assumption | Validation Evidence |
|---|---|---|
| V1 | opencode requires Basic Auth on ALL endpoints | Worklog 0127: "opencode 1.2.27+ requires Basic auth on all HTTP endpoints" |
| V2 | `baseURL` in `provider.options.baseURL` works for routing | Worklog 0128: validated live — "request went to `https://ai.thekao.cloud/v1` (NOT api.openai.com)" |
| V3 | `PUT /auth` writes auth.json but does NOT refresh provider state | `pkg/agent/opencode/client.go:55-57`: "does NOT trigger provider state refresh — call DisposeInstance afterward" |
| V4 | `DisposeInstance` at boot is safe (no active streams) | Validated: agentd starts, opencode becomes ready, no sessions exist yet |
| V5 | `api.opencode.ai` does NOT support CORS | `curl -sI https://api.opencode.ai/v1/models` returns no `Access-Control-Allow-Origin` header (tested 2026-06-05) |
| V6 | `passwordGetter` is already wired on `SecretsHandler` | Worklog 0152: "passwordGetter wired from proxy's K8s-secret-backed getter" |
| V7 | Existing `OpenCodeClient.PushCredentials` + `DisposeInstance` is the proven pattern | Used successfully for credential injection (Epic 10, worklog 0127/0128) |

### Architecture Decision: Server-Side Fallback in Agentd

Given V5 (CORS always blocked), the original Epic 26 architecture (browser makes HTTP calls to provider) will NEVER work for opencode.ai. The relay still adds value because:

1. **Different providers may support CORS** — not all free-tier models go through opencode.ai
2. **SDK clients (Go/Python/Node) don't have CORS** — they can make the calls directly
3. **Future: opencode.ai may add CORS support**

But for the default case (browser + opencode.ai), we need **automatic server-side fallback in agentd**: when the browser reports CORS failure, agentd makes the request directly rather than failing.

This means the relay proxy (`relay_proxy.go`) needs a fallback HTTP client. When `proxy_error` with a CORS-related message arrives, the agentd re-executes the original request directly and streams the response back to opencode.

**Trade-off:** This defeats the purpose of IP-distribution (all requests still come from server IPs). But it's better than non-functional. When SDK clients are used, or when providers support CORS, the browser-side execution path works as designed.

### Scope

#### Task A: Fix Basic Auth on relay push functions (0.5pt)

**Files:** `api/internal/handlers/models.go`

1. `isFreeTierModel(ctx, podIP, modelID)` → add `workspaceID` param, resolve password via `h.passwordGetter`, call `req.SetBasicAuth(agentd.AuthUsername, password)`
2. `pushRelayBaseURL(ctx, podIP, relayBaseURL)` → same: add workspaceID, add basic auth
3. `clearRelayBaseURL` inherits (calls pushRelayBaseURL)
4. Test: httptest server that rejects requests without `Authorization: Basic ...` header

#### Task B: Add DisposeInstance after relay baseURL push (0.5pt)

**Files:** `api/internal/handlers/models.go`, `cmd/workspace-agentd/main.go`

1. After `pushRelayBaseURL` succeeds, call `POST /instance/dispose` on the pod (with basic auth)
2. Re-use the existing `DisposeInstance` pattern from `pkg/agent/opencode/client.go`
3. For the agentd boot-time push (Task C), dispose is called after push (safe: no active sessions at boot)
4. For the `SetModel` API handler: dispose aborts active streams. Per worklog 0152, the project chose per-prompt model injection to avoid this. **Therefore, the API-side `pushRelayBaseURL` should NOT call dispose** — it stages the baseURL for next reload. Only the boot-time path (Task C) calls dispose.

**Revised approach for SetModel:** `pushRelayBaseURL` stages auth.json. The baseURL takes effect on next agent reload (user-initiated via "Reload" button or on pod restart). This matches the Epic 27a principle: credential changes are staged, not force-applied.

#### Task C: Push relay baseURL on workspace boot (1pt)

**Files:** `cmd/workspace-agentd/main.go`

1. After opencode healthz returns healthy (existing `refreshIsHealthyLoop`), if `LLMSAFESPACE_RELAY_URL` is set:
   - Call `PUT /auth/opencode` with `{type: "api", key: "public", metadata: {baseURL: "http://localhost:4097/relay/inference"}}`
   - Call `POST /instance/dispose` (safe at boot — no active sessions)
   - Both with Basic Auth (password from `/sandbox-cfg/password`)
2. Use existing `pkg/agent/opencode.NewClient(baseURL, password).PushCredentials()` + `.DisposeInstance()`
3. Only push once per boot (flag guard)
4. If relay WS is not yet connected at this point, that's OK — the relay proxy returns 503 until connected; opencode will get a failure on the first free-tier request and retry. The WS typically connects within 1-2s.
5. Test: mock opencode server verifies it received PUT /auth/opencode + POST /instance/dispose with correct payload

#### Task D: CORS fallback in agentd relay proxy (1.5pt)

**Files:** `cmd/workspace-agentd/relay_proxy.go`

When a `proxy_error` arrives with a CORS-related message (contains "CORS" or "TypeError" or status=0), the agentd relay proxy should NOT return 502 to opencode. Instead:

1. Make the original HTTP request directly (agentd → provider) using Go's `net/http`
2. Stream the response back to the waiting HTTP handler (which is holding the opencode connection)
3. Log a warning: "relay CORS fallback: making server-side request for <url>"
4. Rate-limit fallback requests (same 60/min as the relay overall)

Implementation: in the handler's `case pe := <-pending.errorCh:` block, if the error looks like CORS, re-execute the original `ProxyRequest` directly instead of returning 502.

**This makes the relay self-healing:** browser tries → CORS fails → agentd does it directly → opencode gets a successful response. No user awareness needed.

5. Test: send proxy_error with CORS message, verify the handler makes a direct HTTP call and returns the result to the HTTP response writer

#### Task E: Integrate `useRelayClient` into frontend (1pt)

**Files:** `frontend/src/pages/ChatPage.tsx`

1. Import `useRelayClient`
2. Call with `{ workspaceId: activeWorkspace?.id, enabled: activeModel?.proxyRequired }`
3. Show status indicator:
   - `connected` → small green dot + "Relay active" tooltip
   - `connecting` → yellow dot + "Relay connecting..."
   - `disconnected`/`error` when `proxyRequired` → orange warning: "Using server fallback for free model"
4. Do NOT block input or chat — the CORS fallback (Task D) ensures requests always work
5. When relay status is `connected` and CORS errors occur, the system degrades gracefully (browser tries → fails → agentd fallback → works)

### Failure Modes Addressed

| Failure Mode | Mitigation |
|---|---|
| CORS blocks browser fetch (ALWAYS for opencode.ai) | Task D: agentd server-side fallback |
| First LLM request before relay WS connected | Relay returns 503 → opencode retries after brief delay. Alternatively: Task D's fallback catches the "relay not connected" error too |
| User never selects a model (free tier is default) | Task C: boot-time push activates relay without user action |
| `PUT /auth` doesn't refresh provider state | Task B: DisposeInstance after push at boot time; for SetModel, staged until next reload |
| Multiple browser tabs | Only one relay client per workspace (handler replaces). All tabs benefit from relay because agentd is the consumer, not individual tabs |
| Network partition (agentd ↔ API server) | `failAllPending("relay disconnected")` fires → opencode gets 502 → retries when relay reconnects. With Task D, CORS fallback catches this too (error contains "relay disconnected" → direct call) |

### Acceptance Criteria

- [ ] `pushRelayBaseURL` succeeds against a real opencode pod (no 401)
- [ ] Fresh workspace with free-tier default has relay active within 30s of pod readiness
- [ ] LLM request with free-tier model succeeds end-to-end (browser CORS fails → agentd fallback → response)
- [ ] Browser relay client connects and shows status indicator
- [ ] When browser successfully fetches (SDK client or CORS-enabled provider), response flows through relay (not fallback)
- [ ] All existing relay tests continue to pass
- [ ] New tests: auth header on push, boot-time push+dispose, CORS fallback in agentd, paid model assertion

### Estimated Effort

- Task A: 0.5pt (add SetBasicAuth + workspace ID param)
- Task B: 0.5pt (dispose after push; conditional on context)
- Task C: 1pt (boot-time push with flag guard + test)
- Task D: 1.5pt (CORS fallback in relay proxy + direct HTTP client + tests)
- Task E: 1pt (React integration + status indicator)
- **Total: 4.5pt**

### Quality Targets

| Dimension | Target | How achieved |
|---|---|---|
| Robustness | 9/10 | Auto-fallback on CORS; failAllPending on disconnect; boot-time activation |
| Reliability | 9/10 | All assumptions validated; DisposeInstance ensures state refresh; no silent failures |
| Security | 9/10 | Basic Auth on all opencode calls; no new attack surface |
| Performance | 8/10 | Direct fallback adds ~0ms over current behavior; relay overhead only when relay actually works |
| Maintainability | 9/10 | Follows existing patterns (PushCredentials, DisposeInstance, passwordGetter); single-responsibility tasks |
| Scalability | 8/10 | Per-workspace relay; fallback doesn't add load (same requests that would go direct anyway) |
| SOLID | 9/10 | Each task is a single concern; fallback uses same HTTP client pattern |
| Idiomatic | 9/10 | Uses existing Go patterns, existing React hook conventions |
| Complexity | 8/10 | Adds fallback complexity but eliminates the "relay never works" failure mode; net simplification of operational concerns |

# Dev-Preview Tunnel Behavior: Regression Test Suite

Empirically determined behavior of the Epic 66 dev-preview tunnel
(`browser → Cloudflare → API DevPreviewHandler → agentd :4097 → app`),
as measured 2026-08-19 from workspace `42ae0489`. All findings below were
verified through the live tunnel with a real browser, cross-checked against
the app-side access log.

## Serving

Start the combined static + WS server (detached, survives shell restarts):

```bash
setsid nohup python3 harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)serve_ws.py \
  >> harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)server.log 2>&1 < /dev/null &
```

- Serves `harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)` on `:5173` with
  `Cache-Control: no-store` on every response.
- WebSocket echo endpoint at `/ws` (raw handshake, no dependencies).
- `serve.py` is the static-only predecessor; `ws_client_test.py` verifies
  the echo endpoint locally: `python3 ws_client_test.py`.

## Confirmed behavior (2026-08-19)

### The policy (from response headers)

```
default-src 'self'; connect-src 'self' wss:; script-src 'self';
style-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none';
frame-ancestors 'none'; form-action 'self'; base-uri 'self';
block-all-mixed-content
```

Plus `x-frame-options: DENY`. Applied by the platform at the proxy layer;
apps cannot opt out.

### Matrix

| Capability | Works | Evidence |
|---|---|---|
| External same-origin JS/CSS | yes | all test pages |
| Inline `<script>` blocks | **NO** | csp-test.html line 1 |
| Inline event handlers (`onclick=` etc.) | **NO** | csp-test.html line 3 |
| Inline `<style>` blocks | **NO** | tier3-test + console violation |
| `style=` attributes | **NO** | tier3-test + console violation |
| CSSOM writes (`el.style.x`, `insertRule`) | yes | tier3-test control |
| `eval()` / `new Function()` | **NO** (EvalError) | tier3-test |
| `<script type="application/json">` data blocks | yes (not stripped) | tier3-test |
| Cross-origin `<script src>` (CDNs) | **NO** | console names `script-src` |
| `fetch()` same-origin | yes | tier3-test |
| `fetch()` cross-origin | **NO** | console names `connect-src` |
| `img` from `data:` URIs | yes (per policy) | policy allows `data:` |
| WebSocket through tunnel | **NO** — upgrade stripped | ws-test + server log |
| iframing the preview | **NO** | `frame-ancestors 'none'` / XFO DENY |
| Caching of responses | yes (hazard) | see "caching" below |

### Key quirks

- **Caching:** the chain (Cloudflare/proxy/browser) can serve stale HTML even
  after app changes. Apps MUST send `Cache-Control: no-store` themselves; the
  proxy does not add it for HTML. Symptom of a stale copy: no new entries in
  the app access log when the user reloads. The access log is ground truth.
- **WebSocket:** the browser is *allowed* to connect (`connect-src 'self' wss:`
  permits it), but the proxy strips `Upgrade`/`Connection` headers; the app
  receives a plain GET. Browser sees close code 1006; server log shows
  `GET /ws 404` (no `WS OPEN`). CSP is not the blocker — the proxy is.
- **Rate limiting (headers only — NOT enforced on the proxy path):** API
  responses carry `x-ratelimit-limit: 100` / `-remaining`. Stress test
  2026-08-19: a 127-request burst through the preview proxy produced zero
  429s and a frozen `remaining` counter — subresource GETs are not counted
  (server log confirms all requests traversed). Treat the headers as
  decorative for preview subresources; document loads appear counted.
- **Streaming (SSE) passes through unbuffered:** 10-chunk SSE drip @ 200ms
  arrived with 197–206ms inter-chunk gaps — exact server cadence. SSE is a
  viable live-reload/HMR-fallback transport today; also proves no buffering
  layer sits on the proxy path (de-risks the WS-forwarding fix).
- **Bodies up to 2 MB verified** intact at ~4.2 MB/s; latency p50 ≈ 50 ms,
  max 110 ms at concurrency 10 (browser → CF → API → agentd → app).
- **Body mutation:** Cloudflare appends a `beacon.min.js` script tag to HTML
  responses (which the platform's own CSP then blocks). Proof that an
  intermediary rewrites response bodies.
- **Port minting unrestricted at tool layer; blocked at proxy layer
  (2026-08-19):** `dev_preview_url` mints URLs for `:4097`/`:22`, but
  DevPreviewHandler denies blocked ports at proxy time. Current denial
  body leaks internal topology (`port denied: agentd user mux (4097)`) and
  is distinguishable from a dead port — a port-scanner oracle. Fix: P1-4.
- **Preview path requires authentication (2026-08-19):** incognito access
  returns `{"error":"Authorization token required"}`. Cross-USER
  authorization (ownership check) untested. THREAT-MODEL T7.
- **Listener processes do not survive container reschedules;** only
  /workspace persists. Restart the server (command above).

## The tests

All pages are self-reporting: load through the tunnel, read the PASS/FAIL
lines. Serve with `serve_ws.py` first.

| Page | Covers |
|---|---|
| `csp-test.html` | inline script vs external file vs inline handler |
| `ws-test.html` | WebSocket upgrade through the tunnel (auto-connects, echoes) |
| `tier3-test.html` | style-src (block/attr/CSSOM), eval, JSON data blocks, CDN script, same/cross-origin fetch |
| `stress.html` | click-gated phased load test: SSE streaming, 2 MB body, request waves at conc 1–10, 429/recovery probing. Reports a pasteable JSON summary. Server needs `serve_stress.py` (adds `/sse`, `/big`). |

Expected results with the current tunnel: every row marked NO above fails,
every row marked yes passes. If a platform fix lands (WS forwarding,
no-store enforcement, CSP relaxation), the relevant line flips — that's the
regression signal.

### Test-authoring lessons (from bugs in earlier versions of these tests)

- Give every style probe its OWN element: CSSOM writes clobber
  `style=` attributes, contaminating measurements.
- A CSSOM probe doubles as a methodology control — it must always pass;
  if it fails, distrust every style result on that load.
- Inline `<script>` in a test page will not run through the tunnel — all
  test JS must be external files (the tunnel's own restriction applies to
  the tests too).

## Platform-team fix list (priority order)

1. Force `Cache-Control: no-store` on HTML in `DevPreviewHandler`
   (eliminates the stale-page failure mode for every workspace).
2. Forward `Upgrade`/`Connection` headers through the proxy chain
   (`connect-src` already permits `wss:` — policy intent is there;
   implementation lags).
3. Decide on CSP posture: relax for dev previews, or implement nonce-based
   CSP to unblock inline-script-dependent frameworks (Next.js App Router
   hydration). Document whichever is chosen.
4. Revisit the 100-request rate limit relative to multi-asset apps, or at
   least document the window.

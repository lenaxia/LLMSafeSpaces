# Worklog: Fix relay-router /provider hang — response-header timeouts (#911)

**Date:** 2026-08-17
**Status:** Complete
**Session:** Root-cause and fix the relay-router `/provider` endpoint hanging cluster-wide
**PR:** (this change)
**Issue:** #911 (relay-router: /provider endpoint times out cluster-wide)

---

## Objective

Fix issue #911: `curl -m 10 http://relay-router.../provider` timed out with
no HTTP response and no error in the router log. The `/provider` fetch is
the free-models path every fresh workspace boot depends on (relay
injector), so the hang starved free-tier model routing cluster-wide.

## Root Cause

Both of the relay-router's HTTP clients and the relay-proxy's upstream
client had **no timeout on the pre-body phase**:

- `cmd/relay-router/proxy.go`: `httpClient: &http.Client{Timeout: 0}` on the
  forwarding path and on the fallback path.
- `cmd/relay-proxy/main.go`: `defaultHTTPClient()` used the default
  transport with dial/TLS timeouts but NO `ResponseHeaderTimeout`.

`http.Client{Timeout: 0}` means no deadline at all. When the upstream
(relay VM, or the fallback's direct opencode.ai egress) accepted the
connection but stalled before writing response headers, the proxy's
`resp.Body.Read()` loop blocked forever: no response, no error in the log,
handler goroutine leaked. Worse, the failure branches returned silently
(no log, no fallback metric), which is why the outage ran 47 days
invisible.

A total body `Timeout` is deliberately NOT used: chat/SSE responses stream
for minutes and a wall-clock deadline would truncate generations mid-stream.

## Fix

Added `newRouterClient(d)` in `cmd/relay-router/proxy.go` (production 5m via
`defaultRouterClient()`) and `ResponseHeaderTimeout` on
`cmd/relay-proxy/main.go`'s transport:

- `DialContext` timeout: 5s (router→relay plaintext HTTP with per-VM tokens;
   WireGuard was removed, worklog 0447). 5s lands inside the workspace
   injector's 10s client window so a blackholed peer surfaces as a bounded
   502, not a hang.
- `TLSHandshakeTimeout`: 10s.
- **`ResponseHeaderTimeout`: 5m** — bounds time-to-first-header. 5m (not 30s)
   because the router is a generic openai-compatible forwarder and a
   non-streaming completion (`stream:false`) sends headers only after the
   FULL generation, which routinely exceeds 30s. Stated assumption (per the
   issue-thread analysis): no legitimate request needs >5m to deliver
   headers; streaming responses get headers immediately.
- **Logging** on the router's forwarding + fallback `Do()`-error branches
   (`logUpstreamError`, which unwraps `*url.Error` so the forwarded
   path/query — which may carry secrets — is never logged). The relay-proxy
   already logged on main; its log is likewise corrected to unwrap.
- **Fallback metrics**: `fallbackProxy.withFallbackMetrics()` records
   outcomes under `relay_router_requests_total{relay="fallback",status=...}`,
   wired in `main.go`.
- No total `Client.Timeout` — body streaming stays unbounded.

Both `routerProxy` and `fallbackProxy` use the bounded client.

## Key Decisions

1. **Head-timeout, not body-timeout**: `ResponseHeaderTimeout` covers the
   dead-peer case (accept-then-stall) without ever cutting a long
   generation. This is the correct granularity for a streaming reverse
   proxy.
2. **Both hops bounded**: the router→relay leg (router's client) AND the
   relay→opencode.ai leg (relay-proxy's client).
3. **5m, not 30s**: chosen per the issue-thread analysis — a `stream:false`
   completion sends headers only after the full generation. Assumption
   stated in-code per Rule 7.
4. **Loud failures**: log (URL-redacted) + fallback metric, so an outage is
   visible in both the log and `/metrics` — the 47-day-invisible signature
   is gone.

## Blockers

None.

## Tests Run

- `TestRouterClientsHaveHeadTimeouts` (new): asserts the PROXY CONSTRUCTORS
  attach a bounded client (transport with non-zero `ResponseHeaderTimeout`,
  no total `Timeout`). Red/green verified against `&http.Client{Timeout: 0}`.
- `TestRouterProxy_StalledRelayBounded502` (new): real `routerProxy` → stalled
  relay → bounded 502 in ~300ms (test-scaled bound), not a hang.
- `TestFallbackProxy_UpstreamErrorRecordedInMetrics` (new): fallback 502 is
  visible as `relay_router_requests_total{relay="fallback",status="502"} 1`.
- `TestForwardFailureLogOmitsPathAndQuery` +
  `TestFallbackFailureLogOmitsPathAndQuery` (new): captured router log during
  a failed forward/fallback must NOT contain a sentinel path/query
  (path-segment secret protection) and must NOT be empty (loud failure).
  Red/green verified against logging the wrapped `url.Error`.
- `TestDefaultHTTPClientHeadTimeout` (new, relay-proxy): transport pins
  `ResponseHeaderTimeout` and no total `Timeout`. Red/green verified.
- `go test -count=1 ./cmd/relay-router/ ./cmd/relay-proxy/` → PASS.
- `go build ./cmd/relay-router/ ./cmd/relay-proxy/` PASS; `go vet` clean;
  `golangci-lint` 0 issues; `gofmt` clean.

## Next Steps

1. PR + review through the onboarded pipeline.
2. After merge: redeploy the relay-router + relay-proxy images so the
   running clusters pick up the timeouts and observability.
3. The issue's confirmed root cause (Zen removed `/provider`; helm
   `freeModelsRefresher.enabled=false` drift) is operator-side and tracked
   separately (#910 re-arm redesign). This PR is the defensive hardening —
   bounded head phase + loud failure signaling — not the endpoint fix.
4. Monitor for 504s on slow-model cold starts; raise `ResponseHeaderTimeout`
   if any legitimate upstream ever exceeds 5m to first header.

## Files Modified

- `cmd/relay-router/proxy.go`
- `cmd/relay-router/proxy_test.go`
- `cmd/relay-proxy/main.go`
- `cmd/relay-proxy/coverage_test.go`
- `cmd/relay-router/main.go`
- `worklogs/NNNN_2026-08-17_relay-router-provider-timeout.md` (this entry)

---

## Correction (round-1 review, 2026-08-17)

The round-1 review found the initial 30s fix overclaimed and under-delivered
on the issue's observability findings. This section records the corrections:

1. **ResponseHeaderTimeout raised 30s → 5m.** 30s would break non-streaming
   completions (`stream:false` sends headers only after the FULL generation;
   long generations routinely exceed 30s). The router is a generic
   openai-compatible forwarder, not a streaming-only path. 5m is the value
   the issue-thread's own analysis chose, with the stated assumption that no
   legitimate request needs >5m to deliver headers. Stated assumption now
   documented in the code.
2. **Silent-failure branches now log.** The `Do()`-error branches in
   `routerProxy.forwardToRelay`, `fallbackProxy`, and the relay-proxy
   `proxyHandler` returned without logging — the "zero errors in its own
   log" signature that hid the outage for 47 days. All three now `log.Printf`
   the error BEFORE the client-context check (which was the silent path),
   without logging the request path/query (path-segment secret protection).
3. **Fallback outcomes now recorded in metrics.** `fallbackProxy` previously
   recorded nothing, so an outage was invisible in `/metrics`. It now has an
   optional `withFallbackMetrics` hook recording requests under
   `relay_router_requests_total{relay="fallback",status="..."}` (502 on
   error, the real status otherwise), wired in `main.go`.
4. **Test-scaled timeouts.** `newRouterClient(d)` makes the
   response-header bound injectable; production uses 5m via
   `defaultRouterClient()`, the stalled-relay handler test uses 300ms so CI
   completes in milliseconds.
5. **Wiring tests added** (round-1 gap): `TestRouterClientsHaveHeadTimeouts`
   now asserts the PROXY CONSTRUCTORS attach the bounded client (a revert to
   `&http.Client{Timeout: 0}` fails it, red/green verified), plus
   `TestDefaultHTTPClientHeadTimeout` (relay-proxy) and
   `TestFallbackProxy_UpstreamErrorRecordedInMetrics` (fallback visibility).
   The handler-level `TestRouterProxy_StalledRelayBounded502` drives the
   real `routerProxy` → relay path (a stalled relay yields a bounded 502).
6. **Honest closure scope.** The issue's confirmed root cause was operator-
   side (Zen removed `/provider`; helm `freeModelsRefresher.enabled=false`);
   the relay-router was never the culprit. This PR is defensive hardening
   (bounded head phase + observability), not a claim to fix the removed
   endpoint. Issue #911 is referenced as the motivation, not claimed closed.
7. **WireGuard comment corrected** — the router→relay path is plaintext HTTP
   with per-VM tokens (WireGuard removed, worklog 0447).

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
(relay VM over WireGuard, or the fallback's direct opencode.ai egress)
accepted the connection but stalled before writing response headers, the
proxy's `resp.Body.Read()` loop blocked forever: no response, no error in
the log, handler goroutine leaked. The `/provider` endpoint returns a small
JSON catalog that should produce headers in milliseconds — a 30s stall means
a dead peer.

A total body `Timeout` is deliberately NOT used: chat/SSE responses stream
for minutes and a wall-clock deadline would truncate generations mid-stream.

## Fix

Added `defaultRouterClient()` in `cmd/relay-router/proxy.go` and applied
`ResponseHeaderTimeout` to `cmd/relay-proxy/main.go`'s transport:

- `DialContext` timeout: 5s (router→relay over WireGuard; 10s for the
  relay→opencode.ai public egress).
- `TLSHandshakeTimeout`: 10s.
- **`ResponseHeaderTimeout`: 30s** — bounds the phase between request-send
  and response-headers; exactly the `/provider` symptom. Generous for the
  relay→upstream hop yet bounded.
- `ExpectContinueTimeout`, `MaxIdleConns(PerHost)`, `IdleConnTimeout`,
  `DisableCompression` — same defaults as the relay-proxy's existing client.
- No total `Client.Timeout` — body streaming stays unbounded.

Both `routerProxy` and `fallbackProxy` now use `defaultRouterClient()`.

## Key Decisions

1. **Head-timeout, not body-timeout**: `ResponseHeaderTimeout` covers the
   dead-peer case (accept-then-stall) without ever cutting a long
   generation. This is the correct granularity for a streaming reverse
   proxy.
2. **Both hops fixed**: the router→relay leg (router's client) AND the
   relay→opencode.ai leg (relay-proxy's client) — the `/provider` hang could
   originate at either, and both had the defect.
3. 30s value: the relay round-trip over WireGuard plus the relay's own
   upstream head latency; generous headroom, still bounded.

## Blockers

None.

## Tests Run

- `TestRouterClientsHaveHeadTimeouts` (new): asserts the router client has a
  configured transport with a non-zero `ResponseHeaderTimeout` and NO total
  `Timeout` — locks the config against regressing to `Timeout: 0`.
  Red/green verified (reverting to `Timeout: 0` fails it).
- `TestRouterProxy_StalledUpstreamTimesOutHead` (new): a server that accepts
  and never writes headers → the router client returns a bounded error in
  ~30s (the ResponseHeaderTimeout), not a hang. Pre-fix this blocks forever.
- `go test -count=1 ./cmd/relay-router/ ./cmd/relay-proxy/` → PASS.
- `go build ./cmd/relay-router/ ./cmd/relay-proxy/` PASS; `go vet` clean;
  `golangci-lint` 0 issues; `gofmt` clean.

## Next Steps

1. PR + review through the onboarded pipeline.
2. After merge: redeploy the relay-router + relay-proxy images so the
   running clusters pick up the timeouts. Verify `/provider` returns
   promptly (the issue's repro: `curl -m 10 .../provider`).
3. Consider raising `ResponseHeaderTimeout` if any legitimate upstream ever
   exceeds 30s to first header (monitor for 504s on slow-model cold starts).

## Files Modified

- `cmd/relay-router/proxy.go`
- `cmd/relay-router/proxy_test.go`
- `cmd/relay-proxy/main.go`
- `worklogs/NNNN_2026-08-17_relay-router-provider-timeout.md` (this entry)

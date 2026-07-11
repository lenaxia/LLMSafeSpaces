# Worklog: G34 — proxy header allowlist (network hardening)

**Date:** 2026-07-11
**Session:** Close G34 (proxy forwards all client headers to sandbox) — first of the network hardening sweep targeted at v0.3.0.
**Status:** Complete

---

## Objective

G34 (from `design/stories/epic-17-security-review/THREAT-MODEL.md` and `security-report-g33-g47.md`) flagged that the workspace reverse-proxy loop at `api/internal/handlers/proxy.go:465-469` copied every client request header verbatim into the upstream request to the tenant pod, then overwrote `Authorization` with HTTP Basic auth for opencode. Effect: the caller's live Bearer JWT, `Cookie`, `Origin`, `Referer`, `X-Forwarded-*` and arbitrary custom headers all reached untrusted agent code inside the sandbox.

Goal of this session: replace the unfiltered header copy with an explicit allowlist, and strip hop-by-hop headers in both directions.

---

## Work Completed

### TDD: failing tests first (`api/internal/handlers/proxy_headers_allowlist_test.go`)

- `TestCopyRequestHeaders_AllowlistOnly` — table-driven, one case per header.
  Asserts `Content-Type`, `Accept`, `X-Request-ID` are forwarded; asserts
  `Authorization`, `Cookie`, `Origin`, `Referer`, `X-Forwarded-Host`,
  `X-Forwarded-Proto`, `Forwarded`, `Connection`, `Accept-Encoding`, and any
  arbitrary `X-Acme-Custom` are dropped.
- `TestCopyRequestHeaders_XForwardedForNotCopiedFromCaller` — pins that the
  caller-controlled `X-Forwarded-For` never reaches the tenant pod (the proxy
  sets its own after the copy).
- `TestCopyResponseHeaders_HopByHopStripped` — table-driven over the
  RFC 7230 §6.1 hop-by-hop set plus `Upgrade`.
- `TestCopyResponseHeaders_StripsSetCookieMultipleValues` — regression for
  multi-valued `Set-Cookie`.
- `TestProxy_G34_CallerAuthorizationNotForwarded` — e2e wiring check through
  the real `ProxyHandler` + gin router + httptest backend; asserts the only
  `Authorization` on the upstream is the proxy's Basic credential and the
  caller's Cookie never arrives.

Verified red before implementing.

### Implementation

- `api/internal/handlers/proxy_helpers.go`:
  - Added `hopByHopHeaders` (RFC 7230 §6.1 set + `Upgrade`) and applied it
    in `copyResponseHeaders` alongside the existing denylist
    (`blockedResponseHeaders`). The existing denylist is retained as
    defence-in-depth (e.g. `WWW-Authenticate` is in `blockedResponseHeaders`
    but also semantically a hop-by-hop concern; keeping both means future
    tightening of one list doesn't reintroduce the leak).
  - Added `copyRequestHeaders` with an explicit allowlist
    (`forwardedRequestHeaders = {Content-Type, Accept, X-Request-ID}`).
    Comment block explains why `Accept-Encoding` is deliberately NOT on the
    allowlist (Go's transport only auto-decompresses when *it* sets the
    header — forwarding the caller's would defeat that and forward gzipped
    bytes to a client that may not have asked for gzip).
- `api/internal/handlers/proxy.go:465-471`: replaced the four-line
  `for k, vs := range c.Request.Header { … }` loop with a single
  `copyRequestHeaders(…)` call. `SetBasicAuth` and `X-Forwarded-For` setting
  follow it unchanged.

### Adversarial self-review (Rule 11)

- Checked every other proxy code path for the same pattern. `proxy_events.go`,
  `proxy_handlers.go`, `proxy_permissions.go` all construct fresh
  `http.Request` values and set headers explicitly via `req.Header.Set(…)`
  — they do not iterate `c.Request.Header`. The terminal WebSocket path
  uses `remotecommand.NewSPDYExecutor` (K8s SPDY), which builds its own
  request to the API server and doesn't propagate client headers either.
  Only `doProxy` had the unfiltered copy.
- Verified no `If-Match` / `If-None-Match` / `Range` use anywhere in
  `api/internal/handlers` — opencode's HTTP API is JSON request/response
  only, so the allowlist is sufficient.
- Caught my own bug during review: initial allowlist included
  `Accept-Encoding`. Backed it out (see comment block in
  `proxy_helpers.go`) — the proxy's `http.Transport` doesn't set
  `DisableCompression`, so forwarding the caller's `Accept-Encoding: gzip`
  would cause opencode to gzip the response without the transport
  auto-decompressing it. Letting the transport handle compression
  transparently is the safer default.

---

## Key Decisions

1. **Allowlist over denylist for request headers.** A denylist blocks what we
   know is dangerous today; an allowlist blocks everything we haven't
   explicitly approved. The tenant pod is untrusted-by-default in the threat
   model, so the request header set it receives must be too.
2. **Keep `blockedResponseHeaders` AND add `hopByHopHeaders`.** The existing
   denylist catches `WWW-Authenticate`, `Proxy-Authenticate`, `Set-Cookie`.
   The hop-by-hop list catches `Connection`, `Keep-Alive`, `Te`, `Trailers`,
   `Transfer-Encoding`, `Upgrade`. They overlap on `Proxy-Authenticate`. The
   redundancy is intentional — the two lists are reasoned about differently
   (one is "dangerous response content", the other is "transport coupling")
   and tightening one shouldn't regress the other.
3. **Do not forward `Accept-Encoding`.** See comment block in
   `proxy_helpers.go`. Net effect: opencode's responses are gzipped by Go's
   transport and auto-decompressed before reaching the proxy's response
   writer. Same as today's behaviour, just defensible.

---

## Assumptions stated and validated (Rule 7)

1. *The only proxy code path that copies caller headers is `doProxy` in
   `proxy.go`.* Validated by `grep -n "for.*range.*\.Header"` across
   `api/internal/handlers/*.go` (non-test) — only my two helpers iterate.
2. *opencode's HTTP API on port 4096 does not need conditional/request
   headers like `If-Match`/`Range`.* Validated by grepping
   `api/internal/handlers` — zero references to those headers.
3. *The proxy sets `X-Forwarded-For` after the header copy.* Validated by
   reading `proxy.go:471` — the `Set("X-Forwarded-For", …)` runs after
   `copyRequestHeaders`, so even if it were on the allowlist it would still
   be overwritten. It is not on the allowlist, by design.
4. *The WebSocket terminal path does not have the same issue.* Validated by
   reading `terminal.go:245-300` — it uses K8s `remotecommand.NewSPDYExecutor`
   which constructs its own SPDY request to the kube API server; no client
   header propagation.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s -run "TestCopyRequestHeaders_AllowlistOnly|TestCopyRequestHeaders_XForwardedForNotCopiedFromCaller|TestCopyResponseHeaders_HopByHopStripped|TestCopyResponseHeaders_StripsSetCookieMultipleValues|TestProxy_G34_CallerAuthorizationNotForwarded" ./api/internal/handlers/
→ ok  github.com/lenaxia/llmsafespaces/api/internal/handlers  0.083s

go test -timeout 90s -short ./api/internal/handlers/...
→ ok  github.com/lenaxia/llmsafespaces/api/internal/handlers  65.370s

go test -timeout 120s -short ./api/internal/server/...
→ ok  github.com/lenaxia/llmsafespaces/api/internal/server  0.226s

go vet ./api/internal/handlers/...   → clean
gofmt -l <changed files>             → clean
```

---

## Next Steps

1. Open this PR for review.
2. While it's in review, start on the next network finding:
   `security/G35-terminal-origin-check` (terminal WebSocket `CheckOrigin: return true`).
3. After G34, terminal-origin, CORS hardening, NetworkPolicy CGNAT drift,
   runtimeClass webhook gate, and JWT iss/aud all land, tag v0.3.0 with
   the new release pipeline (separate epic).
4. Update `design/stories/epic-17-security-review/THREAT-MODEL.md` G34 row
   from 🔴 Open to 🟢 Fixed (separate doc-reconciliation PR after the network
   sweep — see `security/threat-model-reconcile` worklog TODO).

---

## Files Modified

- `api/internal/handlers/proxy_helpers.go` (added `hopByHopHeaders`, `forwardedRequestHeaders`, `copyRequestHeaders`; extended `copyResponseHeaders`)
- `api/internal/handlers/proxy.go` (replaced header loop with `copyRequestHeaders` call)
- `api/internal/handlers/proxy_headers_allowlist_test.go` (new file — TDD test battery)
- `worklogs/0605_2026-07-11_g34-proxy-header-allowlist.md` (this entry)

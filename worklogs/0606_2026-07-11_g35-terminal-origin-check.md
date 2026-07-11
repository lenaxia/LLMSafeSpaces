# Worklog: G35 — terminal WebSocket Origin check

**Date:** 2026-07-11
**Session:** Close G35 (terminal Upgrader accepts any Origin) and remove the dead `WebSocketSecurityMiddleware` / `AllowedWebSocketOrigins` plumbing. Second of the network hardening sweep targeted at v0.3.0.
**Status:** Complete

---

## Objective

The terminal WebSocket handler at `api/internal/handlers/terminal.go:122-124` instantiated the gorilla `Upgrader` with `CheckOrigin: func(r *http.Request) bool { return true }`. That accepts any Origin, including cross-site browser requests. A malicious page on the internet could call the terminal WebSocket endpoint in a user's browser, present the user's single-use ticket (acquired via fetch from the same browser session), and get a shell on the user's workspace. This is classic cross-site WebSocket hijacking (CSWSH).

The fix is a real Origin policy:
- **Same-origin requests** (Origin host:port == request Host) are accepted.
- **Origins in the operator-configured allowlist** are accepted.
- **Missing Origin** (non-browser clients: curl, MCP) is accepted; these authenticate via the single-use ticket, not cookies, so CSRF does not apply.
- **Everything else is rejected.**

---

## Work Completed

### TDD: failing tests first (`api/internal/handlers/terminal_origin_test.go`)

- `TestCheckTerminalOrigin_SameOrigin` — same host:port accepted.
- `TestCheckTerminalOrigin_SameOriginHTTPS` — HTTPS variant.
- `TestCheckTerminalOrigin_CrossSubdomainRejectedByDefault` — `app.example.com` vs `api.example.com`.
- `TestCheckTerminalOrigin_MissingOriginAccepted` — non-browser path.
- `TestCheckTerminalOrigin_CrossOriginRejectedByDefault` — `evil.example.com`.
- `TestCheckTerminalOrigin_AllowlistAccepts` — explicit allowlist entry.
- `TestCheckTerminalOrigin_WildcardAcceptsAll` — `["*"]` restores historical behaviour.
- `TestCheckTerminalOrigin_AllowlistRejectsUnlisted` — origin not in allowlist.
- `TestCheckTerminalOrigin_MalformedOriginRejected` — `://not-a-url`.
- `TestCheckTerminalOrigin_PortMismatchRejected` — same host, different port.
- `TestTerminal_G35_CrossOriginUpgradeRejected` — e2e through `router.ServeHTTP`; asserts the upgrader returns 403 on cross-origin.
- `TestTerminal_G35_SameOriginUpgradePassesOriginCheck` — calls `h.upgrader.CheckOrigin(req)` directly because `httptest.NewRecorder` cannot HTTP-hijack (gorilla needs hijack for the actual upgrade); the contract we care about for G35 is the CheckOrigin decision.

Verified red before implementing.

### Implementation (`api/internal/handlers/terminal.go`)

- New `NewTerminalHandler` signature takes `allowedOrigins []string`.
- New `newCheckOriginChecker` returns a gorilla-compatible `CheckOrigin` function:
  - `*` in allowlist → wildcard accept (operator opt-in for the historical behaviour).
  - Empty Origin → accept (non-browser).
  - Origin `host:port` equals `r.Host` → accept (same-origin).
  - Origin in normalized allowlist → accept.
  - Otherwise reject.
- Helpers `normalizeOrigin`, `asciiEqualFold`, `toLowerASCII` — small and obvious; `asciiEqualFold` is inlined to avoid a direct dependency on gorilla's unexported `equalASCIIFold`.
- Added `HandshakeTimeout: 10s` to the `Upgrader` — was unset previously, so a slow/hung client could hold a goroutine indefinitely. Defence-in-depth, not strictly G35.

### Config + Helm wiring

- `api/internal/config/config.go`: new `Terminal.AllowedOrigins []string` field with a doc block explaining the default-fail-closed semantics.
- `api/internal/app/app.go:740-755`: replaced the dead `wsOrigins := server.DefaultRouterConfig().AllowedWebSocketOrigins; if len(cfg.Security.AllowedOrigins) > 0 && cfg.Security.AllowedOrigins[0] != "*" { wsOrigins = cfg.Security.AllowedOrigins }` derivation with a direct `cfg.Terminal.AllowedOrigins` pass-through to `NewTerminalHandler`.
- `charts/llmsafespaces/values.yaml`: new top-level `terminal.allowedOrigins: []` value with full doc block.
- `charts/llmsafespaces/templates/configmap-api.yaml`: new `terminal:` block emitted when the value is non-empty.

### Dead code removal (Rule 5 — zero tech debt)

While doing this work I confirmed that two pieces of existing plumbing were never wired to anything:

1. **`middleware.WebSocketSecurityMiddleware`** (`api/internal/middleware/security.go:209-275`) — defined a real Origin check + added `Sec-WebSocket-Version: 13` header, but had a stub comment at `security.go:267-270` (`// Validate protocol (implement your validation logic here) // For now, we'll just echo it back`) that echoed an attacker-controllable `Sec-WebSocket-Protocol` header. Never installed on any route; only its own test referenced it. Removed the middleware and its test.
2. **`RouterConfig.AllowedWebSocketOrigins`** (`api/internal/server/router.go:72-73`, default `["*"]` at line 214) — the `wsOrigins` derivation in `app.go:740-743` populated this field, and `app.go:952` passed it to `NewRouter`, but the router never read it. Removed the field, the default, the derivation, and the `AllowedWebSocketOrigins: wsOrigins` line in the `RouterConfig` literal.

The terminal handler now does the actual enforcement at the right layer (the gorilla `Upgrader.CheckOrigin`), and there is exactly one Origin policy for terminal WebSocket connections instead of three (the dead middleware, the dead RouterConfig field, and the live Upgrader).

### Other test updates

- `api/internal/handlers/terminal_test.go`: 7 `NewTerminalHandler` call sites updated for the new signature (all use `nil` for the allowlist — same-origin-only default).
- `api/internal/server/router_terminal_test.go:145`: same.
- `api/internal/middleware/tests/security_test.go`: removed `TestWebSocketSecurityMiddleware`.

---

## Key Decisions

1. **Same-origin default, not same-origin + CORS allowlist.** Per the user's choice for this fix: the operator must explicitly populate `terminal.allowedOrigins` to allow cross-origin browser connections. Decoupling from `cfg.Security.AllowedOrigins` means an operator doesn't have to touch CORS to make the terminal work, and vice versa.
2. **`*` is still honoured as an explicit wildcard.** Operators who really want the historical behaviour can set `["*"]`. The default chart value is `[]`, so out-of-the-box is fail-closed.
3. **Remove the dead code, don't wire it.** The two pieces of dead plumbing had been there for months and would have stayed forever if not removed now. Wiring `wsOrigins` through to the dead middleware would have added a second Origin check layer that contradicts the Upgrader-level one — exactly the "two-adapter drift" the relay-config subsystem worklogs warn about. The right fix is one enforcement point.
4. **Inline `asciiEqualFold` rather than depend on gorilla internals.** Three small functions, total ~15 LoC; not worth a `internal` package import or a `strings.EqualFold` (the latter is Unicode-aware, which is overkill for HTTP header comparison and slightly slower).

---

## Assumptions stated and validated (Rule 7)

1. *Browser WebSocket clients always send an `Origin` header; non-browser clients typically do not.* Validated against RFC 6455 §1.6 + §10.2 and gorilla/websocket's own default `CheckOrigin` (`server.go:90 checkSameOrigin`).
2. *`WebSocketSecurityMiddleware` is never wired to any route.* Validated by `grep -rn "WebSocketSecurityMiddleware" --include="*.go"` — only its own test referenced it.
3. *`RouterConfig.AllowedWebSocketOrigins` is never consumed by the router.* Validated by `grep -rn "AllowedWebSocketOrigins"` — only the struct field, the default, and the dead `app.go` derivation. No read site inside `router.go` or any handler.
4. *`NewTerminalHandler` has only three call sites outside the new test file.* Validated by `grep -rn "NewTerminalHandler"` — `app.go:746`, `terminal_test.go` (7 sites), `router_terminal_test.go:145`. All updated.
5. *`SetBasicAuth` overwrites Authorization on the proxied opencode request, not the terminal path.* The terminal path uses K8s remotecommand SPDY, not the HTTP proxy. Validated by reading `terminal.go:280-300` (no `SetBasicAuth` on the SPDY path).

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s -run "TestCheckTerminalOrigin|TestTerminal_G35" ./api/internal/handlers/
→ ok  github.com/lenaxia/llmsafespaces/api/internal/handlers  0.032s

go test -timeout 180s -short ./api/internal/handlers/... ./api/internal/server/... ./api/internal/middleware/...
→ ok  github.com/lenaxia/llmsafespaces/api/internal/handlers  65.982s
→ ok  github.com/lenaxia/llmsafespaces/api/internal/server   0.249s
→ ok  github.com/lenaxia/llmsafespaces/api/internal/middleware  0.034s
→ ok  github.com/lenaxia/llmsafespaces/api/internal/middleware/tests  0.036s

go build ./...                    → clean
gofmt -l <changed>                → clean
goimports -l <changed .go files>  → clean
```

---

## Next Steps

1. Open this PR for review.
2. After approval + merge, move on to CORS hardening (reject `AllowedOrigins=["*"] + AllowCredentials=true` at config load).
3. Then NetworkPolicy CGNAT drift.
4. Then runtimeClass webhook gate.
5. Then JWT iss/aud.
6. Then doc reconciliation (close G33, G34, G35 in THREAT-MODEL.md).
7. Then release engineering for v0.3.0.

---

## Files Modified

- `api/internal/handlers/terminal.go` (new signature, new origin checker, helpers)
- `api/internal/handlers/terminal_test.go` (signature updates)
- `api/internal/handlers/terminal_origin_test.go` (new file — TDD test battery)
- `api/internal/middleware/security.go` (removed dead `WebSocketSecurityMiddleware`, dropped unused `errors` import)
- `api/internal/middleware/tests/security_test.go` (removed test for dead middleware)
- `api/internal/server/router.go` (removed dead `AllowedWebSocketOrigins` field + default)
- `api/internal/server/router_terminal_test.go` (signature update)
- `api/internal/app/app.go` (replaced dead `wsOrigins` derivation with direct `cfg.Terminal.AllowedOrigins` pass-through)
- `api/internal/config/config.go` (new `Terminal` config block)
- `charts/llmsafespaces/values.yaml` (new `terminal.allowedOrigins` value)
- `charts/llmsafespaces/templates/configmap-api.yaml` (new `terminal:` block)
- `worklogs/0606_2026-07-11_g35-terminal-origin-check.md` (this entry)

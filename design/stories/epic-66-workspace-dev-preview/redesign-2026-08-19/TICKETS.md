# Dev Preview Redesign — Ticket list

Companion to DESIGN.md. **Per-phase verification procedures: see
ACCEPTANCE.md** (exact commands + expected outputs). Ordered for
shippability; phase 0 items are independent and safe to start immediately.
Each ticket lists its verification (tests live in harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/),
new tests T1–T7 per DESIGN.md §8).

## Phase 0 — fixes on current URLs (no breaking changes)

### P0-1: Force Cache-Control: no-store on HTML responses
- Where: DevPreviewHandler (edge) for `text/html` only; respect app-set
  caching for other MIME types.
- Why: chain served stale HTML even after app changes (observed 2026-08-19);
  agents cannot fix this client-side.
- Verify: response headers on `/` show no-store; static assets keep
  app-set cache headers.

### P0-2: Forward WebSocket upgrades end-to-end
- Where: DevPreviewHandler + agentd; hop-by-hop header handling, 101
  pass-through, no post-upgrade buffering, ≥5min idle timeouts.
- Includes: remove bare `wss:` from existing CSP (was broader than
  intended; 'self' covers same-origin WSS).
- Verify: `ws-test.html` PASS through tunnel; server log shows `WS OPEN`,
  not `GET /ws 404`.

### P0-3: Session cookie hardening (routine — audit resolved 2026-08-19)
- Audit result: `lsp_session` is HttpOnly + Secure + host-scoped Domain
  (`api.safespaces.dev`) — solid baseline; preview hosts will never
  receive it. Remaining work is hygiene, not a blocker:
  add explicit `SameSite=Lax`, rename with `__Host-` prefix (blocks
  cookie-shadowing by subdomain tosses), consider shorter rotation than
  the observed ~30-day JWT lifetime (staggered dual-accept release).

### P0-4: Disable Cloudflare body rewriters on preview paths
- Browser Insights/beacon, Rocket Loader, auto-minify, email obfuscation:
  off for preview routes.
- Why: observed beacon.min.js injection into HTML (blocked only by CSP
  today; also breaks byte-accuracy of previews).
- Verify: served HTML is byte-identical to app output.

## Phase 1 — per-workspace origin (flagged)

### P1-1: DNS wildcard + host routing (no cert procurement)
- `*.safespaces.dev` → API ingress; Universal SSL already covers
  single-label hosts (`<ws>-preview`), so no ACM needed. Host-based routing
  (suffix `-preview.safespaces.dev`) to preview pipeline; `/api/*` must 404
  on preview hosts (T5); ingress default-backends unknown hosts.
- Decision recorded: single-label scheme chosen over `<ws>.preview.…` —
  identical origin isolation, zero cert cost, one less cookie-bleed domain.

### P1-2: Signed-URL bootstrap auth + __Host-pv session cookie
- One-time HMAC token (ws, ports from the runtime tool call, exp, jti) →
  host-scoped HttpOnly cookie. TTL: token 24h one-time; cookie 7d or
  workspace suspend.
- Verify: T4 (one-time redemption, cookie lifecycle).

### P1-3: dev_preview_url tooling returns new URL scheme
- `<ws>-preview.safespaces.dev/<port>/...?t=<token>`; flag-gated.
  Port stays a runtime parameter — no port field in workspace settings.

### P1-4: Listener probing + honest 502s + port blocklist hardening (§5.6, THREAT-MODEL T3)
- agentd enumerates pod listeners; tool returns hints for dead ports
  ("no listener on 5173; found: 3000"); proxy returns agent-readable 502
  body when nothing is listening.
- **Verified 2026-08-19:** proxy layer denies :4097 but leaks topology
  (`port denied: agentd user mux (4097)`), and the tool layer still mints
  URLs for any port. Hardening:
  - Blocklist at BOTH tool and proxy layers (proxy is currently the single
    enforcement point): 4096/4097, platform sidecar ports
    (deployment-defined list), <1024. No allowlist.
  - **Error hygiene:** denial responses must be generic AND
    indistinguishable from a dead port (same status/body as the
    nothing-listening case) — the current differential makes the preview
    path a port-scanner oracle with service-name disclosure. Denial
    reasons go to server logs, not responses.

### P1-5: Rate limits for preview host
- Separate budget (proposed 600/5min/workspace, instrument first);
  Retry-After on 429; documented window. Verify: T7.

## Phase 2 — CSP relaxation on new origin

### P2-1: Ship relaxed CSP (DESIGN §5.4) to preview hosts only
- Verify: csp-test + tier3-test flip to PASS on new origin; strict policy
  remains on old path URLs during overlap.

## Phase 3 — soak (2 weeks)

- Dogfood in this workspace; watch 429s, WS stability, cookie oddities.

## Phase 4 — cut over

### P4-1: Deprecate path-based preview URLs
- 410 Gone after notice; remove route from API host. Verify: T6.

## New test suite (build alongside P1)

- T1 cookie absence on preview origin
- T2 cross-workspace isolation
- T3 service worker scope
- T4 bootstrap-token lifecycle
- T5 host segregation (preview host never serves /api/*)
- T6 old-route death (phase 4)
- T7 rate-limit behavior

## Open items needing decisions

- Q3 dashboard origin (frame-ancestors allowlist)
- Q4 rate budget number (instrument, then decide)
- Q5 consumers of old URL format (docs/bookmarks)
- ~~Q6 ACM procurement~~ — resolved by single-label scheme (no cert needed)

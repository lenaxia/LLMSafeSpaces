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

### P0-3: Session cookie hardening — DISPOSITIONED 2026-08-19 (code inspection; see THREAT-MODEL T2 addendum)
- **SameSite=Lax: already shipped in code** — commit 5ff0f2ef (#774,
  2026-08-11) set `SameSiteLaxMode` explicitly on every cookie issuance
  path (router.go setSessionCookie, passkey.go, org_sso.go ×3). The field
  observation of an unset SameSite indicates the deployed build predates
  2026-08-11 — a deployment-lag artifact, not a code gap. Action: none in
  code; deploy current main. (The other field findings — CSP, WS
  stripping, G34 wipe, cache behavior — are structural and confirmed
  present in current main by direct code reading.)
- **`__Host-` rename: DROPPED** — architecturally incompatible with Epic 54
  `OrgSubdomainRouting.CookieDomain` (wildcard `.domain` cookies; the
  `__Host-` prefix forbids Domain attributes). A per-deployment
  conditional cookie name buys protection against a Low-severity
  shadowing nuisance at the cost of name churn + a dual-accept migration.
  Not worth it; HttpOnly + the CORS allowlist carry the real protection.
- **Shorter JWT rotation: DEFERRED** — Epic 56 (durable session KEK)
  territory; not a Phase-0-sized change.
- **New Phase-1 deploy-checklist item (same-site nuance, see DESIGN §5.3
  addendum):** in Epic 54 deployments with wildcard cookieDomain,
  `<ws>-preview.safespaces.dev` is same-site with `api.safespaces.dev`,
  so SameSite=Lax does NOT gate preview→API requests; the CORS origin
  allowlist is the load-bearing control. Verify preview origins are never
  added to `security.allowedOrigins` (they are not by default).

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

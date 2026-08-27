# Epic 67 (proposal): Port-in-Subdomain Preview Origins

**Status:** Proposal (follow-up to epic-66 redesign-2026-08-19, which is completed and implemented)
**Created:** 2026-08-21
**Priority:** Medium — closes the last unfixable class of preview breakage (root-absolute URLs emitted by apps the platform does not control)
**Depends On:** Epic 66 Phase 1/2 (shipped) — preview origins, `__Host-pv` cookie, bootstrap flow, Traefik IngressRoute `llmsafespaces-preview-origins`
**Authoritative for:** The public URL shape of dev preview once port moves from path to host label.

---

## Problem

Epic 66's preview-origin scheme routes by **path prefix**: `https://<uuid>-preview.<base>/<port>/<app-path>`. Any app that emits a root-absolute URL at the browser drops the prefix and breaks. The platform cannot fix all of these at the proxy (see classification below), and it cannot know what apps users will run.

Production incident (2026-08-21, workspace `c547ddb8-…`, app tinyrsvp on :8080):

- App answers unauthenticated requests with `303 Location: /login?return=%2F` — root-absolute.
- Browser lands on `https://<uuid>-preview.safespaces.dev/login?return=%2F` (no port segment).
- API preview-origin handler rejects the prefix-less path; under T3 (indistinguishable-from-dead) it returns the generic `502 workspace dev-preview endpoint unreachable` (`api/internal/handlers/dev_preview.go:323`). Server log carries the real reason (`preview-origin: refused … port not numeric or out of range`).
- Confirmed same class on the *working* workspace `42ae0489-…`: its app serves fine at `/5173/…` but its own `/health` and `/favicon.ico` requests (root-absolute, emitted by the app's HTML) 502 identically.

### Breakage classes under path-prefix routing

| Class | Proxy-fixable? | Notes |
|---|---|---|
| Redirects (`Location: /login`) | Yes (header rewrite) | nginx `proxy_redirect` equivalent; deterministic |
| Cookie `Path=/…` scoping | Yes (header rewrite) | `Path=/` already covers prefixes — only explicit sub-paths matter |
| Framework URL generation | Partially (`X-Forwarded-Prefix`) | Rails/Spring/express honor it; not universal |
| Static HTML links (`href`, `src`, `action`, CSS `url()`) | Partially (HTML body rewrite) | Heuristic, requires buffering, size-cap interactions |
| Runtime JS (`fetch('/api/…')`, SPA `pushState`) | **No** | Impossible at the proxy; the dominant pattern in modern SPAs |

The last row is the driver for this epic. Layers 1–4 above are mitigations with permanent maintenance cost and permanent residual breakage; moving the port out of the path removes the entire column.

## Proposal

Serve preview traffic at `https://<port>-<uuid>-preview.<base>/` — port prepended to the existing host label with a hyphen, so it remains **one DNS label** (e.g. `8080-c547ddb8-…-preview.safespaces.dev`, ~49 chars, under the 63-char label limit). App is always served at `/`; the proxy stops rewriting anything and becomes a dumb pipe.

Precedent: GitHub Codespaces (`<port>-<codespace>.app.github.dev`) and Gitpod (`<port>-<workspace>.ws-<region>.gitpod.io`) both chose this shape for exactly this reason.

### What already fits (validated 2026-08-21)

- **DNS:** grey-cloud wildcard `*.safespaces.dev` covers the new shape unchanged (single-level wildcard matches any single label).
- **TLS:** the `wildcard-safespaces-dev` cert on the IngressRoute covers it unchanged.
- **Edge: zero Traefik changes needed** (pass-3 correction, mechanically verified): the live rule `HostRegexp({host:[a-z0-9-]+-preview\.safespaces\.dev})` already matches port-in-subdomain hosts — `8080-<uuid>-preview.…` and `65535-1044f4f2-…-preview.…` both satisfy `[a-z0-9-]+` — and already passes `garbage-preview.…` through to the API for its **421** (THREAT-MODEL N1, `IsMalformedPreviewHost`, preview_origin.go:183). The edge was deliberately loose when shipped; the new shape needs no edit, no alternation, and no talos-ops-prod coordination (avoiding any repeat of the #2301 broken-router class). **Strict anchored parsing lives in the API handler, not the edge.**
- **Threat model:** T1 stays closed (still not the API origin; the platform session cookie is host-scoped to the API host). T3 (indistinguishable-from-dead) is enforced in the handler exactly as today — closed/denied ports behave identically regardless of where the port was parsed from.
- **No NetworkPolicy change:** traffic still enters via the API pod → `podIP:4097` → agentd, identical to both shipped modes.

### Host disambiguation (stress-test finding F1/F2 — the naive regex is wrong)

The obvious widening `{host:[0-9]+-[a-z0-9-]+-preview\.…}` **also matches legacy hosts whose UUID starts with a digit**: `1044f4f2-ba96-…-preview` parses as port `1044` + invalid workspace `f4f2-ba96-…`. 10/16 hex chars are digits → ~62% of workspaces; 9/16 on the live cluster today. The port-suffix alternative (`<uuid>-<port>-preview`) collides symmetrically: UUIDs *ending* in a digit (~62%, e.g. `42ae…-84e8-preview`) satisfy `[0-9]+-preview`.

Both routes must use **anchored, fixed-length UUID shapes** (in the handler), which are disjoint by construction:

- New: `^[0-9]{1,5}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.<base>$`
- Legacy: `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.<base>$`

Fixed lengths make the partial-eat ambiguity impossible: a UUID's first segment is exactly 8 hex chars before the first `-`, and `[0-9]{1,5}` can span at most 5 — so a digit-leading legacy UUID can never satisfy the new form even when all 8 first-segment chars are digits, and a new-form host's first `-` at position ≤5 can never satisfy `[0-9a-f]{8}`. **Invariant (load-bearing): the port quantifier must stay strictly below 8** (`{1,5}` today; raising it to `{1,8}` reintroduces the collision). A property test over generated UUIDs (digit-leading, digit-ending, all-digit segments) proves disjointness.

### Required changes (sketch)

1. **Bootstrap 302 target** (`preview_origin.go` `HandleBootstrap`): redirect to `https://<port>-<ws>-preview.<base>/` instead of `https://<ws>-preview.<base>/<port>/`.
2. **Preview-host routing** (`PreviewOriginHandler`): parse per the two anchored regexes above (handler-side; edge stays loose — see edge bullet). **Precedence rule: host-port wins** — when the host carries the port, the entire path is app-path, passed verbatim (no path parsing at all); legacy hosts keep path-prefix parsing. **Ordering hazard (pass-3 finding): the bare-root landing branch (preview_origin.go:241-245) fires on `Path == "/"` *before* any port parsing and its `hasValidPreviewCookie` disjunct is not path-shape-aware — applied unchanged to port-hosts, an authenticated user at the app root would get the landing page instead of their app.** It must be gated to legacy hosts; on port-hosts, `/` is the app root (landing only via the unauthenticated-navigation branch). The handler must **reject host-port ≠ token-port** at `?t=` redemption (the payload's `Port` binds the token to one port-host). Prefix-less-path 502s disappear structurally for new-shape URLs. The post-redemption 303 (preview_origin.go:316) retargets from `/​<port><subPath>` to just `<subPath>` on port-hosts.
3. **agentd tool output** (`cmd/workspace-agentd/mcp_server.go` `mcpDevPreviewURL`): origin-mode marker gains the port-in-host form. Frontend `DevPreviewOutput` (`frontend/src/components/chat/MessagePart.tsx:222`) takes the button href **verbatim from the markdown link**, and its marker regex (`port=(\d+)(?: origin=(\S+)|…)?`) already matches the new output — so old UIs link correctly without a sentinel bump; only the "served from `<workspace>-preview.<origin>`" hint text renders cosmetic-stale. Update the hint in the same PR.
4. **Landing page** (`:port` form) + **frontend settings drawer** link builder: new URL shape.
5. **Edge:** no change — see the edge bullet under "What already fits" (mechanically verified; live rule already matches the new shape and preserves N1's 421 path).
6. **Cookie model:** `__Host-pv` is host-only by spec (no `Domain=` allowed on `__Host-` prefixed cookies). Each port-host needs its own bootstrap. **Consequence (stress-test F5): today one cookie covers every port of a workspace (shared host); the new scheme treats each port-host as cookieless on first visit.**
   - **Required behavior (pass-2 correction — the mechanism already ships):** `servePreview`'s landing-page branch (preview_origin.go:285-295) already handles exactly this case for navigations — `Sec-Fetch-Mode: navigate` + no token + no cookie → `serveLanding` with the port prefilled and a one-click bootstrap link, and non-navigations keep the 401. It is host-agnostic; it works on port-hosts unchanged, and its "no auto-redirect, no password prompt" design (phishing-resistance, preview_origin.go:353-361) is preserved. No new loop-guard machinery is needed for the required path.
   - **Optional zero-click enhancement (defer unless wanted):** replace the port>0 landing branch for navigations with `302 → api-origin bootstrap`. Costs: an unauthenticated API session terminates the chain at the API's 401 JSON (no loop, but poor UX — the shipped one-click landing is deliberately nicer); WebSocket handshakes cannot follow redirects at all, so WS/HMR reconnect after cookie expiry 401s until the user re-navigates regardless. Recommend shipping with the shipped landing behavior first.
   - Accepted consequence: distinct preview ports are distinct browser origins — cross-port `fetch` between two preview ports needs CORS; navigation does not. Codespaces/Gitpod accept the same trade.
   - **Documented residual (F7):** OAuth callbacks configured to `localhost:<port>` and hardcoded absolute `localhost` links remain broken — identical to both shipped modes; out of scope, to be stated in `docs/user/dev-preview.md`.
7. **Prerequisite bug (pass-2 root cause — live defect in the current implementation):** `LLMSAFESPACE_API_URL` is set only on the **`credential-setup` init container** (pod_builder.go:671, consumed by `workspace-agentd bootstrap`), while `mcpDevPreviewURL` (`cmd/workspace-agentd/mcp_server.go:234`) runs in the **main container**, which gets `PREVIEW_ORIGIN_BASE_DOMAIN` but not `LLMSAFESPACE_API_URL` — validated on the live pod: init has it, main doesn't. Result: the tool emits a **relative** bootstrap link (`/api/v1/…`), which the browser resolves against `chat.safespaces.dev` — and the chat host's `/api` proxying is itself 502ing today (second, independent live defect; `chat.safespaces.dev/api/health` → 502 while `chat.safespaces.dev/health` → 200). Fix notes: (a) stamp the env on the **main** container next to `PREVIEW_ORIGIN_BASE_DOMAIN`; (b) the value must be the **externally reachable API origin** (`https://api.safespaces.dev`), *not* the in-cluster `--api-service-url` svc URL the init container gets (`http://llmsafespaces-api.llmsafespaces.svc:8080` is browser-unreachable — deriving from `PreviewOriginBaseDomain` per the `apiOrigin()` convention at preview_origin.go:339-344 is the safer source); (c) the chat-host `/api` 502 needs its own ticket — it breaks every relative API link from chat today, preview or not.
8. **Docs** (`docs/user/dev-preview.md`): rewrite the URL-shape section; the "root-absolute URLs break" caveat is deleted rather than documented; add the localhost-residual note from item 6.

### Migration / compatibility

- Keep path-mode and the legacy `<uuid>-preview.<base>/<port>/` host+path route working (both already shipped) for a deprecation window; the chat button and settings drawer switch to the new shape.
- The bootstrap token already carries `Port`; the `?t=` consumption flow is unchanged — only the redirect target moves.

## Explicitly out of scope

- Public/shareable preview URLs (epic-66 D7 decision unchanged — trust-and-safety staffing prerequisite).
- Cross-port session sharing (would require dropping the `__Host-` prefix and a cookie-name collision analysis; revisit only if users hit the CORS consequence in practice).
- Path-based `/dev-preview/*portPath` mode on the API origin (unchanged, unaffected).

## Stress-test survivors (validated 2026-08-21)

- Host label ≤50 chars (`<5-digit port>-<uuid>-preview`); full name well under the 253 limit.
- Single-label shape matches `*.safespaces.dev` (grey-cloud wildcard) and `wildcard-safespaces-dev` unchanged.
- T1 (not the API origin) and T3 (uniform dead/denied behavior) postures intact.
- **Zero agentd/runtime-image changes** — the API still dials `podIP:4097` with `/v1/dev-preview/:port/` prefixing (containment: `pkg/agentd` untouched, no workspace image rebuild).
- `X-Forwarded-Host` (dev_preview.go:250) now carries the true public origin, making it genuinely useful to proxy-aware frameworks (in path mode the prefix was lost).
- No rewrite stack → no body buffering; the size cap and 101/SSE exclusions keep their current shape.

## Acceptance requirements (to be expanded into ACCEPTANCE.md)

1. **Regex disjointness property test** over generated UUIDs (digit-leading, digit-ending, all-digit segments) — no host matches both route forms.
2. Precedence: host-port + any path → path verbatim to app (including path-first-segment-looks-like-port).
3. Token redemption rejects host-port ≠ token-port.
4. Cookie fallback on a fresh port-host: navigation without cookie/token → landing page with port prefilled (shipped `serveLanding` behavior, unchanged semantics); non-navigation → 401; uniform across live/dead/denied ports.
5. **tinyrsvp as the e2e regression fixture** (dogfooding the incident app): root-absolute `303 /login?return=%2F` must complete an auth round trip through the preview origin.
6. Legacy host+path URLs keep working through the deprecation window, including digit-leading-UUID workspaces (the F1 population).
7. Malformed preview hosts (`garbage-preview.<base>`) still reach the API and get 421 (THREAT-MODEL N1) — no host regresses to an edge-level default 404.
8. Port-host app root: authenticated `GET /` on a port-host proxies to the app (never the landing page); unauthenticated navigation gets the landing; non-navigation gets 401.

## Open questions

1. Port label normalization: leading zeros / `0` / `65536` bounds — reuse the existing `port < 1024 || > 65535` deny policy at the host-parsing seam, keeping T3's uniform response.
2. Host-label casing: browsers lowercase Host on send; Traefik HostRegexp is case-sensitive — normalize before match or restrict the regex to lowercase (current route already assumes lowercase).
3. Should the legacy host+path route log a deprecation notice to guide users off it?

## Evidence index (2026-08-21 incident)

- API log: `preview-origin: refused host=c547ddb8-…-preview.safespaces.dev path=/login reason="port not numeric or out of range"` → 502 generic body.
- In-pod probe: `curl -u opencode:$PW http://127.0.0.1:4097/v1/dev-preview/8080/` → `303 Location: /login?return=%2F` (root-absolute; app healthy).
- Working counterexample: `https://42ae0489-…-preview.safespaces.dev/5173/status.html` serves; same workspace's `/health`, `/favicon.ico` 502 with the identical refusal reason.
- Edge verified healthy post talos-ops-prod #2304: preview host without credentials → 401 + `x-request-id` (API middleware), not the pre-fix Traefik 404 (worklog 0813).

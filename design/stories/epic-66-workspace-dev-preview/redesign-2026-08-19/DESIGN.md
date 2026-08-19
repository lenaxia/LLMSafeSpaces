# Dev Preview Redesign: Per-Workspace Origins

**Status:** Draft for platform-team review
**Date:** 2026-08-19
**Evidence base:** black-box testing through the live tunnel (see
`harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)REGRESSION.md` and the test pages in that
directory). The author has no access to API/agentd source; items marked
**[ASSUMPTION]** need verification by the team.

---

## 1. Problem statement

The current dev preview serves workspace apps from the **API origin** under
`/api/v1/workspaces/<ws>/dev-preview/<port>/`. This creates one structural
problem and several policy problems that currently offset each other:

1. **Structural:** preview pages are same-origin with `api.safespaces.dev`.
   The session cookie (`lsp_session`) is sent to previews, and — because
   `connect-src 'self'` *permits* same-origin — any preview JS can call any
   API endpoint with the user's credentials. Preview content is
   agent-authored and prompt-injectable, so this is the primary exfil/CSRF
   surface. No CSP directive can close it.
2. **Policy:** to compensate, the platform enforces a strict CSP
   (`script-src 'self'`, `style-src 'self'`, no eval) that blocks
   Next.js App Router hydration, inline styles, Vue full-build, etc.
   The result is strict where it hurts developers most and permissive where
   it matters most for security.
3. **Implementation gaps:** the chain caches HTML (no `no-store` enforced),
   the proxy strips WebSocket upgrades even though `connect-src` allows
   `wss:`, responses are rate-limited at ~100 requests (window unknown),
   and Cloudflare mutates response bodies (beacon injection).

**The thesis:** move previews to their own origin
(`<workspace>-preview.safespaces.dev`). That shrinks the blast radius of a
malicious preview from "the user's platform session" to "the preview
itself," which in turn makes the strict CSP unnecessary — developers get
inline scripts, eval, styles, and HMR back, and security gets *stronger*
isolation than today.

---

## 2. Goals / non-goals

**Goals**

- G1: Previews are off the API origin; no platform cookies reach them.
- G2: Workspaces are mutually isolated (origin-per-workspace).
- G3: First-class dev experience: inline scripts/styles, eval, WebSocket
  HMR, no stale caching, rate limits sized for real dev workflows.
- G4: Every change verifiable with the existing regression suite
  (extended — see §8).
- G5: Zero-downtime migration; old URLs work throughout the eval period.

**Non-goals**

- Pod-level network isolation (already handled by cluster egress policy).
- Supporting cross-origin API calls *from* preview JS (still blocked;
  rationale in §5.4).
- Multi-user collaboration on one preview.
- Long-lived/public preview links (product question, out of scope; the
  signed-URL model in §5.3 supports it later if wanted).

---

## 3. Current state (all empirically verified 2026-08-19)

```
browser ──Cloudflare──▶ API (DevPreviewHandler) ──▶ agentd :4096/4097 ──▶ app :<port>
            │                   │
   caches HTML             applies CSP:
   injects beacon          script/style-src 'self'
   rate limit ~100         connect-src 'self' wss:
   (window unknown)        frame-ancestors 'none'
   strips WS Upgrade
```

See `REGRESSION.md` for the full capability matrix and evidence.

---

## 4. Proposed architecture

```
                         ┌─────────────────────────────────────────────┐
                         │                Cloudflare                   │
  browser ──HTTPS/WSS──▶ │  *.safespaces.dev (Universal SSL covers      │
                         │  <ws>-preview single-label hosts)            │
                         │  hostname-suffix rules: no beacon / no       │
                         │  rocket loader on *-preview hosts            │
                         │  WS pass-through enabled                     │
                         └──────────────┬──────────────────────────────┘
                                        │ Host: <ws>-preview.safespaces.dev
                                        ▼
                         ┌─────────────────────────────────────────────┐
                         │   Preview edge (DevPreviewHandler, host-    │
                         │   routed mode — same deployment as API,     │
                         │   but NEVER serves /api/* on preview hosts) │
                         │   1. validate preview session cookie        │
                         │      (issued from one-time signed URL)      │
                         │   2. force no-store on HTML                 │
                         │   3. relaxed CSP (§5.4)                     │
                         │   4. forward Upgrade/Connection             │
                         └──────────────┬──────────────────────────────┘
                                        │ ws → agentd (auth injected)
                                        ▼
                         ┌─────────────────────────────────────────────┐
                         │ agentd :4096/4097 in workspace pod          │
                         │ hop-by-hop aware; 101 pass-through          │
                         └──────────────┬──────────────────────────────┘
                                        ▼
                                   app :<port>
```

**URL scheme**

```
https://<workspace-uuid-lowercase>-preview.safespaces.dev/<port>/<path>
e.g. https://42ae0489-8d54-42a3-af62-163e50da84e8-preview.safespaces.dev/5173/index.html
```

- **Single-label host** (`<ws>-preview`, not `<ws>.preview`): Universal SSL
  already covers `*.safespaces.dev`, so no ACM/custom cert is needed.
  Origin isolation is identical (same-origin keys on the full hostname),
  and dropping the intermediate `preview.safespaces.dev` label removes a
  cookie-bleed footgun. Label fits DNS limits (36+8 = 44 < 63 chars).
- Port goes in the **path**: ports within a workspace are one trust domain
  (one agent, one user), so one origin per workspace is correct. No port
  allowlist — see §5.6 for the blocklist-only policy.
- Dev-preview tooling returns this URL directly (it already knows workspace
  and port); the port is a runtime parameter, never a workspace setting.

**Request flow**

1. `dev_preview_url` tool → API mints a **one-time signed bootstrap token**
   (see §5.3) and returns
   `https://<ws>-preview.safespaces.dev/<port>/?t=<token>`.
2. Edge validates token → issues a **host-scoped preview session cookie**
   (`__Host-pv`, HttpOnly, Secure, SameSite=Lax, short TTL) → redirects to
   the clean path.
3. All subsequent requests (subresources, HMR WS, reloads) authenticate via
   that cookie. Token never appears in a second URL; page JS cannot read it
   (HttpOnly); `location.href` exposure is bounded to the first request.
4. Edge strips `t` from the path, rewrites to agentd, proxies both
   directions; for `/ws`-style upgrades, forwards `Upgrade`/`Connection`
   verbatim and stops buffering.

---

## 5. Component design

### 5.1 DNS & Cloudflare

- DNS: one wildcard record `*.safespaces.dev` → API ingress (proxied).
  Exact records (`api.`, `app.`) take precedence per normal DNS matching;
  **the ingress must default-backend/421 unknown hosts** so the wildcard
  exposes nothing unintended.
- **Cert:** none to procure — Cloudflare Universal SSL covers the apex +
  first-level wildcard, and `<ws>-preview` is a single label. (This is a
  deliberate benefit of the single-label scheme over `<ws>.preview.…`,
  which would have required ACM.)
- Cloudflare rules for hosts **ending in** `-preview.safespaces.dev`
  (hostname-suffix expressions):
  - Browser Insights / beacon: **off** (stops body mutation — observed
    injecting `beacon.min.js`).
  - Rocket Loader, auto-minify, email obfuscation: **off** (all body
    rewriters).
  - WebSockets: **on** (zone-level setting).

### 5.2 Routing (API layer)

- Host-header routing: `Host: <uuid>-preview.safespaces.dev` → preview
  pipeline; anything else → API pipeline.
- **Hard rule:** on preview hosts, only the proxy route exists. `/api/*`
  must 404 on preview hosts, and `/api/v1/.../dev-preview/...` path-based
  routes must not respond on the API host once migration completes
  (§7 phase 4). This is the seam that enforces G1 — it must be tested, not
  assumed (§8 T6).
- WebSocket upgrade handling at this layer: pass `Upgrade`/`Connection`
  through, disable response buffering after 101, no timeout shorter than
  5 min for upgraded connections.

### 5.3 Auth model (no platform cookies on preview origin)

- **Bootstrap token:** `base64url(json payload).HMAC-SHA256` where payload =
  `{ws, ports[], exp (~24h), jti}` — `ports` taken from the tool call's
  runtime parameter, not from any workspace setting. Key lives only in the
  API/edge. One-time use: jti marked consumed at first redemption.
- **Preview session cookie:** `__Host-pv=<random 128-bit id>` set on first
  redemption. `__Host-` prefix guarantees host-only + Secure + path=/ —
  the preview origin can never receive a cookie meant for another host,
  and vice versa. Server-side map id → {ws, exp}; TTL e.g. 7 days or
  workspace suspend, whichever first.
- **API session cookie hardening — RESOLVED 2026-08-19 (code inspection):**
  `lsp_session` is HttpOnly + Secure + explicit SameSite=Lax on every
  issuance path in current main (5ff0f2ef, 2026-08-11; the audited
  deployment predated it). The `__Host-` rename is dropped — incompatible
  with Epic 54's optional wildcard `cookieDomain` (`__Host-` forbids
  Domain attributes), defending only a Low-severity shadowing nuisance.
  **Same-site caveat:** preview hosts share the registrable domain, so in
  Epic 54 deployments (wildcard cookieDomain) SameSite does NOT gate
  preview→API requests — the CORS origin allowlist (explicit list;
  credentials only on match; fail-closed wildcard+credentials guard) is
  the load-bearing control for G1. Phase 1 checklist: preview origins are
  never added to `security.allowedOrigins`.
- **agentd auth:** unchanged — the edge continues to inject whatever
  credential agentd expects today (we observed Basic auth on direct
  probes). **[ASSUMPTION: DevPreviewHandler already holds per-workspace
  agentd credentials]**

### 5.4 CSP on the preview origin

Recommended default:

```
default-src 'self';
  script-src 'self' 'unsafe-inline' 'unsafe-eval';
  style-src  'self' 'unsafe-inline';
  img-src    'self' data: blob:;
  font-src   'self' data:;
  connect-src 'self';
  object-src 'none';
  base-uri 'self';
  form-action 'self';
  frame-ancestors https://app.safespaces.dev   [dashboard origin — OPEN Q3]
```

Rationale, given origin separation:

- `unsafe-inline`/`unsafe-eval` for scripts/styles: unblocks Next.js App
  Router, Vue full build, inline styles. Marginal attacker value is now
  near zero — an injected preview page can already serve *external*
  same-origin JS, and there is nothing on this origin left to steal.
- `connect-src 'self'` (strictly, **without** bare `ws:`/`wss:`):
  closes network exfil channels (fetch/XHR/WebSocket to attacker hosts).
  This is what makes the inline relaxations safe. Devs proxy external APIs
  through their own app — which they'd do anyway for API keys.
  *Tradeoff option:* adding `https:` to connect-src enables direct
  browser→public-API calls; it also opens exfil of in-page data and the
  bootstrap-token URL. Not recommended as default; could be a per-preview
  opt-in later.
- `frame-ancestors` allowlisting the dashboard enables embedding previews
  in the product UI — new capability, previously foreclosed by `frame-ancestors 'none'`.
- `object-src 'none'`, `base-uri 'self'`, `form-action 'self'`: keep (free).

Residual gaps (accepted): top-level navigation redirects with query-string
data (a CSP blind spot, low volume); data exfil via navigation is bounded
and noisy.

### 5.5 Proxy behavior fixes (ship independently of origins — §7 phase 0)

- **`no-store` enforcement:** edge forces `Cache-Control: no-store` on
  `text/html` responses (respects app-set caching for other MIME types, so
  hashed assets still cache). Rationale: the stale-HTML failure mode
  already cost a multi-hour debugging session; agents cannot fix this
  client-side.
- **WS forwarding:** as §5.2. Note today's CSP contains bare `wss:` —
  evidence the *policy* always intended WS; close the implementation gap
  and drop the bare scheme in the same change (new policy has none).
- **Streaming responses (SSE) pass through UNBUFFERED — verified.** An
  SSE drip (10 chunks @ 200ms) arrived at the browser with inter-chunk
  gaps of 197–206ms, exactly matching server cadence. Two implications:
  (a) open question Q1 is largely answered — nothing in the chain buffers
  streaming bodies, so WS forwarding has no buffering layer to fight;
  (b) **SSE is a viable live-reload transport today**, and should be the
  documented HMR workaround until WS forwarding ships (e.g.
  `vite --host` with an SSE-based reload plugin, or livereload-style
  tooling).
- **Rate limits — current enforcement measured (2026-08-19):** the
  `x-ratelimit-*` headers (limit 100) appear on responses, but a 127-request
  burst through the proxy path produced **zero 429s and a frozen
  `remaining` counter** — subresource GETs through the preview proxy are
  not counted against the budget (server log confirms all requests
  traversed). The earlier fear that multi-file apps would exhaust the
  budget is therefore not an acute problem today; the headers are
  effectively decorative on the proxy path. Recommendation unchanged for
  the new origin — a real, documented budget with `Retry-After` — but the
  number should come from instrumentation (Q4), not the current 100.
- **Transfer capacity baseline (2026-08-19):** 2 MB body passed intact at
  ~4.2 MB/s; latency p50 ≈ 50 ms / max 110 ms at concurrency 10 through
  the full chain (browser → CF → API → agentd → app). No size or
  concurrency ceilings observed at dev-workload scale.

### 5.6 Port policy (blocklist-only; no settings UX)

The port is a **runtime parameter** of the `dev_preview_url` call, pinned
into the signed token — never a workspace setting. A static port field in
settings would go stale the moment an agent restarts a dev server on a
different port; the agent is the component that knows the real port.

Two DX upgrades instead of a settings field:

1. **Listener probing:** agentd shares the pod's network namespace and can
   enumerate listening ports. `dev_preview_url(port)` should probe before
   returning — e.g. "no listener on 5173; found: 3000, 8080." (Today the
   tool returns a URL for a dead port; observed in the wild.)
2. **Honest 502s:** proxying a port with no listener returns an
   agent-readable error body ("nothing listening on :5173 in this pod").

Proxy target restrictions — blocklist, not allowlist, enforced at BOTH the
tool layer and the proxy layer (either alone is a single point of failure;
**verified 2026-08-19: the tool currently mints URLs for :4097 (agentd) and
:22 without complaint** — see THREAT-MODEL.md T3):

| Blocked | Reason |
|---|---|
| 4096, 4097 (agentd) | Control plane with injected credentials; must never sit behind a public preview URL regardless of its own auth |
| Platform sidecar ports (deployment-defined list) | Metrics/health agents are not written for browser exposure |
| < 1024 | Nothing legit binds there without root; free defense-in-depth |

Everything else (1024–65535 minus the list) is proxiable: the origin model
already bounds the blast radius of any open port to the user's own
workspace, and the signed token scopes which ports a given URL may hit.
No per-user port counts; abuse is handled by rate limits (§5.5).

---

## 6. Security analysis

| Threat | Today | After |
|---|---|---|
| Prompt-injected preview exfiltrates session / calls API as user | **Possible** (same-origin, cookies sent, connect-src 'self' allows it) | Blocked: cross-origin, no cookies, API sends no CORS grants to preview origins |
| Preview from workspace A attacks workspace B | n/a (same origin — mutual exposure) | Blocked: distinct origins, host-only cookies |
| Service worker hijack across workspaces | n/a | Blocked (origin-scoped) |
| Intermediary-injected script (CF beacon-style) executes | Blocked by CSP (accidentally) | Blocked: body rewriters off for preview host |
| Session theft via XSS on preview | High impact (cookie reachable) | Low impact: HttpOnly host-only `__Host-pv` only; nothing platform-valuable on origin |
| Clickjacking preview | Blocked (`frame-ancestors 'none'`) | Blocked except dashboard allowlist (intended) |
| Token leakage | n/a | Bounded: one-time bootstrap token, HttpOnly session, short TTLs |

**Net:** every "after" row is blocked *by architecture*, not by policy —
which is exactly why the CSP can then afford to be developer-friendly.

---

## 7. Migration plan

**Phase 0 — fixes on the current path-based URLs (no breaking changes)**
1. Force `no-store` on HTML (edge).
2. Forward WS upgrades end-to-end; drop bare `wss:` from existing CSP.
3. Audit cookie posture; migrate `lsp_session` to `__Host-` prefix
   (staggered: accept both one release, then retire old name).
4. Disable CF body rewriters on the existing preview path.
   *Exit criteria: regression suite green on old URLs (WS now PASS; cache
   headers present; beacon absent).*

**Phase 1 — new origin behind a flag**
5. DNS wildcard record + host routing + signed-URL auth (§5.1–5.3). No cert procurement needed (Universal SSL covers single-label hosts).
6. `dev_preview_url` returns the new URL (flag-gated per workspace).
   *Exit: new URLs serve, T1–T6 tests green, old URLs still work.*

**Phase 2 — relax CSP on the new origin only**
7. Ship §5.4 policy to preview hosts. Old path URLs keep the strict policy.
   *Exit: csp-test + tier3-test show inline/eval/style PASS on new origin;
   still FAIL on old (deliberate, during overlap).*

**Phase 3 — soak**
8. Workspace dogfoods new origin (this workspace will do — the regression
   suite exists here). Watch: 429 rates, WS stability, cookie oddities.
   *Exit: 2 weeks clean, or issues fixed.*

**Phase 4 — cut over**
9. Old path-based preview returns 410 Gone (after user-facing notice);
   `/api/v1/workspaces/*/dev-preview/*` route removed from the API host.
   *Exit: T6 confirms API-host route is dead.*

---

## 8. Test plan (extends `harness/ (repo: design/stories/epic-66-workspace-dev-preview/redesign-2026-08-19/harness/)`)

**Step-by-step verification commands per phase: see ACCEPTANCE.md.**

Existing pages, re-purposed per phase:

| Test | Page | Phase 0 expect | Phase 2+ expect |
|---|---|---|---|
| WS echo round-trip | `ws-test.html` | PASS | PASS |
| No-store on HTML | header check | PASS | PASS |
| Inline script / handler | `csp-test.html` | FAIL | **PASS** |
| Inline style / eval / new Function | `tier3-test.html` | FAIL | **PASS** |
| JSON data block intact | `tier3-test.html` | PASS | PASS |
| Cross-origin fetch | `tier3-test.html` | FAIL (CSP) | FAIL (CORS — different reason, still blocked) |
| SSE streams unbuffered | `stress.html` phase 1 | PASS (verified 2026-08-19) | PASS |
| Large body integrity | `stress.html` phase 2 | PASS at 2 MB (verified) | PASS |

New tests to add:

- **T1 cookie absence:** on preview origin, `document.cookie` contains no
  `lsp_session`; API-origin cookie jar not readable.
- **T2 cross-workspace:** preview in ws A attempts `fetch('https://<wsB>.preview...')`
  → network/CORS failure (no cookie sent, no CORS headers).
- **T3 service worker scope:** register SW on preview origin; confirm
  `scope` is that origin only.
- **T4 bootstrap token:** redemption is one-time (second use of same `t`
  → 401); cookie survives reload; cookie dies at workspace suspend.
- **T5 host segregation:** `https://<ws>-preview.safespaces.dev/api/v1/...`
  → 404 (T6 is the converse).
- **T6 old-route death (phase 4):** path-based preview URL → 410/404.
- **T7 rate limit:** scripted burst confirms 429 + `Retry-After` at the
  documented budget.

---

## 9. Open questions

1. **Q1** agentd transport to pod internals: does anything between edge and
   app buffer responses (breaks WS/SSE)? **[largely answered empirically —
   SSE streams through unbuffered at 200ms cadence, so no buffering layer
   exists on the proxy path; remaining risk is the WS 101 path specifically,
   which needs code review]**
2. **Q2** Does `DevPreviewHandler` hold per-workspace agentd credentials
   today, or is auth injected another way? (Observed Basic on agentd.)
3. **Q3** Dashboard origin for `frame-ancestors` allowlist — what hosts the
   product UI uses.
4. **Q4** Rate-limit budget: is 600/5min/workspace right? Instrument first,
   decide with data. **[data point 2026-08-19: current limit-100 headers are
   not enforced on proxy-path subresources — 127 requests, 0×429, counter
   frozen. Budgeting can start from real usage telemetry once the preview
   host exists.]**
5. **Q5** Any existing consumers of the path-based preview URL format
   (docs, tooling, user bookmarks) that phase 4 would break?

---

## 10. Work breakdown (rough)

| # | Item | Size | Phase |
|---|---|---|---|
| 1 | no-store enforcement | S | 0 |
| 2 | WS forwarding + CSP `wss:` cleanup | M | 0 |
| 3 | Cookie audit + `__Host-` migration | S–M | 0 |
| 4 | CF page rules (existing path) | S | 0 |
| 5 | DNS wildcard + host routing (no cert needed) | S–M | 1 |
| 6 | Signed-URL auth + `__Host-pv` cookie | M | 1 |
| 7 | Relaxed CSP on preview hosts | S | 2 |
| 8 | Tooling: `dev_preview_url` returns new scheme | S | 1 |
| 9 | Test suite T1–T7 | M | 1–4 |
| 10 | Listener probing + honest 502s (§5.6) | S | 1 |
| 11 | Port blocklist enforcement (agentd/sidecars, <1024) | S | 1 |
| 12 | Deprecation + comms | S | 4 |

Total: roughly 2–3 engineer-weeks; items 1–4 are independently shippable
this sprint and deliver immediate DX value (WS + no caching) with zero
migration risk.

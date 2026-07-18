# Worklog: Traefik CORS expose-headers override — production fix + operator docs

**Date:** 2026-07-18
**Session:** Diagnose and fix a production bug where the chat UI's "Load earlier messages" button never rendered, then close the documentation gap that allowed the bug to bit-rot.
**Status:** Complete — production fix merged (talos-ops-prod PR #2053), operator documentation added.

---

## Objective

1. Root-cause why `res.headers.get("X-Next-Cursor")` returned `null` in the `chat.safespaces.dev` browser despite the header being physically present on the wire.
2. Verify every assumption against live cluster state before recommending a fix (after several earlier wrong guesses).
3. Apply the fix to production.
4. Close the documentation gap that allowed the bug to bit-rot for ~12 days (pagination fix landed in v0.2.2 on 2026-07-07; user reported the symptom on 2026-07-17).

---

## Timeline of wrong hypotheses (recorded so future investigations skip them)

The investigation burned ~3 hours cycling through wrong locations before landing on the actual cause. Each wrong guess is recorded with the reason it was wrong, so the next investigator doesn't repeat the cycle.

| # | Hypothesis | Why it was wrong |
|---|---|---|
| 1 | Stale API deployment — `api.safespaces.dev` running pre-v0.2.2 binary | Refuted: HelmRelease pinned `api.image.tag: "0.3.0"`; pod-direct curl showed all 5 exposed headers being emitted correctly. |
| 2 | Cloudflare Managed Transforms rewriting the response | Refuted: every Managed Transform in the zone was disabled. Managed Transforms can't inject the specific headers seen anyway (`X-Frame-Options: allow-from https://thekao.cloud` is not in any MT pack). |
| 3 | Cloudflare Transform Rules (Modify Response Header) | Refuted: the zone didn't even have the Transform Rules page enabled. |
| 4 | Cloudflare Snippets / Page Rules | Refuted: both empty. |
| 5 | Cloudflare Worker Route on `api.safespaces.dev/*` | Refuted: only Worker Route in the zone was `relay.safespaces.dev/*` (a different hostname — the inference relay from Epic 26). |
| 6 | Header casing `X-Request-Id` vs `X-Request-ID` as fingerprint of emitter | Refuted: HTTP/2 lowercases all headers; browsers normalize display casing. Casing is suggestive at best, never conclusive. Don't repeat this argument. |

**The actual root cause** was in cluster config that wasn't checked until the cluster repo (`talos-ops-prod`) was cloned and inspected. **Lesson reinforced: don't speculate past the evidence.** The wire response had the answer from the first paste — `Access-Control-Allow-Headers` / `Allow-Methods` / `Max-Age` all matched the app's values, only `Expose-Headers` differed. That diff alone ruled out a wholesale-rewrite at any layer and pointed at a surgical override at the origin edge. The investigation should have chased that diff immediately instead of touring the Cloudflare dashboard.

---

## Root cause (verified, not assumed)

The Traefik Middleware `llmsafespaces-api-cors` (namespace `llmsafespaces`, file `talos-ops-prod/kubernetes/apps/llmsafespaces/llmsafespaces/app/middleware-cors.yaml`) had:

```yaml
spec:
  headers:
    accessControlExposeHeaders:
      - X-Request-Id    # only this one; should be 5
```

Traefik's Headers middleware (`header.go:PostRequestModifyResponseHeaders`) unconditionally calls `res.Header.Set("Access-Control-Expose-Headers", exposeHeaders)` on every actual response when the list is non-empty, **overwriting** whatever the Go app emitted. The Go app (`api/internal/middleware/security.go:64`, fixed in commit `1ca06c51`, shipped in v0.2.2 / present in v0.3.0) correctly emits 5 headers:

```go
ExposedHeaders: []string{
    "X-Request-ID",
    "X-RateLimit-Limit",
    "X-RateLimit-Remaining",
    "X-RateLimit-Reset",
    "X-Next-Cursor",
},
```

Because the chat frontend is cross-origin (`chat.safespaces.dev` → `api.safespaces.dev`), the browser hides any response header not named in `Access-Control-Expose-Headers` from JavaScript. `X-Next-Cursor` and three `X-RateLimit-*` headers were silently stripped from the JS view, even though physically present on the wire. The frontend's `useInfiniteQuery` hook read `nextCursor` from `res.headers.get("X-Next-Cursor")`, got `null`, set `hasNextPage=false`, and the "Load earlier messages" button never rendered.

### Why Traefik overrode only one CORS header

Verified against Traefik source (`pkg/middlewares/headers/header.go` at v3.1):

- `PostRequestModifyResponseHeaders` (runs on every actual response) unconditionally `Set()`s: `Access-Control-Allow-Origin`, `Access-Control-Allow-Credentials`, `Access-Control-Expose-Headers`. These are the three fields where Traefik overrides the origin.
- `processCorsHeaders` (runs only on OPTIONS preflight) sets: `Access-Control-Allow-Headers`, `Access-Control-Allow-Methods`, `Access-Control-Max-Age`. These do **not** get overwritten on actual responses — the app's values pass through.

This explains the wire-response pattern exactly: Allow-Headers / Allow-Methods / Max-Age matched the app; only Expose-Headers matched Traefik's config. The bug is specific to the Expose-Headers field because that's the only one where (a) the operator hand-rolled a value AND (b) Traefik unconditionally overrides on actual responses.

---

## Verification matrix (every link in the causal chain)

| # | Claim | Verification |
|---|---|---|
| 1 | Deployed v0.3.0 contains the 5-header list in `security.go` | `git show v0.3.0:api/internal/middleware/security.go` returns the exact list |
| 2 | `1ca06c51` (the fix commit) is in tag v0.3.0 | `git tag --contains 1ca06c51` includes v0.3.0 |
| 3 | Prod HelmRelease sets `allowedOrigins` matching the request Origin | `helm-release.yaml:106` = `https://chat.safespaces.dev`; request Origin matches |
| 4 | App actually emits 5 headers at runtime | `app.go:742-751` overrides only `Development/RequireHTTPS/AllowedOrigins/AllowCredentials/CSP` — never `ExposedHeaders`. Default flows through. |
| 5 | Traefik overrides Expose-Headers on actual responses | Traefik source `header.go:PostRequestModifyResponseHeaders` — unconditional `Set()` when `len > 0` |
| 6 | Traefik does NOT override Allow-Headers / Methods / Max-Age on actual responses | Traefik source `processCorsHeaders` — those fields only set inside `req.Method == http.MethodOptions` preflight branch |
| 7 | Wire response matches predicted pattern | Allow-Headers/Methods/Max-Age match APP values; Expose-Headers matches Traefik's 1-item list |
| 8 | Middleware is bound to the api.safespaces.dev Ingress | `ingress-api.yaml:18` annotation references `llmsafespaces-llmsafespaces-api-cors@kubernetescrd` |
| 9 | No competing override in the chain | `middlewares-secure-headers` (in `networking-chain-no-auth`) has no `accessControlExposeHeaders` field |
| 10 | Live state matches git (no drift) | Read-only cluster investigation confirmed byte-identical Middleware CR |

### Live pod-direct vs Traefik-mediated curl (read-only investigation)

Endpoint: `GET /api/v1/auth/config` (returned 404 — route doesn't exist — but security middleware ran pre-routing, so CORS headers are intact).

| Path | `Access-Control-Expose-Headers` |
|---|---|
| Pod-direct (`curl http://llmsafespaces-api.llmsafespaces.svc.cluster.local:8080/...`) | `X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, X-Next-Cursor` |
| Traefik-mediated (`curl https://api.safespaces.dev/...` via `--resolve`) | `X-Request-Id` |

The diff is the smoking gun.

---

## Fix

### Production fix (talos-ops-prod PR #2053, MERGED)

Added the 4 missing entries to match `DefaultSecurityConfig().ExposedHeaders` in the app:

```yaml
accessControlExposeHeaders:
  - X-Request-Id
  - X-RateLimit-Limit       # added
  - X-RateLimit-Remaining   # added
  - X-RateLimit-Reset       # added
  - X-Next-Cursor           # added
```

Flux reconciled on next sync; user confirmed the "Load earlier messages" button renders after a hard reload.

### Documentation fix (this PR)

Added a "CORS at the edge" subsection to `docs/operator/networking.md` covering:
- The dual-layer CORS reality: app emits it, but Traefik's Headers middleware overrides it on actual responses when `accessControlExposeHeaders` is non-empty.
- The required 5-header list (pinned to match the app) — so future operators don't have to dig through `security.go` to find it.
- The general principle: any ingress middleware that touches `Access-Control-*` must mirror the app's full CORS config, or the app's headers get silently dropped.
- Why this bit-rotted: the chart documents only the nginx-ingress path, leaving Traefik operators to hand-roll middleware with no guidance.

---

## Key decisions

1. **Fix the operator's cluster config, not the app.** The app was already correct. The fix belongs in the cluster repo (`talos-ops-prod`), not the application repo (`llmsafespaces`). Adding a "defense" in the app (e.g. emitting the header twice, once via `Add` and once via `Set`) wouldn't have helped — Traefik's overwrite happens after the app responds, so the second `Set` would also be overwritten. The only fix is to make the edge middleware's expose-list match the app's.

2. **Documentation goes in `networking.md`, not `installation.md`.** The CORS content in `installation.md` is about the *app-side* config (`api.config.security.allowedOrigins`). The new content is about the *ingress-controller interaction* — that's a networking concern. Avoids duplication and matches the existing "Security headers" subsection placement.

3. **No chart change to enforce this.** Considered adding a chart-side rendered Traefik Middleware equivalent to the existing nginx-ingress `configuration-snippet`. Decided against: (a) Traefik middleware lives as a CR, not an annotation, and rendering it conditionally would balloon the chart's surface area; (b) operators running Traefik typically have their own middleware chains (rate-limit, auth, secure-headers) that the chart shouldn't displace. Documentation is the right level — operators read it and apply it to their cluster's middleware patterns.

---

## Blockers

None.

---

## Files modified

### talos-ops-prod (merged in PR #2053)

- `kubernetes/apps/llmsafespaces/llmsafespaces/app/middleware-cors.yaml` — added 4 entries to `accessControlExposeHeaders`.

### llmsafespaces (this PR)

- `docs/operator/networking.md` — new "CORS at the edge" subsection under "Ingress and TLS termination".
- `worklogs/NNNN_2026-07-18_cors-expose-headers-traefik-doc.md` — this worklog.

---

## Out of scope (follow-up issues to file)

1. **`middlewares-secure-headers` cluster-wide misconfiguration.** The shared `middlewares-secure-headers` Middleware in the `networking` namespace sets `accessControlAllowMethods` without setting `accessControlAllowOriginList`. With any `accessControl*` field set, Traefik short-circuits browser preflight at the edge — without `Allow-Origin`, every browser preflight used to fail. That's the original reason the bespoke per-app `llmsafespaces-api-cors` Middleware exists at all (per its file comment). Cleaner fix: drop CORS-related fields from the shared middleware so apps own their CORS, then delete the bespoke per-app middleware. Cluster-wide impact; separate PR in talos-ops-prod.

2. **`X-Frame-Options: allow-from https:thekao.cloud` applied cluster-wide.** The shared `middlewares-secure-headers` Middleware overrides every app's `X-Frame-Options` with a domain-specific value (`${SECRET_DEV_DOMAIN}`), silently downgrading the app's stronger `DENY`. Same for `Referrer-Policy` (drops `strict-`), `Permissions-Policy` (different syntax). Any app that sets its own security headers has them clobbered. Separate cluster-wide cleanup.

3. **Synthetic monitor for `X-Next-Cursor`.** A read-only curl that periodically hits the public API and asserts the response's `Access-Control-Expose-Headers` contains `X-Next-Cursor` would catch this class of bug (edge overrides app) at the user-visible layer, not the source-code layer. The regression test in `security_exposed_headers_test.go` only covers the Go app, which was already correct — it cannot catch this class of bug.

---

## Tests run

Not applicable — this is a docs change in the llmsafespaces repo. The production fix in talos-ops-prod PR #2053 passed all 8 CI checks (Flux Diff helmrelease, Flux Diff kustomization, Kubeconform, Labeler, PR Review AI, e2e workstation archlinux, e2e workstation generic-linux, e2e configure talos). The automated AI reviewer approved with no concerns.

---

## Next steps

1. Open this PR.
2. Verify the docs build succeeds and gh-pages auto-rebuilds via `.github/workflows/docs.yml`.
3. File follow-up issues #1 and #2 above in talos-ops-prod.

# Preview-origin hosts 404 after v0.17.0 — Traefik router error, not an API bug

**Date:** 2026-08-20
**Status:** Root cause found. Fix in review (talos-ops-prod #2304). No LLMSafeSpace code changes required — diagnosis only.
**Severity:** Availability — epic-66 Phase 1 fully dark in production despite a correct application rollout
**Epic:** 66 Phase 1 (per-workspace preview origins, redesign-2026-08-19)

---

## Summary

After the v0.17.0 rollout (Helm v355, chart 0.9.3+bb4a129e35c8), preview-origin hosts
(`https://<uuid>-preview.safespaces.dev/<port>/`) returned a plain `404 page not found`
instead of the expected **401** (preview middleware demanding credentials). The runbook's
leading hypothesis — stale pods running pre-env processes — was wrong. Every application-layer
check passed. The 404 was served by **Traefik at the edge**, not by gin: the preview
IngressRoute merged in talos-ops-prod #2301 errored out in the provider (cross-namespace
middleware reference under a controller that disallows it, plus v3 regex syntax under
`defaultRuleSyntax: v2`) and therefore matched nothing. Traefik's default 404 body is
byte-similar to gin's, which masked the layer for the entire investigation.

---

## Symptom

```
curl -s -o /dev/null -w '%{http_code}\n' https://42ae0489-…-preview.safespaces.dev/5173/
→ 404      (expected: 401 — preview middleware without credentials)
```

---

## What was ruled out (application layer: all healthy)

| # | Check | Result |
|---|---|---|
| 1 | Running pods have the env | `grep -c PREVIEW_ORIGIN_ENABLED` on pod yaml = **2** |
| 2 | Rollout state | generation 293 == observedGeneration 293, updated 2/2, ready 2/2 — complete, no stale generation |
| 3 | Live spec env literals | `ENABLED=true`, `BASEDOMAIN=safespaces.dev`, TOKENSECRET via secretKeyRef (key exists, 64 chars — value never inspected) |
| 4 | Image + health | `api:0.17.0`; `/health` = `{"status":"ok","version":"0.17.0"}` |
| 5 | Handler constructed in the running binary | `GET /api/v1/workspaces/<ws>/dev-preview-bootstrap/5173` → **401**. That route is registered only when `PreviewOriginHandler != nil` (router.go:514) — proves the middleware chain is live in the deployed process |
| 6 | Request reaches gin | No `/5173/` request ever appears in API logs across repeated external probes |

Direct in-pod isolation (`curl localhost:8080` with a preview Host header) was impossible —
API pods are distroless (no shell). Header fingerprinting (below) replaced it.

---

## The discriminator: whose 404?

| Fingerprint | Preview host 404 | gin NoRoute 404 (`api.safespaces.dev/nope`) |
|---|---|---|
| `content-length` | **19** | **18** |
| `x-request-id` / tracing | absent | present via expose-headers policy |
| CORS / CSP / permissions-policy | absent | full API security-header set |

Traefik's default 404 body (`404 page not found\n`) is 19 bytes; gin's is 18. The preview
404 carried none of the API's middleware-set headers — **the request died at the edge and
never reached gin.** Confirmed by Traefik's own access log:

```json
{"DownstreamStatus":404, "OriginStatus":0, "OriginContentSize":0,
 "DownstreamContentSize":19,
 "RequestHost":"42ae0489-…-preview.safespaces.dev", "RequestPath":"/5173/",
 "entryPointName":"websecure", "Overhead":108540}
```

`OriginStatus: 0` — zero origin contact.

---

## Root cause

talos-ops-prod **#2301** (merged 2026-08-20T06:28:05Z) added IngressRoute
`llmsafespaces/llmsafespaces-preview-origins` with **two independent fatal defects**:

1. **Cross-namespace middleware reference.** The route referenced
   `networking/middlewares-rate-limit` from namespace `llmsafespaces`, but the Traefik
   DaemonSet (v3.7.10) runs **without** `allowCrossNamespace`. Traefik logged the router
   error continuously (~1/s) since the merge:

   ```json
   {"level":"error","providerName":"kubernetescrd",
    "ingress":"llmsafespaces-preview-origins","namespace":"llmsafespaces",
    "error":"invalid reference to middleware middlewares-rate-limit: allowCrossNamespace is disabled, cross-namespace are disallowed",
    "message":"Failed to create middleware keys"}
   ```

   An IngressRoute with an invalid middleware reference does not fall back to
   routing-without-middleware — the whole router fails to build and matches nothing.

2. **v3 regex syntax under v2 default.** `HostRegexp(`^[a-z0-9-]+-preview\.safespaces\.dev$`)`
   uses the v3 anchor form, while the DaemonSet sets `--core.defaultRuleSyntax=v2`, whose
   canonical form is the brace template `{host:…}`. (Verified from the live DS args and
   helm-release values; the middleware error above is the only one Traefik surfaces, so
   this defect is config-verified, not independently observed in logs — it would have
   surfacing next had the middleware ref been valid.)

**Why only preview hosts:** DNS splits at the edge — `api.`/`chat.safespaces.dev` resolve
to Cloudflare (104.21.x/172.67.x) and reach the cluster through the long-standing ingress
objects, while the grey-cloud wildcard `*.safespaces.dev` → 76.135.100.247 delivers preview
hostnames directly to Traefik, straight into the broken router.

**Irony worth recording:** the epic-66 T5 hardening requirement ("preview hosts must not
receive the API CORS policy or rate budget") is what pushed #2301 toward a bespoke
middleware chain — and the bespoke chain is what broke routing. The security requirement
was correct; the implementation bypassed platform-standard validation (cross-namespace ref,
syntax version) that the stock chains had already survived.

---

## Fix (talos-ops-prod #2304 — in review)

- New **local** middleware `llmsafespaces/llmsafespaces-preview-rate-limit`
  (avg 100 / burst 200 — verified identical to the live
  `networking/middlewares-rate-limit` values). Real per-workspace budgeting remains
  redesign P1-5; this is the interim edge guard at the cluster-standard rate.
- Match rewritten to v2 brace template:
  `HostRegexp(`{host:[a-z0-9-]+-preview\.safespaces\.dev}`)`.
- No cross-namespace references remain; the failure mode is documented in the file's
  comments for the next editor.

T5 is still honored: neither the API CORS middleware nor the shared secure-headers chain
is attached; protective headers come from the API's own middleware once traffic reaches it.

---

## Verification (post-merge, not yet executed)

Flux: `cluster-llmsafespaces-app` has a 30m interval, but the `github-receiver` webhook
(flux-system, Ready) reconciles on merge — expect flip within ~2 min of merge.

```
curl -sI https://<ws>-preview.safespaces.dev/5173/
# expect: 401 + x-request-id (was: 404 edge, 19 bytes, no API headers)
curl -s https://api.safespaces.dev/health
# expect: {"status":"ok","version":"0.17.0"}  (no regression)
```

Then the real browser bootstrap flow end-to-end: bootstrap URL → 302 → `__Host-pv`
cookie → dashboard served from its own origin.

---

## Lessons

1. **Discriminate 404s by headers, not body text.** Traefik and gin default 404 bodies
   differ by one newline byte; both read as "404 page not found" to a human. The reliable
   signals are the presence of app-middleware headers (x-request-id, CORS, CSP) and
   content-length (19 vs 18).
2. **A runbook premise is an assumption.** "The request reaches the API" was stated as
   verified context and was false. The built-in probe (bootstrap route registration gated
   on handler-nil) is what flipped the investigation from app to edge.
3. **Security-driven custom wiring deserves the same validation as stock wiring.** The T5
   requirement justified a custom middleware chain, but the chain then skipped the
   platform-convention checks (same-namespace refs, syntax version pinning) that the
   standard chains had implicitly passed. Hardening must not become a bypass path.

---

## Assumptions (per Rule 7)

| Assumption | Validation |
|---|---|
| "The request reaches the API" (runbook premise) | **Disproved** — no `/5173/` in API logs; Traefik access log shows `OriginStatus: 0`; header fingerprint matches Traefik default, not gin |
| Running pods predate the env (leading hypothesis) | **Disproved** — env count 2/2 pods; rollout generation 293/293 complete |
| PreviewOriginHandler is constructed in the running binary | Confirmed — `/dev-preview-bootstrap/:port` returns 401; route registers only when the handler is non-nil (router.go:514-516) |
| Cross-ns middleware ref broke the router | Confirmed — Traefik error log verbatim (above), repeating ~1/s since #2301 merged |
| v3-regex-under-v2 is a second fatal defect | Config-verified (live DS `--core.defaultRuleSyntax=v2` vs `^…$` form); not independently observed in logs — Traefik surfaces only the middleware error |
| PR #2304's local middleware mirrors the cluster standard | Confirmed — live `networking/middlewares-rate-limit` = avg 100 / burst 200, identical values |
| Merge lands within ~2 min via webhook | Supported — `github-receiver` Ready in flux-system; 30m interval is the fallback, not the path |

---

## Tests Run (during diagnosis)

None on LLMSafeSpace code — no code changed. Evidence is live-cluster: pod env, deployment
generations, API route probe (bootstrap 401), API/edge log correlation, DNS resolution,
response-header fingerprinting, Traefik provider logs.

---

## Related

- talos-ops-prod **#2301** — introduced the broken IngressRoute (merged 2026-08-20T06:28:05Z)
- talos-ops-prod **#2304** — fix (local rate-limit middleware + v2 syntax), in review
- LLMSafeSpace epic-66 Phase 1 — API side shipped in v0.17.0 (bb4a129e35c8): middleware,
  bootstrap route, `__Host-pv` cookie pipeline; verified live and correct

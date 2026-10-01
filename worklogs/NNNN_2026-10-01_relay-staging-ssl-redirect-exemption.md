# Worklog: Relay staging un-break — /api/v1/internal/ exempted from the SSL→301 redirect (the 5-day permanent-fallback root cause)

**Date:** 2026-10-01
**Session:** Root-caused why relay-only key delivery never staged for ANY workspace (raw-key fallback permanent since the secure path went live ~5d ago); one-line middleware fix + regression pin.
**Status:** Complete

---

## Objective

Every Active workspace's `CredentialsStaged` condition sat at `False / StageFailed` with:

```
credential source unavailable: call api: Get
"https://llmsafespaces-api.llmsafespaces.svc:8080/api/v1/internal/workspaces/<id>/llm-providers?ownerUserID=<uid>":
http: server gave HTTP response to HTTPS client
```

The controller's `--api-service-url` has been `http://…` in every ReplicaSet since 09-18, so the https scheme in the error was unexplained. Find the real mechanism, fix it, and unblock the strict-mode wind-down clock.

---

## Work Completed

### Root cause (diagnosed on the live cluster, workspace d8bed486)

1. **The redirect is live and reproducible.** From inside the cluster, `GET http://llmsafespaces-api.llmsafespaces.svc:8080/api/v1/internal/workspaces/<id>/llm-providers` → `301` with `Location: https://llmsafespaces-api.llmsafespaces.svc:8080/…` — **same internal host, same plaintext port, scheme flipped**.
2. **Go reports the final URL on redirect.** The controller's `CachedLLMProviderSource` (staging_client.go) uses the http:// base URL verbatim — the https in the error is the *redirect target*, not the configured URL. The client follows the 301, attempts TLS against the plaintext port, and fails with exactly the observed error.
3. **The middleware exemption has the wrong prefix.** `SecurityMiddleware` (api/internal/middleware/security.go) enables secure.New's `SSLRedirect` (`RequireHTTPS: true` by default, empty `SSLHost`). The epic-35 exemption (commit `3523dc05`) skips `/internal/…` — which covers the automation webhook groups (router.go:1186+) — but the controller-facing endpoints were registered under **`/api/v1/internal/…`** by US-72.3 (`483a7346`): org-status (router.go:770) and llm-providers (router.go:778). Neither matched; every plain-HTTP ClusterIP call got the 301.
4. **Why it looked like a config problem and wasn't.** `SSLRedirect` with an empty `SSLHost` redirects to `https://<same-host>:<port>` — meaningless for in-cluster traffic, but indistinguishable in the error text from a misconfigured flag. ReplicaSet audit (11 RSs back to 09-18) proved the flag was always http://.
5. **Why the fallback became permanent.** `reconcileRelayStaging` treats source failure as a business failure: condition `StageFailed`, no error requeue (pre-flip contract — raw-key delivery must not be blocked by relay trouble). Verified this workspace has **zero** staging artifacts: no `workspace-relay-<id>` handoff Secret, no `llm-relay-env-<id>-*` envelopes, no `relay-staged-providers` annotation. Nothing ever staged; the wind-down counter can never reach zero; the 7-clean-days clock never starts. All workspaces are in this state.

### Fix

- `api/internal/middleware/security.go`: the pre-SSLRedirect skip now covers both prefixes — `/internal/` (unchanged, automation webhooks) and `/api/v1/internal/` (controller endpoints). One condition, no behavior change for anything public.
- `api/internal/middleware/tests/security_test.go`: regression test `TestSecurityMiddleware_InternalAPIPathsSkipSSLRedirect` pins that plain-HTTP (no `X-Forwarded-Proto` — exactly how the controller calls it in-cluster) reaches the handler for both internal routes, with the incident write-up in the doc comment.

### Verification

- `go test ./api/internal/middleware/...` — green (both packages).
- Live-cluster reproduction of the 301 (python3 http.client from the workspace pod, no redirects followed) before the fix; the same call against a patched API will be asserted post-deploy by watching `CredentialsStaged` flip to `True`.

---

## Key Decisions

1. **Prefix exemption, not a scheme-aware redirect.** The alternative — making `SSLRedirect` a no-op when `X-Forwarded-Proto` is absent — would disable the redirect for every direct-HTTP caller, including genuinely public ones. These routes are cluster-internal BY DESIGN (authenticated by `X-Internal-Token`, never exposed through the ingress); exempting their prefix is the narrow fix and matches the epic-35 precedent.
2. **Both internal route families in one exemption.** org-status (`/api/v1/internal/orgs/:orgID/status`) has the identical latent bug — the controller's org-status client would fail the same way the moment it made a plain call. Fixing only llm-providers would have left the next outage one call away.
3. **No controller-side redirect handling added.** `CachedLLMProviderSource` deliberately has no redirect tolerance (credentials must be fresh, failures loud). Teaching it to cope would have masked the server bug; the server was wrong, not the client.

### Assumptions (Rule 7)

- No internal caller legitimately relies on the 301 for these routes (they call http:// directly and never follow redirects — Go's default client follows up to 10, which is what produced the TLS-on-plaintext error rather than a clean one).
- ~~The ingress does not route `/api/v1/internal/` publicly.~~ **CORRECTED (review r2):** `helm/values.yaml` defaults `api.ingress.enabled=false`, but when an operator enables it the path list is `path: /, pathType: Prefix` — `/api/v1/internal/*` WOULD be publicly routed. The exposure delta of this fix is bounded: over HTTPS through a TLS-terminating ingress, `X-Forwarded-Proto: https` already suppressed the 301 pre-fix, so token-gated HTTPS exposure is unchanged; the delta is plaintext-via-ingress (now served instead of 301ing). Mitigated by ingress-default-off + the handlers' fail-closed `X-Internal-Token` auth (constant-time compare), and identical in kind to the pre-existing `/internal/` carve-out. Follow-up filed below: an ingress path carve-out for `/api/v1/internal/`.

---

## Blockers

None.

---

## Tests Run

- `go test ./api/internal/middleware/...` — PASS (both packages).
- `go test ./api/internal/server/ -run TestRouter_InternalAPIRoutesNotSSLRedirected` — PASS (gofmt-clean after r2's alignment fix).
- Mutation verification (r2 finding): with the `/api/v1/internal/` exemption condition removed from security.go, the seam test FAILS (`expected 403, got 301`) and the middleware subtests FAIL (`301 vs 200`); restored, all pass. The seam test is based on `DefaultRouterConfig()` — the production `RequireHTTPS: true` posture — after r2 identified that a zero-value `RouterConfig` leaves a zero `SecurityConfig` and disarms `SSLRedirect` entirely (the test was tautological as first written).
- Live-cluster reproduction of the 301 (python3 http.client from the workspace pod, redirects not followed) pre-fix; post-deploy assertion: `CredentialsStaged` flips to `True`.

---

## Next Steps

1. Merge + release v0.34.13 (CHANGELOG entry + `helm/Chart.yaml` bump, `make release-tag VERSION=0.34.13`).
2. Post-deploy: watch `CredentialsStaged` flip to `True` across Active workspaces and `llm-relay-env-*` / `workspace-relay-*` Secrets appear; then the fallback-delivery counter should begin its decline and the 7-clean-days clock for the strict-mode flip finally starts.
3. File the follow-up issue: helm ingress path carve-out so `/api/v1/internal/*` is never publicly routed when `api.ingress.enabled=true`.
4. Candidate for the .13 queue: alert on `CredentialsStaged=False` age — the StageFailed-is-not-an-error design let this outage persist for 5 days with only a condition as evidence.

---

## Files Modified

- `api/internal/middleware/security.go` — the exemption: `/api/v1/internal/` added beside the epic-35 `/internal/` skip.
- `api/internal/middleware/tests/security_test.go` — `TestSecurityMiddleware_InternalAPIPathsSkipSSLRedirect`, `TestSecurityMiddleware_InternalPrefixExemptionIsNarrow`, `TestSecurityMiddleware_LegacyInternalPrefixStillExempt`.
- `api/internal/server/router_internal_ssl_test.go` — `TestRouter_InternalAPIRoutesNotSSLRedirected` (real NewRouter wiring; mutation-verified).
- `COORDINATE.md` — claim row.
- This worklog.

### Review rounds (PR #1611, automated reviewer)

- **r1 (REQUEST CHANGES):** worklog number picked manually (collided with main's 1075) → renamed to the `NNNN_` sentinel; narrowness unpinned → negative subtests added (no-trailing-slash, dash-lookalike, substring trap all still 301); old `/internal/` condition unpinned → `LegacyInternalPrefixStillExempt`; no router↔middleware composition test → `TestRouter_InternalAPIRoutesNotSSLRedirected` added.
- **r2 (REQUEST CHANGES):** the seam test as first written was tautological (zero-value `RouterConfig` → zero `SecurityConfig` → `SSLRedirect` disarmed) → rebased on `DefaultRouterConfig()`, mutation-verified failing without the fix, plus a non-internal-route still-301s posture pin; gofmt failure on the test file (struct-literal alignment) → fixed; worklog structure sections missing + disproven ingress assumption → corrected here.

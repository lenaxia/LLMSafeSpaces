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
- The ingress does not route `/api/v1/internal/` publicly (these endpoints carry the internal token auth; the router registers them on the same gin engine but the ingress pathing was not changed here — pre-existing posture, unchanged by this fix).

---

## Follow-ups

- Post-deploy: watch `CredentialsStaged` flip to `True` on Active workspaces and envelope/handoff Secrets appear; then the fallback-delivery counter should begin its decline and the 7-clean-days clock for the strict-mode flip finally starts.
- The `StageFailed`-is-not-an-error design means a silently-broken source persists indefinitely with only a condition as evidence — worth a controller-side alert on `CredentialsStaged=False` age (candidate for the .13 queue).

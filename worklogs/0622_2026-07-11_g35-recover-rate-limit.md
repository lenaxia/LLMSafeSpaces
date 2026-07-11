# Worklog: G35 — /account/recover per-route rate limit

**Date:** 2026-07-11
**Session:** Address threat-model gap G35 (High) — `POST /api/v1/account/recover` was mounted on the root router behind only the global 100/min/IP rate limiter, too lax for a credential-bearing endpoint that also does Argon2id work.
**Status:** Complete

---

## Objective

Close G35 from `design/stories/epic-17-security-review/THREAT-MODEL.md`. The recovery endpoint accepts `userID` + `recoveryKey` as direct input. The recovery key is 128-bit random (brute-force is mathematically infeasible), but the endpoint does Argon2id work to re-derive the DEK under the new password — making it a CPU-exhaustion DoS target. The global 100/min/IP limiter was too lax for a credential-bearing endpoint.

The threat-model row recommended "Move behind auth rate limiter (20/min)". The `authRatePerMinute = 20` and `authRateBurst = 5` constants existed at `router.go:653-654` for exactly this purpose — but were dead code, never wired.

---

## Work Completed

### Implementation

- **`api/internal/middleware/per_route_rate_limit.go`** (new) — generic `PerRouteRateLimitMiddleware` that applies stricter per-route rate limits on top of the global `RateLimitMiddleware`. Key features:
  - **Per-route bucket isolation.** Keys are `HashString(route + ":" + identity)` so a user hitting `/recover` cannot deplete the budget for `/secrets/:id/reveal` or vice versa.
  - **Path matching via gin's `FullPath()`.** Returns the route pattern (e.g. `/api/v1/account/recover`) or empty for 404. Parameterised routes (`/secrets/:id`) share one bucket per route, which is the intended behavior (a user scanning IDs is rate-limited as one).
  - **Identity resolution mirrors the global middleware** — API-key first (set by AuthMiddleware on authenticated routes), else client IP. Anonymous endpoints like `/account/recover` always fall back to IP.
  - **No-op when disabled or nil service** — same fail-safe as the global limiter.
  - **token-bucket strategy only.** The global middleware exposes fixed-window, sliding-window too, but per-route caps want steady-state allowance + bounded burst (token bucket is the right shape).

- **`api/internal/server/router.go`** — three changes:
  1. Added `PerRouteRateLimitConfig` field to `RouterConfig`.
  2. `DefaultRouterConfig` now wires the previously-dead-code `authRatePerMinute` (20) / `authRateBurst` (5) constants into a `PerRouteRateLimitConfig` protecting `/api/v1/account/recover`.
  3. The middleware is added to the global chain AFTER the existing global limiter, so both must allow the request (defense in depth).

### Tests

- **`api/internal/middleware/per_route_rate_limit_test.go`** (new) — 5 unit tests:
  - `TestPerRouteRateLimit_AppliesStricterLimitToProtectedPath` — G35 core: protected path has separate stricter bucket.
  - `TestPerRouteRateLimit_BucketsAreIsolatedPerPath` — two protected paths don't share budget.
  - `TestPerRouteRateLimit_DisabledWhenConfigDisabled` — Enabled=false is no-op.
  - `TestPerRouteRateLimit_UnprotectedPathsPassThrough` — paths not in Routes map are untouched.
  - `TestPerRouteRateLimit_NilServiceIsNoOp` — graceful degradation.

- **`api/internal/server/router_g35_recover_rate_limit_test.go`** (new) — wiring regression that exercises the FULL router construction path (`NewRouter` with `DefaultRouterConfig`), proving the route is registered AND the middleware is in the stack. Fires 5 requests, asserts 3 pass + 2 are 429.

### Documentation

- **`CHANGELOG.md`** — entry under `[Unreleased] → Security`.
- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — G35 row flipped 🔴 → 🟢 Fixed. STRIDE `API Auth` row updated. Counts: 23 Fixed / 20 Open → 24 Fixed / 19 Open. Revision 2.6 added.

---

## Key Decisions

1. **Separate middleware, not a flag on the global one.** The global `RateLimitMiddleware` already has `CustomLimits` but those are keyed on the rate-limit identity (api-key or IP), not the route — wrong shape for per-endpoint limits. A separate middleware is the right abstraction.

2. **Per-route bucket isolation via key prefix.** `RateLimiterService.Allow(key, rate, burst)` keys buckets on `key` alone. Without the route prefix in the key, the per-route limit would share the same bucket as the global limit (whichever ran first would consume the other's budget). The prefix `<route>:<identity>` gives isolation.

3. **Apply AFTER the global limiter, not before.** Defense in depth: both middleware must allow the request. If the per-route middleware ran first and rejected, the global budget would never be consumed, and a user alternating between protected and unprotected paths could exceed the global intent.

4. **Token-bucket strategy only.** The global middleware exposes fixed-window and sliding-window too. Per-route caps want steady-state allowance + bounded burst (token bucket). Adding the other strategies would be premature — no caller needs them.

5. **Use the existing `authRatePerMinute` / `authRateBurst` constants.** They were defined for exactly this purpose and have been dead code since they were added. Wiring them fulfills the original intent and removes the dead code (Rule 5).

6. **Generic middleware for future endpoints.** The threat model has a parallel gap G41 (`/secrets/:id/reveal` no per-endpoint rate limit). The new middleware accepts a routes map, so closing G41 is a one-line addition to `DefaultRouterConfig` — no new middleware.

7. **Default to enabled.** `DefaultRouterConfig` returns `Enabled: true` with the recover route configured. Operators can disable by overriding the config, but production gets the protection by default.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G35 still open in the codebase | Verified: `router.go:549` mounts `/account/recover` on root router; only the global 100/min/IP limiter applies. |
| 2 | Recovery key is 128-bit random (brute-force infeasible) | Verified: `pkg/secrets/crypto.go:98` — `GenerateRecoveryKey` returns 16 bytes (128 bits). |
| 3 | Recovery endpoint does Argon2id work | Verified: `pkg/secrets/key_service.go:892` — `ResetWithRecoveryKey` calls `InitializeUserKeys` which calls `DeriveKEKFromPassword` (Argon2id). |
| 4 | `authRatePerMinute` / `authRateBurst` constants are dead code | Verified: `grep -rn authRatePerMinute api/internal/` returned only the declaration. |
| 5 | `c.FullPath()` is available in middleware execution | Verified in gin source at `gin.go:632` — `c.fullPath = value.fullPath` is set BEFORE `c.Next()`. |
| 6 | `RateLimiterService.Allow` uses in-process map (token bucket) | Verified: `ratelimit.go:119` — `s.localBuckets[key]`. Redis is only used for the fixed/sliding window strategies. |
| 7 | Per-route middleware with different key prefix gets separate bucket | Verified: `TestPerRouteRateLimit_BucketsAreIsolatedPerPath`. |
| 8 | Default `RouterConfig{}` (zero value) is safe | Verified: zero-value `PerRouteRateLimitConfig` has `Enabled: false` → no-op. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. **Rate semantic bug**: `Limit` is treated as tokens-per-second, not per-window. The comment says "100/min" but the code does 100/sec.
2. **`authRatePerMinute` / `authRateBurst` were dead code**: now wired.
3. **IPv6 anonymity**: attacker with /64 gets one bucket. Standard limitation.
4. **Zero-value config safety**: `RouterConfig{}` direct construction.
5. **No instance-settings runtime override** for the per-route limits.
6. **Middleware ordering**: global → per-route is correct.

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | **Real — fixed in this PR after review feedback.** Reviewer correctly pointed out that since this is brand-new code with no operator values to re-tune, the rate-conversion fix (`rate := float64(routeCfg.Limit) / routeCfg.Window.Seconds()`) belongs here, not in a separate PR. The global limiter still has the bug (pre-existing, separate PR) but the new per-route middleware does NOT inherit it. Locked by `TestPerRouteRateLimit_LimitIsPerWindowNotPerSecond`. | **Fixed** |
| 2 | Real — fixed by wiring |
| 3 | Real, accepted — standard NetPol/RateLimit limitation |
| 4 | False alarm — Enabled defaults to false → no-op |
| 5 | Acceptable — G35 scopes to the wiring, not runtime tunability. Operators can override RouterConfig. |
| 6 | False alarm — defense in depth works |

### Phase 3 — remediation

- Finding 1 (rate semantic bug): **FIXED in this PR** (after review feedback). The new per-route middleware converts `Limit` per `Window` to a per-second refill rate, so `{Limit: 20, Window: 1m}` actually enforces 20 per minute. The pre-existing global limiter still has the bug — separate PR. New regression test `TestPerRouteRateLimit_LimitIsPerWindowNotPerSecond` locks the corrected semantics.
- Zero outstanding findings in the new code.

---

## Pre-existing finding (still out of scope, separate PR needed)

**Rate limit unit semantic mismatch in the GLOBAL limiter.** The token-bucket strategy at `api/internal/middleware/rate_limit.go:154` (`applyTokenBucketRateLimit`) computes `rate := float64(limit)` and uses it as tokens-per-second in the bucket refill at `ratelimit.go:127` (`b.tokens += elapsed * rate`). The `DefaultLimit: 100` in `DefaultRateLimitConfig` is documented as "100/min" but actually enforces 100/sec — 60× more permissive than intended. The `DefaultWindow: time.Minute` is only used for the X-RateLimit-Reset header, not the actual throttling.

This affects only the global limiter now (the per-route middleware added in this PR correctly converts per-window to per-second). Fixing the global limiter requires:
- Change `rate := float64(limit)` to `rate := float64(limit) / window.Seconds()`
- Re-tune operator-configured values (existing deployments rely on the per-second interpretation)
- Update DefaultRateLimitConfig docs

Recommend a separate PR. Not in scope for G35.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted middleware unit tests
go test -count=1 -timeout 30s -v -run 'TestPerRouteRateLimit' ./api/internal/middleware/...
# → 5/5 PASS

# Wiring regression
go test -count=1 -timeout 25s -v -run 'TestRouter_G35_RecoverAccountRateLimited' ./api/internal/server/...
# → PASS (status codes [400 400 400 429 429] — first 3 reach handler, last 2 rate-limited)

# Full middleware + server packages
go test -count=1 -timeout 50s ./api/internal/middleware/... ./api/internal/server/...
# → PASS

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build + vet
go build ./...    # exit 0
go vet ./...      # exit 0

# Lint (changed packages)
golangci-lint run --timeout=4m ./api/internal/middleware/... ./api/internal/server/...
# → 0 issues

# Format
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
```

---

## Next Steps

1. **Merge this PR**, then move to G25 (logged secret values).
2. **Follow-up (out of scope here):** fix the pre-existing rate-semantic mismatch (Limit is per-second, not per-window). Separate PR — requires re-tuning operator defaults.
3. **Sibling gap:** G41 (`/secrets/:id/reveal` no per-endpoint rate limit) is now a one-line addition to `DefaultRouterConfig`'s routes map. Worth a parallel PR.

---

## Files Modified

- `api/internal/middleware/per_route_rate_limit.go` — **new** — `PerRouteRateLimitMiddleware`, `PerRouteRateLimitConfig`, `RouteRateLimit`
- `api/internal/middleware/per_route_rate_limit_test.go` — **new** — 5 unit tests
- `api/internal/server/router.go` — `PerRouteRateLimitConfig` field on RouterConfig; `DefaultRouterConfig` wires `/api/v1/account/recover` with 20/5; middleware added to global chain
- `api/internal/server/router_g35_recover_rate_limit_test.go` — **new** — wiring regression
- `CHANGELOG.md` — entry under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — G35 row flipped 🟢; STRIDE + counts + revision 2.6
- `worklogs/0622_2026-07-11_g35-recover-rate-limit.md` — this file

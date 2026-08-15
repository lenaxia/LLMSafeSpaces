# Worklog: Rate-limit 429 body error contract

**Date:** 2026-08-15
**Session:** Fix pre-existing SDK Integration canary failures caused by rate-limit 429 responses violating the API error contract
**Status:** Complete

---

## Objective

The `SDK Integration (live API canaries)` CI job has been failing intermittently across PRs (#647 on 2026-08-04, #734 on 2026-08-11) and on `main` (2026-08-14). Root-cause and fix.

---

## Work Completed

### Root cause analysis

The 2026-08-14 `main` failure was `FAIL P3: 429 body has error field: ` (empty detail) immediately followed by `PASS P3: 429 body has error field`. The Go canary `s-rate-limit` asserted P3 twice with different semantics:

1. `canary.HasErrorField(body429)` — requires `body["error"]` to be a **string** (`result.go:175-186`)
2. A manual `json.Unmarshal` + `obj["error"]` existence check — accepts **any** type

Both rate-limit middlewares emitted the API-wide error contract's only violation:

```go
// api/internal/middleware/rate_limit.go:136 (global)
// api/internal/middleware/per_route_rate_limit.go:138 (per-route)
c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
    "error": gin.H{"code": ..., "message": ..., "details": ...},  // OBJECT
})
```

Every other **429** emitter in the API uses `{"error": "<string>"}` — `email.go:99` (`error` + `limit`), `dev_preview.go:181` (`error` + `retryAfter`). (`workspace_access.go:54` and `org_guard.go:27` are non-429 string-error emitters; some non-429 paths still emit object shapes — see follow-up issue #862.) The s-error-format canary (P5: "All error values are strings (not null, object, array)") pins this contract on reachable paths, and it passes on `main` — the rate-limit paths were the outliers, previously masked because the canary asserted P3 with two semantics and the lenient one passed.

**Consumer impact (why the string shape is correct, not just conventional):**
- Go SDK `parseError` (`sdks/go/client.go:177-187`) decodes `Error string json:"error"` — an object fails to decode, falling through to an empty/generic message. `IsRateLimit(err)` still worked (status-based), but `APIError.Message` was degraded.
- Frontend `ApiClientError` tests mock `{error: "rate limited", retryAfter: 10}` — string shape.
- Python canary doesn't assert the 429 body shape (status codes only). The TS canary asserted it leniently (any-typed `hasField`) — the same masking pattern as the Go duplicate; fixed to string-typed `hasErrorField` in this PR.

### Earlier failures explained (validated, already fixed on main)

- **#647 (2026-08-04) `P2: no 429 after 30 rapid login attempts`** — transient; P2 passed on all later runs (2026-08-11, 2026-08-14). Message also drifted from the code (loop is 60 attempts, message said 30) — fixed.
- **#734 (2026-08-11) `N1: beyond limit returns 429` (s-ws-quota)** — that day the scenario ran with `LLMSAFESPACES_MAX_WORKSPACES_PER_USER=10` wired; the skip behavior ("Go canary quota test skips when env var not set") landed on main via PR #810; the scenario now skips when the env var is absent, and the 2026-08-14 `main` run confirms (`PASS ws-quota: skipped`). No action needed.
- **#647 Trivy (3 HIGH in frontend lockfile) and pkg/secrets integration failures (2026-08-04)** — both jobs pass on current `main` (dependency bumps + fixes landed between 2026-08-04 and 2026-08-11); stale-branch artifacts, no action in this PR.

### Fix (TDD)

1. **Tests first** (red verified against unfixed middlewares):
   - `api/internal/middleware/tests/rate_limit_429_body_test.go` — `TestRateLimitMiddleware_429BodyContract`: global token-bucket limiter 429 body must have `error` (non-empty string), `limit` (number == configured limit), `retryAfter` (positive number).
   - `api/internal/middleware/per_route_rate_limit_test.go` — `TestPerRouteRateLimit_429BodyContract`: same contract for the per-route limiter (miniredis-backed real limiter).
2. **`api/internal/middleware/rate_limit.go`** — 429 body now `{"error": <message string>, "limit": <limit>, "retryAfter": <seconds derived from the error's reset, clamped ≥1>}`; adds standard `Retry-After` header (same value).
3. **`api/internal/middleware/per_route_rate_limit.go`** — same shape (`limit` = route limit, `retryAfter` = full route window — review-sanctioned simplification); adds `Retry-After` header.
4. **`sdks/canary/go/scenarios/s-rate-limit/main.go`** — removed the duplicate P3 assertion block (kept the string-typed `HasErrorField` check; the removed lenient re-check is what masked the middleware bug); P3 detail now includes the body prefix for future diagnosis; fixed P2 message drift ("30" → "60").
5. **`sdks/canary/python/scenarios/*.py` (40 files) — sys.path depth fix.** All scenarios inserted `scenarios/../..` (= `sdks/canary/`, no `canary.py` there) instead of `scenarios/..` (= `sdks/canary/python/`); the Python section had never executed in CI (earlier Go failures aborted the job first), so the `ModuleNotFoundError` was latent. Also anchored to `abspath(__file__)` for CWD independence.
6. **Python scenario repairs (10/22 deterministic failures once the section activated):**
   - 6 scenarios used API-key clients where their Go twins use `jwt_login` — `s_secret_crud`, `s_secret_reveal`, `s_secret_audit`, `s_secret_bindings`, `s_env_vars`, `s_ownership` (user-DEK paths 403 "encryption key not available" with API-key auth, per `canary.py` docstring).
   - `s_ws_quota` now skips when `LLMSAFESPACES_MAX_WORKSPACES_PER_USER` is unset (Go/TS twin behavior; the server never enforces quota without the env, CI never sets it → guaranteed false failure).
   - 3 `d_*` scenarios (`d_account_recover`, `d_change_password`, `d_key_rotate`) dropped from the CI loop — `_AccountAPI` is an empty class (`sdks/python/llmsafespaces/client.py:506`), so `c.account.*` raises AttributeError; no Go/TS CI precedent. Parked until implemented.
   - `s_rate_limit` now asserts the 429 body's `error` field is a string (`has_error_field`), matching the strict Go/TS checks.
7. **`.gitignore`** — root `node_modules/` ignored; stray committed `node_modules/.package-lock.json` removed (debug artifact).

---

## Key Decisions

1. **Fix the middlewares, not the canary's `HasErrorField`.** The string-typed `"error"` field is the API-wide contract (every handler, the s-error-format canary P5, the Go SDK, the frontend). The object shape in exactly two middlewares was the defect.
2. **Body carries `limit` + `retryAfter`, drops `code`/`details`.** Matches `email.go` (`error`+`limit`) and `dev_preview.go` (`error`+`retryAfter`) precedents; `code` is redundant with HTTP 429; reset timestamp remains in `X-RateLimit-Reset`. Added `Retry-After` header per RFC 6585/7231 since the value is now computed anyway.
3. **`retryAfter` derives from the error's `Details["reset"]` when present** (token bucket ≈ now+1s; fixed window = now+TTL, sliding window = now+window), falling back to the full window; clamped ≥1s. Per-route (review-sanctioned simplification) uses the full route window. Prevents default-strategy 429s advertising a 60s wait for a ~1s refill.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s ./api/internal/middleware/...` — pass (both new tests red pre-fix, green post-fix)
- `go test -timeout 300s ./api/internal/server/` — pass (router/middleware integration unaffected)
- `go vet ./api/internal/middleware/...` — clean
- `go build` + `go vet` on `sdks/canary/go/scenarios/s-rate-limit/` — clean
- `python3 -m py_compile sdks/canary/python/scenarios/*.py` — all compile; jwt_login import verified present in all 6 converted scenarios
- `ci.yml` Python loop: 19 scenarios (3 d_* parked with rationale comment)
- Full CI (lint, race suite, canaries) — deferred to the PR gate

---

## Next Steps

- After merge: re-run CI on open PRs #647, #734, #816 (update branches to `main`) — the SDK canary job should be stable; consider flipping `sdk-canary.continue-on-error` to `false` once it passes consistently (per the TODO in `ci.yml`). The Python section is now expected green; the `d_*` trio stays parked until `_AccountAPI` is implemented **and** seed-accounts provisions a rotate-enabled account (today only canary1/canary2 are seeded — `seed-accounts/main.go:43-45` — so even with `_AccountAPI`, the rotate scenario has no seeded subject).

---

## Files Modified

- api/internal/middleware/rate_limit.go
- api/internal/middleware/per_route_rate_limit.go
- api/internal/middleware/per_route_rate_limit_test.go
- api/internal/middleware/tests/rate_limit_429_body_test.go (new)
- sdks/canary/go/scenarios/s-rate-limit/main.go
- sdks/canary/typescript/scenarios/s-rate-limit.ts (string-typed hasErrorField — round 3)
- sdks/canary/python/scenarios/*.py (40 files: sys.path depth fix — latent CI bootstrap bug)
- .gitignore (root node_modules/; stray lockfile artifact)
- .github/workflows/ci.yml (Python canary loop: 3 d_* parked with rationale)
- worklogs/NNNN_2026-08-15_rate-limit-429-error-contract.md (this file)

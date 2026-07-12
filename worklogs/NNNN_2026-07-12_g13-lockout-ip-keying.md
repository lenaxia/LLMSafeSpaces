# Worklog: G13 — Account lockout IP+email keying

**Date:** 2026-07-12
**Session:** Close G13 (Medium) — the last code-fixable gap in the threat model.
**Status:** Complete

---

## Objective

Close G13. The lockout counter was keyed on email only — `lockout:<email>`. An attacker who knew a victim's email could submit bad passwords from any IP and trigger the lockout (DoS amplification). Fix: include the client IP in the lockout key.

---

## Implementation

- **`api/internal/services/auth/auth.go`**:
  - New `WithClientIP(ctx, ip) context.Context` helper (exported for router use).
  - New `clientIPFromContext(ctx) string` reader.
  - New `lockoutKey(email, clientIP string) string` helper: returns `lockout:<email>` when IP is empty (backward compat), `lockout:<email>:<ip>` when IP is set.
  - All three lockout sites (Login lockout check, `recordFailedAttempt`, `clearFailedAttempts`) now call `clientIPFromContext` + `lockoutKey`.

- **`api/internal/server/router.go`**: login route now wraps the context with `auth.WithClientIP(c.Request.Context(), c.ClientIP())` before calling `Login`.

### Tests

3 new tests in `api/internal/services/auth/auth_test.go`:
- `TestLogin_G13_AttackerFromDifferentIPCannotLockVictim` — G13 core: attacker from IP A cannot lock victim on IP B.
- `TestLogin_G13_SameIPLockoutStillWorks` — same-IP lockout still triggers (original behavior preserved).
- `TestLogin_G13_NoIPContextFallsBackToEmailOnly` — backward compat for callers without `WithClientIP`.

---

## Key decisions

1. **Context propagation, not interface change.** Changing `Login(ctx, req)` to `Login(ctx, req, ip)` would touch 20+ callers and mocks. The context approach mirrors `agentpush.WithAuth` and requires only one new line in the router.

2. **Email+IP key, not IP-only.** Keying on IP alone would let a distributed attacker bypass the lockout entirely. Email+IP means the attacker needs `lockoutAttempts` failures from the SAME IP to trigger — and the global rate limiter caps per-IP requests.

3. **Backward compat fallback.** When `clientIP` is empty (no `WithClientIP` call), the key falls back to email-only. Internal callers and tests that haven't been updated still work correctly.

---

## Assumptions

| # | Assumption | Validation |
|---|---|---|
| 1 | G13 still open | Verified: `auth.go:1000` had `lockout:%s` with email only. |
| 2 | Client IP is available at the login call site | Verified: `router.go:806` has `c.ClientIP()`. |
| 3 | `statefulMockCache` (from G38 PR) persists lockout counters | Verified by `TestLogin_G13_SameIPLockoutStillWorks` (lockout triggers). |

---

## Tests Run

- Targeted G13 tests: 3/3 PASS
- Full auth suite: PASS (20.3s)
- Lint: 0 issues

---

## Files Modified

- `api/internal/services/auth/auth.go` — `WithClientIP`, `clientIPFromContext`, `lockoutKey`, three lockout sites updated
- `api/internal/server/router.go` — login route sets `WithClientIP` before calling `Login`
- `api/internal/services/auth/auth_test.go` — 3 new G13 tests
- `CHANGELOG.md`, `THREAT-MODEL.md` — G13 flipped 🟢; counts 37/2/11; revision 2.11

# Worklog: G38 — ChangePassword revokes all sessions

**Date:** 2026-07-11
**Session:** Address threat-model gap G38 (High) — `POST /api/v1/account/change-password` did not invalidate outstanding JWTs, leaving stolen pre-change tokens valid until natural expiry.
**Status:** Complete

---

## Objective

Close G38 from `design/stories/epic-17-security-review/THREAT-MODEL.md`. The existing `RotateKeyHandler.ChangePassword` (`api/internal/handlers/secrets.go:597`) re-wrapped the DEK with the new password (via `KeyService.ChangePassword`, which evicts the caller's cached DEK and durable `jwt_sessions` row) and updated the bcrypt hash, but never revoked the JWT signatures themselves. A stolen token that predated the change kept working until natural expiry — on re-login under the new password, the cached DEK was repopulated, after which the stolen token could keep reading secrets.

The password-reset flow already solves this: `password_reset.go:309-315` calls `auth.Service.RevokeAllUserSessions` (OWASP ASVS V2.5.2). G38 was the same invariant missing from the user-initiated change-password path.

---

## Work Completed

### Implementation (TDD red → green)

- **`api/internal/handlers/secrets.go`** — added `SessionRevoker` interface (one method, `RevokeAllUserSessions`); added `revoker` + `logger` fields to `RotateKeyHandler`; added `SetSessionRevoker` and `SetLogger` setters; invoked the revoker from `ChangePassword` after both `KeyService.ChangePassword` and `UpdatePasswordHash` commit, with non-fatal error handling that mirrors `password_reset.go:309-315`.

- **`api/internal/app/app.go`** — wired `rotateKeyHandler.SetLogger(log)` and `rotateKeyHandler.SetSessionRevoker(authSvc)` next to the existing `SetPasswordUpdater` call. Type-assertion guard mirrors the parallel pattern at `app.go:536` (`secretsHandler.SetPasswordVerifier`).

### Tests

- **`api/internal/handlers/change_password_test.go`** (new) — 5 unit tests covering:
  - `TestChangePassword_RevokesAllSessionsOnSuccess` — happy path; revoker called exactly once with caller's userID.
  - `TestChangePassword_RevokerErrorIsNonFatal` — revoker error does not flip a successful change to 5xx.
  - `TestChangePassword_WrongPasswordDoesNotRevoke` — wrong old password does NOT revoke (avoids griefing).
  - `TestChangePassword_NoRevokerWired_StillSucceeds` — optional-setter path (pre-G38 behavior preserved for non-production wiring).
  - `TestChangePassword_Unauthenticated_Returns401` — missing auth context short-circuits before touching the key service or revoker.

- **`api/internal/services/auth/stateful_mock_cache_test.go`** (new) — `statefulMockCache` CacheService implementation backed by a thread-safe in-memory map. The pre-existing `mockCache` is uniformly no-op, which made it impossible to assert post-change-password JWT rejection end-to-end (revocation markers never persisted). The new cache honors Set/Get/SetObject/GetObject but not TTL — sufficient for in-process e2e tests that complete in seconds.

- **`api/internal/services/auth/auth_e2e_all_test.go`** — switched `setupRealAuthRouter` from `mockCache{}` to `newStatefulMockCache()`; wired `rotateHandler.SetSessionRevoker(authSvc)`; extended `TestE2E_RealAuth_ChangePassword` to assert (1) the pre-change JWT is rejected with 401 on `GET /secrets` immediately after the change, and (2) a freshly-issued post-change JWT still works on the same endpoint. The WrongOld test stays unchanged.

### Documentation

- **`CHANGELOG.md`** — added an entry under `[Unreleased] → Security` describing the fix and the OWASP rationale.
- **`design/stories/epic-17-security-review/THREAT-MODEL.md`** — flipped G38 row from 🔴 Open to 🟢 Fixed with the file:line evidence and regression-test list; updated the STRIDE `API Auth` row to mark G38 closed; updated the implementation-status counts (21/22/7 → 22/21/7); added revision 2.4 to the revision history.

---

## Key Decisions

1. **Revoke ALL sessions, including the caller's.** Matches password-reset behavior and the strict reading of OWASP ASVS V2.5.2. UX cost: one re-login. Security benefit: explicit re-authentication with the new password proves the user knows it, and the caller's own JWT is treated identically to any other outstanding token — no special-casing.

2. **Best-effort revocation, non-fatal on error.** The password has already been changed cryptographically (DEK re-wrap and bcrypt hash both committed). A revocation failure cannot roll that back. Mirrors `password_reset.go:309-315`. Logged at Warn so operators see the degradation.

3. **Order: revoke after both the DEK re-wrap AND bcrypt update commit.** A stolen JWT cannot race the response — by the time revocation runs, the password is fully changed. If we revoked earlier and the bcrypt update failed, the user would be locked out of every session despite the password not actually changing.

4. **Optional setter pattern, not a constructor parameter.** Mirrors the existing `SetPasswordUpdater` and `SetAuditFunc` setters on the same handler. Non-production wiring (test harnesses, alternative auth backends where `svc.Auth` is not `*auth.Service`) silently degrades to pre-G38 behavior. This is intentional: the type-assertion guard at `app.go:536` already follows this convention.

5. **`SessionRevoker` is a separate interface from `KeyRotator`.** Revocation is a side effect of changing the password, not a key operation. Keeping it on the key-rotator interface would force every test double that implements `KeyRotator` to also implement revocation. Caller-side interface segregation.

6. **`RotateKey` is intentionally NOT changed.** `RotateKey` rotates the KEK that wraps the same DEK; the password is unchanged and existing sessions still derive the same DEK. The threat G38 addresses (JWT theft post-password-change-to-defend) does not apply to KEK rotation.

7. **`RecoverAccount` is intentionally NOT changed in this PR.** It uses `ResetWithRecoveryKey` (which re-initializes the DEK) and does not currently revoke sessions either. Same threat-model gap as G38, but out of scope — the threat model scopes G38 to `ChangePassword` specifically. Flagged here for a follow-up.

---

## Assumptions (Rule 7) — stated and validated

| # | Assumption | Validation |
|---|---|---|
| 1 | G38 still open in the codebase (not just the threat model) | Verified: `secrets.go:597-632` (current handler) has no revocation call; `key_service.go:805-884` evicts the caller's session DEK but does not touch other JWTs' validity. |
| 2 | `auth.Service.RevokeAllUserSessions` is the correct primitive | Verified: `auth.go:1163-1196` writes both per-jti and per-hash Redis revocation markers and clears durable `jwt_sessions` rows. Already wired into `password_reset.go:312` for the same purpose. |
| 3 | Revoke ALL sessions including the caller's is the correct semantic | Verified: `password_reset.go:309-315` revokes unconditionally (no "except caller" carve-out). OWASP ASVS V2.5.2 is unambiguous. |
| 4 | Best-effort is correct (non-fatal on revocation error) | Verified: `password_reset.go:311-314` logs and continues. Cryptographic change is irreversible. |
| 5 | Order: revoke after DB commits | Verified: `password_reset.go:295-315` runs revocation as Step 3, after Step 1 (DEK reinit) and Step 2 (bcrypt update). |
| 6 | `RotateKey` does NOT need the same treatment | Verified: `key_service.go:1013-` (`RotateKeyWithPassword`) re-wraps the same DEK with a new KEK; password is unchanged. Threat model G38 scopes to ChangePassword only. |
| 7 | `RecoverAccount` is out of scope for this PR | Verified: threat model G38 row scopes to `ChangePassword`. RecoverAccount flagged as follow-up. |
| 8 | Setter pattern matches existing convention | Verified: `secrets.go:547-555` (existing `SetPasswordUpdater`, `SetAuditFunc`). |
| 9 | Production wiring uses `*auth.Service` (concrete type) | Verified: `app.go:614` already type-asserts `svc.Auth.(*auth.Service)`. |
| 10 | Pre-existing `mockCache{}` cannot exercise revocation end-to-end | Verified: `auth_sessionid_test.go:174-194` — every method is no-op. Added `statefulMockCache` rather than mutate the shared mock. |

---

## Adversarial Self-Review (Rule 11)

### Phase 1 — finding candidates

1. Slow `RevokeAllUserSessions` blocks the HTTP request (up to 5s on Redis/PG hiccup).
2. Client disconnect mid-revocation leaves some sessions unrejected (partial revocation).
3. `trackUserSession` may have missed the caller's session at login time, so the caller's JWT survives revocation.
4. Should the response return a fresh JWT to avoid the re-login round-trip?
5. Race between `KeyService.ChangePassword`'s DEK eviction (line 881) and my new revoker call.
6. `err.Error()` log path leaks secret material.
7. `RevokeAllUserSessions` never returns non-nil in production — dead error-handling branch.
8. Type-assertion failure silently degrades.
9. E2E stateful cache ignores TTL.
10. `stateful_mock_cache_test.go` is 130 lines — overkill?
11. Attacker who knows the password can grief by repeatedly calling change-password.
12. New-password policy enforcement.

### Phase 2 — validation

| # | Real? | Disposition |
|---|---|---|
| 1 | Real but acceptable — same as password_reset (≤5s timeout). User clicked the button; brief wait is fine. | Documented, no fix |
| 2 | **Real but pre-existing.** `password_reset.go:312` uses the same `c.Request.Context()` pattern. Fixing this requires detaching revocation from the request lifecycle (background context with retry). Out of scope for G38. | Flagged as follow-up |
| 3 | False alarm. `trackUserSession` runs on every successful login. The only way it's missed is if Redis is down at login, in which case the user has no session to revoke. Plus `KeyService.ChangePassword` already evicts the caller's cached DEK at `key_service.go:881`. | False alarm |
| 4 | Design choice, not a bug. Returning a fresh JWT would save a round-trip but skip the explicit re-authentication. Threat model requires invalidation, not re-issuance. | Not a finding |
| 5 | False alarm. `KeyService.ChangePassword` runs to completion (including the durable-session delete at line 881) before returning. My revoker call runs after the return. No interleaving. | False alarm |
| 6 | False alarm. `err.Error()` of a cache/PG error contains no user data. | False alarm |
| 7 | False alarm. The interface contract permits errors; the current impl is best-effort by design. Defensive code is correct for the interface, not for one impl. | False alarm |
| 8 | Real, intentional. Mirrors `app.go:536`. Documented in code comment. | Not a finding |
| 9 | Real, acceptable. TTL enforcement is an auth-layer invariant; G38 tests revocation, not TTL expiry. | Not a finding |
| 10 | False alarm. Inline map is simpler than miniredis (no goroutine, no port). Surface matches what the test needs. | False alarm |
| 11 | False alarm. Attacker who knows the password already has full account access; griefing adds nothing. | False alarm |
| 12 | False alarm. Binding tag `min=8` already enforced. | False alarm |

### Phase 3 — remediation

- Finding 2 (partial revocation on client disconnect): documented in this worklog as a follow-up gap, not in scope for G38. The fix is a separate PR that detaches revocation from the request context across BOTH `password_reset` and `change_password`.
- Zero other real findings.

---

## Blockers

None.

---

## Tests Run

```bash
# Targeted unit tests (this PR)
go test -count=1 -timeout 60s -run 'TestChangePassword' -v ./api/internal/handlers/...
# → 5/5 PASS

# Targeted e2e tests (this PR)
go test -count=1 -timeout 90s -run 'TestE2E_RealAuth_ChangePassword' -v ./api/internal/services/auth/...
# → 2/2 PASS (including the extended G38 regression assertion)

# Full auth package (regression check after cache swap)
go test -count=1 -timeout 150s ./api/internal/services/auth/...
# → PASS (18.7s)

# Full repository test suite
go test -timeout 240s -short ./...
# → 67 packages ok, 0 FAIL

# Build
go build ./...
# → exit 0

# Vet
go vet ./...
# → exit 0

# Lint (changed packages)
golangci-lint run --timeout=4m ./api/internal/handlers/... ./api/internal/app/... ./api/internal/services/auth/...
# → 0 issues

# Format
gofmt -l <changed files>      # clean
goimports -l <changed files>  # clean
```

---

## Next Steps

1. **Merge this PR**, then move to the next High gap in the threat model.
2. **Follow-up gap (out of scope here):** detach `RevokeAllUserSessions` from the request context so client disconnect cannot cause partial revocation. Affects both `password_reset.go:312` and `secrets.go:ChangePassword`. Suggest a small `BackgroundSessionRevoker` wrapper that takes a fresh `context.Background()` with a timeout, plus a regression test that cancels the request context mid-revocation and asserts all sessions are still revoked.
3. **Sibling gap:** `RecoverAccount` (`secrets.go:635`) has the same shape as pre-G38 `ChangePassword` — uses `ResetWithRecoveryKey` but does not revoke sessions. Worth a parallel PR if the threat model is extended to cover it.
4. **Next threat-model gap to address:** recommended order is G37 (workspace env-var blocklist) → G35 (`/account/recover` rate limit) → G25 (logged secret values) → G36 (workspace secret cleanup on deletion) → G28 (bind-handler no-op).

---

## Files Modified

- `api/internal/app/app.go` — wire `SetLogger` + `SetSessionRevoker` on `rotateKeyHandler`
- `api/internal/handlers/secrets.go` — `SessionRevoker` interface, `revoker`/`logger` fields, `SetSessionRevoker`/`SetLogger` setters, revoker invocation in `ChangePassword`
- `api/internal/handlers/change_password_test.go` — **new** — 5 unit tests
- `api/internal/services/auth/auth_e2e_all_test.go` — switch to stateful cache; wire `SetSessionRevoker`; extend `TestE2E_RealAuth_ChangePassword` with the G38 regression assertions
- `api/internal/services/auth/stateful_mock_cache_test.go` — **new** — `statefulMockCache` CacheService implementation
- `CHANGELOG.md` — entry under `[Unreleased] → Security`
- `design/stories/epic-17-security-review/THREAT-MODEL.md` — G38 row flipped to 🟢 Fixed; STRIDE row + counts + revision history updated
- `COORDINATE.md` — claimed the work, will release on merge
- `worklogs/NNNN_2026-07-11_g38-changepassword-revokes-sessions.md` — this file

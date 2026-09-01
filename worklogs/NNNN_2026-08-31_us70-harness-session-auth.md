# Worklog: US-70.0 AC-1 (layer 2) — harness session auth for DEK-gated secret authoring

**Date:** 2026-08-31
**Session:** Pool run 33447976676 (post-#1197) still failed AC-1 at `bind_env` — with the UUID workspace name visible, so the resolution fix worked and a second layer surfaced. Root-caused and fixed.
**Status:** Complete — pin tests green; pool re-dispatch after merge.

---

## Objective

Make `bind_env`/AC-F actually create user secrets from the harness.

---

## Root cause (validated in code)

`CreateSecret` resolves the user's DEK via `GetDEK(ctx, sessionID, matchedSigningKey)` (`secret_service.go:78-80`) — a **JWT-session** lookup (Redis `dek:<jti>`). The harness authenticates with an **API key** (no session) → `ErrDEKUnavailable` → `SetWorkspaceEnv` 500s ("failed to create env var"). The psql-seeded user also had no `user_keys` row and a dummy password hash, so no login was possible either. (The SDK canary confirms the class: it uses `JWTClient` and skips DEK-dependent ops without a password.)

## Work Completed

- `local/lib/us70-common.sh`: `seed_session` replaces the psql user insert — **register through the API** (`POST /auth/register`; turnstile disabled in e2e values; no email verifier wired → immediately email_verified; provisions `user_keys` under the server KEK) then **login** (`POST /auth/login`) → `AUTH_TOKEN` + `OWNER_ID` (the API-minted `users.id` **UUID** — ownership identity for api_keys/workspaces/CR `spec.owner`, not the username). The deterministic `lsp_` API-key row is still psql-inserted under `OWNER_ID` for resolution-only ops (auth matches the raw key: `WHERE k.key = $1`).
- `bind_env`: Bearer `AUTH_TOKEN` (die if unset) and **diagnosable failures** — captures HTTP code + first 400 bytes of the body instead of curl's silent `-f`.
- `seed_workspace(ws, [runtime_class])`: owned by `OWNER_ID` (call sites updated in both suites; faults' F3 `user_keys`/audit queries keyed on `OWNER_ID` too).
- `local/us-70-secret-delivery-e2e.sh` AC-F: secret create + bind switched to `AUTH_TOKEN` — with an API key the follow-up push would degrade sessionless and the ssh key would never deliver.
- Pin: `TestUS70Harness_SessionAuthPins` (register/login/OWNER_ID/AUTH_TOKEN presence; forbids the psql user insert; AC-F session-auth count).

## Key Decisions

1. **Register through the API, never psql** — the user row needs a real password hash + `user_keys` under the master KEK; registration is the only path that provisions both correctly.
2. **`OWNER_ID` (uuid) ≠ `USER_ID` (username)** — threaded through every ownership surface; the username remains only the login handle.
3. **Both tokens kept** — JWT for DEK-gated authoring, API key for suspend/activate/probes (validates both auth surfaces the fleet uses).

## Assumptions (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A-1 | Register is open in e2e clusters | `turnstile.enabled: false` (helm defaults); no verifier wired → `email_verified=true` immediately (`auth.go:1005-1015`) |
| A-2 | Login caches the DEK under the returned JWT's jti | Register runs `UnlockDEKWithSigningKey(..., jti, ...)` (`auth.go:1000`); login re-unlocks; `GetDEK(jti)` hits the same cache |
| A-3 | `/auth/me` returns the user id | `AuthResponse{Token, User}` shape (`types/auth.go:48`); `.user.id` read with fallback |
| A-4 | API-key auth matches the raw key | `GetUserByAPIKey`: `WHERE k.key = $1 AND k.active` (database.go:415) |

## Blockers

None. Re-dispatch the pool after merge.

---

## Tests Run

- `go test -timeout 60s ./local/` — ok (incl. the new session-auth pins)
- `bash -n` ×2 suites + lib — clean

---

## Next Steps

1. Merge → re-dispatch `us-70-delivery-pool.yml`; AC-1 should now reach `wait_phase`.
2. #1194 (US-70.2) rebased over W8; iterate CI to green and merge.
3. If the pool surfaces a layer 3, capture the body (bind_env now prints it).

---

## Files Modified

- `local/lib/us70-common.sh` (seed_session/login_harness_user/OWNER_ID, bind_env diagnostics + AUTH_TOKEN, seed_workspace signature)
- `local/us-70-secret-delivery-e2e.sh` (call sites, AC-F auth)
- `local/us-70-faults-e2e.sh` (call sites, F3 ownership keys)
- `local/us70_harness_script_test.go` (TestUS70Harness_SessionAuthPins)

## Addendum (review rounds): unhappy-path pins + JWT-expiry robustness

- Fake-API-driven die paths (Go test spins httptest; the extracted bash
  helpers run against it): register-500 + login-401 → die naming the
  failure; /auth/me 500 → die at id resolution.
- `bind_env` now retries once through a re-login on 401 — pool dwells can
  outlive a short JWT TTL; pinned end-to-end (rejected token → re-login →
  refreshed token accepted, exactly one re-login).
- Handler-level regression: `TestSetWorkspaceEnv_APIKeyAuth_NoDEKSession_500`
  pins the original bug as an executable contract.

Closes #1196 (with #1197): the issue's resolution/root-cause layer landed in
#1197; this PR is the second layer the re-dispatched pool exposed (secret
authoring is DEK-session-gated). AC-1 is green only with both.

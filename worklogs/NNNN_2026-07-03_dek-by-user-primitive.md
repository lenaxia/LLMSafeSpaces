# Worklog: DEK-by-user primitive — foundation for background auto-push

**Date:** 2026-07-03
**Session:** Add `KeyService.GetDEKForUser(userID)` so background paths (workspace watcher, controller-triggered auto-push after silent pod recreation) can retrieve a user's DEK without a live user request. Foundation for the follow-up "silent pod recreation" fix.

**Status:** Complete. Ready for PR.

## Motivation

PR #494 (merged 6a0dc526) hooked user-DEK auto-push to the `/status` endpoint's request context. That works when the frontend is polling `/status` and a pod-identity transition is observable. It fails when:

- The pod is recreated silently (kubectl delete pod → controller re-creates without a visible phase transition, or phase transitions that the frontend hasn't invalidated its query on yet).
- The user was on a chat page and their frontend polls `/sessions`/`/models` far more frequently than `/status` (verified live at 30s poll interval on Active workspaces).
- The API server itself just deployed a new column and hasn't persisted any pod-identity observations yet.

I saw this live on workspace `890fad31` on 2026-07-03: v87 deployed, user resumed the workspace, `/status` was polled zero times for the ~40 minutes after the new pod came up. The user's `~/.ssh` and `GH_TOKEN` were empty; #494 never fired because the DB row had a stale `last_seen_pod_start_time` and no /status poll happened to compare against the CRD.

The **root architectural issue**: the fix relied on a request-context-supplied DEK. If we can retrieve the DEK without a live request, we can trigger the auto-push from anywhere — the K8s watcher, controller callbacks, a periodic reconciliation loop. This PR builds that primitive.

## What this PR does

Just the primitive. Detection design (where to fire the push) is deliberately deferred to a follow-up so this PR stays scoped and the primitive can be reviewed on its own merits.

### `JWTSessionStore.ListActiveJWTSessionsForUser`

New interface method + Pg implementation. Uses the existing `idx_jwt_sessions_user_id` index (schema 000001). Filters expired rows inline via `WHERE expires_at > NOW()`. Ordered `created_at DESC` so callers pick the most-recent session first (most likely wrapped under the CURRENT active signing key, minimizing rotation-fallback iterations). Bounded by caller-supplied limit.

### `secrets.SigningKeyEnumerator`

New interface exposing "the API's active JWT signing keys, primary first then previous keys." Callback pattern with early-exit (`return false` to stop). Implementations copy bytes per-iteration so callers can safely mutate for their KDF work without corrupting subsequent deliveries.

Implemented by `auth.Service.EachSigningKey` — trivial wrapper around the existing `signingKeyByIndex(i)` accessor.

Wired into `KeyService` via a new setter `SetSigningKeyEnumerator`. Nil-safe: when unset, `GetDEKForUser` returns `ErrDEKUnavailable` (same sentinel as "no session").

### `KeyService.GetDEKForUser`

The primitive. Resolution order:

1. `ListActiveJWTSessionsForUser(userID, 5)` → candidate rows. Empty → `ErrDEKUnavailable`.
2. For each row (most-recent first):
   a. Check Redis under `dek:<jti>`. Cache hit → return. Redis error → log Warn (aligning with the sibling `GetDEK` "Redis DEK lookup failed" pattern) and fall through to unwrap.
   b. Iterate signing keys. First successful `DecryptSecret` → write back to Redis under this jti (if remaining TTL > 0; guard aligns with `rehydrateDEKFromJWTSession`), return DEK.
3. All rows exhausted with no unwrap → `ErrDEKUnavailable`.

Bounded by `jwtSessionUserLookupLimit = 5` — a user with more than 5 concurrent sessions where NONE of them wraps under a currently-known signing key is well outside our rotation window; further rows aren't going to unwrap either.

Errors:
- `ErrDEKUnavailable` for every legitimate "no user context" case (no session, no signing key unwraps, no wiring).
- Genuine PG/cache errors returned verbatim so operators can distinguish "user logged out" from "infrastructure down."

Cache write-back is best-effort — a cache write failure doesn't fail the return; we still deliver the DEK the caller asked for.

Why not add a Redis secondary index `dek_by_user:<userID>` too? YAGNI. The PG query is one indexed lookup (idx_jwt_sessions_user_id + expires_at filter). Auto-push happens on the order of pod-recreations per user per day — a few dozen PG round-trips per hour cluster-wide is negligible. Cold-Redis case (Valkey restart) means the secondary Redis index would be empty anyway; the PG fallback is the correct primary. If workload changes (e.g. detection ends up firing per-poll rather than per-recreation), we add the index then, with data.

## TDD

Tests written first for every method. Adversarial-validated by neutering each of:

- `ListActiveJWTSessionsForUser` sort → `TestListActive_OrdersMostRecentFirst` fails.
- `GetDEKForUser` cache-write-back → 2 tests fail on "must have populated dek:<jti>" assertion.
- `GetDEKForUser` unwrap-error suppression (silently return garbage) → `TestGetDEKForUser_UnwrappableSurfacesDEKUnavailable` fails.
- `GetDEKForUser` signing-key iteration (stop after first) → `TestGetDEKForUser_FallsBackToPreviousSigningKey` fails.
- `GetDEKForUser` cache-hit short-circuit → `TestGetDEKForUser_UsesCachedDEKIfPresent` fails.
- `GetDEKForUser` wiring nil-guard → panic (verified test catches it).
- `EachSigningKey` early-exit → `TestEachSigningKey_StopsWhenCallbackReturnsFalse` fails.

## What's NOT in this PR

- Detection logic. No new code fires `GetDEKForUser` yet. This is intentional: the primitive is reviewable on its own, and the detection design has its own tradeoffs (agentd-signal vs watcher-driven vs middleware-hook) that deserve their own PR.
- Removal of #494's `PodIdentityTracker` + DB columns. Deferred until the replacement detection lands.
- Frontend changes.

## Files

- `pkg/secrets/jwt_session_store.go` — add interface method + Pg impl.
- `pkg/secrets/jwt_session_store_test.go` — 5 new tests, mock method inlined.
- `pkg/secrets/key_service.go` — `SigningKeyEnumerator` interface + `SetSigningKeyEnumerator` setter + `GetDEKForUser` + `tryUnwrapRowWithKnownKeys` helper + `jwtSessionUserLookupLimit` const.
- `pkg/secrets/key_service_get_dek_for_user_test.go` — 12 tests + fixture + `fakeDEKCache` (with injectable errors) + `staticSigningKeys` + `captureLogger`.
- `api/internal/services/auth/auth.go` — `EachSigningKey` implementing the enumerator.
- `api/internal/services/auth/auth_signing_key_enumerator_test.go` — 4 tests.
- `api/internal/app/app.go` — one-line wiring: `keyService.SetSigningKeyEnumerator(authSvc)`.

## Test summary

24 new tests across 4 packages (12 GetDEKForUser + 6 ListActive mock + 4 EachSigningKey + 2 PG integration). Full sweep (`go test ./api/... ./pkg/...`) green except for an unrelated repolint check about an unassigned worklog number on origin/main (from a separate merge).

## Follow-up

Next PR: replace `#494`'s `maybeAutoPushOnPodTransition` + `workspace_agent_state.last_seen_pod_*` columns with a detection design that uses `GetDEKForUser` to fire from a background context. Options captured in worklog 0590 on branch `fix/secret-manifestation-agentd-signal`.

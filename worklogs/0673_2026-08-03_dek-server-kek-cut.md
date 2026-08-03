# Worklog: Cut password DEK tier — server-KEK-only

**Date:** 2026-08-03
**Session:** Collapse the two-tier DEK model (password-derived Argon2id vs server-KEK master RootKeyProvider) to server-KEK-only.
**Status:** Complete
**Depends on:** PR #621 (migrate-passkey-dek tool), worklog 0662 (migration runbook)

---

## Objective

Remove the password-derived DEK tier entirely. Every user's DEK is wrapped by
the server-side master RootKeyProvider. This eliminates the hybrid gap (password
user + passkey login → broken DEK) by removing the dispatch that produced it, and
simplifies the encryption layer from two tiers to one.

## Work Completed

### Production code (28 files, -3164 LOC net)
- **Deleted from `key_service.go`:** `InitializeUserKeys` (password provisioning),
  `ChangePassword`, `ResetWithRecoveryKey`, `RotateKeyWithPassword`, `RotationResult`
- **Collapsed `UnlockDEKWithSigningKey`:** unconditional `rootKeyProvider.Decrypt`
  (the `dekSourceIsServerWrapped` dispatch and password else-branch are gone)
- **Deleted endpoints:** `POST /account/rotate-key`, `/account/change-password`,
  `/account/recover` + handlers + routes + app.go wiring
- **Simplified `/auth/unlock-dek`:** password-less server-KEK re-unwrap (no body)
- **Updated auth.go:** `Register`/`Login`/password-reset provisioning →
  `InitializeUserKeysServerKEK`; `AuthResponse.RecoveryKey` is now always `""`
- **Schema:** migration `000014` backfills `dek_source='server_kek'` + tightens
  CHECK to `('server_kek', 'passkey')` + sets column default to `'server_kek'`
- **Retained:** `DeriveKEKFromPassword` (sealed provider uses it), `WrapDEK`/`UnwrapDEK`
  (migration CLI), `GenerateRecoveryKey` (test fixtures)

### Tests
- Deleted password-tier test functions (~90 refs fixed across ~20 files)
- Updated all fixtures to use `InitializeUserKeysServerKEK` + wire `rootKeyProvider`
- Rewrote `auth_unlock_test.go` for password-less contract
- Deleted obsolete `ChangePassword`/`RotateKey`/`Recover` e2e tests

### SDK cleanup
- Removed `RotateKey`/`ChangePassword`/`Recover` methods from Go, TypeScript,
  Python, Java SDKs
- Deleted `d-account-recover`, `d-change-password`, `d-key-rotate` canary scenarios
- Removed 3 account routes from `sdks/openapi.yaml`

## Key Decisions
- **Migration CHECK allows both `'server_kek'` and `'passkey'`** — the passkey
  handlers provision with `dek_source='passkey'` to distinguish the auth source.
  Both share the same server-KEK unwrap path.
- **Password param kept in `UnlockDEKWithSigningKey` signature** — ignored, for
  call-site stability. Avoids touching every handler/service that calls it.
- **`/auth/unlock-dek` endpoint retained** but repurposed — password-less server-KEK
  re-unwrap for the Redis-cache + durable-session-miss case.

## Deploy Order (CRITICAL)
1. Run `cmd/migrate-passkey-dek` for each user (PR #621, worklog 0662)
2. Deploy this PR
3. Deploying in the wrong order orphans every password-tier user's secrets

## Tests Run
- `go vet ./...` — pass (including `-tags=integration`)
- `go test ./pkg/secrets/` — pass
- CI: Lint, Frontend, FK cascade, pkg/secrets integration, migration checks — all pass
- `golangci-lint` — clean (pre-existing funlen warnings only)

## Files Modified
- `worklogs/0673_2026-08-03_dek-server-kek-cut.md` — this entry (added)
- `pkg/secrets/crypto.go` — deleted password-only primitives, kept shared
- `pkg/secrets/key_service.go` — collapsed dispatch, deleted password methods
- `pkg/secrets/postgres_provider.go` — updated compile-time interface assertion
- `api/internal/services/auth/auth.go` — server-KEK provisioning only
- `api/internal/handlers/auth_unlock.go` — password-less soft-unlock
- `api/internal/handlers/password_reset.go` — server-KEK reinit
- `api/internal/handlers/secrets.go` — deleted RotateKeyHandler + endpoints
- `api/internal/server/router.go` — removed deleted routes
- `api/internal/app/app.go` — removed RotateKeyHandler wiring
- `api/migrations/000014_dek_source_server_kek_only.{up,down}.sql` — schema
- `helm/migrations/000014_*` — mirror
- `sdks/openapi.yaml` — removed deleted routes
- `sdks/{go,typescript,python,java}/` — removed stale SDK methods
- `sdks/canary/{go,typescript}/scenarios/` — removed stale canaries
- ~20 test files — updated fixtures, deleted password-tier tests

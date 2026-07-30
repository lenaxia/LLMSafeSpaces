# Worklog: Epic 58 — SSO users get server-KEK-wrapped DEK by default (foundation for Epic 59 passkeys)

**Date:** 2026-07-30
**Status:** Implementation
**Depends on:** Epic 50 (shipped — RootKeyProvider + apiKeyProv + rotate-kek coordinator), Epic 56 (shipped — durable jwt_sessions)
**Unblocks:** Epic 59 (passkey-only login) — passkey users consume the same `dek_source='server_kek'` machinery.

## Context

User intent: passkey support, passwordless default signup, "use password" as explicit opt-in (Phase 2 of Epic 59). Epic 59 depends on Epic 58 because both non-password paths (SSO + passkey) answer the same DEK-source question: a user without a password has nothing to derive a DEK from. This worklog implements Epic 58's machinery so Epic 59 can consume it.

Deployment state: testing phase, no existing SSO users. The SSO-specific open questions from the design (Q2 default flag, Q3 backfill of existing SSO users, Q5 user notification) are therefore MOOT — there is no SSO population to backfill or notify. The design's recommended defaults are taken implicitly (auto-provision server-KEK for new SSO users; no separate Helm flag).

## Assumptions (Rule 7) — stated and validated

- A1: The master-KEK `RootKeyProvider` (a.k.a. `apiKeyProv`, master-kek purpose) is already wired into `KeyService.rootKeyProvider` via `SetAPIKeyStore`. Validated: app.go:691 + key_service.go:156-158. Server-KEK user DEKs reuse this provider — no new provider.
- A2: server-KEK user DEKs use the SAME master-kek purpose as api_keys (design Epic 58:64). Validated by design + A1.
- A3: `GetDEK` never unwraps from a password (Redis cache + jwt_sessions rehydrate only). Validated: key_service.go:502-609. The server-KEK branch belongs ONLY in `UnlockDEKWithSigningKey` + provisioning.
- A4: SSO users today have NO user_keys row (random unusable bcrypt hash, resolveUser never calls InitializeUserKeys). Validated: sso.go:606-638.
- A5: SSO callback does NOT unlock a DEK today (only GenerateToken). Validated: sso.go:575-599.
- A6: `user_keys.salt` is NOT NULL today; server-KEK rows have no Argon2 salt → must become nullable. Validated: initial_schema:531.
- A7: `pgRotationStore` (cmd/rotate-kek/store.go) is a pre-existing stub for ALL tables — returns "not implemented" for provider_credentials/api_keys/org_sso_configs already. NOT introduced by this epic. The production-ready surface is the `RotationCoordinator` (rotation.go) which IS tested with a fake store. This epic wires the coordinator for user_keys; the PG store stub remains pre-existing tech debt affecting all tables equally.
- A8: next migration numbers are 000007/000008 (last existing is 000006). Validated: ls api/migrations.

## Design

- **D1 — `dek_source` on `users`, not `user_keys`.** ENUM('password','server_kek'), default 'password'. Semantic companion to password_hash. Epic 59 adds 'passkey' later.
- **D2 — server-KEK DEK wrapped by master-kek provider (same as api_keys).** One provider, one purpose. `KeyService.rootKeyProvider.Decrypt` unwraps it.
- **D3 — `DEKSource` on `UserKeyRecord`, populated via JOIN in PgKeyStore.GetUserKey.** `UnlockDEKWithSigningKey` branches internally on `record.DEKSource` — signature unchanged (least invasive; record carries everything needed to unwrap). Fail-closed if `dek_source==server_kek` but `rootKeyProvider==nil`.
- **D4 — `KeyService.InitializeUserKeysServerKEK(ctx, userID)`** provisioning helper: random DEK → `rootKeyProvider.Encrypt` → CreateUserKey with nil salt, no recovery blob, KeyVersion=ActiveVersionOf(rootKeyProvider). Caller sets dek_source='server_kek'. Used by SSO resolveUser + Epic 59 passkey signup.
- **D5 — `ChangePassword` flips dek_source to 'password' after re-wrap** (server_kek→password opt-up; one-directional per design — once a user sets a password they keep the stronger tier).
- **D6 — `ChangePassword` must also handle the reverse unwrap:** a server_kek-tier user changing password unwraps with `rootKeyProvider.Decrypt` (not old password — they have none... actually SSO users have no password; ChangePassword is only meaningful once they set one). Resolve: ChangePassword branches on record.DEKSource for the OLD unwrap.
- **D7 — SSO provisioning + unlock routed through auth.Service** via a new narrow interface in the sso package (`UserKeyManager`: `ProvisionServerKEKKeys` + `IssueTokenAndUnlockDEK`). auth.Service implements both (it holds keyService + dbService + jwtSecret). sso.completeLogin provisions new users + uses IssueTokenAndUnlockDEK (which provisions-if-no-keys then unlocks, uniformly handling new + pre-existing SSO users). Keeps sso.Service decoupled from keyService internals.
- **D8 — rotate-kek coordinator:** add `user_keys` to RotateAll; `purposeForTable` returns "master-kek" for user_keys; `RotationRow` gains `DEKSource`; the store contract filters to server_kek/passkey rows (password-tier rows CANNOT be rotated — CLI has no password). Password rows skipped.
- **D9 — `users.dek_source` read/written** in GetUser/GetUserByEmail/CreateUser SELECT + a new UserUpdates.DEKSource field for UpdateUser.

## Out of scope (per design + testing-phase state)

- Helm flag `sso.serverKekDefault` (Q2) — no SSO users; auto-provision is the default.
- Backfill of existing SSO users (Q3) — none exist; the provision-if-no-keys path in IssueTokenAndUnlockDEK handles any future case naturally.
- User notification/disclosure (Q5) — no affected users.
- Per-org KEK — assessed and rejected separately (prior conversation); KMS backing (Epic 57) is the load-bearing blast-radius control.
- `dek_source='passkey'` enum value — owned by Epic 59.

## Files

New: api/migrations/000007_users_dek_source.up.sql + .down.sql, 000008_user_keys_salt_nullable.up.sql + .down.sql
Modified: pkg/types/auth.go; api/internal/services/database/database.go; pkg/secrets/pg_key_store.go; pkg/secrets/key_service.go; api/internal/services/auth/auth.go; api/internal/services/sso/sso.go; api/internal/app/app.go; pkg/secrets/rotation.go; cmd/rotate-kek/main.go


## Implementation summary (Epic 58 — minimal, non-speculative)

**Schema** — `api/migrations/000007_users_dek_source.{up,down}.sql` (text + CHECK, matching the `users.status` convention; Epic 59 widens the CHECK to add 'passkey'); `000008_user_keys_salt_nullable.{up,down}.sql` (drops NOT NULL so server-KEK rows can have NULL salt).

**Crypto core** (`pkg/secrets`):
- `UserKeyRecord.DEKSource` field; `KeyStore.UpdateWrappedDEKAndSource` interface method (atomic re-wrap + dek_source flip).
- `dekSourceServerKEK` local const (pkg/secrets stays decoupled from pkg/types per Rule 12).
- `UnlockDEKWithSigningKey` branches on `record.DEKSource`: server_kek → `rootKeyProvider.Decrypt` (password ignored, fail-closed `ErrServerKEKUnavailable` if no provider); password → existing Argon2id path.
- `InitializeUserKeysServerKEK` provisions a random DEK wrapped by the master-KEK provider (nil salt, no recovery blob — DEK recoverable from master KEK, consistent with API keys).
- `ChangePassword` branches the OLD unwrap on dek_source AND uses `UpdateWrappedDEKAndSource` for the server_kek→password opt-up transition (atomic).
- `PgKeyStore.GetUserKey` JOINs users.dek_source; `CreateUserKey` atomically flips users.dek_source='server_kek' in the same tx when provisioning; `UpdateWrappedDEKAndSource` is a single-tx UPDATE of both tables.

**Auth/SSO glue**:
- `auth.KeyServiceInterface.InitializeUserKeysServerKEK`; `auth.Service.ProvisionServerKEKKeys` + `IssueTokenAndUnlockDEK` (generate token → provision-if-no-keys → unlock; provision failure degrades gracefully, token still returned, mirrors Login's best-effort semantics).
- `sso.UserKeyManager` interface; `sso.issueSession` uses the manager when wired, falls back to `TokenIssuer.GenerateToken` otherwise (nil-safe). `completeLogin` routes through it.
- `app.go` wires `authSvc` as both `TokenIssuer` and `KeyManager`.

**Rotation** — `rotation.go`: `user_keys` added to `RotateAll`; `purposeForTable` returns "master-kek" for user_keys; `RotationRow.DEKSource` documents the store contract (password-tier rows excluded — not rotatable). `rotate-kek` CLI `validTables` + the `all` walk include `user_keys`.

## Key finding during implementation (adversarial self-review, Rule 11)

Initial draft added `defer zeroBytes(dek)` to `UnlockDEKWithSigningKey` (mirroring the existing `defer zeroBytes(kek)`). This broke `TestE2E_PasswordReset_OldDEKCannotDecryptAfterReinit`: `testDEKCache.CacheDEK` stores the slice **by reference** (no copy), so zeroing the DEK at function return corrupted the cached value → both pre/post-reset DEKs read back as all-zeros → the erasure assertion failed. Root cause: the codebase convention is to zero derived KEKs but NEVER the DEK (it is cached/shared/aliased). Fixed by removing the `defer zeroBytes(dek)` additions in both `UnlockDEKWithSigningKey` and `InitializeUserKeysServerKEK`. Validated against the clean tree (test passes) and reproduced the failure on my tree before the fix.

## Verification

- `go build ./...` — clean.
- `go vet ./...` (compiles all tests) — clean.
- `gofmt -l` / `goimports -l` on changed files — clean (after `gofmt -w`/`goimports -w`).
- `go test ./pkg/secrets/` — PASS (incl. new `key_service_serverkek_test.go`, rotation user_keys tests).
- `go test ./api/internal/services/auth/` — PASS (incl. new `auth_serverkek_test.go`).
- `go test ./api/internal/services/sso/` — PASS (incl. new `sso_keymanager_test.go`).
- `go test ./api/internal/handlers/` — PASS.
- golangci-lint not installed in this environment; vet + gofmt + goimports run as the available static-analysis gate.

## Status

Epic 58 (foundation) COMPLETE. Unblocks Epic 59 (passkey-only), which consumes the `dek_source='server_kek'` machinery and adds the `'passkey'` enum value + WebAuthn credential storage + ceremonies + recovery codes + frontend default-swap (Phase 2).

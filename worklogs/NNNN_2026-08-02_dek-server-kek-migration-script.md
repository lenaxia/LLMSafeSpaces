# Worklog: DEK password→server_kek migration script

**Date:** 2026-08-02
**Session:** Add a one-shot operator tool to re-wrap a user's DEK from the legacy password-derived tier (Argon2id) to the server-KEK tier (master RootKeyProvider), ahead of cutting the password tier entirely.
**Status:** Complete
**Depends on:** Epics 50 (master KEK), 58 (server_kek tier), 59 (passkey tier)

---

## Objective

Eliminate the two-tier DEK model (password-derived vs server-wrapped) by moving
every user onto the server-KEK tier. This is the prerequisite for a follow-up cut
that deletes the password tier. This session delivers the **migration tool**; the
deletion cut is a separate PR.

The trigger was a real gap discovered while reviewing the passkey flow: a password
user who enrolls a passkey and then logs in via it gets a broken DEK (the passkey
login passes `nil` as the password to `UnlockDEKWithSigningKey`, which can't
unwrap a password-wrapped DEK). Collapsing to server-KEK-only removes the dispatch
that produces the gap.

## Approach chosen (and rejected alternatives)

With ~2 users (one active with secrets, one with none), three options were weighed:

1. **Destructive reset** (fresh DEK, re-enter secrets) — cleanest cut, but data loss.
2. **Lazy at-login hook shipped in the API** — preserves data, but ships dead code
   (the password unwrap branch) for one cycle.
3. **One-shot operator script** (chosen) — preserves data, cut stays pure deletion,
   cost is one operator command. Strictly better than (2) for N≈1.

## Assumptions

- The operator can supply the user's current password (needed for the one-time
  unwrap). If not, fall back to destructive reset.
- The script runs with the SAME master-KEK material the API uses at boot (same env
  vars / sealed files). Provider construction mirrors `newRootKeyProvider` for the
  self-hosted paths (static + sealed). Cloud-KMS deployments are out of scope —
  they already provision all users on the server-KEK tier.

## Work Completed

### cmd/migrate-passkey-dek (new)
- `main.go` — standalone binary. Reads `--db-url`, `--user` (email or ID),
  `--password-file` (or `PASSKEY_MIGRATE_PASSWORD`), provider flags, `--dry-run`.
- Migration core (`migrateUser`) is PG-independent and accepts a `secrets.KeyStore`
  interface, so it is unit-testable without a database.
- Idempotent: a user already on a server-wrapped tier is skipped.
- Fail-closed: a wrong password fails at `UnwrapDEK` (AES-GCM auth-tag) before any
  write; `dek_source` is left untouched and the run is retryable.
- Atomic commit via `PgKeyStore.UpdateWrappedDEKAndSource` (one tx flips both the
  wrap and `users.dek_source`).
- Post-migration verification re-reads the row and asserts `dek_source='server_kek'`.
- `key_version` is set to `ActiveVersionOf(provider)` (consistent with fresh
  server-KEK provisioning), not the prior password-tier version.

### Provider construction
- Static: reads `LLMSAFESPACES_MASTER_SECRET_FILE` (colon list, last = active) /
  `LLMSAFESPACES_MASTER_SECRET` / legacy `LLMSAFESPACES_DEK_MASTER_KEY`, derives the
  `master-kek` purpose key via HKDF-SHA256 — mirrors `activeMasterSecret` +
  `deriveServerKey` in `api/internal/app/secrets_adapters.go`.
- Sealed: `--sealed-key-file` + `--passphrase-file` → `NewSealedKeyProvider`.
- Master material is zeroed after derivation.

## Key Decisions

- **Reuse existing building blocks** (`DeriveKEKFromPassword`, `UnwrapDEK`,
  `RootKeyProvider.Encrypt`, `PgKeyStore.UpdateWrappedDEKAndSource`) rather than
  adding a new `KeyService` method. This keeps the cut PR smaller (no new method
  to later delete) and keeps the migration logic in one throwaway tool.
- **key_version = ActiveVersionOf(provider)**, matching `InitializeUserKeysServerKEK`.
  Carrying over the password-tier version would be meaningless and could confuse
  the rotation coordinator.

## Tests Run

- `go build ./cmd/migrate-passkey-dek/` — pass
- `go vet ./cmd/migrate-passkey-dek/` — pass
- `go test ./cmd/migrate-passkey-dek/ -v` — 5/5 pass:
  - `RoundTripsDEK` — DEK decrypts to the identical plaintext after re-wrap (zero data loss)
  - `WrongPassword_FailsClosed` — no commit, dek_source unchanged
  - `AlreadyServerKEK_IsIdempotent` — skipped, no write
  - `DryRun_NoWrite` — dek_source unchanged
  - `NoKeyMaterial` — errors cleanly

## Next Steps

1. Merge this script to `main`.
2. **Operator runs it** for the one active user (with `--dry-run` first to confirm
   the unwrap succeeds), then for real. Watch `dek_source` flip to `server_kek`.
3. Open the **deletion cut PR**: remove the password tier (see the scope map —
   `DeriveKEKFromPassword`/`WrapDEK`/`UnwrapDEK`/`GenerateRecoveryKey`, the
   `InitializeUserKeys`/`ChangePassword`/`ResetWithRecoveryKey`/`RotateKeyWithPassword`
   methods, the password-tier endpoints, the dispatch branch) + schema backfill
   + close the `GetDEK` server-KEK fallback gap + docs rewrite.
4. After the cut ships, this script can be deleted (or kept as a documented one-off).

## Files Modified

- `worklogs/NNNN_2026-08-02_dek-server-kek-migration-script.md` — this entry (added)
- `cmd/migrate-passkey-dek/main.go` — new migration binary
- `cmd/migrate-passkey-dek/main_test.go` — new unit tests
- `.gitignore` — add `/migrate-passkey-dek` (per-binary convention)

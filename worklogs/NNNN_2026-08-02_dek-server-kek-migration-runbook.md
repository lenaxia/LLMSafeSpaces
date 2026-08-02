# Worklog: DEK server-KEK migration runbook

**Date:** 2026-08-02
**Session:** Operational runbook for re-wrapping a user's DEK from the password tier to the server-KEK tier using `cmd/migrate-passkey-dek` (PR #621). This is the prerequisite to cutting the password DEK tier entirely.
**Status:** Complete (runbook); migration execution pending operator run.

---

## Objective

Re-wrap every existing user's DEK from the legacy password-derived tier
(`Argon2id(password, salt)` → AES-GCM) to the server-KEK tier
(`RootKeyProvider.Encrypt`) so the follow-up "cut the password tier" PR is a pure
deletion with no data loss.

Population: ~2 users (one active with secrets, one with no secrets). Both must
end on `dek_source='server_kek'` before the cut deploys.

## Tool

`cmd/migrate-passkey-dek` (merged in PR #621, worklog 0662). One-shot, idempotent,
fail-closed, atomic. Built from the same `pkg/secrets` building blocks as the API.

## Assumptions (validate before running)

1. **You can supply each user's current password** for the one-time unwrap. The
   recovery key is NOT a migration path — it is one-time-display and not stored
   in plaintext, so the operator does not have it. If you cannot get the password,
   fall back to the destructive reset path (§Fallback) — fresh DEK, orphans old
   ciphertext, user re-enters secrets.
2. **You have the master-KEK material** the API uses at boot:
   - Static provider: `LLMSAFESPACES_MASTER_SECRET_FILE` (colon-separated file
     list, last = active) or `LLMSAFESPACES_MASTER_SECRET` (value env). The script
     derives the `master-kek` purpose key via HKDF-SHA256, matching
     `deriveServerKey` in `api/internal/app/secrets_adapters.go`.
   - Sealed provider (recommended for production self-hosted): the sealed root-key
     file + passphrase file, passed via `--sealed-key-file` / `--passphrase-file`.
   - Cloud-KMS deployments are out of scope — those already provision all users on
     the server-KEK tier, so this runbook does not apply.
3. **You have the Postgres connection string** for the API database
   (`--db-url`), reachable from where you run the binary.
4. **No API changes ship between the migration and the cut.** Specifically, do not
   deploy a new user-provisioning change that re-introduces `dek_source='password'`
   rows. (The cut PR removes that path.)

## Pre-flight

```bash
# 1. Build the binary (any commit at or after PR #621):
go build -o migrate-passkey-dek ./cmd/migrate-passkey-dek/

# 2. Snapshot current state (audit):
psql "$DATABASE_URL" -c "SELECT id, email, dek_source FROM users ORDER BY email;"
# Expected: most rows show dek_source='password'. Users with no key material
# (never created a secret) show whatever the column default is AND have no
# user_keys row — the script handles them as "nothing to migrate" via GetUserKey
# returning nil.

# 3. Stage the password securely (never on the command line / shell history):
install -m 600 /dev/stdin /tmp/u1.pw <<< '<the-user-password>'   # or: cat > /tmp/u1.pw
# Alternatively: export PASSKEY_MIGRATE_PASSWORD='<password>'
```

## Migration (per user)

```bash
# ALWAYS dry-run first — confirms the unwrap succeeds without writing.
./migrate-passkey-dek \
  --db-url "$DATABASE_URL" \
  --user alice@example.com \
  --password-file /tmp/u1.pw \
  --sealed-key-file /sealed/root-key.bin --passphrase-file /sealed/passphrase \
  --dry-run
# → "DRY RUN — would flip dek_source "password"→"server_kek" and re-wrap N-byte DEK"

# Then for real (drop --dry-run):
./migrate-passkey-dek \
  --db-url "$DATABASE_URL" \
  --user alice@example.com \
  --password-file /tmp/u1.pw \
  --sealed-key-file /sealed/root-key.bin --passphrase-file /sealed/passphrase
# → "OK — dek_source is now "server_kek" for user <uuid>"
```

For the static-provider deployment, omit `--sealed-key-file`/`--passphrase-file`
and ensure the `LLMSAFESPACES_MASTER_SECRET(_FILE)` env vars are set in the shell.

Repeat for each user. For a user with **no key material** (never created a secret),
the script exits cleanly with "user ... has no key material (nothing to migrate)"
— they will be provisioned server-KEK on first secret creation after the cut.

## Verify

```bash
# Every row must show server_kek before the cut deploys:
psql "$DATABASE_URL" -c "SELECT email, dek_source FROM users;"
# Expected: all rows dek_source='server_kek' (or no user_keys row — handled later).
```

Then have the migrated user log in and confirm their personal secrets still decrypt
(API key reveal, provider credential test). The migration is lossless by
construction (the round-trip test `TestMigrateUser_RoundTripsDEK` proves the DEK
decrypts to the identical plaintext), but a live smoke-test is the final gate.

## Safety properties

| Scenario | Outcome |
|---|---|
| Wrong password | `UnwrapDEK` fails (AES-GCM auth-tag) → no commit → `dek_source='password'` unchanged. Retryable. |
| Crash mid-migration | The atomic `UpdateWrappedDEKAndSource` tx rolls back → row still password-wrapped → retry. |
| Already migrated | `dek_source` is already `server_kek` → skipped (idempotent). Safe to re-run. |
| Wrong user identifier | Email lookup returns no row → clean error, no write. |
| No master KEK configured | `buildRootKeyProvider` errors at startup before any DB access. |

## Rollback

The migration is **irreversible by design** — the down-path cannot reconstruct the
password-wrapped DEK once the plaintext is re-wrapped under the master KEK. If a
migration is botched in a way that corrupts the wrap (the script's fail-closed
guards prevent this, but defensively):

- **If the row still decrypts** (e.g., wrong key_version but intact ciphertext):
  re-run the script — it is idempotent on the current state and will re-wrap
  correctly once the password is supplied.
- **If the row is unrecoverable**: use the destructive reset path (§Fallback) for
  that user — fresh DEK, old secrets orphaned.

## Fallback (destructive reset — only if password is unavailable)

If a user's password cannot be obtained for the one-time unwrap, their existing
secrets are not recoverable (the whole point of the password tier). Reset to a
fresh server-KEK DEK:

- Repurpose the existing email password-reset flow (`POST /account/password-reset`
  → `password_reset.go:Confirm`), which after the cut calls
  `InitializeUserKeysServerKEK` (fresh DEK, orphans old ciphertext). The user
  re-enters their provider credentials / API keys.

For the ~2-user population, the active user is expected to be migratable (password
available); the no-secrets user is a no-op. Destructive reset is the documented
last resort, not the expected path.

## Post-migration (next PR)

Once all rows show `dek_source='server_kek'`, open the **deletion cut PR**:

1. Delete the password-tier code: `DeriveKEKFromPassword`, `WrapDEK`/`UnwrapDEK`,
   `GenerateRecoveryKey`, the password branch of `UnlockDEKWithSigningKey`,
   `InitializeUserKeys`, `ChangePassword`, `ResetWithRecoveryKey`,
   `RotateKeyWithPassword`, the `UpdateWrappedDEK*` KeyStore methods, the
   `/account/rotate-key`+`/account/change-password`+`/account/recover` endpoints.
2. Schema: backfill migration `UPDATE users SET dek_source='server_kek'` + tighten
   the CHECK (irreversible down).
3. Close the `GetDEK` server-KEK fallback gap (re-unwrap from `user_keys` via
   `rootKeyProvider.Decrypt` on cache+session miss) — required once the
   `/auth/unlock-dek` password endpoint is gone.
4. Docs: rewrite `docs/architecture/secrets.md`, `docs/operator/security.md`,
   `pkg/secrets/README.md` to describe the single server-KEK tier (the existing
   docs are already stale — they say HKDF where the code uses Argon2id).
5. This runbook + the `cmd/migrate-passkey-dek` binary can be deleted once the cut
   ships and the migration is confirmed stable.

## Files Modified

- `worklogs/0672_2026-08-02_dek-server-kek-migration-runbook.md` — this entry (added)

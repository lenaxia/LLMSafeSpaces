# Epic 58: SSO Users Get Server-KEK-Wrapped DEK by Default

**Status:** Design (not yet pickup-able — needs sign-off on the threat-model tradeoff below)
**Created:** 2026-07-12
**Origin:** Operator UX pain point — auto-provisioned SSO users hit a "set a password first" prompt before they can save personal secrets (LLM keys, SSH keys, env-secrets). The prompt is correct under today's two-tier model but is the single biggest SSO-onboarding friction.
**Depends On:** Epic 56 (durable DEK session — shipped; the `jwt_sessions` table is what makes a server-wrapped DEK usable without a password)
**Does NOT depend on:** Epic 57 (cloud KMS), Epic 51 (gVisor)

---

## Problem statement

Today, every personal secret in `user_secrets` is encrypted with a per-user DEK derived from the user's password via Argon2id (see `docs/operator/security.md` §"Per-user DEKs"). The platform genuinely cannot decrypt these without the password — that is the user-password tier documented in worklog 0616.

Auto-provisioned SSO users have no password. They get a random unusable bcrypt hash (`api/internal/services/sso/sso.go:633`), so `InitializeUserKeys` cannot derive a KEK and no `user_keys` row is created for them. Every personal-secret operation fails with a "set a password first" prompt until they pick one. The result:

- SSO users cannot save personal LLM credentials, SSH keys, or env-secrets without breaking the SSO-only flow they were provisioned into.
- Org-scoped workspaces still work (server-side injection uses the admin/org provider), so the prompt is "merely" a UX cost — but it is the first thing an SSO user hits when they try to do anything personal.
- The honest framing in `docs/operator/security.md:206` calls this out explicitly: "Personal credential operations stay unavailable until they set a password; org workspaces still work via server-side injection."

The user has asked for SSO users to get the server-KEK by default so this prompt disappears.

---

## The threat-model tradeoff — read this before approving

This is not a small code change. It inverts the two-tier model.

| Property | Today (password tier) | After this epic (SSO server-KEK tier) |
|---|---|---|
| What encrypts personal secrets | DEK derived from user's password (Argon2id, never on server) | DEK wrapped by the server KEK (same key that wraps API keys, SSO client secrets, admin/org LLM creds) |
| Platform can decrypt without user knowledge? | **No** — needs the password, which the platform never holds | **Yes** — the KEK is held by the platform (or its KMS); the platform can decrypt every personal secret of an SSO user without their involvement |
| Compromise of the master KEK exposes... | API keys, SSO client secrets, admin/org LLM credentials | **All of the above PLUS every personal secret of every SSO user** |
| Recovery story if user loses access | Re-derive DEK from password (or recovery key) | Re-unwrap DEK from `user_keys` with the server KEK — no user-side recovery primitive |

This is the same property API keys and SSO client secrets already have. The tradeoff is not new to the platform — it is new **for personal secrets of SSO users specifically**. Password users keep the stronger tier.

The decision this epic asks you to make: **is the UX win of removing the "set a password" prompt for SSO users worth downgrading their personal secrets from the user-password tier to the server-KEK tier?**

Reasonable people can answer either way:

- **Yes:** SSO users have already delegated their identity to the IdP. The platform trusts the IdP for authentication; trusting it (and itself) for personal-secret encryption is a small incremental step. The user-password tier is still available to SSO users who set a password; this epic adds a server-KEK default, it does not remove the stronger option.
- **No:** The two-tier model is documented and honest. Auto-provisioning SSO users into the weaker tier by default means most SSO users will never know they could have a stronger tier, and a master-KEK compromise will silently hit them harder than password users. The "set a password" prompt is the discoverability surface for the stronger tier; removing it removes the discoverability too.

The Epic 50 / 57 hardening arc (cloud KMS, audit logging of every decrypt) makes the server-KEK tier much stronger than it was when the two-tier model was documented. Under cloud KMS with audit logging, the practical risk of "platform silently decrypts an SSO user's personal secret" is bounded by the audit trail and the cloud-side key boundary. The threat-model regression is smaller under KMS than under static/sealed. **This epic should be paired with a recommendation (not a requirement) that SSO-server-KEK deployments use KMS, not local providers, for the master KEK.**

---

## Design: per-user DEK wrapped by server KEK, not by password-derived KEK

The change is small at the data-model layer and concentrated at boot.

### Data model

Two schema changes are needed (the `dek_source` enum and a nullable `Salt`; see Files). The existing `user_keys` table already has:

```
WrappedDEK         BYTEA   -- the DEK, wrapped by some KEK
WrappedDEKRecovery BYTEA   -- optional, nil for opt-out users
Salt               BYTEA   -- Argon2id salt for password-derived KEK
KeyVersion         INTEGER -- the wrapping key's version
```

Today `WrappedDEK` is wrapped by a KEK derived from `Salt` + the user's password. After this epic, for SSO-provisioned users, `WrappedDEK` is wrapped by the **`master-kek` purpose key** — the same `RootKeyProvider` that wraps API-key DEKs today (`api/internal/app/app.go:593`, the `apiKeyProv` path). `Salt` is `NULL` because no Argon2id derivation is involved — and because `Salt` is currently `NOT NULL` (`api/migrations/000001_initial_schema.up.sql:531`), dropping that constraint is the second schema change. `KeyVersion` tracks the master-KEK version.

The `WrappedDEK` / `KeyVersion` columns are reused as-is; the new state is expressed by `users.dek_source`, not by a column on `user_keys`. A new column or flag indicating "this `user_keys` row is server-KEK-wrapped, not password-wrapped" is needed so decrypt knows which path to take — it can live on the existing `users` table rather than adding a column to `user_keys`. Specifically:

- `users.dek_source` (new, ENUM: `'password'`, `'server_kek'`, default `'password'`). Set to `'server_kek'` for SSO-auto-provisioned users under this epic. Existing SSO users who set a password transition to `'password'`.

### Boot path

The password→KEK→DEK derivation today happens in `KeyService.UnlockDEKWithSigningKey` at login (`pkg/secrets/key_service.go:322` — `DeriveKEKFromPassword` + `UnwrapDEK`), **not** in `GetDEK`; `GetDEK` (`pkg/secrets/key_service.go:502`) only does Redis-cache lookup plus `jwt_sessions` durable rehydrate and never touches a password or Argon2id. The branch this epic adds: when `users.dek_source == 'server_kek'`, the login/provisioning path skips the Argon2id derivation and unwraps `WrappedDEK` via the master-KEK `RootKeyProvider` (the same provider used for `apiKeyProv`). The provider already exists, is already wrapped in `AuditedProvider` (US-50.12), and is already wired in `app.go`.

The change is one branch on the unlock/provisioning path plus the per-user flag. ~50 lines of code + tests.

### Provisioning path

In `sso.resolveUser`, when creating a new SSO user:

1. Generate a random 32-byte DEK (`crypto/rand`).
2. Wrap it with the master-KEK provider (`provider.Encrypt(ctx, dek)`).
3. `CreateUserKey` with the wrapped DEK, no salt, no recovery blob, `KeyVersion` from `ActiveVersionOf(masterKekProvider)`.
4. Set `users.dek_source = 'server_kek'`.

The user immediately has a working DEK and can save personal secrets. No "set a password" prompt.

### Password-transition path (existing SSO users + opt-up)

`KeyService.ChangePassword` (`pkg/secrets/key_service.go:805`) already re-wraps the DEK when the user sets a password (HTTP handler: `RotateKeyHandler.ChangePassword`, `api/internal/handlers/secrets.go:625` — there is no `auth.ChangePassword`). Add: after a successful `ChangePassword` from an SSO user (`dek_source == 'server_kek'`), flip `dek_source` to `'password'`. The transition is one-directional in the user's favor — they move to the stronger tier voluntarily.

The reverse transition (password → server_kek) is NOT added. Once a user has set a password they keep the stronger tier. This makes the stronger tier sticky and prevents an attacker who briefly controls the platform from silently downgrading password users.

### Recovery

Today: a user with `WrappedDEKRecovery` can recover their DEK from a recovery key. Under this epic, SSO-server-KEK users do not have a recovery blob — their DEK is recoverable from the master KEK, which is operator-controlled. This is consistent with how API-key DEKs work today. If the operator rotates the master KEK (per `helm/KEK-ROTATION.md`), server-KEK-wrapped user DEKs need to be re-wrapped too — **but the `rotate-kek` CLI does not currently walk `user_keys` at all.** `RotationCoordinator.RotateAll` (`pkg/secrets/rotation.go:183`) walks only `{"provider_credentials", "api_keys", "org_sso_configs"}`, and `cmd/rotate-kek/main.go:38` hard-codes the same `validTables` set (`{"all", "provider_credentials", "api_keys", "org_sso_configs"}`). Adding `user_keys` to the walk is **new work** for this epic, not a branch in an existing walk: a new table entry in `RotateAll`, a new `validTables` entry in the CLI, and a per-row `dek_source` branch so `'server_kek'` rows re-wrap with the master-KEK provider.

---

## Files (new + modified)

- `api/migrations/000046_users_dek_source.up.sql` (new) — add `dek_source` ENUM column.
- `api/migrations/000047_user_keys_salt_nullable.up.sql` (new) — drop `NOT NULL` on `user_keys.salt` (`api/migrations/000001_initial_schema.up.sql:531`) so server-KEK rows with no Argon2id derivation can have a NULL salt.
- `pkg/types/user.go` (modified) — add `DEKSource` field to the `User` struct.
- `api/internal/services/database/pg_user_store.go` (modified) — read/write `dek_source`.
- `api/internal/services/sso/sso.go` (modified) — `resolveUser` provisions a server-KEK-wrapped DEK.
- `api/internal/handlers/secrets.go` (modified) — `RotateKeyHandler.ChangePassword` (`api/internal/handlers/secrets.go:625`) is the HTTP handler for the password-change path. (There is no `auth.ChangePassword`; the `dek_source` flip lives on `KeyService.ChangePassword`, see below.)
- `pkg/secrets/key_service.go` (modified) — the unlock/provisioning path branches on `dek_source`; server-KEK path uses the master-KEK provider instead of Argon2id derivation. `KeyService.ChangePassword` (`pkg/secrets/key_service.go:805`) flips `dek_source` to `'password'` after re-wrap.
- `pkg/secrets/rotation.go` (modified) — add `user_keys` to `RotateAll`'s table list (it is absent today — `pkg/secrets/rotation.go:183`) and branch per-row on `dek_source`; server-KEK rows re-wrap with the master-KEK provider.
- `cmd/rotate-kek/main.go` (modified) — add `user_keys` to `validTables` (`cmd/rotate-kek/main.go:38`) and wire the master-KEK provider into the new walk.
- `docs/operator/security.md` (modified) — replace the "set a password first" sentence with the two-tier model under SSO + a recommendation that SSO-server-KEK deployments use KMS for the master KEK.
- `docs/architecture/secrets.md` (modified) — the mermaid diagram gains a `server-KEK DEK` path for SSO users.

~350 LoC of code change + ~12 tests. The user's "small code change" framing is wrong by an order of magnitude; this is a multi-file change touching the unlock/provisioning path, the rotation CLI (which does not yet walk `user_keys` — that walk is new work, see Recovery above), and two schema migrations, with a threat-model tradeoff that needs explicit operator sign-off.

---

## Open questions for sign-off

1. **Accept the threat-model tradeoff?** Personal secrets of SSO users move from "platform cannot decrypt" to "platform can decrypt, bounded by the KEK's protection (KMS + audit, if configured)." This is the central decision; without it, this epic does not start.

2. **Default for new SSO users.** Provision with `dek_source='server_kek'` automatically, or behind a Helm flag (`sso.serverKekDefault: true`, default false)? The flag gives existing deployments an opt-in path; auto-default makes the UX win the epic is for. Recommendation: flag, default true, with a clear release-note callout of the threat-model change.

3. **Existing SSO users.** Migrate them to `server_kek` automatically on next login (one-time backfill in the SSO callback), or leave them on the "set a password" path until they choose? Recommendation: backfill — the prompt is what we are removing.

4. **Operator-visible audit.** Today the decrypt audit log (`secret_audit_log`) records every decrypt on `apiKeyProv` / `orgCredsProv` / `providerCredsProv`. Personal secrets of SSO users under this epic would add `user_secrets` decrypts to the same log. Is that the right surface, or does it need a distinct action label to distinguish "platform decrypted an SSO user's personal secret" from "platform decrypted an API key"? Recommendation: distinct action label — the operator should be able to alert on the former independently.

5. **User notification/disclosure.** Existing SSO users' personal secrets move from "platform cannot decrypt" to "platform can decrypt" silently from their perspective. Should the platform notify/disclose this to affected SSO users (e.g. on first login post-epic, with a "your personal secrets are now recoverable by the platform" notice), or treat it as an operator-only decision documented in release notes? Recommendation: disclose on first login post-epic and in release notes — silent tier downgrade is the discoverability loss flagged in the threat-model section above.

---

## Acceptance criteria

1. An auto-provisioned SSO user can save, retrieve, and reveal a personal secret without ever setting a password. The "set a password first" prompt is gone.
2. The `users.dek_source` column is populated correctly for: (a) all password users (`'password'`), (b) all SSO users post-epic (`'server_kek'`), (c) SSO users who later set a password (`'password'`).
3. `rotate-kek` correctly re-wraps both password-derived and server-KEK-wrapped `user_keys` rows. A regression test covers both branches.
4. `KeyService.GetDEK` returns the same DEK for an SSO user before and after a master-KEK rotation (the rotation CLI walk hit their row).
5. The decrypt audit log distinguishes server-KEK personal-secret decrypts with a distinct action label.
6. `docs/operator/security.md` and `docs/architecture/secrets.md` describe the new tier accurately, including the threat-model tradeoff and the recommendation to pair this with KMS.
7. Existing password users are unaffected — no schema migration of their rows, no change to their decrypt path.

---

## Out of scope

| Item | Owner |
|---|---|
| Passkey-only login (no password path at all) | Epic 59 |
| Per-org KEK tier choice | Future — would let an org admin opt their org into "all members use password tier even under SSO." Not needed until an org actually asks for it. |
| Server-KEK as default for ALL users (not just SSO) | Rejected — password users get the stronger tier by default; this epic is specifically about the SSO onboarding friction. |

# Epic 59: Passkey-Only Login

**Status:** Design pass (deferred — needs UX and recovery story sign-off before pickup)
**Created:** 2026-07-12
**Origin:** User intent — "eventually remove password support and only support passkey/SSO." The user has been explicit this is a future state, not current priority. This document scopes what "remove password support" actually means.
**Depends On:** Epic 43 (org-scoped login — shipped), Epic 54 (org-scoped login domain routing — shipped), Epic 58 (SSO server-KEK default — designed in this session; **passkey users face the same DEK-source question as SSO users**)
**Does NOT depend on:** Epic 57 (cloud KMS), Epic 51 (gVisor)

---

## Problem statement

The user wants to eventually remove password login entirely. Two paths exist today for a user to authenticate:

1. **Password** — `POST /api/v1/auth/login` with email + password. Used to derive the DEK (via Argon2id) for the user-password encryption tier.
2. **SSO** — org-scoped OIDC, `GET /api/v1/auth/sso/:orgSlug/...`. Auto-provisioned SSO users have no password and (under Epic 58) get a server-KEK-wrapped DEK.

The user wants a third path — **passkeys** (WebAuthn / FIDO2) — to replace password login for users who are not in an SSO org. The Epic 54 design conversations explicitly deferred passkeys ("passkeys can wait") and noted there are zero passkey primitives in the codebase today.

This epic is the design pass for that future state. It is not pickup-able until Epic 58 is signed off, because passkey users face the same DEK-source question as SSO users — and answering it twice would lock in two different answers.

---

## Scope: what "passkey-only" actually means

"Remove password support" is ambiguous. Three interpretations, with very different scopes:

| Interpretation | Scope | Verdict |
|---|---|---|
| **(a) Passkey as an additional login option; password remains** | Add WebAuthn handlers + table; users with a passkey can use it OR password. | **Rejected by user intent** — does not "remove password support," just adds another path. |
| **(b) Passkey as the default for new users; existing passwords grandfathered; password removal is a later cleanup** | Add WebAuthn handlers + table + auto-enroll passkey on first login; existing users keep their password; eventually deprecate password endpoints. | **Pickup-able as a phased rollout.** This is the realistic shape. |
| **(c) Hard cutover: delete password endpoints, force-reset every user** | Migrate every user off password in one release. | **Rejected** — destructive, blocks on every user re-enrolling, loses the password-derived DEK tier for existing users with no migration path. |

This design proceeds with **(b)**: passkey becomes the primary non-SSO login path; password remains as a deprecated fallback during a long migration window; a future epic removes the password endpoints once telemetry shows zero usage.

---

## DEK-source question — the load-bearing decision

Passkey users, like SSO users, do not present a password. So their personal secrets cannot be encrypted with a password-derived DEK. Two options:

- **(i) Server-KEK-wrapped DEK** (same as Epic 58's SSO default). Passkey users get the same lower tier as SSO users. Personal secrets are platform-decryptable. Recovery is via the master KEK, not via a user-held secret.
- **(ii) DEK wrapped by the passkey itself.** WebAuthn lets the authenticator sign an arbitrary challenge; in principle the passkey could unwrap the DEK. In practice this is fragile: passkeys are non-exportable by design, so losing the authenticator loses the DEK forever unless a recovery path exists. The recovery path typically reduces to "another passkey + a recovery code," which is option (i) with extra steps.

Recommendation: **(i), same as Epic 58.** A passkey is an authentication factor, not an encryption-key source. Conflating the two produces a fragile system. Passkey users get a server-KEK-wrapped DEK, just like SSO users under Epic 58. This also makes the two non-password paths (SSO + passkey) consistent at the encryption layer.

This is why Epic 59 depends on Epic 58 sign-off. The DEK-source decision must be answered once for both epics.

---

## Data model

New table `user_passkeys`:

```sql
CREATE TABLE user_passkeys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         character varying(36) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id   bytea NOT NULL UNIQUE,        -- WebAuthn credential ID (authenticator-generated)
    public_key      bytea NOT NULL,                -- COSE-encoded public key
    sign_count      bigint NOT NULL DEFAULT 0,     -- cloned-authenticator detection
    transports      text[],                        -- usb, nfc, internal, etc.
    name            text,                          -- user-assigned label ("YubiKey 5C", "iPhone Face ID")
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_used_at    timestamptz,
    CONSTRAINT user_passkeys_user_credential UNIQUE (user_id, credential_id)
);
CREATE INDEX user_passkeys_credential_id_idx ON user_passkeys (credential_id);
```

Multiple passkeys per user are required for recovery (see below). One passkey per user is a footgun — losing it locks the user out.

`users.dek_source` (added by Epic 58 as the enum `('password', 'server_kek')`) gains a third enum value, `'passkey'`, via an Epic 59 migration — Epic 58 does **not** add `'passkey'`. Default for new passkey-enrolled users; transitions to `'passkey'` from `'password'` when an existing user enrolls a passkey and voluntarily disables their password.

`users.password_hash` becomes nullable under this epic (so a passkey-only user has `NULL`). Thebcrypt cost-12 hash stays for users who still have passwords; new passkey-only users have no hash and the login endpoint refuses password login for them.

---

## Authentication flow

WebAuthn has two phases: **ceremony** (registration) and **assertion** (login). Both are challenge-response; both require the server to persist challenges (short-lived, single-use) and verify the signed response against the registered public key.

### Registration (`POST /api/v1/auth/passkey/register/begin` + `/finish`)

- Authenticated (user is already logged in via password or SSO).
- Server generates a challenge, persists it (Redis with 5-minute TTL keyed by user ID), returns WebAuthn registration options.
- Client (browser) calls `navigator.credentials.create()`, returns the attestation.
- Server verifies attestation, extracts public key + credential ID, writes `user_passkeys` row.
- After first successful passkey registration, the user is offered the choice to disable password login. Disabling sets `password_hash = NULL` and `dek_source = 'passkey'` after re-wrapping the DEK under the server KEK (same transition path as Epic 58).

### Login (`POST /api/v1/auth/passkey/login/begin` + `/finish`)

- User enters email (or org-scoped subdomain routes them per Epic 54).
- Server looks up `user_passkeys` by user ID, generates a challenge, persists it, returns allowed credentials.
- Client calls `navigator.credentials.get()`, returns the assertion.
- Server verifies assertion (signature + sign-count check for cloned-authenticator detection), issues a JWT.

The login flow consumes the same `auth.Service.GenerateToken(userID)` path that SSO uses. The DEK retrieval path post-login is the same shape as Epic 58's server-KEK path: the unlock/provisioning flow (a passkey-time analogue of `KeyService.UnlockDEKWithSigningKey`, `pkg/secrets/key_service.go:322`) branches on `dek_source == 'passkey'` and unwraps `WrappedDEK` via the master-KEK provider rather than deriving a KEK from a password. `GetDEK` (`pkg/secrets/key_service.go:502`) only does Redis-cache lookup + `jwt_sessions` rehydrate — it never unwraps from a password, so the branch belongs on the unlock/provisioning path, not on `GetDEK`. Same code path as SSO; the `dek_source` value differs but the unwrap is identical.

---

## Recovery — the hard part

A passkey-only user who loses their authenticator has no password to fall back to. Three layers of recovery, all required:

1. **Multiple passkeys.** The UI must encourage users to enroll at least two passkeys (different authenticators). A user with two passkeys who loses one still has the other. The "name" field on `user_passkeys` exists to make this discoverable ("YubiKey 5C", "iPhone Face ID").

2. **Recovery codes.** On passkey enrollment, the server generates one-time-use recovery codes (10 codes, each 20 random characters). Each code is bcrypt-hashed and stored in a new `user_recovery_codes` table. A recovery code can be used once in place of a passkey to log in, after which the user must enroll a new passkey. This is the standard pattern (GitHub, Google, etc.).

3. **Operator-assisted recovery (last resort).** If the user loses both passkeys AND their recovery codes, only an operator can recover the account — by issuing a one-time reset token after out-of-band identity verification. This MUST reset the user's DEK (a new DEK is generated; the old `user_secrets` ciphertext is orphaned because nothing can decrypt it). This is the same recovery primitive that exists today for password users who lose both password and recovery key — it is documented as a destructive operation.

The recovery story is why passkey-only is harder than passkey-as-additional-option. Without (1), (2), and (3) the system is more fragile than today's password flow, not less.

---

## Migration story

Existing password users do not lose their password. The rollout is:

1. **Phase 1 (this epic):** Passkey endpoints land. Existing users can enroll passkeys; new users get prompted to enroll one at registration but can skip. `dek_source` stays `'password'` for users who don't enroll.
2. **Phase 2 (separate epic):** New users default to passkey-only (still can choose password during a transition window). `dek_source = 'passkey'` becomes the default for new registrations.
3. **Phase 3 (separate epic):** Password endpoints deprecated. Existing password users get a 90-day window to enroll a passkey or transition. After the window, password endpoints return 410 Gone. This is irreversible per user and requires operator-run tooling to handle users who missed the window.

Phase 1 is the only thing this epic covers. Phases 2 and 3 are documented here for traceability but live in future epics.

---

## Files (new + modified) — Phase 1 only

- `api/migrations/000047_user_passkeys.up.sql` (new) — `user_passkeys` table.
- `api/migrations/000048_user_recovery_codes.up.sql` (new) — `user_recovery_codes` table.
- `api/migrations/000049_users_password_hash_nullable.up.sql` (new) — drop `NOT NULL` on `users.password_hash`.
- `api/migrations/000050_users_dek_source_add_passkey.up.sql` (new) — extend the `dek_source` enum (defined by Epic 58 as `('password', 'server_kek')`) to add `'passkey'`. Epic 58 does not add this value; Epic 59 owns it.
- `pkg/types/passkey.go` (new) — DTOs for registration/login.
- `api/internal/services/passkey/passkey.go` (new) — wraps `github.com/go-webauthn/webauthn`. Challenge generation, attestation verification, assertion verification. ~300 LoC.
- `api/internal/handlers/passkey.go` (new) — registration/login HTTP handlers.
- `api/internal/server/router.go` (modified) — register passkey routes under `/api/v1/auth/passkey/...`.
- `api/internal/services/auth/auth.go` (modified) — `ChangePassword` and the passkey-disable-password path share a "transition DEK to server-KEK" helper.
- `frontend/src/pages/PasskeyEnrollPage.tsx` (new) — uses `@simplewebauthn/browser`.
- `frontend/src/pages/LoginPage.tsx` (modified) — passkey button alongside SSO + password (per Epic 54's contract).
- `docs/getting-started/concepts.md` (modified) — passkey concept.
- `docs/operator/security.md` (modified) — three-tier DEK-source model (password / server-KEK-via-SSO / server-KEK-via-passkey).

~600 LoC backend + ~400 LoC frontend + ~15 tests. This is a real project, not a single PR.

---

## Open questions for sign-off

1. **DEK source for passkey users.** Server-KEK-wrapped (same as Epic 58 SSO default) — accept, or argue for the passkey-as-encryption-key option? Recommendation: server-KEK. See "DEK-source question" above.

2. **Recovery-code UX.** Auto-generate on first passkey enrollment (force the user through the recovery-code flow), or make recovery codes opt-in? Recommendation: auto-generate, force-display once, cannot-dismiss-without-acknowledging. Users who skip recovery codes are the ones who need them most.

3. **Sign-count enforcement.** WebAuthn sign-count is the cloned-authenticator detection primitive. Strict enforcement (refuse login on count regression) breaks some authenticators (U2F-only tokens, software authenticators). Lenient enforcement (log regression but allow login) gives up the detection value. Recommendation: lenient by default, strict behind a per-user flag for high-security orgs.

4. **Operator-assisted recovery.** The "operator resets the DEK" path is destructive (user loses all personal secrets). Is that acceptable, or do we need a non-destructive operator recovery (which would require storing a backup of the DEK under operator-held key, defeating the threat-model benefit)? Recommendation: destructive. Document clearly. Personal secrets are user-owned; if the user loses every factor they held, the secrets are lost. This matches today's password-recovery story.

5. **Cross-device flow.** Passkey login from a device that doesn't have the credential (e.g. new laptop) requires either a hybrid transport (QR + phone) or a recovery code. Both are browser/authenticator features, not ours — but the UX must guide users through the choice. Recommendation: detect failure and surface a "use recovery code or another device" UI.

---

## Acceptance criteria (Phase 1)

1. An existing user can enroll a passkey via the UI; the passkey appears in `user_passkeys` and works for the next login.
2. A passkey-enrolled user can disable password login; `password_hash` becomes NULL, `dek_source` becomes `'passkey'`, the DEK is re-wrapped under the server KEK, and subsequent password login attempts return 401.
3. A passkey-only user can save, retrieve, and reveal personal secrets — same UX as a password user, same backend path as an SSO-server-KEK user.
4. Recovery codes are generated at enrollment, displayed once with a "save these" warning, and a recovery code can be used to log in once (consumed) and force re-enrollment of a new passkey.
5. The login page shows passkey as an option alongside password and SSO.
6. Sign-count regression is logged at Warn but does not block login.
7. `docs/operator/security.md` describes the three DEK-source tiers accurately.

---

## Out of scope (Phase 1)

| Item | Owner |
|---|---|
| Phase 2 (new users default to passkey-only) | Future epic — depends on Phase 1 telemetry. |
| Phase 3 (password endpoints removed) | Future epic — 90-day migration window from Phase 2. |
| Per-org passkey policy (require passkey for org members) | Future — org policy extension. |
| Hardware-attested passkeys only (requiring attestation from a specific vendor) | Future — threat-model decision per deployment. |
| Magic-link / email-only login | Rejected in Epic 54 (D54-1): "Email is for invitations + notifications only, never auth." |

---

## Sequencing

```
Epic 58 (SSO server-KEK default) ─── must land first; resolves the DEK-source question.
                                          │
Epic 59 Phase 1 (passkey enrollment) ─── consumes Epic 58's dek_source='passkey' branch.
                                          │
Future: Epic 59 Phase 2 (new-user default)
                                          │
Future: Epic 59 Phase 3 (password removal)
```

Epic 59 cannot start until Epic 58 is signed off. The two epics answer the same encryption-layer question; doing Epic 59 first would force an answer that might conflict with Epic 58's resolution.

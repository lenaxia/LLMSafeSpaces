BEGIN;

-- Epic 58: per-user DEK source. Semantic companion to password_hash: which
-- encryption tier this user's personal secrets live in.
--   'password'   DEK wrapped by a KEK derived from the user's password (Argon2id).
--                The platform genuinely cannot decrypt without the password.
--   'server_kek' DEK wrapped by the master-KEK RootKeyProvider (same provider as
--                api_keys). Used by SSO auto-provisioned users (this epic) and,
--                in Epic 59, passkey-only users.
--
-- text + CHECK (not a PG ENUM type) matches the existing users.status pattern
-- (000001_initial_schema.up.sql:595) and is simpler to extend — Epic 59 adds the
-- 'passkey' value by widening this CHECK constraint.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS dek_source text NOT NULL DEFAULT 'password';

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_dek_source_check;

ALTER TABLE users
    ADD CONSTRAINT users_dek_source_check
    CHECK (dek_source = ANY (ARRAY['password'::text, 'server_kek'::text]));

COMMIT;

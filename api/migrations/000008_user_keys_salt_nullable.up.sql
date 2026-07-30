BEGIN;

-- Epic 58: allow user_keys.salt to be NULL. A password-tier user's DEK is wrapped
-- by a KEK derived via Argon2id(password, salt) — salt is required. A server-KEK
-- user (SSO auto-provisioned under this epic; passkey-only under Epic 59) has no
-- Argon2id derivation — their DEK is wrapped directly by the master-KEK
-- RootKeyProvider — so their salt is NULL. The 000001 schema declared salt
-- NOT NULL (000001_initial_schema.up.sql:531); this drops that constraint.
ALTER TABLE user_keys ALTER COLUMN salt DROP NOT NULL;

COMMIT;

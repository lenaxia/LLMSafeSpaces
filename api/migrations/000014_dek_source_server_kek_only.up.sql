-- Collapse the DEK tier model to server-KEK-only.
-- All existing password-tier and passkey-tier users are backfilled to
-- 'server_kek'. The CHECK constraint is tightened to accept only
-- 'server_kek'. The column default is updated so future INSERTs that
-- omit dek_source (e.g. test fixtures, legacy code paths) get the
-- correct value. This migration is IRREVERSIBLE — the down migration
-- cannot reconstruct password-wrapped DEKs (the plaintext was
-- re-wrapped under the master KEK by the migrate-passkey-dek tool).

UPDATE users SET dek_source = 'server_kek' WHERE dek_source IN ('password', 'passkey');

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_dek_source_check;
ALTER TABLE users ADD CONSTRAINT users_dek_source_check CHECK (dek_source = 'server_kek');
ALTER TABLE users ALTER COLUMN dek_source SET DEFAULT 'server_kek';

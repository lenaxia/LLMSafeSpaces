-- Collapse the DEK tier model to server-KEK-only.
-- All existing password-tier and passkey-tier users are backfilled to
-- 'server_kek'. The CHECK constraint is tightened to accept only
-- 'server_kek'. This migration is IRREVERSIBLE — the down migration
-- cannot reconstruct password-wrapped DEKs (the plaintext was
-- re-wrapped under the master KEK by the migrate-passkey-dek tool).

UPDATE users SET dek_source = 'server_kek' WHERE dek_source IN ('password', 'passkey');

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_dek_source_check;
ALTER TABLE users ADD CONSTRAINT users_dek_source_check CHECK (dek_source = 'server_kek');

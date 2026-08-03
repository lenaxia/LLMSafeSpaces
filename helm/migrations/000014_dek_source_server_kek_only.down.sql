-- Irreversible: cannot reconstruct password-wrapped DEKs once the
-- plaintext has been re-wrapped under the master KEK. This down
-- migration only restores the CHECK constraint to allow any value
-- (for schema rollback without data reconstruction).

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_dek_source_check;
ALTER TABLE users ADD CONSTRAINT users_dek_source_check CHECK (dek_source IN ('password', 'server_kek', 'passkey'));

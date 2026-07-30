BEGIN;

-- Epic 59: add the 'passkey' value to the users.dek_source enum set. A
-- passkey-only user's DEK is wrapped by the master-KEK provider (same machinery
-- as 'server_kek' from Epic 58) — the value distinguishes the auth source for
-- audit/telemetry, not a different encryption tier. The unwrap code treats
-- 'passkey' identically to 'server_kek'.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_dek_source_check;

ALTER TABLE users
    ADD CONSTRAINT users_dek_source_check
    CHECK (dek_source = ANY (ARRAY['password'::text, 'server_kek'::text, 'passkey'::text]));

COMMIT;

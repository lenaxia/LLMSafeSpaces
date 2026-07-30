BEGIN;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_dek_source_check;

ALTER TABLE users
    ADD CONSTRAINT users_dek_source_check
    CHECK (dek_source = ANY (ARRAY['password'::text, 'server_kek'::text]));

COMMIT;

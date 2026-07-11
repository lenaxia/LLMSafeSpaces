BEGIN;

DROP INDEX IF EXISTS idx_user_secrets_global_default;
ALTER TABLE user_secrets DROP COLUMN IF EXISTS global_default;

COMMIT;

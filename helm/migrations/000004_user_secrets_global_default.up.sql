BEGIN;

ALTER TABLE user_secrets
    ADD COLUMN IF NOT EXISTS global_default boolean NOT NULL DEFAULT false;

-- Partial index for efficient lookup of all global-default secrets per user.
-- Used when seeding bindings on workspace creation.
CREATE INDEX IF NOT EXISTS idx_user_secrets_global_default
    ON user_secrets (user_id)
    WHERE global_default = true;

COMMIT;

BEGIN;

-- Re-impose NOT NULL. Only safe to roll back when no server-KEK rows exist
-- (every user_keys.salt must be non-NULL). Operators rolling this back after
-- server-KEK users exist must first re-derive salts or accept data loss for
-- those users' personal secrets.
ALTER TABLE user_keys ALTER COLUMN salt SET NOT NULL;

COMMIT;

-- #1812 down: drop the inline gate script columns. Rows that used the
-- gate degrade to prompt-only routine triggers (the legacy shape).

BEGIN;

ALTER TABLE triggers DROP COLUMN IF EXISTS script_language;
ALTER TABLE triggers DROP COLUMN IF EXISTS script_handler;

COMMIT;

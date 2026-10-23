-- 000044 down: drop the #1882 durable death mark. Write-side
-- bookkeeping only — the effects tail normally hard-deletes the row.

BEGIN;

ALTER TABLE session_index
    DROP COLUMN IF EXISTS deleted_at;

COMMIT;

-- 000048 down: drop the #1755 cascade marker. Write-side bookkeeping
-- only — the archived column (000035) is the state that survives.

BEGIN;

ALTER TABLE session_index
    DROP COLUMN IF EXISTS archived_by_cascade;

COMMIT;

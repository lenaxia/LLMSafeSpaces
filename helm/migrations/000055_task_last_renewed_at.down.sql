-- 000055_task_last_renewed_at (down) — drop the renewal clock. The
-- column is telemetry + the lapse arm's ancillary record (the arm
-- itself reads last_progress_at), so a downgrade loses the split's
-- observability and reverts to the ambiguous single-clock world but
-- breaks no constraint and no row shape. last_progress_at is
-- untouched in both directions (it is the pre-existing column).

BEGIN;

ALTER TABLE tasks DROP COLUMN IF EXISTS last_renewed_at;

COMMIT;

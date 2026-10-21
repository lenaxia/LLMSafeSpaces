-- 000040 down: drop the failed-state attribute overlay (#1813).

BEGIN;

ALTER TABLE tasks
    DROP COLUMN IF EXISTS last_attempt_failed,
    DROP COLUMN IF EXISTS failure_count,
    DROP COLUMN IF EXISTS last_failure_at;

COMMIT;

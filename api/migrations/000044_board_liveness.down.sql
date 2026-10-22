-- 000044 down: drop platform-derived liveness storage (#1847).

BEGIN;

DROP TABLE IF EXISTS board_session_liveness;

ALTER TABLE tasks
    DROP COLUMN IF EXISTS appeal_until,
    DROP COLUMN IF EXISTS lapse_reason;

COMMIT;

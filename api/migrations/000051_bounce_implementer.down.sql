-- 000051_bounce_implementer (down) — drop the bounce-consumer identity
-- column. The forward-exit capture and the bounce's implementer
-- resolution revert with it (the pre-#1959 guard reads
-- assignee_session again).

BEGIN;

ALTER TABLE tasks
    DROP COLUMN IF EXISTS implementer_session;

COMMIT;

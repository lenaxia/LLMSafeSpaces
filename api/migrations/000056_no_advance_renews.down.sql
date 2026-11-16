-- 000056_no_advance_renews down — reverse the heartbeat-orbit budget column.

ALTER TABLE tasks DROP COLUMN IF EXISTS no_advance_renews;

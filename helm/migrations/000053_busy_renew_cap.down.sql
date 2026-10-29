-- 000053_busy_renew_cap down — reverse the busy-gate bound column.

ALTER TABLE tasks DROP COLUMN IF EXISTS busy_renews;

-- 000054_depose_salvage (down) — drop the salvage pointer slot. The
-- depose audit events (board_events) are history rows and stay.

ALTER TABLE tasks DROP COLUMN IF EXISTS salvage;

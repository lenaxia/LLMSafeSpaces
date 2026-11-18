-- 000061_board_events_tombstone_indexes — down: drop the #2107
-- tombstone anchor's partial indexes (the gone-row read degrades to
-- the honest 404 without them; no data shape changes either way).

DROP INDEX IF EXISTS public.idx_board_events_task_short_id;
DROP INDEX IF EXISTS public.idx_board_events_task_item;

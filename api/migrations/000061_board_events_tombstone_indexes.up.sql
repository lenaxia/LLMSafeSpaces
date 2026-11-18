-- 000061_board_events_tombstone_indexes — the #2107 gone-row audit
-- trail's index floor. The tombstone read anchors a deleted task's
-- timeline through board_events; without these, every anchor probe
-- (including the honest-404 fork — each typo on the events route)
-- sequential-scans the append-only audit table, which grows a row per
-- heartbeat renewal and every per-call audit write. Two partial
-- indexes serve the two anchor arms:
--   * item_id, for UUID-shaped input (the system's internal address)
--   * lower(payload->>'short_id'), the expression index matching the
--     short-id arm's predicate exactly (phrase ids resolve
--     case-insensitively)
-- Both restricted to item_kind='task' AND item_id IS NOT NULL — the
-- anchor never looks at any other shape.

CREATE INDEX IF NOT EXISTS idx_board_events_task_item
    ON public.board_events (item_id)
    WHERE item_kind = 'task' AND item_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_board_events_task_short_id
    ON public.board_events (lower(payload->>'short_id'))
    WHERE item_kind = 'task' AND item_id IS NOT NULL
      AND payload -> 'short_id' IS NOT NULL;

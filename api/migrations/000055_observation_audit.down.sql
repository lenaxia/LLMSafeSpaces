-- 000055_observation_audit (down) — drop the observation plane's
-- audit trail. The board_events ledger (nudge/wedge/reclaim/depose
-- actions) is untouched; only the prunable recent window goes away.

DROP TABLE IF EXISTS task_observation_events;

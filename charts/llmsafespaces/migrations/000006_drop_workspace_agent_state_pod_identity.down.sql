BEGIN;

-- Restore migration 000005's columns for down-migration testing.
-- The columns are re-added nullable so a rollback doesn't require
-- backfilling data (which was itself proxy state, not source of truth).
ALTER TABLE workspace_agent_state
    ADD COLUMN IF NOT EXISTS last_seen_pod_name text,
    ADD COLUMN IF NOT EXISTS last_seen_pod_start_time timestamp with time zone;

COMMIT;

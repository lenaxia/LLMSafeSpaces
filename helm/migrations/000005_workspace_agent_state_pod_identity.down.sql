BEGIN;

ALTER TABLE workspace_agent_state
    DROP COLUMN IF EXISTS last_seen_pod_start_time,
    DROP COLUMN IF EXISTS last_seen_pod_name;

COMMIT;

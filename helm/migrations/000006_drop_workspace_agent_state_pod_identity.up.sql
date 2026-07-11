BEGIN;

-- Worklog 0591: replace the pod-identity detection primitive from
-- worklog 0589 (migration 000005) with a watcher-driven, agentd-signal-
-- based auto-push. The last_seen_pod_name + last_seen_pod_start_time
-- columns on workspace_agent_state were a proxy for "did the pod
-- recreate since we last observed it"; the new design uses agentd's
-- own userCredsPresent signal (surfaced via the CRD), which is the
-- direct source of truth for "does the pod have user-DEK content."
--
-- Data in these columns was only populated between v87 (#494 deploy)
-- and this migration. Dropping is safe: no consumers remain after
-- PodIdentityTracker removal.

ALTER TABLE workspace_agent_state
    DROP COLUMN IF EXISTS last_seen_pod_start_time,
    DROP COLUMN IF EXISTS last_seen_pod_name;

COMMIT;

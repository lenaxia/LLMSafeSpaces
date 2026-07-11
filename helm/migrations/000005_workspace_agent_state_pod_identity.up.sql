BEGIN;

-- Add pod-identity tracking columns to workspace_agent_state so the API
-- can detect pod recreations and trigger an auto-push of user-DEK secrets
-- (env-secrets, SSH keys, user-owned LLM providers) that phase-1 boot
-- cannot deliver because it lacks the user's DEK.
--
-- (pod_name, pod_start_time) is the identity tuple: both change on every
-- pod recreation. Nullable — a NULL/NULL pair means "no observation yet"
-- and the API treats the next status read as the initial observation
-- (no auto-push fires; the tuple is recorded so subsequent transitions
-- can be detected). See worklog 0589 for the design rationale.
ALTER TABLE workspace_agent_state
    ADD COLUMN IF NOT EXISTS last_seen_pod_name text,
    ADD COLUMN IF NOT EXISTS last_seen_pod_start_time timestamp with time zone;

COMMIT;

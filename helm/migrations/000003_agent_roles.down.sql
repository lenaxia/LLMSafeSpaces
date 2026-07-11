-- Epic: Agent Customization, US-2.1: Rollback agent roles table.

BEGIN;

ALTER TABLE workspace_prompts DROP COLUMN IF EXISTS agent_role_id;

DROP TABLE IF EXISTS agent_roles;

COMMIT;

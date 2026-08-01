-- Epic 53 rollback: drops the three MCP tables and restores the org_policies CHECK
-- to its pre-000012 state (the 6-key set established by migration 000002).

BEGIN;

DROP TABLE IF EXISTS public.mcp_server_auto_apply;
DROP TABLE IF EXISTS public.mcp_server_bindings;
DROP TABLE IF EXISTS public.mcp_servers;

ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_check;
ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_chk;
DO $$ BEGIN
    ALTER TABLE org_policies ADD CONSTRAINT org_policies_key_chk CHECK (key IN (
        'allowed_models',
        'allowed_providers',
        'max_workspaces_per_member',
        'max_active_workspaces_per_member',
        'sys_prompt_org',
        'allow_user_prompt'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

-- Epic 64 rollback: drops the seven trigger/workflow tables and restores the
-- org_policies CHECK to its pre-000016 state (the 8-key set established by 000012).

BEGIN;

DROP TABLE IF EXISTS public.trigger_fires;
DROP TABLE IF EXISTS public.workflow_node_runs;
DROP TABLE IF EXISTS public.workflow_runs;
DROP TABLE IF EXISTS public.webhook_deliveries;
DROP TABLE IF EXISTS public.webhooks;
DROP TABLE IF EXISTS public.triggers;
DROP TABLE IF EXISTS public.workflows;

ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_check;
ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_chk;
DO $$ BEGIN
    ALTER TABLE org_policies ADD CONSTRAINT org_policies_key_chk CHECK (key IN (
        'allowed_models',
        'allowed_providers',
        'max_workspaces_per_member',
        'max_active_workspaces_per_member',
        'sys_prompt_org',
        'allow_user_prompt',
        'allow_user_mcp_servers',
        'max_mcp_servers_per_workspace'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

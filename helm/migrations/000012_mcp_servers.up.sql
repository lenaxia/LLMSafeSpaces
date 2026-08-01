-- Epic 53: MCP Server Integration (US-53.2 + US-53.11 policy keys).
--
-- Creates three tables that mirror the proven Epic 30 credential shape:
--   1. mcp_servers           — registered external MCP servers (platform/org/user scope)
--   2. mcp_server_bindings   — pure join workspace ↔ server (additive, no precedence — D4)
--   3. mcp_server_auto_apply — auto-bind rules (target all/org/user), mirrors credential_auto_apply
--
-- And extends org_policies CHECK with two governance keys:
--   - allow_user_mcp_servers        (bool)  org-admin toggle gating member MCP servers (mirrors
--                                                allow_user_prompt; default locked)
--   - max_mcp_servers_per_workspace (int)   per-workspace MCP server quota (default 5)
--
-- Secret portions (env + headers maps) live ONLY in the encrypted ciphertext blob; display fields
-- (name, transport, url, command, args, enabled) are plaintext so listing never decrypts.

BEGIN;

-- 1. mcp_servers ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.mcp_servers (
    id           uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_type   text NOT NULL,
    owner_id     text NOT NULL,
    name         text NOT NULL,
    transport    text NOT NULL,
    url          text,
    command      text,
    args         jsonb DEFAULT '[]'::jsonb NOT NULL,
    timeout_ms   integer,
    ciphertext   bytea NOT NULL,
    key_version  integer DEFAULT 1 NOT NULL,
    enabled      boolean DEFAULT true NOT NULL,
    created_at   timestamp with time zone DEFAULT now() NOT NULL,
    updated_at   timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mcp_servers_owner_type_check CHECK ((owner_type = ANY (ARRAY['user'::text, 'org'::text, 'admin'::text]))),
    CONSTRAINT mcp_servers_transport_check CHECK ((transport = ANY (ARRAY['http'::text, 'sse'::text, 'stdio'::text]))),
    -- remote transports require a url; stdio requires a command.
    CONSTRAINT mcp_servers_endpoint_check CHECK (
        ((transport = ANY (ARRAY['http'::text, 'sse'::text])) AND url IS NOT NULL)
        OR (transport = 'stdio'::text AND command IS NOT NULL)
    ),
    CONSTRAINT mcp_servers_name_check CHECK ((name ~ '^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$'::text))
);

ALTER TABLE ONLY public.mcp_servers
    ADD CONSTRAINT mcp_servers_pkey PRIMARY KEY (id);

-- Per-owner uniqueness on name (name is also the opencode mcp config key).
ALTER TABLE ONLY public.mcp_servers
    ADD CONSTRAINT mcp_servers_owner_name_uniq UNIQUE (owner_type, owner_id, name);

CREATE INDEX IF NOT EXISTS idx_mcp_servers_owner ON public.mcp_servers USING btree (owner_type, owner_id);

DROP TRIGGER IF EXISTS trg_mcp_servers_updated_at ON public.mcp_servers;
CREATE TRIGGER trg_mcp_servers_updated_at BEFORE UPDATE ON public.mcp_servers
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 2. mcp_server_bindings ------------------------------------------------------
-- Pure join table. Multiple MCP servers are all injected (additive composition), so there is NO
-- within_priority column — unlike workspace_credential_bindings (D4).
CREATE TABLE IF NOT EXISTS public.mcp_server_bindings (
    workspace_id uuid NOT NULL,
    server_id    uuid NOT NULL,
    source_type  text DEFAULT 'explicit'::text NOT NULL,
    created_at   timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mcp_server_bindings_source_type_check CHECK ((source_type = ANY (ARRAY['explicit'::text, 'auto'::text])))
);

ALTER TABLE ONLY public.mcp_server_bindings
    ADD CONSTRAINT mcp_server_bindings_pkey PRIMARY KEY (workspace_id, server_id);

ALTER TABLE ONLY public.mcp_server_bindings
    ADD CONSTRAINT mcp_server_bindings_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.mcp_server_bindings
    ADD CONSTRAINT mcp_server_bindings_server_id_fkey FOREIGN KEY (server_id) REFERENCES public.mcp_servers(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_mcp_server_bindings_workspace ON public.mcp_server_bindings USING btree (workspace_id);

-- 3. mcp_server_auto_apply ----------------------------------------------------
-- Mirrors credential_auto_apply: target_type 'all' (platform) carries NULL target_id; 'org'/'user'
-- carry the concrete id. Two partial UNIQUE indexes preserve referential integrity for the NULL
-- case (Postgres treats NULLs as distinct in a plain UNIQUE constraint).
CREATE TABLE IF NOT EXISTS public.mcp_server_auto_apply (
    server_id   uuid NOT NULL,
    target_type text NOT NULL,
    target_id   text,
    created_at  timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mcp_server_auto_apply_target_type_check CHECK ((target_type = ANY (ARRAY['all'::text, 'org'::text, 'user'::text])))
);

ALTER TABLE ONLY public.mcp_server_auto_apply
    ADD CONSTRAINT mcp_server_auto_apply_server_id_fkey FOREIGN KEY (server_id) REFERENCES public.mcp_servers(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_mcp_auto_apply_all ON public.mcp_server_auto_apply USING btree (target_type) WHERE (target_type = 'all'::text);
CREATE INDEX IF NOT EXISTS idx_mcp_auto_apply_org ON public.mcp_server_auto_apply USING btree (target_id) WHERE (target_type = 'org'::text);
CREATE INDEX IF NOT EXISTS idx_mcp_auto_apply_user ON public.mcp_server_auto_apply USING btree (target_id) WHERE (target_type = 'user'::text);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_auto_apply_unique_all ON public.mcp_server_auto_apply USING btree (server_id, target_type) WHERE (target_id IS NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_auto_apply_unique_targeted ON public.mcp_server_auto_apply USING btree (server_id, target_type, target_id) WHERE (target_id IS NOT NULL);

-- 4. org_policies CHECK extension (US-53.11 governance keys) ------------------
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

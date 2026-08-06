-- Epic 64: Triggers & Workflows (US-64.2 data model).
--
-- Creates seven tables that enable deterministic DAG-structured pipelines running
-- inside workspace pods, fired by cron/webhook triggers:
--
--   1. workflows             — workflow definitions (owner_type user|org, spec, input schema)
--   2. triggers              — cron/webhook sources that fire workflows or scripts
--   3. webhooks              — HMAC-signed inbound receiver config (one per webhook trigger)
--   4. webhook_deliveries    — idempotency dedup (mirrors stripe_events shape)
--   5. workflow_runs         — execution state machine (queued|running|succeeded|failed|canceled|timed_out)
--   6. workflow_node_runs    — per-node status/input/output/error (written after each transition)
--   7. trigger_fires         — fire audit + action result (fired|delivered|failed|skipped|auto_disabled)
--
-- Key invariants enforced at the DB layer:
--   - Single-in-flight-per-workflow (D8): partial UNIQUE index uq_workflow_run_single_inflight
--     atomically rejects a second queued/running row for the same workflow_id. No check-then-insert
--     race (the TOCTOU class Epic 23 hardened against).
--   - Idempotency on webhooks: webhook_deliveries(webhook_id, dedup_key) UNIQUE — duplicate
--     deliveries are 200 "duplicate" instead of double-fires (copy the Stripe pattern).
--   - Per-(owner) UNIQUE on workflow/trigger names so list/slug lookups are deterministic.
--
-- And extends org_policies CHECK with one governance key:
--   - allow_user_workflow_create (bool)  org-admin toggle gating user-scope workflow/trigger
--                                               authoring (mirrors allow_user_mcp_servers).
--
-- No CRD changes — workflows are API-owned relational data (per design D6 + CRD-type-ownership rule).

BEGIN;

-- 1. workflows -----------------------------------------------------------------
-- A workflow is a deterministic DAG of script/agent/http/condition nodes that runs
-- inside a target workspace pod. owner_type is user|org (admin/platform tier is v2).
-- spec_yaml is the author's canonical input; spec_json is the parsed/validated DAG
-- denormalized for execution. input_schema is a JSON Schema validating run input.
CREATE TABLE IF NOT EXISTS public.workflows (
    id                  uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_type          text NOT NULL,
    owner_id            text NOT NULL,
    name                text NOT NULL,
    slug                text NOT NULL,
    description         text DEFAULT ''::text NOT NULL,
    spec_yaml           text NOT NULL,
    spec_json           jsonb NOT NULL,
    input_schema        jsonb,
    target_workspace_id uuid,
    status              text DEFAULT 'draft'::text NOT NULL,
    defaults            jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at          timestamp with time zone DEFAULT now() NOT NULL,
    updated_at          timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workflows_owner_type_check CHECK ((owner_type = ANY (ARRAY['user'::text, 'org'::text]))),
    CONSTRAINT workflows_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'archived'::text]))),
    CONSTRAINT workflows_name_check CHECK ((name ~ '^[a-zA-Z0-9][a-zA-Z0-9 _-]{0,127}$'::text)),
    CONSTRAINT workflows_slug_check CHECK ((slug ~ '^[a-z0-9][a-z0-9-]{0,127}$'::text))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflows_pkey') THEN
        ALTER TABLE public.workflows ADD CONSTRAINT workflows_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflows_owner_name_uniq') THEN
        ALTER TABLE public.workflows ADD CONSTRAINT workflows_owner_name_uniq UNIQUE (owner_type, owner_id, name);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflows_owner_slug_uniq') THEN
        ALTER TABLE public.workflows ADD CONSTRAINT workflows_owner_slug_uniq UNIQUE (owner_type, owner_id, slug);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflows_target_workspace_id_fkey') THEN
        ALTER TABLE public.workflows ADD CONSTRAINT workflows_target_workspace_id_fkey FOREIGN KEY (target_workspace_id) REFERENCES public.workspaces(id) ON DELETE SET NULL;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_workflows_owner ON public.workflows USING btree (owner_type, owner_id);
CREATE INDEX IF NOT EXISTS idx_workflows_status ON public.workflows USING btree (status) WHERE (status = 'active'::text);

DROP TRIGGER IF EXISTS trg_workflows_updated_at ON public.workflows;
CREATE TRIGGER trg_workflows_updated_at BEFORE UPDATE ON public.workflows
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 2. triggers ------------------------------------------------------------------
-- A trigger maps a source (cron | webhook) to an action (run_workflow | run_script).
-- source_config and target_config are typed JSON blobs validated at the API layer.
-- Manual runs do NOT use a trigger row — they go through POST /workflows/:id/runs
-- directly with trigger_id = null on the run (design D5: dropped 'manual' source type).
-- Circuit breaker: consecutive_failures auto-increments on terminal-failed runs and
-- resets on first success; when >= auto_disable_after the scheduler sets enabled=false.
CREATE TABLE IF NOT EXISTS public.triggers (
    id                       uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_type               text NOT NULL,
    owner_id                 text NOT NULL,
    name                     text NOT NULL,
    description              text DEFAULT ''::text NOT NULL,
    enabled                  boolean DEFAULT true NOT NULL,
    source_type              text NOT NULL,
    source_config            jsonb DEFAULT '{}'::jsonb NOT NULL,
    target_type              text NOT NULL,
    target_config            jsonb DEFAULT '{}'::jsonb NOT NULL,
    consecutive_failures     integer DEFAULT 0 NOT NULL,
    auto_disable_after       integer DEFAULT 10 NOT NULL,
    last_fired_at            timestamp with time zone,
    next_fire_at             timestamp with time zone,
    created_at               timestamp with time zone DEFAULT now() NOT NULL,
    updated_at               timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT triggers_owner_type_check CHECK ((owner_type = ANY (ARRAY['user'::text, 'org'::text]))),
    CONSTRAINT triggers_source_type_check CHECK ((source_type = ANY (ARRAY['cron'::text, 'webhook'::text]))),
    CONSTRAINT triggers_target_type_check CHECK ((target_type = ANY (ARRAY['run_workflow'::text, 'run_script'::text]))),
    CONSTRAINT triggers_auto_disable_after_check CHECK ((auto_disable_after >= 1)),
    CONSTRAINT triggers_consecutive_failures_check CHECK ((consecutive_failures >= 0)),
    CONSTRAINT triggers_name_check CHECK ((name ~ '^[a-zA-Z0-9][a-zA-Z0-9 _-]{0,127}$'::text))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'triggers_pkey') THEN
        ALTER TABLE public.triggers ADD CONSTRAINT triggers_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'triggers_owner_name_uniq') THEN
        ALTER TABLE public.triggers ADD CONSTRAINT triggers_owner_name_uniq UNIQUE (owner_type, owner_id, name);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_triggers_owner ON public.triggers USING btree (owner_type, owner_id);
-- Scheduler's due-fire lookup (cron only, enabled, due now).
CREATE INDEX IF NOT EXISTS idx_triggers_due_cron ON public.triggers USING btree (next_fire_at) WHERE ((source_type = 'cron'::text) AND (enabled = true));

DROP TRIGGER IF EXISTS trg_triggers_updated_at ON public.triggers;
CREATE TRIGGER trg_triggers_updated_at BEFORE UPDATE ON public.triggers
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 3. webhooks ------------------------------------------------------------------
-- One row per webhook trigger. secret_cipher is the KEK-encrypted HMAC secret
-- (exact mcp_servers crypto envelope pattern). The signature IS the credential —
-- the receiver route is public (no JWT). IP allowlist is an early-reject before HMAC.
CREATE TABLE IF NOT EXISTS public.webhooks (
    id                  uuid DEFAULT gen_random_uuid() NOT NULL,
    trigger_id          uuid NOT NULL,
    secret_cipher       bytea NOT NULL,
    key_version         integer DEFAULT 1 NOT NULL,
    allowed_ips         cidr[] DEFAULT '{}'::cidr[] NOT NULL,
    idempotency_mode    text DEFAULT 'header'::text NOT NULL,
    idempotency_header  text DEFAULT 'X-Request-ID'::text NOT NULL,
    created_at          timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT webhooks_idempotency_mode_check CHECK ((idempotency_mode = ANY (ARRAY['header'::text, 'hash'::text, 'disabled'::text])))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'webhooks_pkey') THEN
        ALTER TABLE public.webhooks ADD CONSTRAINT webhooks_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'webhooks_trigger_id_fkey') THEN
        ALTER TABLE public.webhooks ADD CONSTRAINT webhooks_trigger_id_fkey FOREIGN KEY (trigger_id) REFERENCES public.triggers(id) ON DELETE CASCADE;
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'webhooks_trigger_id_uniq') THEN
        ALTER TABLE public.webhooks ADD CONSTRAINT webhooks_trigger_id_uniq UNIQUE (trigger_id);
    END IF;
END $$;

-- 4. webhook_deliveries --------------------------------------------------------
-- Idempotency dedup (copy the stripe_events shape). dedup_key is the caller-supplied
-- header value OR sha256(body+timestamp-window). UNIQUE enforces at-most-once fire
-- per dedup key per webhook — a duplicate delivery returns 200 "duplicate".
CREATE TABLE IF NOT EXISTS public.webhook_deliveries (
    id            uuid DEFAULT gen_random_uuid() NOT NULL,
    webhook_id    uuid NOT NULL,
    dedup_key     text NOT NULL,
    delivered_at  timestamp with time zone DEFAULT now() NOT NULL
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'webhook_deliveries_pkey') THEN
        ALTER TABLE public.webhook_deliveries ADD CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'webhook_deliveries_webhook_dedup_uniq') THEN
        ALTER TABLE public.webhook_deliveries ADD CONSTRAINT webhook_deliveries_webhook_dedup_uniq UNIQUE (webhook_id, dedup_key);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'webhook_deliveries_webhook_id_fkey') THEN
        ALTER TABLE public.webhook_deliveries ADD CONSTRAINT webhook_deliveries_webhook_id_fkey FOREIGN KEY (webhook_id) REFERENCES public.webhooks(id) ON DELETE CASCADE;
    END IF;
END $$;

-- 5. workflow_runs -------------------------------------------------------------
-- Execution state machine. spec_snapshot is an IMMUTABLE copy of spec_json at run
-- start (D6) — editing a workflow does not affect in-flight runs. status is the
-- six-state enum (no library). error_code is machine-readable (UI grouping/filtering).
-- trigger_id/trigger_fire_id are nullable (null = manual run via POST /workflows/:id/runs).
CREATE TABLE IF NOT EXISTS public.workflow_runs (
    id                uuid DEFAULT gen_random_uuid() NOT NULL,
    workflow_id       uuid NOT NULL,
    spec_snapshot     jsonb NOT NULL,
    input             jsonb,
    output            jsonb,
    status            text DEFAULT 'queued'::text NOT NULL,
    error_code        text,
    error             jsonb,
    trigger_id        uuid,
    trigger_fire_id   uuid,
    workspace_id      uuid NOT NULL,
    started_at        timestamp with time zone,
    finished_at       timestamp with time zone,
    created_at        timestamp with time zone DEFAULT now() NOT NULL,
    updated_at        timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workflow_runs_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'canceled'::text, 'timed_out'::text]))),
    CONSTRAINT workflow_runs_error_code_check CHECK ((error_code IS NULL OR (error_code = ANY (ARRAY[
        'node_failed'::text,
        'workspace_unavailable'::text,
        'canceled'::text,
        'timed_out'::text,
        'validation_error'::text,
        'schema_mismatch'::text,
        'output_oversize'::text,
        'agent_not_found'::text,
        'session_not_found'::text,
        'secret_not_found'::text,
        'script_failed'::text,
        'script_output_invalid'::text,
        'api_restart'::text
    ]))))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_runs_pkey') THEN
        ALTER TABLE public.workflow_runs ADD CONSTRAINT workflow_runs_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_runs_workflow_id_fkey') THEN
        ALTER TABLE public.workflow_runs ADD CONSTRAINT workflow_runs_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id) ON DELETE CASCADE;
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_runs_trigger_id_fkey') THEN
        ALTER TABLE public.workflow_runs ADD CONSTRAINT workflow_runs_trigger_id_fkey FOREIGN KEY (trigger_id) REFERENCES public.triggers(id) ON DELETE SET NULL;
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_runs_workspace_id_fkey') THEN
        ALTER TABLE public.workflow_runs ADD CONSTRAINT workflow_runs_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_workflow_runs_workflow_status ON public.workflow_runs USING btree (workflow_id, status);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON public.workflow_runs USING btree (status) WHERE ((status = ANY (ARRAY['queued'::text, 'running'::text])));
CREATE INDEX IF NOT EXISTS idx_workflow_runs_trigger ON public.workflow_runs USING btree (trigger_id) WHERE (trigger_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_created_at ON public.workflow_runs USING btree (created_at DESC);

-- Single-in-flight-per-workflow (D8): the partial UNIQUE index makes the insert
-- itself the atomic gate. Two concurrent webhook deliveries that both pass a count
-- check would both insert — the index prevents that. The second insert gets a unique
-- violation that the API maps to 409 "already running" + Retry-After: 30.
-- v1 hardcodes single-in-flight (no maxConcurrentRuns setting); N>1 + queueing is v2.
CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_run_single_inflight
    ON public.workflow_runs (workflow_id)
    WHERE (status IN ('queued'::text, 'running'::text));

DROP TRIGGER IF EXISTS trg_workflow_runs_updated_at ON public.workflow_runs;
CREATE TRIGGER trg_workflow_runs_updated_at BEFORE UPDATE ON public.workflow_runs
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 6. workflow_node_runs --------------------------------------------------------
-- Per-node state within a run. node_id matches spec_snapshot.nodes[].id (NOT the
-- current spec — in-flight runs are pinned). output capped at maxNodeOutputBytes
-- (default 1 MiB) by the executor; oversize FAILS the node (no spill in v1).
-- branch is set only on condition nodes (matched edge sourceHandle).
CREATE TABLE IF NOT EXISTS public.workflow_node_runs (
    id                uuid DEFAULT gen_random_uuid() NOT NULL,
    workflow_run_id   uuid NOT NULL,
    node_id           text NOT NULL,
    node_type         text NOT NULL,
    status            text DEFAULT 'pending'::text NOT NULL,
    attempt           integer DEFAULT 0 NOT NULL,
    input             jsonb,
    output            jsonb,
    branch            text,
    error_code        text,
    error             jsonb,
    started_at        timestamp with time zone DEFAULT now() NOT NULL,
    finished_at       timestamp with time zone,
    CONSTRAINT workflow_node_runs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'skipped'::text]))),
    CONSTRAINT workflow_node_runs_node_type_check CHECK ((node_type = ANY (ARRAY['script'::text, 'agent'::text, 'http'::text, 'condition'::text]))),
    CONSTRAINT workflow_node_runs_attempt_check CHECK ((attempt >= 0))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_node_runs_pkey') THEN
        ALTER TABLE public.workflow_node_runs ADD CONSTRAINT workflow_node_runs_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'workflow_node_runs_workflow_run_id_fkey') THEN
        ALTER TABLE public.workflow_node_runs ADD CONSTRAINT workflow_node_runs_workflow_run_id_fkey FOREIGN KEY (workflow_run_id) REFERENCES public.workflow_runs(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_workflow_node_runs_run ON public.workflow_node_runs USING btree (workflow_run_id, node_id);

-- 7. trigger_fires -------------------------------------------------------------
-- Fire audit + action result. status 'skipped' covers missed-fire policy (controller
-- downtime, logged not silent) AND single-in-flight reject (no orphan 'fired' rows
-- when the run was rejected). status 'auto_disabled' is the circuit breaker trip.
CREATE TABLE IF NOT EXISTS public.trigger_fires (
    id              uuid DEFAULT gen_random_uuid() NOT NULL,
    trigger_id      uuid NOT NULL,
    source_type     text NOT NULL,
    input_envelope  jsonb,
    action_type     text NOT NULL,
    action_result   jsonb,
    status          text NOT NULL,
    fired_at        timestamp with time zone DEFAULT now() NOT NULL,
    completed_at    timestamp with time zone,
    CONSTRAINT trigger_fires_source_type_check CHECK ((source_type = ANY (ARRAY['cron'::text, 'webhook'::text]))),
    CONSTRAINT trigger_fires_action_type_check CHECK ((action_type = ANY (ARRAY['run_workflow'::text, 'run_script'::text]))),
    CONSTRAINT trigger_fires_status_check CHECK ((status = ANY (ARRAY['fired'::text, 'delivered'::text, 'failed'::text, 'validation_error'::text, 'rate_limited'::text, 'skipped'::text, 'auto_disabled'::text])))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'trigger_fires_pkey') THEN
        ALTER TABLE public.trigger_fires ADD CONSTRAINT trigger_fires_pkey PRIMARY KEY (id);
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'trigger_fires_trigger_id_fkey') THEN
        ALTER TABLE public.trigger_fires ADD CONSTRAINT trigger_fires_trigger_id_fkey FOREIGN KEY (trigger_id) REFERENCES public.triggers(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_trigger_fires_trigger ON public.trigger_fires USING btree (trigger_id, fired_at DESC);

-- 8. org_policies CHECK extension (allow_user_workflow_create) -----------------
-- Mirrors allow_user_mcp_servers: gates whether user-scope workflow/trigger authoring
-- is permitted at all (D11). Org members are still bound by org-admin-owned workflows;
-- this policy governs the *user-scope* surface only. Default locked.
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
        'max_mcp_servers_per_workspace',
        'allow_user_workflow_create'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

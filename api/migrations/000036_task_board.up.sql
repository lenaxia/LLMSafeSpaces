-- Migration 000036 — Task Board (design 0067, issue #1696 / #1698, rulings R1-R9).
-- Entities per R1: project → epic → task (no stories, no group field).
-- User-level only per R7 (user_id owner, no owner_kind discriminator).
-- Single target workspace per project per R8. Blocking is purely structural
-- per R3/R4: board_dependencies is the ONLY blocking mechanism.
-- Postgres-arbitrated dispatch state lives on tasks (§4.4): claim_token,
-- lease_expires_at, attempts + the claim-provenance flag the throttle's
-- conditional rollback consumes. No in-memory scheduler state exists.

-- 1. projects ------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS public.projects (
    id                   uuid DEFAULT gen_random_uuid() NOT NULL,
    short_id             text NOT NULL,
    user_id              text NOT NULL,
    name                 text NOT NULL,
    target_workspace_id  uuid,
    worker_slots         integer DEFAULT 2 NOT NULL,
    slot_cooldown_until  timestamp with time zone,
    tags                 text[] DEFAULT '{}'::text[] NOT NULL,
    status               text DEFAULT 'active'::text NOT NULL,
    created_at           timestamp with time zone DEFAULT now() NOT NULL,
    updated_at           timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT projects_pkey PRIMARY KEY (id),
    CONSTRAINT projects_short_id_check CHECK ((short_id ~ '^prj-[0-9a-z]{8}$'::text)),
    CONSTRAINT projects_worker_slots_check CHECK ((worker_slots >= 1)),
    CONSTRAINT projects_status_check CHECK ((status = ANY (ARRAY['active'::text, 'archived'::text]))),
    CONSTRAINT projects_uq_user_short UNIQUE (user_id, short_id)
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'projects_target_workspace_id_fkey') THEN
        ALTER TABLE public.projects
            ADD CONSTRAINT projects_target_workspace_id_fkey
            FOREIGN KEY (target_workspace_id) REFERENCES public.workspaces(id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_projects_user_status ON public.projects USING btree (user_id, status);
CREATE INDEX IF NOT EXISTS idx_projects_slot_cooldown ON public.projects USING btree (slot_cooldown_until)
    WHERE (slot_cooldown_until IS NOT NULL);

DROP TRIGGER IF EXISTS trg_projects_updated_at ON public.projects;
CREATE TRIGGER trg_projects_updated_at BEFORE UPDATE ON public.projects
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 2. epics -----------------------------------------------------------------------
-- DRR state (per-epic) lives here: vft (virtual finish time = claims served,
-- globally comparable), deficit (claims consumed this round), quantum (round
-- allowance, config default 1). Postgres is the only fairness state (§4.4).
CREATE TABLE IF NOT EXISTS public.epics (
    id           uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id   uuid NOT NULL,
    short_id     text NOT NULL,
    title        text NOT NULL,
    outcome      text DEFAULT ''::text NOT NULL,
    status       text DEFAULT 'draft'::text NOT NULL,
    priority     text DEFAULT 'P2'::text NOT NULL,
    project_name text NOT NULL,
    tags         text[] DEFAULT '{}'::text[] NOT NULL,
    vft          bigint DEFAULT 0 NOT NULL,
    deficit      bigint DEFAULT 0 NOT NULL,
    quantum      integer DEFAULT 1 NOT NULL,
    created_at   timestamp with time zone DEFAULT now() NOT NULL,
    updated_at   timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT epics_pkey PRIMARY KEY (id),
    CONSTRAINT epics_short_id_check CHECK ((short_id ~ '^epc-[0-9a-z]{8}$'::text)),
    CONSTRAINT epics_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'active'::text, 'paused'::text, 'completed'::text, 'archived'::text]))),
    CONSTRAINT epics_priority_check CHECK ((priority = ANY (ARRAY['P0'::text, 'P1'::text, 'P2'::text, 'P3'::text]))),
    CONSTRAINT epics_quantum_check CHECK ((quantum >= 1)),
    CONSTRAINT epics_uq_project_short UNIQUE (project_id, short_id),
    -- composite-FK anchor: tasks reference (id, project_id) so the parent is
    -- structurally guaranteed to belong to the same project.
    CONSTRAINT epics_uq_id_project UNIQUE (id, project_id)
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'epics_project_id_fkey') THEN
        ALTER TABLE public.epics
            ADD CONSTRAINT epics_project_id_fkey
            FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_epics_project_status ON public.epics USING btree (project_id, status);
CREATE INDEX IF NOT EXISTS idx_epics_drr ON public.epics USING btree (vft) WHERE (status = 'active'::text);

DROP TRIGGER IF EXISTS trg_epics_updated_at ON public.epics;
CREATE TRIGGER trg_epics_updated_at BEFORE UPDATE ON public.epics
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 3. tasks -----------------------------------------------------------------------
-- Dispatch state is Postgres-arbitrated (§4.4): attempts has exactly ONE
-- increment site (the claim statement); claim_incremented_attempts is true
-- iff the live token was minted by an incrementing grant (claim/task_start)
-- — the throttle's rollback precondition. attempts is non-negative by
-- construction (CHECK is the belt beneath the conditional decrement).
CREATE TABLE IF NOT EXISTS public.tasks (
    id                          uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id                  uuid NOT NULL,
    epic_id                     uuid NOT NULL,
    short_id                    text NOT NULL,
    title                       text NOT NULL,
    description                 text DEFAULT ''::text NOT NULL,
    stage                       text DEFAULT 'backlog'::text NOT NULL,
    pending_origin              text,
    priority                    text DEFAULT 'P2'::text NOT NULL,
    project_name                text NOT NULL,
    epic_name                   text NOT NULL,
    tags                        text[] DEFAULT '{}'::text[] NOT NULL,
    acceptance_criteria         jsonb DEFAULT '[]'::jsonb NOT NULL,
    is_blocked                  boolean DEFAULT false NOT NULL,
    assignee_session            text,
    claim_token                 uuid,
    lease_expires_at            timestamp with time zone,
    last_progress_at            timestamp with time zone,
    attempts                    integer DEFAULT 0 NOT NULL,
    claim_incremented_attempts  boolean DEFAULT false NOT NULL,
    cooldown_until              timestamp with time zone,
    stage_entered_at            timestamp with time zone DEFAULT now() NOT NULL,
    created_at                  timestamp with time zone DEFAULT now() NOT NULL,
    updated_at                  timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tasks_pkey PRIMARY KEY (id),
    CONSTRAINT tasks_short_id_check CHECK ((short_id ~ '^tsk-[0-9a-z]{8}$'::text)),
    CONSTRAINT tasks_stage_check CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'pending_input'::text, 'cancelled'::text]))),
    CONSTRAINT tasks_pending_origin_check CHECK ((pending_origin = ANY (ARRAY['worker'::text, 'escalated'::text, 'dependency_abandoned'::text, 'drift_review'::text]))),
    CONSTRAINT tasks_priority_check CHECK ((priority = ANY (ARRAY['P0'::text, 'P1'::text, 'P2'::text, 'P3'::text]))),
    CONSTRAINT tasks_attempts_check CHECK ((attempts >= 0)),
    -- pending_origin is set exactly when the row is in pending_input (§4.1:
    -- four origins with distinct exits; cleared on exit).
    CONSTRAINT tasks_pending_origin_set CHECK ((stage = 'pending_input'::text) = (pending_origin IS NOT NULL)),
    -- a claimed row always carries a worker identity (claim/throttle/reclaim
    -- keep the pair in lockstep); in_review deliberately retains identity
    -- with no token (the bounce/RC2 consumer, §4.1).
    CONSTRAINT tasks_claim_needs_assignee CHECK ((claim_token IS NULL) OR (assignee_session IS NOT NULL)),
    CONSTRAINT tasks_uq_project_short UNIQUE (project_id, short_id)
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_project_id_fkey') THEN
        ALTER TABLE public.tasks
            ADD CONSTRAINT tasks_project_id_fkey
            FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_epic_id_project_fkey') THEN
        ALTER TABLE public.tasks
            ADD CONSTRAINT tasks_epic_id_project_fkey
            FOREIGN KEY (epic_id, project_id) REFERENCES public.epics(id, project_id);
    END IF;
END $$;

-- Ready-set scan: STABLE predicates only (§4.1 — the time-varying cooldown
-- predicate filters at scan time; an index predicated on cooldown_until IS
-- NULL would exclude exactly the post-throttle rows the query must re-serve).
CREATE INDEX IF NOT EXISTS idx_tasks_ready_set
    ON public.tasks USING btree (project_id, priority, created_at)
    WHERE (stage = 'backlog'::text AND is_blocked = false);
-- Reclaim scan (RC1): pending_input rows are excluded by the stage predicate.
CREATE INDEX IF NOT EXISTS idx_tasks_reclaim
    ON public.tasks USING btree (lease_expires_at)
    WHERE (stage = 'in_progress'::text);
CREATE INDEX IF NOT EXISTS idx_tasks_project_stage ON public.tasks USING btree (project_id, stage);
CREATE INDEX IF NOT EXISTS idx_tasks_assignee ON public.tasks USING btree (assignee_session)
    WHERE (assignee_session IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_tasks_tags ON public.tasks USING gin (tags);

DROP TRIGGER IF EXISTS trg_tasks_updated_at ON public.tasks;
CREATE TRIGGER trg_tasks_updated_at BEFORE UPDATE ON public.tasks
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- stage_update_guard (RC5, §4.1): cancelled is immutable-terminal — no stage
-- transition out of cancelled exists; only audit/display fields may change.
CREATE OR REPLACE FUNCTION public.board_tasks_stage_update_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.stage = 'cancelled'::text AND NEW.stage IS DISTINCT FROM OLD.stage THEN
        RAISE EXCEPTION 'board: cancelled task % is terminal (stage immutable)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_tasks_stage_update_guard ON public.tasks;
CREATE TRIGGER trg_tasks_stage_update_guard BEFORE UPDATE ON public.tasks
    FOR EACH ROW EXECUTE FUNCTION public.board_tasks_stage_update_guard();

-- 4. board_dependencies ------------------------------------------------------------
-- Ruling R4: N-ary task→task edges, within a project (cross-epic allowed),
-- never cross-project. Same-project is structurally guaranteed (both endpoint
-- FKs resolve within one project via the composite anchor), but enforced
-- explicitly so a violation is a clean 409 rather than an FK error. Acyclicity
-- is enforced by full DFS on insert. The PK makes duplicate edges impossible.
CREATE TABLE IF NOT EXISTS public.board_dependencies (
    task_id            uuid NOT NULL,
    depends_on_task_id uuid NOT NULL,
    created_by         text NOT NULL,
    created_at         timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_dependencies_pkey PRIMARY KEY (task_id, depends_on_task_id)
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'board_dependencies_task_id_fkey') THEN
        ALTER TABLE public.board_dependencies
            ADD CONSTRAINT board_dependencies_task_id_fkey
            FOREIGN KEY (task_id) REFERENCES public.tasks(id) ON DELETE CASCADE;
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'board_dependencies_depends_on_task_id_fkey') THEN
        ALTER TABLE public.board_dependencies
            ADD CONSTRAINT board_dependencies_depends_on_task_id_fkey
            FOREIGN KEY (depends_on_task_id) REFERENCES public.tasks(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_board_dependencies_reverse
    ON public.board_dependencies USING btree (depends_on_task_id);

-- board_dependency_guard: BEFORE INSERT — same project, not self-referential,
-- acyclic (DFS). Violations raise with a stable errcode the service maps to
-- 409 + did-you-mean suggestions (ruling R4a).
CREATE OR REPLACE FUNCTION public.board_dependency_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_task_project   uuid;
    v_dep_project    uuid;
    v_cycle          uuid;
BEGIN
    IF NEW.task_id = NEW.depends_on_task_id THEN
        RAISE EXCEPTION 'board: task % cannot depend on itself', NEW.task_id
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT project_id INTO v_task_project FROM public.tasks WHERE id = NEW.task_id;
    SELECT project_id INTO v_dep_project FROM public.tasks WHERE id = NEW.depends_on_task_id;

    IF v_task_project IS DISTINCT FROM v_dep_project THEN
        RAISE EXCEPTION 'board: cross-project dependency rejected (task % in project %, target % in project %)',
            NEW.task_id, v_task_project, NEW.depends_on_task_id, v_dep_project
            USING ERRCODE = 'check_violation';
    END IF;

    -- Acyclicity: does the target (transitively) depend on the source?
    WITH RECURSIVE downstream(task_id) AS (
        SELECT NEW.depends_on_task_id
        UNION
        SELECT bd.task_id
        FROM public.board_dependencies bd
        JOIN downstream d ON bd.depends_on_task_id = d.task_id
    )
    SELECT task_id INTO v_cycle FROM downstream WHERE task_id = NEW.task_id LIMIT 1;

    IF v_cycle IS NOT NULL THEN
        RAISE EXCEPTION 'board: dependency edge would create a cycle through task %', v_cycle
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_board_dependency_guard ON public.board_dependencies;
CREATE TRIGGER trg_board_dependency_guard BEFORE INSERT ON public.board_dependencies
    FOR EACH ROW EXECUTE FUNCTION public.board_dependency_guard();

-- 5. board_transitions ---------------------------------------------------------------
-- Ruling R2: evidence_url per transition + measured stage-transition
-- timestamps (cycle time is derived from this log — never an estimate column;
-- no story-points field exists, deliberately).
CREATE TABLE IF NOT EXISTS public.board_transitions (
    id           bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    task_id      uuid NOT NULL,
    from_stage   text NOT NULL,
    to_stage     text NOT NULL,
    evidence_url text,
    actor        jsonb NOT NULL,
    at           timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_transitions_pkey PRIMARY KEY (id)
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'board_transitions_task_id_fkey') THEN
        ALTER TABLE public.board_transitions
            ADD CONSTRAINT board_transitions_task_id_fkey
            FOREIGN KEY (task_id) REFERENCES public.tasks(id) ON DELETE CASCADE;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_board_transitions_task_at
    ON public.board_transitions USING btree (task_id, at);

-- 6. board_events ---------------------------------------------------------------------
-- Idempotency + audit (§4.2): id is the caller-supplied requestId (a UUID;
-- reconciler ids are namespaced 'reconciler:...'). PK (project_id, id) is the
-- replay-convergence key (invariant 4) — the outbox clientMessageID
-- discipline, SQL-shaped: the effect and the event row commit together, a
-- replayed requestId hits the uniqueness violation and the service returns
-- the original outcome (200-with-original, never an error).
CREATE TABLE IF NOT EXISTS public.board_events (
    id          text NOT NULL,
    project_id  uuid NOT NULL,
    item_kind   text,
    item_id     uuid,
    action      text NOT NULL,
    actor       jsonb NOT NULL,
    payload     jsonb,
    created_at  timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_events_pkey PRIMARY KEY (project_id, id),
    CONSTRAINT board_events_item_kind_check CHECK ((item_kind = ANY (ARRAY['project'::text, 'epic'::text, 'task'::text])))
);

CREATE INDEX IF NOT EXISTS idx_board_events_project_created
    ON public.board_events USING btree (project_id, created_at);

-- 7. board_sweeps ---------------------------------------------------------------------
-- Reconciler watermark (§4.5): incremental board_events.created_at cursor so
-- the drift checks never rescan. Engine-internal state; the smallest additive
-- home for it (flagged in the lane report).
CREATE TABLE IF NOT EXISTS public.board_sweeps (
    scope         text NOT NULL,
    last_event_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at    timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_sweeps_pkey PRIMARY KEY (scope)
);

DROP TRIGGER IF EXISTS trg_board_sweeps_updated_at ON public.board_sweeps;
CREATE TRIGGER trg_board_sweeps_updated_at BEFORE UPDATE ON public.board_sweeps
    FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

-- 000038 down: restore the pre-#1747 shape. DESTRUCTIVE CAVEATS:
--   * priority columns return with the P2 default (per-row values are
--     unrecoverable — they were dropped in the up migration).
--   * cancelled rows deleted by the up migration are unrecoverable
--     (the audit log in board_events keeps the record).
--   * workspace-crossing edges added under the relaxed guard are not
--     re-validated (the restored same-project guard applies to NEW
--     inserts only).

BEGIN;

-- Pause flags go.
ALTER TABLE public.tasks DROP COLUMN IF EXISTS paused;
ALTER TABLE public.projects DROP COLUMN IF EXISTS paused;

-- Priority fields return (P2 default).
ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS priority text DEFAULT 'P2'::text NOT NULL;
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_priority_check;
ALTER TABLE public.tasks
    ADD CONSTRAINT tasks_priority_check CHECK ((priority = ANY (ARRAY['P0'::text, 'P1'::text, 'P2'::text, 'P3'::text])));
ALTER TABLE public.epics ADD COLUMN IF NOT EXISTS priority text DEFAULT 'P2'::text NOT NULL;
ALTER TABLE public.epics DROP CONSTRAINT IF EXISTS epics_priority_check;
ALTER TABLE public.epics
    ADD CONSTRAINT epics_priority_check CHECK ((priority = ANY (ARRAY['P0'::text, 'P1'::text, 'P2'::text, 'P3'::text])));

DROP INDEX IF EXISTS idx_tasks_ready_set;
CREATE INDEX idx_tasks_ready_set
    ON public.tasks USING btree (project_id, priority, created_at)
    WHERE (stage = 'backlog'::text AND is_blocked = false);

-- The cancelled stage + its immutability guard return.
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_stage_check;
ALTER TABLE public.tasks
    ADD CONSTRAINT tasks_stage_check CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'pending_input'::text, 'cancelled'::text])));

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

-- board_transitions regains its task FK (historical rows surviving a
-- delete under #1747 cannot re-reference: they are DELETED first).
DELETE FROM public.board_transitions bt
 WHERE NOT EXISTS (SELECT 1 FROM public.tasks t WHERE t.id = bt.task_id);
ALTER TABLE public.board_transitions
    ADD CONSTRAINT board_transitions_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES public.tasks(id) ON DELETE CASCADE;

-- The dependency guard returns to same-project scope.
CREATE OR REPLACE FUNCTION public.board_dependency_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_task_project   uuid;
    v_dep_project    uuid;
    v_cycle          int;
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

    WITH RECURSIVE deps_up(task_id) AS (
        SELECT NEW.depends_on_task_id
        UNION
        SELECT bd.depends_on_task_id
        FROM public.board_dependencies bd
        JOIN deps_up u ON bd.task_id = u.task_id
    )
    SELECT 1 INTO v_cycle FROM deps_up WHERE task_id = NEW.task_id LIMIT 1;

    IF v_cycle IS NOT NULL THEN
        RAISE EXCEPTION 'board: dependency edge would create a cycle through task %', v_cycle
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;

COMMIT;

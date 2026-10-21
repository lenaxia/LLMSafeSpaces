-- 000039_agent_board_manager_tools (issue #1747, design 0067 amendment)
--
-- The owner rulings of 2026-10-19 (base + consolidated 6261010901 + KISS
-- 6261037825 + round 3 6261147201):
--   * Cancel is ELIMINATED: no 'cancelled' terminal stage, no cancel
--     trigger. delete_task/delete_epic replace cancel (delete removes
--     the object, not the history).
--   * Priority fields are GONE ("no priority fields, no position
--     fields"): claim order is deterministic FIFO (creation order) in
--     exactly one place (pkg/board candidateSQL).
--   * Pause arrives at project and task level as a FLAG (never a
--     stage): tasks.paused, projects.paused.
--   * Dependency edges may cross epics (and projects) WITHIN a
--     workspace, never across workspaces: the guard's same-project
--     check relaxes to same-target-workspace.
--   * board_transitions loses its task FK: delete keeps the history.

BEGIN;

-- 1. The cancelled terminal state leaves the vocabulary. Existing
--    cancelled rows are deleted (the state is eliminated, not migrated;
--    boards are days old at v0.39 and the audit log keeps the record).
DELETE FROM public.tasks WHERE stage = 'cancelled';

ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_stage_check;
ALTER TABLE public.tasks
    ADD CONSTRAINT tasks_stage_check CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'pending_input'::text])));

-- 2. Priority fields are gone (final ruling: dependency-only ordering).
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_priority_check;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS priority;
ALTER TABLE public.epics DROP CONSTRAINT IF EXISTS epics_priority_check;
ALTER TABLE public.epics DROP COLUMN IF EXISTS priority;

-- The ready-set index follows the FIFO order (project, creation).
DROP INDEX IF EXISTS idx_tasks_ready_set;
CREATE INDEX idx_tasks_ready_set
    ON public.tasks USING btree (project_id, created_at, id)
    WHERE (stage = 'backlog'::text AND is_blocked = false);

-- 3. Pause flags (#1747): scheduling lever, blocks claiming only.
ALTER TABLE public.projects ADD COLUMN IF NOT EXISTS paused boolean NOT NULL DEFAULT false;
ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS paused boolean NOT NULL DEFAULT false;

-- 4. The stage_update_guard trigger guarded the cancelled terminal
--    state — with cancel eliminated there is no immutable-terminal
--    stage for the DB to guard (the Go machine enumerates transitions;
--    done rows refuse every move at the service layer).
DROP TRIGGER IF EXISTS trg_tasks_stage_update_guard ON public.tasks;
DROP FUNCTION IF EXISTS public.board_tasks_stage_update_guard();

-- 5. board_transitions: delete keeps the history — the task FK goes
--    (rows survive their task; task_id becomes a historical reference).
ALTER TABLE public.board_transitions DROP CONSTRAINT IF EXISTS board_transitions_task_id_fkey;

-- 6. Dependency guard: same-project → same-target-workspace (#1747:
--    cross-epic edges allowed within a workspace, never cross-workspace).
CREATE OR REPLACE FUNCTION public.board_dependency_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_task_ws   uuid;
    v_dep_ws    uuid;
    v_cycle     int;
BEGIN
    IF NEW.task_id = NEW.depends_on_task_id THEN
        RAISE EXCEPTION 'board: task % cannot depend on itself', NEW.task_id
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT p.target_workspace_id INTO v_task_ws
      FROM public.tasks t JOIN public.projects p ON p.id = t.project_id
     WHERE t.id = NEW.task_id;
    SELECT p.target_workspace_id INTO v_dep_ws
      FROM public.tasks t JOIN public.projects p ON p.id = t.project_id
     WHERE t.id = NEW.depends_on_task_id;

    IF v_task_ws IS DISTINCT FROM v_dep_ws THEN
        RAISE EXCEPTION 'board: cross-workspace dependency rejected (edges live within a workspace)'
            USING ERRCODE = 'check_violation';
    END IF;

    -- Acyclicity: would the source become reachable from the target by
    -- following depends-on edges forward? (a -> c is cyclic iff c's
    -- transitive dependency closure already contains a.)
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

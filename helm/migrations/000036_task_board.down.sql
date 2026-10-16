-- Migration 000036 down — Task Board (design 0067). Reverse of the up
-- migration in dependency order (events → transitions → dependencies →
-- tasks → epics → projects → sweeps → guard functions).

DROP TRIGGER IF EXISTS trg_board_sweeps_updated_at ON public.board_sweeps;
DROP TABLE IF EXISTS public.board_sweeps;

DROP TABLE IF EXISTS public.board_events;

DROP TABLE IF EXISTS public.board_transitions;

DROP TRIGGER IF EXISTS trg_board_dependency_guard ON public.board_dependencies;
DROP FUNCTION IF EXISTS public.board_dependency_guard();
DROP TABLE IF EXISTS public.board_dependencies;

DROP TRIGGER IF EXISTS trg_tasks_stage_update_guard ON public.tasks;
DROP FUNCTION IF EXISTS public.board_tasks_stage_update_guard();
DROP TRIGGER IF EXISTS trg_tasks_updated_at ON public.tasks;
DROP TABLE IF EXISTS public.tasks;

DROP TRIGGER IF EXISTS trg_epics_updated_at ON public.epics;
DROP TABLE IF EXISTS public.epics;

DROP TRIGGER IF EXISTS trg_projects_updated_at ON public.projects;
DROP TABLE IF EXISTS public.projects;

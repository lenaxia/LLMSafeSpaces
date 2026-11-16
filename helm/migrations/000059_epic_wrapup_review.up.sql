-- 000059_epic_wrapup_review — the wrap-up engine's row-shape migration
-- (epic wrap-up review, low-level design §1b; the design named this
-- migration 000056 at design time — main took 000055 for
-- task_last_renewed_at in #2067 and 000056 for the wrapUpReviewer
-- config key in #2071, so this file takes the next free number per the
-- duplicate-version class pin in api/migrations/migration_set_unit_test.go).
--
-- tasks.kind is the engine-owned row-class marker: 'story' is the
-- default and the only value user-created rows can hold (the service
-- layer never writes 'wrapup' outside the engine mint); 'wrapup' marks
-- engine-minted wrap-up review rows. Self-exclusion (the drain +
-- completion predicates count kind='story' rows only) and the dispatch
-- carve-outs (ClaimNext / dispatchDrive candidate scans) key off this
-- column.
--
-- epics gains the on/off flag + the wrap-up lifecycle state. Explicit
-- columns, never derived (high-level design D2): wrapup_task_id is the
-- in-flight pointer, wrapup_count the consecutive-close ledger,
-- wrapup_ack_pending the human circuit-breaker (the cap/ack verb lives
-- in the wrapup-gap-recursion story), last_wrapped_at the coverage
-- watermark (advanced at close by the verdict story).

BEGIN;

ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'story';
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_kind_check;
ALTER TABLE public.tasks ADD CONSTRAINT tasks_kind_check
    CHECK ((kind = ANY (ARRAY['story'::text, 'wrapup'::text])));
CREATE INDEX IF NOT EXISTS idx_tasks_kind ON public.tasks USING btree (epic_id, kind);

ALTER TABLE public.epics ADD COLUMN IF NOT EXISTS wrap_up_review boolean NOT NULL DEFAULT false;
ALTER TABLE public.epics ADD COLUMN IF NOT EXISTS last_wrapped_at timestamp with time zone;
ALTER TABLE public.epics ADD COLUMN IF NOT EXISTS wrapup_count integer NOT NULL DEFAULT 0;
ALTER TABLE public.epics ADD COLUMN IF NOT EXISTS wrapup_task_id uuid;
ALTER TABLE public.epics ADD COLUMN IF NOT EXISTS wrapup_ack_pending boolean NOT NULL DEFAULT false;

-- Reference integrity for the in-flight pointer: deleting the wrap-up
-- row clears the pointer physically (the service-level clear + audit
-- event rides the same transaction in delete.go; ON DELETE SET NULL is
-- the floor, never the only belt).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'epics_wrapup_task_id_fkey') THEN
        ALTER TABLE public.epics
            ADD CONSTRAINT epics_wrapup_task_id_fkey
            FOREIGN KEY (wrapup_task_id) REFERENCES public.tasks(id) ON DELETE SET NULL;
    END IF;
END $$;

COMMIT;

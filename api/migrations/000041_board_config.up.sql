-- Migration 000041 — Task Board Config (#1832 item 6, part 1: storage).
--
-- Board-wide configuration, storage from day one: a GLOBAL defaults map
-- (board_config_defaults) plus an optional PER-PROJECT override map
-- (board_project_config_overrides), merged at READ time — an unset key
-- in the override map inherits the global default (and a global key
-- that was never written inherits the code default). Per-project
-- becomes UI-only later.
--
-- Shape mirrors instance_settings (key text + value jsonb rows, the
-- settings system's tier model) scoped to the board domain. The key
-- CHECK constraint is the physical belt beneath the service validation
-- (unknown keys reject at the API first); growing the key set (issue:
-- "room to grow") is a deliberate migration: add the key to
-- pkg/board/config.go's vocabulary and relax this constraint.
--
-- Initial keys (#1832): reviewColumn (off|manual|auto),
-- autoAssignReviewer (bool), defaultWorkerSlots (int > 0).
--
-- No board_events rows ride these tables: board_events is the
-- project-scoped idempotency+audit log for board item actions
-- (project_id NOT NULL, item_kind IN project|epic|task); the global
-- map has no project, and a config PUT is replace-semantics —
-- naturally idempotent by construction.

BEGIN;

CREATE TABLE IF NOT EXISTS public.board_config_defaults (
    key        text NOT NULL,
    value      jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_config_defaults_pkey PRIMARY KEY (key),
    CONSTRAINT board_config_defaults_key_check CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'defaultWorkerSlots'::text])))
);

CREATE TABLE IF NOT EXISTS public.board_project_config_overrides (
    project_id uuid NOT NULL,
    key        text NOT NULL,
    value      jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_project_config_overrides_pkey PRIMARY KEY (project_id, key),
    CONSTRAINT board_project_config_overrides_key_check CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'defaultWorkerSlots'::text])))
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'board_project_config_overrides_project_id_fkey') THEN
        ALTER TABLE public.board_project_config_overrides
            ADD CONSTRAINT board_project_config_overrides_project_id_fkey
            FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;
    END IF;
END $$;

COMMIT;

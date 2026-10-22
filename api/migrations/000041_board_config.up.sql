-- Migration 000041 — Task Board Config (#1832 item 6, part 1: storage).
--
-- Board-wide configuration, storage from day one: a USER-SCOPED
-- defaults map (board_config_defaults — the owner's board-wide
-- defaults across ALL their projects, ruling: boards are user-level
-- per design 0067 R7, so "board-wide" means the user's boards, NOT
-- the instance) plus an optional PER-PROJECT override map
-- (board_project_config_overrides), merged at READ time — an unset
-- key in the override map inherits the user default (and a user key
-- that was never written inherits the code default). Per-project
-- becomes UI-only later.
--
-- Merge tier today: code defaults → user defaults → project
-- overrides. A future INSTANCE tier (instance > user > project) is
-- deliberately NOT built now (owner ruling on PR #1840); adding it
-- later is an additive migration (an instance-scoped defaults table
-- consulted beneath the user map), not a row-shape change here.
--
-- Shape mirrors instance_settings (key text + value jsonb rows, the
-- settings system's tier model) scoped to the board domain. The key
-- CHECK constraint is the physical belt beneath the service
-- validation (unknown keys reject at the API first); growing the key
-- set (issue: "room to grow") is a deliberate migration: add the key
-- to pkg/board/config.go's vocabulary and relax this constraint.
--
-- Initial keys (#1832): reviewColumn (off|manual|auto),
-- autoAssignReviewer (bool), defaultWorkerSlots (int > 0).
--
-- No board_events rows ride these tables (explicit owner decision,
-- not a silent deviation): board_events is the project-scoped
-- idempotency+audit log for board item actions (project_id NOT NULL,
-- item_kind IN project|epic|task); the user defaults map has no
-- project, and a config PUT is replace-semantics — naturally
-- idempotent by construction. No consumer needs config provenance in
-- #1832 (PR-B automation and PR-D timeline do not read config
-- events). Revisit trigger: if config provenance is ever needed
-- (e.g. "why did reviewColumn flip"), add a coalesced
-- one-event-per-write later.

BEGIN;

CREATE TABLE IF NOT EXISTS public.board_config_defaults (
    user_id    text NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    key        text NOT NULL,
    value      jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT board_config_defaults_pkey PRIMARY KEY (user_id, key),
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

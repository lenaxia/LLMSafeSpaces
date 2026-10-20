-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Workspace phrase IDs (#1757): revert. Purely additive edge alias;
-- the UUID primary key and all internal joins are untouched.

DROP INDEX IF EXISTS idx_workspaces_phrase;

ALTER TABLE public.workspaces
    DROP COLUMN IF EXISTS phrase_id;

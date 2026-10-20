-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Short session IDs (design 0066, #1691): revert. The column is purely
-- additive (an edge alias resolved once at the API boundary; internal
-- joins stay keyed on the harness session_id), so dropping it loses no
-- state the harness does not already own.

DROP INDEX IF EXISTS idx_session_index_short;

ALTER TABLE public.session_index
    DROP COLUMN IF EXISTS short_id;

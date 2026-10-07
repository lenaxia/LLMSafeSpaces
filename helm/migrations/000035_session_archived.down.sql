-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE public.session_index
    DROP COLUMN IF EXISTS archived;

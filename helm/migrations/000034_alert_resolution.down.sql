-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

DROP INDEX IF EXISTS public.session_alerts_unresolved_idx;

ALTER TABLE public.session_alerts
    DROP COLUMN IF EXISTS resolved_at;

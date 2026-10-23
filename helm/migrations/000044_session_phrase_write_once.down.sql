-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- #1850: revert the short_id write-once pin. The trigger and its
-- function are pure guard rails — no data shape changes to undo.

DROP TRIGGER IF EXISTS trg_session_index_short_id_write_once ON public.session_index;

DROP FUNCTION IF EXISTS public.session_index_short_id_write_once();

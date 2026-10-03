-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Alert resolution semantics (D6 #998 follow-up). The session_alerts
-- feed is append-only 24h history; consumers that need the CURRENT
-- condition ("is a session hung now?") previously reconstructed it
-- client-side from busy snapshots — a race-prone reconstruction (the
-- hung-badge latch bug family). resolved_at makes the condition
-- first-party: NULL while the hang is live, set when the hang ends
-- (D6 sweep observation, the leave-Active watch event, or the
-- read-side heal aging a lost resolution). Consumers read
-- alert = 'session_hung' AND resolved_at IS NULL.

ALTER TABLE public.session_alerts
    ADD COLUMN IF NOT EXISTS resolved_at timestamptz NULL;

-- The resolution UPDATE targets one workspace's unresolved rows; the
-- partial index keeps that scan trivial and stays tiny (only live
-- hangs exist in it).
CREATE INDEX IF NOT EXISTS session_alerts_unresolved_idx
    ON public.session_alerts (workspace_id)
    WHERE resolved_at IS NULL;

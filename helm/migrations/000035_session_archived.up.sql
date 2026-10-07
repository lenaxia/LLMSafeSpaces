-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Session archiving (#1627). Archived is a platform-level read-only
-- marker on the session index: enforced at the API proxy layer (chat
-- sends rejected; history viewable; unarchive instant). The agent
-- itself is untouched — no agent-side state changes. Existing rows
-- default to NOT archived, and the API/SDK listing field is omitempty
-- (ABSENT = not archived), so no data migration is required.

ALTER TABLE public.session_index
    ADD COLUMN IF NOT EXISTS archived boolean NOT NULL DEFAULT false;

-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- #1499: user-level saved prompts. One row per prompt — CRUD semantics
-- (row-per-item natural delete/uniqueness), the mcp_servers/user_secrets
-- owner-scoped precedent. Content is NOT encrypted: a prompt is not a
-- credential (user_secrets tier) — it rides the workspace-name/settings
-- trust level. Name uniqueness is per user, exact match.

CREATE TABLE IF NOT EXISTS public.user_prompts (
    id         uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id    text NOT NULL,
    name       text NOT NULL,
    content    text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_prompts_pkey PRIMARY KEY (id),
    CONSTRAINT user_prompts_user_name_key UNIQUE (user_id, name)
);

CREATE INDEX IF NOT EXISTS idx_user_prompts_user
    ON public.user_prompts (user_id);

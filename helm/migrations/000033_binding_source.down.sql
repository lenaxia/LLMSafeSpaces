-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE public.user_secret_bindings
    DROP CONSTRAINT IF EXISTS user_secret_bindings_bind_source_check;

ALTER TABLE public.user_secret_bindings
    DROP COLUMN IF EXISTS bind_source;

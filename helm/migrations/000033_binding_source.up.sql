-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Binding provenance for user_secret_bindings. Records WHY a row
-- exists: 'manual' (user action — bindings UI, SetWorkspaceEnv,
-- SetBindings) or 'global_default' (policy materialization —
-- workspace-create seeding and the secretsreconcile policy step).
-- The reconcile loop adds/removes ONLY global_default rows; manual
-- rows are the user's explicit claim and always win.
--
-- Existing rows default to 'manual': retroactively distinguishing
-- legacy seed rows from manual ones is impossible, and manual is the
-- conservative label (policy convergence never removes it).

ALTER TABLE public.user_secret_bindings
    ADD COLUMN IF NOT EXISTS bind_source varchar(16) NOT NULL DEFAULT 'manual';

ALTER TABLE public.user_secret_bindings
    DROP CONSTRAINT IF EXISTS user_secret_bindings_bind_source_check;

ALTER TABLE public.user_secret_bindings
    ADD CONSTRAINT user_secret_bindings_bind_source_check
    CHECK (bind_source IN ('manual', 'global_default'));

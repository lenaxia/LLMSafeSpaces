-- #773: billing/usage query-index alignment.
--
-- GetUsage / GetUsageByWorkspace filter usage_events on (owner_id,
-- owner_type, event_time) but the only owner-scoped index covers
-- `period` (idx_usage_owner_period) — a different column, so the
-- user-facing usage reads fell back to scan-and-filter on busy
-- instances.
--
-- The billing-cron workspace sweeps (ListAllWorkspaceOwners,
-- ListAllWorkspacesForBilling) filter `WHERE deleted_at IS NULL`, but
-- idx_workspaces_deleted is the inverted partial (IS NOT NULL — finds
-- deleted rows). The covering INCLUDE makes both sweeps index-only.

BEGIN;

CREATE INDEX IF NOT EXISTS idx_usage_owner_event_time
    ON public.usage_events USING btree (owner_id, owner_type, event_time);

CREATE INDEX IF NOT EXISTS idx_workspaces_active
    ON public.workspaces USING btree (id) INCLUDE (user_id, storage_size)
    WHERE (deleted_at IS NULL);

COMMIT;

-- Reverse of 000026: drop the #773 query-index alignment indexes.

BEGIN;

DROP INDEX IF EXISTS idx_workspaces_active;
DROP INDEX IF EXISTS idx_usage_owner_event_time;

COMMIT;

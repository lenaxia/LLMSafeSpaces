-- Reverse of 000027: drop the atomic quota reservation table.

BEGIN;

DROP TABLE IF EXISTS public.usage_quota_reservations;

COMMIT;

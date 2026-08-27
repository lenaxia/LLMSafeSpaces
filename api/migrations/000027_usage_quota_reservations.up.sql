-- #768(c): atomic quota reservations.
--
-- CheckQuota was a bare read-then-return: no advisory lock, no
-- reservation — two concurrent requests from the same owner both
-- observed "remaining=1" and both proceeded, letting users exceed
-- request quotas by their concurrency factor.
--
-- usage_quota_reservations holds in-flight request slots. The
-- reservation path (metering.Service.ReserveQuota) serializes per
-- (owner, event_type) on a transaction advisory lock, counts committed
-- usage_events plus unexpired reservations against the limit, then
-- inserts the slot — the check-then-act window is atomic. Rows are
-- short-lived: expiry bounds the window (a reservation counts against
-- the quota only while the request is plausibly in flight, so the
-- over-restriction is momentary and in the safe direction), and the
-- metering reaper deletes expired rows.

BEGIN;

CREATE TABLE IF NOT EXISTS public.usage_quota_reservations (
    id BIGSERIAL PRIMARY KEY,
    owner_id text NOT NULL,
    owner_type text DEFAULT 'user'::text NOT NULL,
    event_type text NOT NULL,
    quantity bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT usage_quota_reservations_event_type_check CHECK ((event_type = ANY (ARRAY['compute_seconds'::text, 'llm_request'::text, 'llm_tokens'::text, 'storage_bytes'::text, 'api_call'::text]))),
    CONSTRAINT usage_quota_reservations_owner_type_check CHECK ((owner_type = ANY (ARRAY['user'::text, 'org'::text]))),
    CONSTRAINT usage_quota_reservations_quantity_check CHECK ((quantity > 0))
);

CREATE INDEX IF NOT EXISTS idx_quota_res_owner_active
    ON public.usage_quota_reservations USING btree (owner_id, owner_type, event_type, expires_at);

CREATE INDEX IF NOT EXISTS idx_quota_res_expiry
    ON public.usage_quota_reservations USING btree (expires_at);

COMMIT;

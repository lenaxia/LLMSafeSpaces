BEGIN;

-- Epic 59: one-time-use recovery codes for passkey-only users. Each code is
-- bcrypt-hashed (cost 12, same as password_hash) — a recovery code is a shared
-- secret and is stored exactly like one. A code is single-use: used_at IS NULL
-- means available; consuming sets used_at. A recovery-code login consumes one
-- code and forces re-enrollment of a new passkey.
--
-- 10 codes generated at passkey enrollment per the design; the partial index
-- keeps the "available codes for this user" lookup cheap.
CREATE TABLE IF NOT EXISTS public.user_recovery_codes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     character varying(36) NOT NULL,
    code_hash   text NOT NULL,
    used_at     timestamp with time zone,
    created_at  timestamp with time zone DEFAULT now() NOT NULL
);

CREATE INDEX IF NOT EXISTS user_recovery_codes_user_available_idx
    ON public.user_recovery_codes (user_id)
    WHERE used_at IS NULL;

ALTER TABLE ONLY public.user_recovery_codes
    DROP CONSTRAINT IF EXISTS user_recovery_codes_user_id_fkey;
ALTER TABLE ONLY public.user_recovery_codes
    ADD CONSTRAINT user_recovery_codes_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

COMMIT;

BEGIN;

-- Epic 59: WebAuthn / passkey credentials. One user may hold multiple passkeys
-- (the recovery story requires ≥2 to survive authenticator loss). The private
-- key never leaves the authenticator; this table holds only public material:
-- credential_id, public_key, sign_count (cloned-authenticator detection), and
-- display metadata. A compromise of this table leaks no secret material.
CREATE TABLE IF NOT EXISTS public.user_passkeys (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         character varying(36) NOT NULL,
    credential_id   bytea NOT NULL,
    public_key      bytea NOT NULL,
    attestation_type text NOT NULL DEFAULT 'none',
    attestation_format text NOT NULL DEFAULT 'none',
    aaguid          uuid,
    sign_count      bigint NOT NULL DEFAULT 0,
    transports      text[],
    name            text,
    created_at      timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at    timestamp with time zone,
    CONSTRAINT user_passkeys_user_credential UNIQUE (user_id, credential_id)
);

-- credential_id is globally unique (authenticator-generated); enforce it so two
-- users cannot bind the same credential.
CREATE UNIQUE INDEX IF NOT EXISTS user_passkeys_credential_id_idx
    ON public.user_passkeys (credential_id);

CREATE INDEX IF NOT EXISTS user_passkeys_user_id_idx
    ON public.user_passkeys (user_id);

ALTER TABLE ONLY public.user_passkeys
    DROP CONSTRAINT IF EXISTS user_passkeys_user_id_fkey;
ALTER TABLE ONLY public.user_passkeys
    ADD CONSTRAINT user_passkeys_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

COMMIT;

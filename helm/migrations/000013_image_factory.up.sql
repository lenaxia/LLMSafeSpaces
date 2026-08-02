-- Image Factory (design/0046 + design/0047).
--
-- Six tables implementing the Talos-style image factory:
--   1. image_factory_platform_config — single-row platform config (architectures)
--   2. image_factory_bases           — operator-approved bases, composite (name, version)
--   3. image_factory_extensions      — immutable-once-published catalog extensions
--   4. image_factory_known_failures  — auto-grown combination blocklist
--   5. image_factory_configs         — saved user/org/platform configs (with frozen resolved_values)
--   6. image_factory_builds          — one row per API dispatch (retry is inside the workflow)
--
-- Key design constraints enforced here:
--   - Extensions are immutable-once-published (design #7): there is no schema
--     mechanism for editing type/value/file_spec/supported_bases after insert;
--     only retired / review_requested / description are mutable. Enforcement
--     is in the store layer (admin PUT rejects build-field changes).
--   - Friendly-name uniqueness is scoped (design #25). A composite UNIQUE fails
--     for platform scope (NULL owner_id + NULL org_id; PG treats NULLs as
--     distinct). Use partial unique indexes per scope instead.
--   - resolved_values JSONB shape is pinned in design/0047 (ResolvedValues
--     map); not enforced by CHECK because the shape is application-defined.

BEGIN;

-- 1. image_factory_platform_config -------------------------------------------
-- Single-row table. The CHECK (id = 1) plus DEFAULT 1 makes it structurally
-- a singleton; the application seeds the row on first boot.
CREATE TABLE IF NOT EXISTS public.image_factory_platform_config (
    id              integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    architectures   text[] NOT NULL DEFAULT '{linux/amd64}',
    updated_at      timestamp with time zone NOT NULL DEFAULT now()
);
INSERT INTO public.image_factory_platform_config (id) VALUES (1)
    ON CONFLICT (id) DO NOTHING;

-- 2. image_factory_bases -----------------------------------------------------
-- Composite key (name, version): old versions persist so existing configs
-- keep working after a bump (design #8). is_default is the picker default;
-- at most one row per name should have it true (enforced in store layer to
-- avoid a cross-row partial index that would need re-encoding the rule).
CREATE TABLE IF NOT EXISTS public.image_factory_bases (
    name        text NOT NULL,
    version     text NOT NULL,
    image       text NOT NULL,
    tag         text,
    digest      text,                            -- wins over tag when set
    is_default  boolean NOT NULL DEFAULT FALSE,
    created_at  timestamp with time zone NOT NULL DEFAULT now(),
    updated_at  timestamp with time zone NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);

-- 3. image_factory_extensions ------------------------------------------------
-- IMMUTABLE build fields (type, value, file_spec, supported_bases) after
-- insert. retired / review_requested / description are the only mutable
-- columns. To change a build input, publish a new ID and retire the old.
CREATE TABLE IF NOT EXISTS public.image_factory_extensions (
    id                text PRIMARY KEY,
    type              text NOT NULL,
    value             text NOT NULL,
    file_spec         jsonb,
    supported_bases   text[] NOT NULL,
    retired           boolean NOT NULL DEFAULT FALSE,
    review_requested  boolean NOT NULL DEFAULT FALSE,
    description       text,
    created_at        timestamp with time zone NOT NULL DEFAULT now(),
    updated_at        timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT image_factory_extensions_type_chk CHECK (type IN ('apt', 'mise', 'file'))
);

-- 4. image_factory_known_failures -------------------------------------------
-- The blocklist. One row per genuinely-failed combination, written only on
-- a real failure (transient retry is inside the GH Actions workflow).
-- Keyed by (selection_hash, base_name) — selection_hash is the schematic
-- hash, so the blocklist is content-addressed just like configs. base_name
-- (not version) blocks across all versions of a base.
CREATE TABLE IF NOT EXISTS public.image_factory_known_failures (
    selection_hash  text NOT NULL,
    selection       text[] NOT NULL,             -- sorted extension IDs, for display
    base_name       text NOT NULL,
    explanation     text,
    failure_reason  text,                        -- raw log tail, admin-only
    detected_at     timestamp with time zone NOT NULL DEFAULT now(),
    retriable       boolean NOT NULL DEFAULT TRUE,
    PRIMARY KEY (selection_hash, base_name)
);

-- 5. image_factory_configs ---------------------------------------------------
-- resolved_values is a cached projection of the immutable extension values
-- at save time (design #10). NOT part of the hash preimage. Survives
-- extension retirement for accurate decode.
CREATE TABLE IF NOT EXISTS public.image_factory_configs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hash            text NOT NULL,               -- s-<sha256[:16]> over (selection IDs + base name)
    name            text NOT NULL,
    selection       text[] NOT NULL,
    resolved_values jsonb NOT NULL,
    base_name       text NOT NULL,
    base_version    text NOT NULL,
    scope           text NOT NULL,
    owner_id        uuid,                        -- member_id for member scope; NULL for platform
    org_id          uuid,                        -- org_id for org scope; NULL otherwise
    status          text NOT NULL DEFAULT 'building',
    created_at      timestamp with time zone NOT NULL DEFAULT now(),
    updated_at      timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT image_factory_configs_scope_chk CHECK (scope IN ('member', 'org', 'platform')),
    CONSTRAINT image_factory_configs_status_chk CHECK (status IN ('building', 'ready', 'rejected'))
);

-- Friendly-name uniqueness is scoped. A plain UNIQUE (scope, owner_id,
-- org_id, name) fails for platform scope (both NULLs; PG treats NULLs as
-- distinct). Partial unique indexes per scope encode the real rule.
CREATE UNIQUE INDEX IF NOT EXISTS idx_cfg_name_member
    ON public.image_factory_configs (owner_id, name) WHERE scope = 'member';
CREATE UNIQUE INDEX IF NOT EXISTS idx_cfg_name_org
    ON public.image_factory_configs (org_id, name) WHERE scope = 'org';
CREATE UNIQUE INDEX IF NOT EXISTS idx_cfg_name_platform
    ON public.image_factory_configs (name) WHERE scope = 'platform';

-- Lookup-by-hash for decode. A hash may be referenced by configs in
-- multiple scopes (member + platform both name the same combo), so this is
-- non-unique.
CREATE INDEX IF NOT EXISTS idx_cfg_hash ON public.image_factory_configs (hash);

-- 6. image_factory_builds ----------------------------------------------------
-- One row per API dispatch. Transient retry happens inside the GH Actions
-- workflow (design #12), so the API sees exactly one dispatch + one final
-- result. callback_token is the per-build secret the workflow POSTs back
-- (design #18); ConstantTimeCompare on the callback endpoint.
CREATE TABLE IF NOT EXISTS public.image_factory_builds (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    config_id       uuid NOT NULL REFERENCES public.image_factory_configs(id),
    hash            text NOT NULL,
    base_name       text NOT NULL,
    base_version    text NOT NULL,
    resolved_values jsonb NOT NULL,
    architectures   text[] NOT NULL,
    image_ref       text,
    digest          text,
    status          text NOT NULL,
    gh_run_id       bigint,
    callback_token  text,
    failure_reason  text,
    explanation     text,
    triggered_by    uuid,
    started_at      timestamp with time zone NOT NULL DEFAULT now(),
    finished_at     timestamp with time zone,
    CONSTRAINT image_factory_builds_status_chk CHECK (status IN ('dispatched', 'succeeded', 'failed'))
);

-- Coalescing probe: find an existing in-flight or successful build for a
-- (hash, base_version). Without this index the probe is a full scan; with
-- it, the POST /configs happy path is index-only.
CREATE INDEX IF NOT EXISTS idx_builds_dedupe
    ON public.image_factory_builds (hash, base_version, status);

-- Per-config build history.
CREATE INDEX IF NOT EXISTS idx_builds_config
    ON public.image_factory_builds (config_id);

-- Callback lookup by gh_run_id (the on-read derivation path queries GH by
-- run id, then finds the build row).
CREATE INDEX IF NOT EXISTS idx_builds_gh_run
    ON public.image_factory_builds (gh_run_id) WHERE gh_run_id IS NOT NULL;

COMMIT;

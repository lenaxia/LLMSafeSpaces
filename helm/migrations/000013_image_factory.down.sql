-- Image Factory rollback (migration 000013).
-- Drops the six tables in reverse dependency order. builds must drop
-- before configs (builds.config_id FKs configs.id).

BEGIN;

DROP TABLE IF EXISTS public.image_factory_builds;
DROP TABLE IF EXISTS public.image_factory_configs;
DROP TABLE IF EXISTS public.image_factory_known_failures;
DROP TABLE IF EXISTS public.image_factory_extensions;
DROP TABLE IF EXISTS public.image_factory_bases;
DROP TABLE IF EXISTS public.image_factory_platform_config;

COMMIT;

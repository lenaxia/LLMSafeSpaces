-- Reverse of 000023: restore the pre-fix floating default. Only
-- meaningful when rolling the application back to a schema-v10-or-older
-- binary, which both expects and re-seeds this value anyway.

BEGIN;

INSERT INTO instance_settings (key, value)
VALUES ('workspace.defaultImage', to_jsonb('ghcr.io/lenaxia/llmsafespaces/base:latest'::text))
ON CONFLICT (key) DO NOTHING;

COMMIT;

-- #1812: triggers gain the inline gate script columns.
--
-- script_language + script_handler carry an INLINE executable script
-- (language + handler source — workflow-script-node parity, NOT a path
-- pointer; script_path stays as the legacy pre-script). The handler
-- returns {proceed: bool, vars: {str: str}}: proceed=false exits the
-- fire with no LLM call; true fires the prompt with jinja-style
-- {{key}} substitution from vars plus a trailing all-vars metadata
-- block. Any script failure is fail-closed. Empty strings = no script
-- (the legacy prompt-only shape) — every existing row is unaffected.

BEGIN;

ALTER TABLE triggers ADD COLUMN IF NOT EXISTS script_language text NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS script_handler text NOT NULL DEFAULT '';

COMMIT;

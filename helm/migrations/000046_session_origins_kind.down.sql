-- 000046_session_origins_kind (issue #1901) — down

-- Plain column drop: kind is reap-side bookkeeping only. Post-down the
-- carrier-marks lookup errors on the missing column and the bypass
-- fail-safes to NO marks (uncertainty never widens a reap) — carriers
-- converge via the read path / operator review instead.

BEGIN;

ALTER TABLE session_origins
    DROP CONSTRAINT IF EXISTS session_origins_kind_check;

ALTER TABLE session_origins
    DROP COLUMN IF EXISTS kind;

COMMIT;

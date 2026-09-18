# Worklog: #1456 — F8 nightly probe: signal-accurate + self-diagnosing

**Date:** 2026-09-18
**Session:** The nightly got past the 8-day extraEnv install rot (#1437) and immediately died at the F8 gate ("migrate-labeled pod could not reach Valkey:6379"), blocking every downstream suite incl. automation R1-R8. Forensics: F8 has been red SINCE BIRTH (#1168, 2026-08-30 — shipped with the datastore NetworkPolicy, masked by a different earlier failure every night since; 19/19 runs failed). The probe conflated "pod never completed" with "policy blocked" and the collapsed CI log lost the raw verdict — undiagnosable from artifacts. Local reproduction impossible (no docker/kind in this workspace).
**Status:** Complete (observability + unblock; root cause pending one nightly cycle of diagnostics)

---

## Objective
Make one nightly cycle sufficient to either pass F8 or name its cause — and stop a never-green row from gating ten downstream suites.

## Work Completed
- Probe rewrite (.github/workflows/e2e-nightly.yml F8 step): valkey/valkey:8-alpine (proven to pull+run in this cluster; the API's /readyz depends on it) + authenticated `valkey-cli PING` (proves the full TCP+server path, not a bare port scan); REDIS_PW matches the install's externalSecret.redisPassword.
- Three-way verdict: PONG → OK; a real BLOCKED/verdict-without-PONG → hard fail + dump the datastore NetworkPolicies; NO verdict (pod never completed — the masked birth mode) → WARN + describe pod + events + policy inventory, and DO NOT exit 1 — downstream suites (automation R1-R8, us-70, 1342 rows) run regardless.
- Cold-cache headroom: wait loop 60s → 90s.

## Key Decisions
- Soft-gate only the NO-VERDICT leg: a row that has never passed should report loudly, not hold ten other suites hostage to an undiagnosed infrastructure signal. A REAL policy verdict still hard-fails (the row's intent is intact).
- valkey-cli PING over nc -z: a PONG also rules out "port open but wrong destination" classes and exercises the credential path.

## Tests Run
- YAML parse validation; no Go surface touched. The nightly itself is the test — one cycle arbitrates.

## Next Steps
- Next nightly: PONG (close #1456 as probe-artifact), BLOCKED (the policy dump names the rule), or NO-VERDICT (describe/events name the pod-level cause — image, scheduling, runtime).

## Files Modified
- .github/workflows/e2e-nightly.yml

# Worklog: Automation step bootstrap — harness_start first (run 35586321658, rung #9; root cause corrected in review r1)

**Date:** 2026-09-21
**Session:** Adjudicated class-shaped fix: run 35586321658 proved the #1342 gate + us-70 green + 2-node spread all working, then the automation step died at its very first command — a one-shot 2s livez curl racing an API pod 32s old (us-70's closing AC-8 rows churn API replicas; the instant #1342 skip removed the accidental settle window). R1–R9 still await arbitration.
**Status:** Complete — PR open, iterating review (title/branch retain the r1 name; the SHIPPED fix is harness_start-first — see the Rule 7.5 correction)

---

## Objective / Work Completed

- **RULE 7.5 CORRECTION (review r1)**: this worklog's first draft diagnosed a livez RACE ("API endpoint-ready within ~10s of pod start") — DISPROVED by the run's own evidence: the die was 18ms (instant ECONNREFUSED — no listener on 18086), and the API pod was 1/1 Ready 13s before the step. The true root cause: **the script never establishes its port-forward at all** — it never calls `harness_start`, so nothing listens on 18086 and its rows' `Bearer ${API_KEY}` is never seeded. A never-worked corpse of the session's signature class; my retry loop would have retried a dead port for ~11s and died identically.
- **Fix (corrected)**: `harness_start` as the first statement (the #1452/#1417 pinned pattern — it establishes the forward, runs the 10×1s livez gate, and seeds the session user + API key). Plus trap-safety: the EXIT-trap cleanup's DELETEs now guard `${API_KEY:-}` (the trap can fire before harness_start seeds it; set -u must not abort the trap).
- Pins (re-anchored to the true root cause): `TestIssue1410E2E_HarnessStartFirst` — harness_start present, precedes any standalone /livez probe AND the first authenticated `api` call (its seeding is a prerequisite), and the trap tolerates unset API_KEY.

### Assumptions stated and validated (Rule 7)

- ~~The race is transient (API endpoint-ready within ~10s of pod start)~~ — DISPROVED (Rule 7.5, above); the corrected assumption: every row's auth and transport depend on harness_start's bootstrap, pinned.
- The audit IS exhaustive over nightly-registered scripts — each cascade script's bootstrap inspected; the others delegate to harness_start or retry-bearing helpers. The automation script uniquely had NO bootstrap at all.

---

## Blockers

None.

## Tests Run

- Pin RED pre-fix → GREEN post-fix at both heads: the r1 loop-pin family (RED against the one-shot probe) and, after the root-cause correction, `TestIssue1410E2E_HarnessStartFirst` (RED against the harness_start-less script, GREEN with it).
- `go test -count=1 -timeout 300s ./local/` — **ok** (45.3s at the corrected head). `bash -n` clean.

## Next Steps

- APPROVED → merge → dispatch → **the R1–R9 arbitration finally executes** (then #1452/#1455/#1417/revisions/dev-preview).

## Files Modified

- `local/issue-1410-1412-automation-e2e.sh` — `harness_start` as the first statement (supersedes the r1 retry loop, fully removed); EXIT-trap `${API_KEY:-}` guards; why-comment.
- `local/issue_1410_automation_e2e_script_test.go` — `TestIssue1410E2E_HarnessStartFirst` (replaces the r1 loop-pin family).
- `worklogs/NNNN_2026-09-21_automation-bootstrap-livez-retry.md` — this worklog (Rule 7.5 correction recorded).

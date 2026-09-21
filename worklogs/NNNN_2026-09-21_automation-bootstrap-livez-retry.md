# Worklog: Cascade bootstrap livez retry — the automation script's one-shot probe (run 35586321658, rung #9)

**Date:** 2026-09-21
**Session:** Adjudicated class-shaped fix: run 35586321658 proved the #1342 gate + us-70 green + 2-node spread all working, then the automation step died at its very first command — a one-shot 2s livez curl racing an API pod 32s old (us-70's closing AC-8 rows churn API replicas; the instant #1342 skip removed the accidental settle window). R1–R9 still await arbitration.
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

- **Audit (the class)**: every nightly cascade script's bootstrap probe shape inspected. All already retry EXCEPT the automation script: 1452/1455/1417/dev-preview bootstrap via `harness_start` (us70-common.sh — 10×1s loop + confirming curl); us-70-revisions has `wait_local_livez`; us-68 has its own 10×1s loop. `local/issue-1410-1412-automation-e2e.sh:65-66` was the class's sole one-shot (`curl -sfm 2 … || die`).
- **Fix**: the automation bootstrap now runs the same 10×1s retry loop + confirming curl (the harness_start shape), with the why-comment (run 35586321658 evidence).
- Pins (TDD red→green): `TestIssue1410E2E_BootstrapLivezRetries` (loop present, probes livez, breaks on success, and PRECEDES the fatal die — dead-code ordering guard) + `TestIssue1410E2E_BootstrapLivezRetriesExecutes` (the REAL loop+die block against a counting fake curl: 4 transient failures absorbed → BOOT-OK; never-answers → loud die).

### Assumptions stated and validated (Rule 7)

- The race is transient (API endpoint-ready within ~10s of pod start) — the us-68/harness_start 10×1s shape is the established budget for exactly this forward+livez bootstrap across the suite family; the run's dump showed the pod Running-but-fresh at the die instant.
- The audit is exhaustive over nightly-registered scripts — each cascade script's bootstrap inspected (grep + read); the others delegate to shared retry-bearing helpers.

---

## Blockers

None.

## Tests Run

- New pins RED pre-fix → GREEN post-fix (incl. both executable legs).
- `go test -count=1 -timeout 300s ./local/` — **ok** (32.1s). `bash -n` clean.

## Next Steps

- APPROVED → merge → dispatch → **the R1–R9 arbitration finally executes** (then #1452/#1455/#1417/revisions/dev-preview).

## Files Modified

- `local/issue-1410-1412-automation-e2e.sh` — bootstrap retry loop + why-comment.
- `local/issue_1410_automation_e2e_script_test.go` — the two new pins.
- `worklogs/NNNN_2026-09-21_automation-bootstrap-livez-retry.md` — this worklog.

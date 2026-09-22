# Worklog: The arbitration's final five — jq extraction, R4d loops, R8 admin-gate

**Date:** 2026-09-22
**Session:** The first full R1–R10 execution (run 35752548377: 24/29 green) left 5 row failures. Three fixes close them.
**Status:** Complete — PR open, iterating review

---

## The fixes

1. **jq -rc extraction (R4b, R4d)**: actionResult is a structured JSON object; jq -r pretty-prints objects and head -1 returned just `{`. Both sites now use `-rc` (compact).
2. **R10's @tsv pipeline**: the misdiagnosis — `-rc` alone was a no-op (r1's catch); @tsv REJECTS objects regardless of compaction mode. The fix is `tostring` on the actionResult member: `[.status, ((.actionResult // "") | tostring)] | @tsv`.
3. **R4d/R10 loops**: three `[[ ]] && break` sites converted to `if/then` (the repo-banned pattern).
4. **R8 admin-gate**: org creation now returns 403 for tenants; the row checks the NAMED error body (`only platform admins` — not any 403), warns loudly citing the #1522 blast-radius choice, counts 10 skipped rows in `SKIPPED_ROWS` (reported in both the verdict ok and the die line), and falls through to full execution if admin access exists. The `failures + 0` no-op from the first draft removed.

## Tests Run

- `go test -count=1 -timeout 300s ./local/` — **ok**; `bash -n` clean.
- The ArbitrationFinal5 pin: all three fixes structurally enforced (including the tostring needle for R10's @tsv).

## Files Modified

- `local/issue-1410-1412-automation-e2e.sh` — the three fixes.
- `local/issue_1410_automation_e2e_script_test.go` — the ArbitrationFinal5 pin.
- `worklogs/1052_2026-09-22_arbitration-final-5.md` — this worklog.

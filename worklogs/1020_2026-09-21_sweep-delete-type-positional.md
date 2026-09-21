# Worklog: Sweep delete grammar — explicit resource-type positional (run 35547975296)

**Date:** 2026-09-21
**Session:** Same-lane one-liner from the 35547975296 FAST triage: #1498's loud-failure path fired on its first real execution and exposed a second stacked bug — the sweep delete passed bare UUID names with no resource type, so kubectl parsed the first UUID as the type (`error: the server doesn't have a resource type "e2e5d000-…"`). The historic xargs-kc no-op had hidden this forever; the sweep has never successfully deleted anything.
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

- `local/us-70-secret-delivery-e2e.sh`: both sweep deletes carry the explicit type — `… delete --wait=false workspace` — with xargs appending the bare names after it (valid kubectl grammar).
- `local/us70_harness_script_test.go`: both sweep fakes now model the tool's CONTRACT, not just invocation recording — a delete lacking an explicit `workspace` type positional exits 9 (grammar violation), which turns the clean-scenario pins red against the typeless form (verified red pre-fix) and green with the type; the pre-wave trace assertion tightened to `delete --wait=false workspace`.

### Assumptions stated and validated (Rule 7)

- kubectl grammar: `delete --wait=false workspace <name>…` is the valid form; a bare first positional parses as the resource type — proven by the production error verbatim in run 35547975296.
- The fakes' grammar model (type must appear positionally after `delete`) rejects the broken form and accepts the fixed one — verified both directions via the red→green run.

---

## Blockers

None.

## Tests Run

- `go test -timeout 120s ./local/ -run 'TestUS70PreWaveSweep|TestUS70PostWaveSweep|TestUS70Sweeps_'` — RED pre-fix (clean scenarios fail on the grammar violation), GREEN post-fix (10/10 subtests).
- `go test -timeout 300s ./local/` (full package) — **ok** (25.1s). `bash -n` clean.

## Next Steps

- APPROVED → orchestrator merges same-hour → re-dispatch → the four-run-blocked arbitration (R1–R9 + every remaining suite).

## Files Modified

- `local/us-70-secret-delivery-e2e.sh` — type positional on both sweep deletes.
- `local/us70_harness_script_test.go` — grammar-validating fakes + tightened trace assertion.
- `worklogs/1020_2026-09-21_sweep-delete-type-positional.md` — this worklog.

# Worklog: 1342 ExecuteSmoke — the last nightly harness under shared shims — Refs #1474/#1480/#1482

**Date:** 2026-09-19
**Session:** Orchestrator-assigned residual on branch `fix/1342-execution-smoke` (worktree wt-1453): 1342's execution smoke, unblocked by #1478's harness_start landing.
**Status:** Complete

---

## Objective

Close the ExecuteSmoke corpus: every nightly-registered harness script (now 10 of 10) demonstrably executes under the shared shims to a row verdict with a pinned traversal depth.

---

## Work Completed

1. **Env knobs for 1342's wait budgets** (`R1_TOOL_WAIT_S`/`R1_RESTART_WAIT_S`/`R1_ORPHAN_WAIT_S`/`R2_TOOL_WAIT_S`/`R2_RESPAWN_WAIT_S`/`R2_REPAIR_WAIT_S`) — the six hardcoded real-time `wait_for` budgets made the shim run burn ~12 minutes (real-time deadlines; predicates never true under shims). Defaults are the historical values — nightly behavior byte-identical; smoke sets 1s. Same pattern as the siblings' FIRE_WAIT_S/RUN_WAIT_S/R4_WAIT_S. Flagged for reviewer veto in the PR body (test-infra ergonomics, not a behavioral fix).
2. **`TestIssue1342E2EScript_ExecuteSmoke`** — traversal to 1342's own gate (`failure(s)` warn + exit 1), zero `whsec_`, depth pin "R2: tool never reached running state" (proves R1's rows and R2's setup executed; budget-independent text). Deterministic under shims: R1+R2 rows note_fail on semantic assertions, gate fires, 1.5s runtime.
3. Manual shim verification before encoding: full traversal, exit 1 at the gate, zero secret occurrences.

---

## Key Decisions

1. **Knobs over a date shim** — faking the clock to blow deadlines would be fragile and dishonest; env knobs follow established sibling precedent and change nothing by default.
2. **Depth pin on the R2 row text** rather than a timestamp-bearing timeout line (budget-dependent text would couple the pin to the knob value).

---

## Blockers

None.

---

## Tests Run

- `go test -run TestIssue1342 -v ./local/` — 6/6 PASS (smoke 1.5s).
- `go test -timeout 600s -count=1 ./local/` — ok (22.2s full package — the complete smoke corpus, 10 harness scripts).
- `go vet`, gofmt clean. Memory directive: local package only.

---

## Next Steps

- Review loop to APPROVED; orchestrator merges.

---

## Files Modified

- `local/issue-1342-graceful-restart-e2e.sh` — six wait-budget env knobs (defaults unchanged)
- `local/issue_1342_e2e_script_test.go` — + ExecuteSmoke
- `worklogs/NNNN_2026-09-19_1342-execution-smoke.md` — this worklog

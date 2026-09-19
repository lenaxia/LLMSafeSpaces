# Worklog: e2e WS_BASE UUID canonicality — 1452/1342 scripts un-gated for tonight's nightly

**Date:** 2026-09-19
**Session:** fix/e2e-wsbase-uuid-canonicality — apply the #1455-r3 cross-lane finding to the issue1452 and issue1342 e2e scripts before tonight's nightly opens the #1456 gate
**Status:** Complete

---

## Objective

The issue1452 and issue1342 e2e scripts carry the R0-fatal WS_BASE defect found on #1455's script (PR #1475 r3): 9-hex first groups (`e2e145200`/`e2e134200`) make `ws_id` construct non-canonical UUIDs that PostgreSQL rejects at the `seed_workspace_metadata` INSERT (`ON_ERROR_STOP=1` + `set -euo pipefail` → the scripts die at R0). Both are #1456-gated and have never executed; tonight's nightly opens the gate. Fix the bases to 8-hex and mirror the canonicality pin so the class cannot recur.

---

## Work Completed

- `local/issue1452-routine-session-index-e2e.sh`: WS_BASE → `e2e14520-0000-4000-8000-000000000000`.
- `local/issue-1342-graceful-restart-e2e.sh`: WS_BASE → `e2e13420-0000-4000-8000-000000000000`.
- Mirrored `TestIssue1452E2EScript_WorkspaceIDCanonical` and `TestIssue1342E2EScript_WorkspaceIDCanonical` (same shape as `TestIssue1455E2EScript_WorkspaceIDCanonical`: simulate `ws_id` across suffixes, assert canonical 8-4-4-4-12).

---

## Key Decisions

Scope held to exactly the delegation (two scripts + mirrored pins, nothing else). The fix is byte-identical in shape to #1475's r3 fix; the pin text carries the class provenance (#1455 r3 / PR #1464 cross-lane note 5739668437).

### Assumptions stated and validated (Rule 7)

- The defect is live in both scripts on main @ d15b0148 — validated by grep (9-hex groups present pre-fix).
- The 8-hex bases construct canonical UUIDs — validated by the new pins (green post-fix; the same pin class was mutation-verified red on the old base by #1475's reviewer).

---

## Blockers

None.

---

## Tests Run

- `go test -run 'TestIssue1452E2EScript_WorkspaceIDCanonical|TestIssue1342E2EScript_WorkspaceIDCanonical' -v ./local/` — both PASS.
- Edits verified landed via grep before commit.

---

## Next Steps

- Fast-track review; the orchestrator may merge same-hour ahead of tonight's nightly. The nightly run then exercises the previously-dead rows for the first time (pre-attributed expected UUID failures superseded by this fix).

---

## Files Modified

- `local/issue1452-routine-session-index-e2e.sh` — WS_BASE 8-hex
- `local/issue-1342-graceful-restart-e2e.sh` — WS_BASE 8-hex
- `local/issue_1452_e2e_script_test.go` — mirrored canonicality pin
- `local/issue_1342_e2e_script_test.go` — mirrored canonicality pin
- `worklogs/1005_2026-09-19_e2e-wsbase-uuid-canonicality.md` — this worklog

---

## CORRECTION (r2 — the reviewer proved the r1 causal story wrong; recorded per append-only discipline)

**What r1 got wrong:** r1's narrative ("9-hex WS_BASE → PG rejects the seed INSERT → dies at R0 tonight") described an UNREACHABLE state. `local/lib/us70-common.sh:51` sets `WS_BASE` at source time, so both scripts' `WS_BASE="${WS_BASE:-…}"` lines were dead code — the lib's canonical `e2e5d000` prefix applies at runtime; PostgreSQL never sees the 9-hex IDs. My validation proved presence-by-grep, not execution (Rule-7 lesson: grep proves a literal exists in a file, never that the line executes — the exact failure mode the rule exists to prevent).

**The REAL tonight-blocker (r1 missed it):** neither script calls `harness_start` → `seed_workspace`'s `OWNER_ID` guard aborts at R0 before any UUID handling. This is the #1455-r2 class, already pinned there.

**r2 diff (mission-scoped growth, disclosed to and approved by the orchestrator):**
- Both scripts: `harness_start` added (the actual un-gate), and WS_BASE switched to the UNCONDITIONAL assignment (the us-70-revisions r21 pattern) — the pinned literal is now the LIVE per-script prefix instead of dead code, restoring per-script isolation on the shared pool cluster.
- `local/issue-1455-scriptenv-e2e.sh`: same unconditional flip (my own script shared the dead-default flaw — harmless at runtime via the lib fallback, but its pin blessed the wrong layer); its companion regex updated to match the live form.
- Companions: canonicality-pin regexes now match the unconditional form (a `:-` default fails the pin as dead-post-source); `harness_start` needles added to 1452/1342; jq-compile pin added for 1342 (found + fixed a stub gap on its first run — `$m`).
- Comments corrected: the pins no longer assert the false "died at R0 on the UUID" event.

## Tests Run (r2)

- `bash -n` all three scripts — ok.
- `go test -run 'TestIssue1452|TestIssue1342|TestIssue1455' ./local/` — ok (all companion pins, including the new 1342 jq-compile pin and the updated canonicality regexes).

## Files Modified (r2 additions)

- `local/issue1452-routine-session-index-e2e.sh` — harness_start + unconditional WS_BASE
- `local/issue-1342-graceful-restart-e2e.sh` — harness_start + unconditional WS_BASE
- `local/issue-1455-scriptenv-e2e.sh` — unconditional WS_BASE (live form)
- `local/issue_1452_e2e_script_test.go` — harness_start needle; canonicality pin → live form
- `local/issue_1342_e2e_script_test.go` — harness_start needle; canonicality pin → live form; jq-compile pin
- `local/issue_1455_e2e_script_test.go` — canonicality pin → live form

## CORRECTION (r4 — record accuracy)

- The r2 test run selects **14** companion tests (1342: 5, 1452: 5, 1455: 4), not 15 as the PR body first claimed — count verified by run. The irony of a count-by-assertion landing in the PR that documents the presence-by-grep lesson is noted; stated validation must match execution, every time.
- The nightly schedule fact lives in e2e-nightly.yml (`0 6 * * *`); coordination-window information from outside the repo does not belong in PR bodies as fact.

## REBASE NOTE (post-r4 — main advanced: #1480 sister-smokes 5e776a66, #1477 dc8e2e85)

Rebased onto origin/main @ 4f03c6f6 per the orchestrator's resolution. On BOTH 1452 files main's version is taken wholesale — #1480 landed harness_start (as the first statement, with the livez-ordering subtlety and its own execution-smoke + ordering pins), making my 1452 hunks redundant; dropping them also drops my 1452 canonicality pin (it requires the unconditional form and would fail against main's still-dead `:-` default — that residual is the non-blocking follow-up class the reviewer already ticketed). Kept unchanged: the 1342 script hunks (harness_start + unconditional WS_BASE — 1342 remains uncovered by #1480), the 1455 script live-form flip, the 1342/1455 companion pins (live-form assert-all canonicality, 1342 jq-compile, 1342 harness_start needle), and this worklog. The r4-approved substance is unchanged; the only delta vs the approved head is removing hunks main already carries.

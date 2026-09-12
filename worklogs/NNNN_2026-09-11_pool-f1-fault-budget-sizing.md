# Worklog: F1 autopush-heal fault-budget sizing (pool-green)

**Date:** 2026-09-11
**Session:** Root-cause and fix the pre-existing `F1 autopush heal timeout` delivery-pool failure on main (epic-71 #1314, flagged unowned in comment 5628098598; agent: opencode-vesper). Scope: the F1 ROW — the pool's other red row (AC-1b XDG) is a separate root cause, fixed in PR #1326 (r2 correction: an earlier draft of this worklog said "AC-1b already fixed by #1317"; that claim was WRONG — #1317 fixed the unit-test env-leak manifestation only. The AC-1b cluster row failed byte-identically on main pre-#1317 (34549292454), on #1317's own head (34552707878), and on this branch (34566214570) — it is deterministically red wherever it runs and owed its own fix, now #1326: the row pinned the #1301 symlink mechanism that 1d0e5be1 legitimately replaced with a copy.)
**Status:** In Review (PR #1321; merge gate = [r9: superseded the r1-era "green F1 dispatch on the branch" wording] the combined-head full-green record — landed: run 34631603837 = SUCCESS on 31632400 with F1 PASS in the log; see Tests Run)

---

## Objective

Get the US-70 delivery pool green on main by root-causing the F1 row ("autopush heal did not converge env delivery within 300s") rather than masking it.

---

## Work Completed

### Root-cause analysis (run 34549292454 log forensics)

- F1 arms `LLMSAFESPACES_FAULT_INJECTION=${FAULT_COUNT}:POST:/internal/v1/pod-bootstrap` (FAULT_COUNT=24), probe burns 1, the boot phase burns its retries (bootstrapFetchAttempts=3 — [r9: reconciled with the sizing arithmetic's "boot retries 3"; the early-forensics "~4-6" figure included incidental divergent-pod pulls now counted in the slack term]), and the heal path must burn the REST one fault per pull before any pull can succeed.
- The heal driver is `api/internal/services/secretsreconcile` (60s level-triggered pass; per-workspace exponential backoff 5s doubling toward a 10-min cap, +25% jitter). Every eligible re-notify → agentpush POST /v1/resync-secrets → pod's in-process conditional pull → one faulted pod-bootstrap → burn 1.
- Arithmetic: burns at 5+10+20+40+80… cannot exhaust a 24-budget inside F1's 300s converge window. Observed: last fault burned 01:27:45, deadline ~01:27:04 — 41s past — then the heal still needed a clean pull + apply + session-aware restart + statusz mirror (≤60s). **Unsatisfiable by construction at 24** with any bounded cadence. This matches F6's own history: at 16 AND 24 the seam was budget-lottery (runs 34276744182, 34284549387), which is why F6 self-arms 6.
- Pod-side rate limit is NOT a factor: `resyncDefaultMinInterval = 2s`.
- AC-1b (the other pool-red) was NEVER fixed by #1317 (r3 correction of this bullet: #1317 fixed only the unit-test env-leak; the cluster row failed byte-identically on #1317's own head, 34552707878) — it is a separate root cause, fixed by #1326. F1 and AC-1b were BOTH blockers; this PR owns F1.

### Fix (TDD)

- `local/us70_harness_script_test.go` — new pin `TestUS70FaultsScript_F1BudgetSizedToConvergeWindow` + `f1BudgetCeiling = 8` and `f1BudgetFloor = 6` (review r1): workflow FAULT_COUNT must be within [6,8], with the sizing arithmetic in the comment. Verified red-first at 24 (ceiling) and at 5 (floor, review r1) before the fix.
- `.github/workflows/us-70-delivery-pool.yml` — `FAULT_COUNT: "24"` → `"8"` with the sizing rationale; updated the faults-suite step comment.
- `local/us-70-faults-e2e.sh` — default `:-24` → `:-8`; header + F6 budget-note comments updated (F1-sized; F6 self-arms — the "sized so F1+F6 both fit" rationale was stale).

### Product finding filed (not fixed here, cross-stream)

`secretsreconcile.notifyBackoffCap = 10min` (doubling from 5s) is out of family with epic-71's bounded-convergence thesis (1b's `LeaseConvergenceBound=30s`/`ReconcileCadence=15s`, 0b's `ParkedSweepInterval=60s`): worst-case post-fault secret-delivery heal approaches 10+ minutes. The cap's stated rationale (don't hammer legacy pods) is weakened by `legacy_format` divergence still being notified although it can never converge by notify. Proposed to the #1312 budget-table owner: cap ≤90s + skip notify on `legacy_format`. Deliberately NOT changed in this PR — a delivery constant retune belongs with the budget table, and the harness fix alone makes F1 deterministic.

---

## Key Decisions

- **Harness fix over constant retune in this PR.** F1 fails on budget arithmetic, not on the 10-min cap; changing the cap does not make 24 burns fit 300s anyway (cap 15s ⇒ ~255s burn + ~120s heal, still over). Right-sizing the arm follows F6's precedent and keeps delivery-critical constants with their owner (#1312 budget table).
- **8 = probe 1 + boot retries 3 (#1300 Fix B, `bootstrapFetchAttempts=3`) + divergent-pod slack 2 + heal burns 2.** Smaller risks the degrade-window sample racing (handled: warn+continue); larger re-enters lottery territory.

## Assumptions stated and validated (Rule 7)

1. Each eligible re-notify burns exactly one fault — validated in `agentpush.go` (notify → pod pull chain) + the run log (15 heal-window fires, gaps matching 5/10/20/40/80×jitter ladder).
2. Pod-side rate limit is not the bottleneck — validated: `resyncDefaultMinInterval=2s`.
3. Only F1 consumes the initial arm; F6 self-arms — validated by reading both rows; F2-F5 don't use the seam.
4. The lockstep pin has no other hardcoded 24 — validated by grep (workflow line 55 + script line 57 post-change; pre-fix 48/53).

## Blockers

None. (Product finding handed to #1312 owner — see above.)

## Tests Run

- `go test -timeout 120s ./local/` — green (new pin red-first at 24, green at 8; lockstep + all harness pins pass).
- `bash -n local/us-70-faults-e2e.sh` — clean.
- `gofmt -l local/`, `go vet ./local/` — clean.
- Cluster rows: **merge-gate run 34566214570 on head 97560fb3** — F1 PASS (~57s seed→converge), F2–F6 all green (faults step SUCCESS). The run's only red is the AC-1b row (pre-existing main-red; see the corrected Session note — fixed separately in #1326). Full-pool green evidence rides the combined dispatch on the final heads.

## Next Steps

- Open PR, ride the automated review cycle; dispatch the pool on the branch; merge only on a green F1 run (plus the rest of the suite).
- After merge: confirm the next scheduled pool run green on main (the 2026-09-09 last-green is stale).
- 0a (#1315) investigation continues per the reserved comment.

## Files Modified

- `.github/workflows/us-70-delivery-pool.yml`
- `local/us-70-faults-e2e.sh`
- `local/us70_harness_script_test.go`
- `worklogs/NNNN_2026-09-11_pool-f1-fault-budget-sizing.md` (this file)

# Worklog: Nightly AC-13 unblock — runner-appropriate scale + verified sweeps (35539089435 adjudication)

**Date:** 2026-09-20
**Session:** Adjudicated remediation pair (a)+(d) from dispatched-run 35539089435's triage (evidence: issuecomment-5753158300): nightly RESUME_SCALE 100→10, and the sweep silent-failure fix whose root cause the triage pinned down — xargs cannot exec shell functions.
**Status:** Complete — PR open, iterating review

---

## Objective

AC-13 (us-70) died at wave 2 on `FailedScheduling: Insufficient cpu` (5 pods 0/2 Pending, zero containers — capacity class, no product involvement). Two envelope defects: (a) the nightly's RESUME_SCALE=100 was authored for the pool's calibrated dind runner — infeasible-by-construction on the hosted runner (~10 standing workspace pods observed ceiling); (d) the pre-wave sweep reported "3 workspaces (90-92) deleted" while all three pods were still 2/2 Running nine minutes later — and the census's 10 standing pods (leaks included) were exactly why wave 2 could not schedule.

## Root cause for (d) — proven

Both sweeps (`local/us-70-secret-delivery-e2e.sh:621` pre-wave, `:855` post-wave) piped names into `xargs -r -n 20 kc --context … delete … >/dev/null 2>&1 || true`. `kc()` is a **bash function** (`local/lib/us70-common.sh:61`) — xargs can only exec binaries, so every sweep in history no-opped with `kc: command not found`, swallowed by the redirect, while the ✓ lines reported fiction. Consistent with all evidence: pods Running (not Terminating), zero Terminating strings in the entire run log, "✓ … deleted" logged.

## Work Completed

- **(a)** `.github/workflows/e2e-nightly.yml`: `RESUME_SCALE: 100` → `10`, with the envelope-honesty comment (nightly = regression detection on its envelope; pool retains 40-scale fidelity; wave-capped boots of 5; 100 was infeasible-by-construction). Job-timeout comment updated.
- **(d)** Both sweeps rewritten: xargs drives `kubectl` (the executable) directly; a failed delete **dies loudly**; termination is **verified** with a bounded poll (pre-wave: 60s, dies on leftovers — capacity is load-bearing; post-wave: 120s, warns on leftovers — the row's assertions are already done at that point); the ✓ lines now state "verified gone". PRE_SWEPT/POST_SWEPT selection awks unchanged (existing `TestUS70SweepSelection` pins stay green untouched).
- Pins (TDD red→green): `TestUS70AC13_NightlyScaleFitsRunner` (literal 10, anti-pin 100); `TestUS70Sweeps_ExecutableKubectlWithVerifiedOutcome` (anti-pin `xargs … kc`, anti-pin the swallow shape, verified-sweep pins); `TestUS70PreWaveSweep_Executes` — the REAL pre-sweep block executed against a fake kubectl with `kc` defined as the production function: clean delete → verified-gone + kubectl receives `delete --wait=false`; failed delete → loud die; wedged termination → die naming leftovers. (Test-harness bug found during red: the fake kubectl's delete-detection matched on arg position — fixed to scan args.)

### Key decisions

1. **Scale 10** (top of the sanctioned 8–10 band): with (d) freeing the 3 leak pods, peak standing ≈ 2 earlier-row pods + 10 batch = 12, inside the empirically observed 10–12-pod ceiling; boots stay wave-capped at 5 (the 25-boot crash-loop boundary is documented pool history).
2. **Pre-wave dies / post-wave warns on wedged termination**: pre-wave capacity is load-bearing for AC-13 itself (a silent leak there IS the failure mode being fixed); post-wave runs after the row's assertions — loud warning, never blocks the remaining rows.
3. **Script defaults untouched** — the pool's RESUME_SCALE default (100) and its semantics keep full fidelity; only the nightly's env changes.

### Assumptions stated and validated (Rule 7)

- xargs cannot invoke shell functions — bash/xargs semantics; the failure shape reproduced exactly by the executable pin (old form: fake kubectl never sees the delete).
- Node ceiling 10–12 workspace pods — from the 35539089435 census (10 Running incl. leaks; wave 2's +5 failed).
- Existing sweep pins unaffected — verified by the full `./local/` package passing with the selection awks unchanged.

---

## Blockers

None.

## Tests Run

- `go test -timeout 120s ./local/ -run 'TestUS70AC13_NightlyScaleFitsRunner|TestUS70Sweeps_|TestUS70PreWaveSweep_Executes'` — RED pre-fix (all), GREEN post-fix.
- `go test -timeout 300s ./local/` (full package) — **ok** (22.2s).
- `bash -n local/us-70-secret-delivery-e2e.sh` — clean; `e2e-nightly.yml` — YAML-validated.

## Next Steps

- Iterate review to APPROVED → orchestrator merges → manual dispatch → R1–R9 arbitration (blocked four consecutive runs behind this pair) plus #1342/#1452/#1455/revisions/dev-preview/us-63/benchmarks first executions.

## Files Modified

- `.github/workflows/e2e-nightly.yml` — RESUME_SCALE 10 + comments.
- `local/us-70-secret-delivery-e2e.sh` — both sweeps verified, executable-driven, loud.
- `local/us70_harness_script_test.go` — the three new pin tests.
- `worklogs/NNNN_2026-09-20_nightly-ac13-scale-and-sweep.md` — this worklog.

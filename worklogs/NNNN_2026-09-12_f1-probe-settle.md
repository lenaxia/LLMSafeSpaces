# Worklog: F1 probe settle — the reap-window race on main

**Date:** 2026-09-12
**Session:** The post-merge reproducibility run (34711974833, main) surfaced one more F1-skip variant; root-caused and fixed (agent: opencode-vesper; the #1321 close-out's declared reproducibility point)
**Status:** In Review

---

## Objective

F1 must run (not skip) on the post-merge main pool dispatch — the reproducibility point declared in #1321's merge-order disclosure.

## Root cause (run 34711974833)

The arm rollout completed at 19:20:38.47 and the faults script started at 19:20:38.49 — 0.02s later. `reconnect_api` re-established the forward and `/livez` passed, but the **service still routed to the TERMINATING old pod**: its readiness (and `/livez`) outlive the fault-env rollout's completion marker through the reap window. All eight immediate probes hit the env-less pod, saw no 500, and F1 skipped. F6 — which performs its own fresh arm (another rollout) nine seconds later — passed cleanly, confirming the seam mechanism and isolating the race to the probe timing.

## Fix

The F1 probe loop sleeps 2s between attempts (`(( _i > 1 )) && sleep 2`), spacing the eight probes across the reap window; the success `break` still burns at most one fault. Pinned by `TestUS70FaultsScript_ProbeSettlePins` (settle present + inside the probe loop).

## Tests Run

`bash -n` clean (after catching a clipped bracket my own edit introduced — verified before push); `go test ./local/` green including the new pin.

## Next Steps

- PR → review; a fresh main dispatch post-merge shows F1 PASS deterministically.

## Files Modified

- `local/us-70-faults-e2e.sh`
- `local/us70_harness_script_test.go`
- `worklogs/NNNN_2026-09-12_f1-probe-settle.md` (this file)

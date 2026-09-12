# Worklog: F1 probe settle — the reap-window race on main

**Date:** 2026-09-12
**Session:** The post-merge reproducibility run (34711974833, main) surfaced one more F1-skip variant; root-caused and fixed (agent: opencode-vesper; the #1321 close-out's declared reproducibility point)
**Status:** In Review

---

## Objective

F1 must run (not skip) on the post-merge main pool dispatch — the reproducibility point declared in #1321's merge-order disclosure.

## Root cause (run 34711974833)

The arm rollout completed at 19:20:38.47 and the faults script started at 19:20:38.49 — 0.02s later. `reconnect_api` re-established the forward and `/livez` passed, but the **service still routed to the TERMINATING old pod**: its readiness (and `/livez`) outlive the fault-env rollout's completion marker through the reap window. All eight immediate probes hit the env-less pod, saw no 500, and F1 skipped. F6 — which performs its own fresh arm (another rollout) nine seconds later — passed cleanly, confirming the seam mechanism and isolating the race to the probe timing.

## Fix (r11 — the r10 settle-only draft was insufficient, caught in review)

The settle alone cannot work: `kubectl port-forward svc/…` pins ONE pod at establishment, and if it resolved to the terminating pre-arm pod, no probe through that forward can ever observe a 500 (401-while-alive → 000-on-reap; the 660s termination grace per helm/values.yaml:78 dwarfs any settle — the r10 comment's "covers the grace in practice" clause was false and is corrected here). The fix is `probe_seam`: alternating probe rounds (2s settle between tries) and **forward re-establishment** (`reconnect_api` — fresh svc resolution) between failed rounds, bounded at 5 rounds. Both F1 and F6 route through it — F6's identical race (its own arm rollout precedes its probe) is covered, not just F1's. Pinned by `TestUS70FaultsScript_ProbeSettlePins` with **loop-membership enforcement** (the pin slices probe_seam's body; the r10 pin's placement claim was mutation-bypassable).

## Tests Run

`bash -n` clean; `go test ./local/` green including the rewritten pin. Behavioral evidence: pool dispatch on this branch (see PR thread).

## Next Steps

- PR → review with the branch dispatch as the merge-time behavioral evidence.

## Files Modified

- `local/us-70-faults-e2e.sh`
- `local/us70_harness_script_test.go`
- `worklogs/NNNN_2026-09-12_f1-probe-settle.md` (this file)

# Worklog: Design 0061 §10/§11 — the migration-recovery + exit-85 + posture e2e (closes #1546/#1548's evidence)

**Date:** 2026-09-24
**Session:** Critical-path lane on branch `feat/0061-migration-e2e` (wt-1453).
**Status:** Complete

## Objective

The end-to-end migration exercise that formally closes the #1546/#1548 incidents (relay flip DOA + split-brain), gating v0.34.8: the full migration ARC (relay-only delivery → relay failure → direct-read fallback → RECOVERY), M1's exit-85 crash-loud arming live, and the posture-gate convergence assertions — per design 0061 §11's three owner rulings.

## Work Completed

**`local/us61-migration-recovery-e2e.sh`** (the lane's deliverable; rides the standing post-flip install, the drill family's conventions):
- R1 — the fallback precondition (compact re-seat of the M2 arm, standalone-runnable): tear the handoff → resync → raw canary delivers, `relay_fallback_deliveries_total` DELTA-increments (baseline pre-tear — a stale series is not a fresh delivery), `CredentialsStaged=False/relay_fallback_delivery`.
- R2 — RECOVERY (first live pin of the arc's recovery half): suspend/activate forces the reconcile → the controller RE-CREATES the deleted handoff (upsertRelayHandoff's NotFound→Create path, staging.go) → the fresh batch carries the token again → `CredentialsStaged` heals to True. The #1548 stranded-workspace class closes: a migration blip is a bounded window.
- R3 — EXIT 85 (§11 ruling 2, live): the unarmable shape (router URL → http://127.0.0.1:9, deliberately NO wait flag — the exit code IS the assertion) → the pod TERMINATES with the DISTINCT code 85 (pod jsonpath, not log archaeology) + M1's exact refusing line in the previous container's log → restore (URL captured from the live args pre-break) → Ready again. Fully reversible.
- R4 — POSTURE CONVERGENCE (§11 ruling 3's gate assertions against the exercised cluster): all-Ready in BOTH rendered namespaces, the 45s stability window (identical restart snapshots), the armed line, zero forbidden in any pod log — failure-checked enumeration (the M3 gate lesson: process substitution is invisible to set -e), unreadable logs are red, prior crashed containers included (R3's exit-85 corpse must be denial-free — arming refusal is not an RBAC denial).

**Nightly wiring** (the recorded-execution vehicle): two steps after the sweep — the M2 fallback script (w5's `us72-m2-fallback-e2e.sh`, previously unwired — rides this lane's vehicle) then mine, both ARMED on the drill chain (`!cancelled()` + the full prerequisite ladder; the recovery additionally gates on the fallback arm — a broken relay plane makes posture assertions infrastructure noise, not evidence). Distinct PORTFWD_PORTs (18090/18091 — the port-isolation pin).

**Pin test** (`local/us61_migration_recovery_e2e_script_test.go`, 8 functions): bash syntax, rows-in-order (the EXECUTED log lines, not the header prose), the recovery arc's load-bearing assertions, the exit-85 contract (exact literals + the no-wait window), the posture row mirroring the gate's assertions, harness conventions (all-hex WS base — the M2 script's r5 lesson; delta-pinned counter), the nightly wiring (ordered, armed).

**Pin amendments (deliberate, the pins working as designed)**: the #1541 lane-hardening armed-set pin gained the two migration steps (exact conditions; new drillDone/m2Chain constants — the drill's OWN success is the precondition, so the chain extends past drill-shape) — the exactness pin exists to force exactly this conscious decision.

## Key Decisions

1. **A new script, not edits to w5's**: their PR (#1559) is in review — same-file edits invite conflicts; the arc rows (recovery/arming/posture) are cleanly separable. The nightly runs both, ordered.
2. **R1 re-seats the degraded state** rather than depending on w5's script having run: standalone-runnable is a family convention; the compact fallback rows double as the arc's precondition.
3. **The exit-85 lever is the router URL** (the chart's `workspaceRouterURL` → the controller's `--llm-relay-router-url`): the exact unarmable shape M1's startup guard rejects; restore-from-live-args (not a hardcoded URL) keeps the row portable.
4. **Layered deps, not rebases-yet**: the branch merges M2 (#1559 @ 11e19089) and M3 (#1556 @ 8d7fb40e) heads per the orchestrator's instruction; the moment they merge, rebase onto main (the merge commits collapse). If review reshapes either surface, adapt.

## Tests Run

- `./local/` full package GREEN (24.3s) — incl. all 8 new pins + the amended hardening/sweep pins.
- `./pkg/secrets/` green (the layered M2 surface), `go build ./...` clean.
- `bash -n` on the script; the nightly YAML parses (yaml.safe_load).
- gofmt clean, golangci-lint (new-from-rev) 0 issues.

## Blockers

None. Dependencies: M2 (#1559) and M3 (#1556) merge first (the recorded order); this PR rebases onto main at that point.

## Next Steps

- Review loop to APPROVED; orchestrator adjudicates + merges (after M2/M3).
- First nightly execution = the recorded evidence for #1546/#1548 closure (rows R1–R4 + w5's migration rows).

## Files Modified

- `local/us61-migration-recovery-e2e.sh` (new) — the arc + arming + posture script
- `local/us61_migration_recovery_e2e_script_test.go` (new) — the 8 pins
- `.github/workflows/e2e-nightly.yml` — the two ARMED steps (ports 18090/18091)
- `local/us72_nightly_lane_hardening_test.go` — the armed-set amendment (drillDone/m2Chain + the two entries)
- `worklogs/NNNN_2026-09-24_migration-recovery-e2e.md`

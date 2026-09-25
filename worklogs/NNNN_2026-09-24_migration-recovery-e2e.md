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

## Review Round 1 + THE ROOT CAUSE (this PR is now the incident's closure)

**The drift chase (the posture gate's first real §5.4 catch):** the gate's rebuilt Assert 2 named "wiring drift" on my discriminator run (flag ON the spec, controller healthy, armed line absent from BOTH containers). Hermetic determination: Dockerfile build path CLEAN (distroless ENTRYPOINT = the binary, no wrapper — theory 1 refuted, no release-path drift); the tree binary reproduced the no-arm boot locally; the out-of-range-TTL probe (must exit 85 instantly if the flag parses true) sailed past — **`registerRelayFlags` returned the struct BY VALUE while flag.BoolVar bound into the local: every parsed flag wrote an orphaned original; main's copy stayed zero. `relayFlags.enabled` was ALWAYS false. The controller has NEVER armed under any values, on any deployment, since e81a500f (#1448, US-72.3, 09-18) — v0.34.4 through v0.34.7 all carry it. This is #1548's literal root cause: the reported split-brain (API emission active, staging never runs) is this bug's signature.**

**The fix:** `registerRelayFlags() *relayFlags` (the parse targets and the consumed struct are the same memory) + the `registerRelayFlagsInto(fs)` seam. Red/green: pre-fix TTL probe boots past silently; post-fix exits 85 with the exact refusal literal. Regression pins parse a fresh FlagSet against the RETURNED struct (both the argv values and the documented defaults — the orphaning also zeroed the namespace/TTL defaults the caller consumed).

**The review's three blockers, worked:** (1) ExecuteSmoke landed (shim-driven traversal to the verdict gate; R3's ORIG_URL die converted to a row-verdict with the shipped-default fallback so the smoke traverses; the setup's staged-state gate became R0, a note_fail row — the M2 script's shape); (2) "Closes #1548" → Refs (AC5/AC6 are owner-side; the root-cause fix lands HERE — the closure map states it); (3) both suspend levers poll `wait_phase Suspended` before activate (the run-36049595521 409 class — no fixed sleeps). Plus: the canonical-WS_BASE pin, the source-truth drift pins for the three grepped literals, R4's stability snapshot now covers BOTH namespaces (gate parity), and the .gitleaks us72_m2 entry deduped (main's r8 owns it). The earlier "R3's exit-85 corpse must be denial-free" claim is reworded honestly: the restore creates a new pod, so R4's --previous is a belt, not the corpse check (the worklog overstated it).

## Review Rounds 2 + 3 (the peel's close-out)

**Gate GREEN — twice:** run 36077812346 on `fd1990a5` (the first green in branch history) and again on `17483cff` (run 36087507236). The OK line at head: the api-leader-election AND api-platform-info bindings live at install+0 and +40s; the API SA can get leases and list deployments. Forbidden-hit log audit, all FOUR greens (`gh run view <id> --log | grep -ci forbidden`; 36077812346 + 36077809401 on `fd1990a5`, 36087507236 + 36087505425 on `17483cff`): **the invariant that survives every observer is ZERO actual pod-log denial lines in every run** — every grep hit, in every fetch by every observer, is the workflow's own text (Assert 3's echoed script/header/env block + its "OK: zero forbidden lines" line, or the bindings step's echoed FAIL carrier). The raw COUNT follows gh's per-FETCH step-name resolution, not the run: Assert 3's step NAME contains "forbidden" and its output block is 38 lines — names resolved → 38+1 = 39; column renders UNKNOWN STEP → content-only 6+1 = 7. Observed values, attributed: 36077812346 = 39 (author, four consecutive fetches) / 7 (r4 audit; r6 fetches); 36087507236 = 39 (author) / 7 (r6 fetches, all-UNKNOWN-STEP that day); 36077809401 = 7 (author + r4 + r6); 36087505425 = 39 (r4 audit) / 7 (author; r6). Three of the four runs have shown BOTH values across observers and fetches — no run-role or SHA taxonomy survives. The count never was the evidence; zero denials is.

**r2's two blockers (both found by review, both real, both fixed red-first at `17483cff`):** (1) the **glued-doc loss** — `rbac.yaml`'s `{{- end }}` + comment/`if` chain ate the `---`, fusing the api-platform-info RoleBinding into the relay-safe ClusterRole doc; last-wins decode drops the first at apply (the SAME mechanism as the original api-leader-election apply-loss — the chart's own template whitespace, never helm; the drafted upstream filing was RETRACTED on this mechanism). Fixed by the separator restore; the doc-boundary class pin (yaml.v3 duplicate-key over every rendered doc) ran RED at the unfixed head on the glued doc first. (2) the **`registerFreeModelsFlags` orphaning** — the byte-identical twin of the relay bug; #1546 Defect 3 re-armed. Fixed with the same shape (struct + pointer return + `Into` seam), twin pins mutation-verified. The gate step became **"Require the RBAC bindings live"** (both bindings at +0/+40s + both can-i probes), and all three r2-requested pins landed.

**Class sweep (orchestrator-run):** every flag-registration site is pointer-safe at head — relay + free-models (both fixed here), `registerUploadStagingFlag` + `registerAgentdBinaryHashFlags` (return the pointers they bind — immune by construction). Value-copy class CLOSED tree-wide.

**r3 verdict:** all substantive work confirmed mutation-verified end-to-end; #1546 closure AGREED by the skeptical pass. Blocking = two now-false comments this PR's own step promotion left in the gate file (the "Diagnosis-only (never gates)" preface; the helm-pin experiment framing that a settled result outgrew) — both rewritten at this head. Record nits taken: PR title's `closes #1546/#1548` keyword adjacency dropped; the sweep enumeration corrected to the full registrar list. The doc-boundary posture table (2 postures) is recorded as cheap polish — the skeptical pass strict-decoded six postures clean.

**Still open:** r4 on the comment-fix head; the rebase-on-merge over #1556's M3 surface must actually happen (recorded in the PR's dependency-layering note).

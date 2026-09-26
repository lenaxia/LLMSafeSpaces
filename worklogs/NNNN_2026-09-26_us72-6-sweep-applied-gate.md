# Worklog: US-72.6 sweep — the APPLIED conjunct of the convergence gate (run 36216981147)

**Date:** 2026-09-26
**Session:** Run 36216981147 (main @ 0a5b284f, first run with the #1570 fix): drill PASSED again; the sweep EXECUTED with the HIT diagnosability working — and the HIT lines answered the question the directive asked. R3 PASSED (the boot scrub fired on the residue-bearing resume: LegacyKeysScrubbed=True/KeysRemoved config=1, post-boot sweep zero — the cross-container chain verified live). R1/R2 failed with named paths: /agentd-config/agent-config.json, /workspace/.local/opencode/auth.json, /sandbox-runtime/rt/auth.json.
**Status:** Complete

---

## Objective

Close the last sweep gap. The directive prescribed scrub-coverage extension to both hit paths; the evidence showed a different root cause — this PR implements what the evidence supports and documents the deviation.

## Work Completed

### The diagnosis (from the HIT paths + the same run's passing rows)

- **R1's 3 hits are the M2 fallback window's live-delivery bytes, still present because the token batch had STAGED but not APPLIED.** The sweep's setup gated on `CredentialsStaged=True` — the CONTROLLER-side staging verdict. Run 36216981147 proved that verdict can stand True while the pod still runs the fallback raw batch (the fresh workspace's live config carried the raw canary). The decisive control: the DRILL's R2 — same cluster, same flow, same surfaces — swept ZERO, and its gate additionally verifies `apiKey=token` in the live config before sweeping. The token apply OVERWRITES the fallback surfaces (drill R2's zero includes rt/auth.json); the residue is apply-TIMING, not missing scrub coverage.
- **R2's 2 remaining hits are the same residue** (the subcommand scrub correctly removed the planted legacy file; the live surfaces were still pre-apply). With the setup gate fixed, R2's zero-check runs on a converged baseline.
- **The /workspace/.local/opencode/auth.json hit is the symlink view of rt/auth.json** (grep follows the link — one underlying file, two reported paths).

### Why the scrub is NOT extended to the hit paths (the deviation, per the evidence)

The directive prescribed cleaning "the agentd agent-config.json key fields and the rt store's auth.json". Those are the LIVE delivery surfaces: in the converged era they carry TOKENS. The scrub cannot distinguish a token entry from a raw-key entry by shape — stripping key fields there would strip the pod's working credentials (the strand-the-pod failure mode the M2 fallback exists to prevent). The correct mechanism already exists: the token APPLY overwrites these surfaces (proven by drill R2's zero + this run's R3 post-boot zero after config_converged held). The sweep's gate — not the scrub — was the gap. Deviation noted here, in the PR body, and to the orchestrator.

### The fix

The setup gate becomes TWO conjuncts: `CredentialsStaged=True` (the cheap controller-side prerequisite, kept) AND the pod-fresh APPLIED evidence — `config_converged()` (the live config's apiKey is the token, not the canary; baseURL is the router), polled up to 300s. The `config_field`/`config_apikey`/`config_baseurl`/`config_converged` helpers (built for R3 in #1570) are HOISTED to the setup block and shared by both gates. When the conjunct holds, R1/R2 evaluate the converged posture; the fallback window's transient bytes are gone by overwrite, not by scrub.

### The directive's sequencing question, answered

"With full coverage, does R1 pass on a standing install whose residue came from the drill's own R3→R4 window?" — YES, and it does not depend on a boot scrub event: the token apply overwrites those paths (the drill's own R2 zero-canary on the same surfaces after its R3 rollback window is the standing proof; this run's R3 post-boot zero after `config_converged` is the resume proof). The design-semantics question (whether the FLIP itself should trigger a one-time scrub of live surfaces) does not arise: the apply IS the cleaner for live surfaces; the boot scrub is for PVC-residue shapes only. No owner decision needed on that specific question — recorded for the owner anyway.

## Key Decisions

1. **Gate over scrub**: the evidence (drill R2 zero, R3 post-boot zero) attributes the hits to apply-timing; scrubbing live token-bearing surfaces would be both unnecessary and harmful.
2. **Keep both conjuncts**: CredentialsStaged is the cheap controller-side prerequisite; config_converged is the proof. (r1 correction of this very bullet: the original text called CredentialsStaged "the cheap fast-fail" — the implemented code does NOT fast-fail; after a STAGED failure the APPLIED poll still runs its full window, which is arguably better diagnostics: the two failure rows distinguish never-staged from staged-never-applied.)
3. **K1 carve-out unchanged** (non-frontables excluded, owner decision pending).

## Blockers

None.

## Tests Run

- `go test ./local/ -run 'TestUS72Sweep' -count=1 -v` — all pins green including the two new: `TestUS72Sweep_SetupGateIsAppliedFresh` (the APPLIED conjunct present in setup, before R1) and the updated `TestUS72Sweep_R3GateIsPodFresh` (helpers hoisted; R3 still never polls the stale condition).
- `go test ./local/ -count=1` — full package green (31.8s). `bash -n` clean. `make repolint` — all checks passed.

## Next Steps

1. Merge → the closure nightly: with the drill green (#1559/#1570 lineage) and the sweep's gates honest, the run should produce the sweep-green evidence for the closure draft's last open row.
2. The owner-decisions row (K1 carve-out; envtest items) remains the only other open checklist line.

## Files Modified

`local/us-72-rogue-agent-sweep.sh` (the two-conjunct setup gate; the hoisted helpers), `local/us72_sweep_test.go` (the setup-gate pin + the R3-pin update), this worklog.

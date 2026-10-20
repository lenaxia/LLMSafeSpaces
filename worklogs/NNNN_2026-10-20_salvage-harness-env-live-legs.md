# Worklog: salvage — #1759 harness env hygiene + live permission-tier legs over the merged base

**Date:** 2026-10-20
**Session:** Salvage PR A of the closed #1764 (Refs #1759), rebased onto current main (#1762's landed implementation): the integration-harness env construction hardening and the live external_directory legs with verdict-only probe logging.
**Status:** Complete

---

## Objective

Carry the #1764 lane's harness and live-leg deliverables onto the merged #1762 base, with the env change honestly relabeled after the mandated red-first experiment falsified its original premise.

---

## Work Completed

- **Harness env construction** (integration_harness.go): inherited copies of every child-override key (OPENCODE_CONFIG, XDG_DATA_HOME, XDG_CONFIG_HOME, HOME, OPENCODE_SERVER_PASSWORD) are stripped before the test values are appended; ONE map drives both, so the strip and append lists cannot drift. Framed as deterministic-env hygiene — correct by construction under EITHER duplicate-resolution semantics.
- **Premise correction (Rule 11 Phase 2)**: the harness's original 2026-09-13 comment claimed duplicated env vars resolve first-match in the child's libc. The mandated mutation experiment (append-only shape restored via file-copy mutation, pin re-run on this workspace pod whose runner env carries XDG_DATA_HOME=/workspace/.local) showed the child honors the APPENDED values — the pinned 1.18.15 is a Bun-built binary with last-match/process.env-overwrite semantics. The first-match narrative is preserved as history, marked contradicted-by-experiment. Consequence recorded in the PR body: the #1759 first-green-run credential leak was enabled by the new "/workspace/*" config allow alone; the earlier "env leak adopted the pod's auth store" mechanism was inference, not observation.
- **Behavioral pin** (TestIntegrationHarness_ChildHonorsSyntheticDataHome): the booted child's structured log must appear under the harness's synthetic XDG_DATA_HOME — catches any regression that stops delivering overrides. Labeled behavioral, not red-first proof (passes under both env shapes on this binary, by design of the experiment).
- **Live legs transferred onto the #1762 floor** (permissiontiers_integration_test.go): tierBootExtDir helper; the ordering probe with an allow-only control leg (specific deny beats earlier broad allow — findLast observed); pod-qualified /home/sandbox and /workspace allow legs (cgroup-pin skip precedent); the deny-preservation leg (cat of a .ssh child still denies WITH the root allows rendered, our rule quoted in the engine dump). All verified green against the LANDED floor — every key the legs assert against exists in #1762's permissiontiers.go (checked before transfer).
- **Verdict-only logging for credential-shaped probes** (tierBashLeg): a regression that lets such a command execute must never pour credential bytes into the test log — booleans only.

## Key Decisions

- Relabel-not-defend: when the experiment falsified the env-strip's premise, the change shipped as hygiene with the history preserved and the correction explicit, rather than dropped (the dedup remains load-bearing against whichever semantics any future pinned binary uses) — orchestrator-ruled.
- Legs transferred verbatim from 23cde048; no grammar-note references dangle (the one soft "see permissiontiers.go" pointer resolves against the landed file).

## Blockers

None.

## Tests Run

- `go test -count=1 ./pkg/agent/opencode/` → ok.
- `go test -count=1 -tags=integration -run 'TestIntegrationHarness_ChildHonorsSyntheticDataHome|TestPermissionTier' -v ./pkg/agent/opencode/` → 14/14 PASS on the real pinned 1.18.15 (includes #1762's own corpus + model tests on the landed floor).
- Mutation evidence (pre-implementation, file-copy discipline): append-only env shape → the behavioral pin still PASSES (the falsifying observation; saved transcript-free, log deleted).

## Next Steps

- PR review loop to APPROVED; packet to the orchestrator.
- Salvage B after the mandated live experiment: floor + "/workspace/.local/opencode/*" children deny, bash-read of the auth path, verdict-only logging — outcome (a) deny exists vs (b) unpreventable, feeding the grammar note's wording.

## Files Modified

- pkg/agent/opencode/integration_harness.go — env strip+append via one map; corrected comment
- pkg/agent/opencode/integration_harness_env_test.go — behavioral pin (new)
- pkg/agent/opencode/permissiontiers_integration_test.go — live legs + tierBashLeg (transferred from 23cde048)
- worklogs/NNNN_2026-10-20_salvage-harness-env-live-legs.md — this worklog

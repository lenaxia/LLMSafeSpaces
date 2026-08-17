# Worklog: overlay delivery live-validation findings — fix both

**Date:** 2026-08-17
**PR:** (this PR)
**Issue:** #863 (validation comment 5321264009)
**Session:** follow-up to in-cluster validation on TEST
**Status:** code complete, suites green

## Objective

Fix the two findings from the first live-cluster validation of agentd
overlay delivery (TEST cluster, chart v0.15.11).

## Work Completed

1. **AgentdVerified=True on legacy pods (finding 2).** markAgentdVerified
   gated only on the controller flag; during the rollout window (flag
   on, pre-existing pods on the baked binary) every legacy pod was
   stamped verified on next reconcile despite never running the
   entrypoint check. Now gated on the POD carrying the overlay
   (AGENTD_IMAGE_VOLUME=1 in the workspace container env — set only in
   the same builder branch that adds the volume). The same gate now
   protects detectAgentdVerificationFailure from misclassifying an
   unrelated exit-81 on a legacy pod. Two regression tests
   (LegacyPodNeverVerified, LegacyPodExit81NotMisread); gate
   mutation-verified red/green locally.
2. **Dropped startup logs (finding 1).** The validation+pin-resolution
   block ran before ctrl.SetLogger; controller-runtime's delegating
   sink discards pre-SetLogger output, so BOTH the resolved-pins Info
   line and — worse — the fatal validation/resolution Error lines were
   invisible (a silent os.Exit(1)). Block moved to after SetLogger and
   before SetupControllers (which needs the resolved pins); no
   behavioral ordering change otherwise.

## Tests Run

- controller/internal/workspace unit suite incl. 2 new regressions — green
- envtest agentd-pins suite (3 legs) vs 1.31 assets — green
- gate mutation: removing podHasAgentdOverlay from markAgentdVerified →
  LegacyPodNeverVerified FAIL (4 failure lines); restored → green

## Next Steps

- Redeploy TEST cluster controller to clear the false-positive
  conditions stamped during its soak window (conditions are idempotent
  — the next reconcile after this fix removes nothing retroactively;
  affected workspaces' conditions self-correct only on state change, so
  consider bumping restartGeneration on the test workspace or ignoring
  the stale True during soak).

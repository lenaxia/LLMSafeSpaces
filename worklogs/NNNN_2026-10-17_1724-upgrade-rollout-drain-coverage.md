# Worklog: #1724 — platform-upgrade session kill: code-path analysis, rollout invariant pins, blast-radius runbook

**Date:** 2026-10-17
**Session:** Incident-fix lane, issue #1724 (P1: v0.38.0 rollout killed ALL in-flight sessions; suspected overlay-digest → spec-hash fleet recreation).
**Status:** Analysis + invariant pins + runbook complete; root-cause discrimination pending owner telemetry export; D20/suspend drain ruling requested on the issue.

## Objective

Charter: (1) deterministic code-path analysis of how overlay digests enter the workspace pod spec and why #761 drain-defer did not protect the rollout — posted EARLY; (2) fix per the issue's normative direction ("an upgrade must never kill a healthy in-flight turn"), escalating design choices that need an owner ruling; (3) red-first kill-tests extending the #761 suite; (4) runbook blast-radius contract + ops-repo comment text; (5) no regressions to #761/#1687/#1685/#1719 suites.

## Root cause (code-path analysis — deterministic, posted on #1724)

The issue's suspected mechanism is **refuted at code level**:

1. Overlay digests enter the pod spec ONLY at build time — `wireAgentdOverlay` (`agentd_overlay.go:84-105`) / `wireOpencodeOverlay` from `buildPod`; per-arch binary pins resolve at controller STARTUP from the index annotations (`overlay_pins.go`, `main.go:267-323`) and land as env pins in the same build pass.
2. **No spec-hash drift replacement exists.** No reconcile path compares a running pod's overlay image-volume reference or env pins against the controller's configured delivery images. The overlay design is explicitly lazy (`agentd_overlay.go` header: "any pod (re)creation — launch, resume, controller restart — pulls the current pinned digest").
3. Complete pod-deletion inventory (every `deletePodByName` site): restart-gen / password-secret heal / arch drift (#761-drained), suspend family incl. org-D20/idle/timeout (#1507 grace-only, no drain), health-restart (ungated by design), terminate/Failed (by design). A digest bump enters NONE of them.
4. **Why #761 didn't protect:** no deferrable deletion ran — the drain gate was structurally out of the path. Prediction for vmexport: the three drain counters flat through the window.
5. Corroboration: v0.37.0's deploy (talos-ops-prod `62fbf043`) made the IDENTICAL change class (git ref + 4 tags + both overlay digests) with no incident.

Live-trigger candidates (ranked, discriminating telemetry requested on the issue): (1) API/controller/frontend roll severed SSE — pods alive, streams dropped (v0.38.0 changed the usagestream consumer); (2) org-status (D20) misfire — the one Active-phase deletion that can fire fleet-wide from an API-side change with NO drain; (3) health-restart storm. Ruling requested: whether D20 org-suspension should route through the #761 drain (option A) — `force_recycle.go`'s #1505 contract comment says the drain "stays for every automated path (org suspension, ...)" while the code implements #1507 grace-only; the documented contracts diverge.

## Work Completed

### (a) Code-path analysis comment on #1724 (early deliverable)

Posted within the first working session: injection sites, deletion-path inventory table, why the drain never fired, counter-evidence (v0.37.0), ranked live-trigger candidates + the exact four metric series that discriminate, fix plan split into no-ruling-needed vs. escalated. https://github.com/lenaxia/LLMSafeSpaces/issues/1724#issuecomment-6230850093

### (b) Kill-tests — `controller/internal/workspace/upgrade_rollout_kill_test.go`

Extends the #761 suite (`session_drain_test.go` fixtures: `statuszStub`, `reconcilerForDrain`, `makeRestartGenDrainWorkspace` conventions) in the #1719 red-first pattern:

- `TestUpgrade_OverlayDigestChange_BusySessionsZeroKills` — the exact upgrade delta (both overlay digests + resolved pins swapped on the reconciler, modeling the rolled controller) with BUSY in-flight sessions: 3 post-upgrade reconciles, pod survives every pass, phase stays Active, requeue is never the drain poll interval, the running pod's spec still carries the OLD digest (no in-place mutation), all three drain counters unchanged, zero `SessionDrain*` events, no drain window opened.
- `TestUpgrade_OverlayDigestChange_IdlePodAlsoUntouched` — laziness holds even idle (no churn).
- `TestUpgrade_RecreatedPodPicksUpNewDigest` — the lazy roll actually rolls: busy turn defers an unrelated restart-gen bump across the digest change; turn ends → recycle → handleCreating/buildPod emits the NEW digest + NEW resolved pin.
- `TestUpgrade_DeletionPathInventory` — source-guard drift pin (the `opencode_overlay_contract_test.go` pattern, via go/ast): every `deletePodByName` call site in the package must appear in `classifiedDeletionPaths` with its ruling, and every inventory entry must still exist on disk. A new controller-initiated pod deletion cannot land without conscious drain classification.

**Red-first teeth validation:** mutated `handleActive` with an eager digest-drift recycle (compare pod's agentd volume ref vs `r.AgentdImage`, delete+Creating on mismatch) — both digest-change kill-tests FAIL against the mutation (pod killed). Mutation discarded; tests ship green against compliant main. This is the honest form of red-first for an invariant that currently holds: the pin is provably load-bearing.

### (c) Runbook — `helm/MONITORING-OPERATIONAL.md`

New section "Upgrade blast-radius contract (which Helm fields touch workspace pods)": per-field table (agentdDelivery/opencodeDelivery digests → NEW pods only; controller tag → drain-window reset only; api tag → SSE re-connect with #1687/#1666 resync; runtimeEnvironments.base → new workspaces only; org status → the one fleet-wide no-drain kill path), the complete kill-path inventory pointer, and the discriminating telemetry for rollout-incident triage.

### (d) Ops-repo comment text — posted on #1724 (talos-ops-prod NOT edited, per charter)

Suggested `helm-release.yaml` comment block pinning the blast-radius expectations at the bump site (digest fields are lazy; expect stream blips from the api/frontend rolls; drain series must stay flat).

## Escalated (blocking those items only)

- D20 org-suspension drain ruling (options A/B/C on the issue) — depends on the owner's telemetry export.
- If telemetry shows pods never died (streams only), the remaining work belongs to the #1687/#1666 resync lane; the pins and runbook here stand regardless.

## Verification

- `go test ./controller/internal/workspace/ -count=1` — full package green (68s), incl. the #761 drain suite, #1507 suspend pins, #863/#935/#1685-adjacent suites.
- `go vet ./controller/internal/workspace/` clean; goimports applied.
- Mutation check: eager drift-recycle fails both digest-change kill-tests; reverted.

## Files Modified

- `controller/internal/workspace/upgrade_rollout_kill_test.go` (new)
- `helm/MONITORING-OPERATIONAL.md` (new section)
- `CHANGELOG.md` (entry)

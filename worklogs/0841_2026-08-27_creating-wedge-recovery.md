# Worklog: Creating-phase wedge recovery — FN3c + restartGeneration rebuild (#935)

**Date:** 2026-08-27
**Session:** Close the #935 controller gaps. Gap-verification first: Gap 2 (agentd overlay skips credential-setup) is ALREADY SHIPPED via #1021/#1023 (design 0051 steps 1-2 — platform-init/bootstrap/materialize containers run FROM the digest-pinned agentd image; no baked binary executes in overlay mode). Gaps 1 and 3 were still open at HEAD and are implemented here.
**Status:** Complete

---

## Objective

No workspace can sit in Creating forever silently; every user-facing escape hatch works in the phase where workspaces get stuck.

---

## Work Completed

### Gap 1 — FN3c: init-container crashloop recovery in `handleCreating`

An init container that runs and crash-loops leaves the pod Pending with `Waiting/CrashLoopBackOff` — matching none of the existing signals (PodFailed never fires for CLO pods; FN3b requires that NO container ever started, and a crashing init HAS started; agentd-verify only covers exit 81/82). Result was the incident: a silent 2s hot-requeue, no event/condition/backoff, 8 and 12 days.

- `initContainerCrashLooping(pod)` helper + a new branch in `handleCreating`: Pending + any init CLO + pod older than `stuckScheduledPendingTimeout` (10 min, reused) → delete pod + `enterRecovery(FailureClassProcess)` — the standard Epic 24 machinery (backoff, restart budget, SafeMode, events).
- **Placement is load-bearing**: AFTER `detectAgentdVerificationFailure` and `detectPlatformBootFailure`. Those detector-owned failures are surfaced-not-recovered by design (0051 step 1: deleting the pod cannot fix a platform code bug); FN3c catches everything else. Pinned by `TestReconcile_Creating_PlatformBootFailure_PrecedesCrashloopRecovery` (with a precondition assertion so it can't pass vacuously).
- Generic classification, no exit-code special-casing (per the issue's design note).

### Gap 3 — restartGeneration bump in Creating deletes the pod

The bump branch cleared recovery counters but left the existing (crash-looping) pod alone; since the pod existed, the create branch never ran — `RestartWorkspace`/`RefreshWorkspaceCompute` were silent no-ops in exactly the phase where workspaces wedge. Now: bump + existing non-terminating pod → delete + persist status + requeue → create branch rebuilds from CURRENT spec (re-resolving `spec.runtime` for non-pinned refs). Parity with `handleActive`'s bump path. Placed after the `isPodTerminating` guard (no double-delete race).

### Tests (red-first)

- `TestReconcile_Creating_InitCrashLoop_BeyondTimeout_EntersProcessRecovery` — the incident's exact shape (credential-setup CLO, RestartCount 33, the ENOSPC-era unmarshal error message): FailureClassProcess, backoff set, pod deleted.
- `..._BelowTimeout_NoRecovery` — young CLO (false-positive guard).
- `..._PlatformBootFailure_PrecedesCrashloopRecovery` — platform-named CLO surfaces, never pod-deleted.
- `TestReconcile_Creating_RestartGenerationBump_DeletesExistingPod` + `..._TerminatingPod_Waits`.

### Gap-2 disposition (no code — already shipped)

Verified at HEAD: overlay mode builds `platform-init`/`platform-bootstrap`/`platform-materialize` from `r.AgentdImage` (digest-pinned) directly (#1021), with the agentd image volume mounted on them (#1023). The incident's root cause (stale BAKED binary executing in init) cannot occur in overlay mode; legacy-no-overlay keeps the baked bash path by design (0051 step-5 deletion candidate) and is now covered by FN3c + the Gap-3 escape hatch (bump + change spec.runtime actually rebuilds).

---

## Key Decisions

- FN3c after the two detectors, not inside the Pending block: preserves 0051's surfaced-not-recovered semantics for platform-owned failures while recovering everything else.
- Reuse `stuckScheduledPendingTimeout` (10 min) for the CLO gate — same order as kubelet's own CLO stabilization; matches the issue's spec.
- No heartbeat/exit-code parsing: generic classification is the right level (issue's design note).

## Assumptions (stated + validated)

1. Gap 2 shipped via #1021/#1023 — validated by reading `platform_init.go` (Image: r.AgentdImage on all three platform containers) and pod_builder's overlay branch.
2. Fake-client pods need finalizers to carry a deletionTimestamp — discovered when the terminating-pod fixture panicked; convention applied.
3. `enterRecovery` keeps phase=Creating and gates recreation via NextRetryAt — validated in recovery_policy.go:95+.

## Blockers

None.

## Tests Run

- `go test -race ./controller/internal/workspace/` — ok (5 new tests red-first + full existing suite)
- `go test -race ./controller/...` — ok; golangci-lint 0 issues; gofmt clean

## Next Steps

- PR; closes #935 (all three gaps: 1+3 here, 2 shipped via #1021/#1023).
- #1053 (mcp SendMessage SSE content) next.

## Files Modified

- `controller/internal/workspace/phase_creating.go` — FN3c branch, bump-delete branch, initContainerCrashLooping
- `controller/internal/workspace/creating_wedge_test.go` — new

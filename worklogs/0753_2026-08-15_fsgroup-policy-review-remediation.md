# Worklog: fsGroupChangePolicy OnRootMismatch

**Date:** 2026-08-15
**Session:** Complete PR #816 review remediation — integration + e2e coverage for the fsGroupChangePolicy fix
**Status:** Complete

---

## Objective

Address the REQUEST CHANGES findings on PR #816 (`fsGroupChangePolicy: OnRootMismatch` — stop chowning the entire PVC on every pod start) and refresh the branch against `main` (Go 1.26.6 toolchain bump #853 fixed the stale-branch govulncheck failure GO-2026-6218/GO-2026-6091 and the `TestProxyBuffer_DefaultConfigBufferFullReachable` race-suite failure, both of which pass on current main).

---

## Work Completed

### Review findings addressed

1. **Integration test (missing)** — added `TestE2E_Reconcile_PodSpec_FSGroupChangePolicyPersisted` and `...OnResume` to `pod_spec_consistency_test.go`. Both drive the REAL Reconcile loop (fake-client persisted pod, not the builder in isolation):
   - Create path: Pending → Creating → pod persisted → assert `Spec.SecurityContext.FSGroupChangePolicy == OnRootMismatch`.
   - Resume path: Resuming → Creating → pod re-created → same assertion. Resume is where the production incident was observed (workspace restart stuck 5+ min in `Init:0/2`, kubelet `VolumePermissionChangeInProgress` over 315,923 files), so it gets its own guard — a future regression in `handleCreating`/pod-spec normalization can no longer strip the field and pass the unit test alone.
2. **E2e assertion (missing)** — `local/test.sh` Test 3 now asserts the LIVE pod (selected via the `llmsafespaces.dev/workspace` label, constants.go:61) carries `fsGroupChangePolicy=OnRootMismatch` via kubectl jsonpath, dying loudly otherwise.
3. **Style nit** — package-level `var fsGroupChangeOnRootMismatch` replaced with a function-local `policy` value following the surrounding `trueVal` pattern (no mutable shared state, tight scope).
4. **Branch refresh** — merged `main` (through f6cbb834) into the branch; brings Go 1.26.6, relay/SSE fixes, and CI workflow updates.

### Assumptions stated and validated (per Rule 7)

- **Resume re-creates the pod through the same `handleCreating` path** — validated by reading `phase_creating.go` (resume → Creating → `buildPod` → Create) and empirically by the new resume integration test passing.
- **The e2e label selector matches the built pod** — validated against `constants.go:61` (`LabelWorkspace = "llmsafespaces.dev/workspace"`) and `pod_builder.go:44-50`.
- **Gradual rollout note (from review, non-blocking)**: running pods keep the old behavior until their next recreation — acceptable, no mass restart.

---

## Key Decisions

1. Resume-path assertion as a separate test rather than a shared helper — the incident was on resume; a dedicated test name documents that forever.
2. Kept the fix itself (13 lines) untouched — the reviewer verified red/green on it; only coverage and style changed.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s ./controller/internal/workspace/ -run 'FSGroupChangePolicy'` — pass (unit + both new integration tests)
- `go test -timeout 600s ./controller/...` — pass (full controller suite on merged branch)
- `bash -n local/test.sh` — syntax clean
- Full CI deferred to PR gate

---

## Next Steps

- Iterate PR #816 through review to APPROVE, then merge (squash).

---

## Files Modified

- controller/internal/workspace/pod_builder.go
- controller/internal/workspace/pod_spec_consistency_test.go
- local/test.sh
- worklogs/0753_2026-08-15_fsgroup-policy-review-remediation.md (this file)

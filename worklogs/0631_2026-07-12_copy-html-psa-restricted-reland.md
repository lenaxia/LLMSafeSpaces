# Worklog: #468 frontend copy-html initContainer PSA-restricted fix (re-land)

**Date:** 2026-07-12
**Session:** The original PR #500 fixed #468 (frontend `copy-html` initContainer missing `capabilities.drop: [ALL]` and a container-level `seccompProfile`, making the chart uninstallable on PSA `restricted` namespaces). PR #500 was opened 2026-07-04 against the `charts/llmsafespaces/` layout; PR #526 renamed the chart to `helm/` on 2026-07-11, leaving PR #500's paths unreachable. PR #500 was closed stale; this re-lands the same fix on the current layout.
**Status:** Complete

---

## Objective

Re-implement the #468 fix on the post-rename `helm/` layout, with the same TDD discipline and recurrence guard PR #500 had, and credit the original work.

---

## Work Completed

### `helm/templates/frontend-deployment.yaml`

Added to the `copy-html` initContainer's `securityContext`:
- `capabilities.drop: [ALL]` — fixes the blocking PSA error
- `seccompProfile.type: RuntimeDefault` — defense-in-depth; otherwise inherited from the pod-level securityContext. Added explicitly to match the main `frontend` container.

### `helm/chart_test.go`

Two helpers + two tests:

- `initContainerByName(deploy, name)` — walks `spec.template.spec.initContainers`. The existing `containerByName` only walks `containers`, missing initContainers entirely.
- `allContainersAndInitContainers(deploy)` — returns every container + initContainer in a Deployment, for recurrence guards that must assert a property holds for every container in the pod.
- `TestF4b_FrontendCopyHtmlInitContainer_PSARestricted` — focused regression: renders the chart, finds the `copy-html` initContainer via the new helper, asserts `capabilities.drop` contains `ALL`, `seccompProfile.type=RuntimeDefault`, and `allowPrivilegeEscalation=false`.
- `TestF4c_FrontendAllContainersDropAllCapabilities` — broad recurrence guard: iterates every container AND initContainer in the frontend pod, asserts each drops ALL capabilities. Prevents this class of bug on any future container/initContainer addition.

Verified RED then GREEN:
- Pre-fix: both tests fail at `copy-html initContainer must set capabilities (#468)`.
- Post-fix: both pass.

---

## Key Decisions

1. **`allContainersAndInitContainers` instead of just `initContainerByName`.** The recurrence guard needs to walk both arrays. PR #500 wrote the recurrence guard by manually iterating containers then initContainers inline in the test; a helper makes the intent clearer and the test shorter. The helper is a sibling of `containerByName` and follows its shape.

2. **Assert seccompProfile explicitly in the focused test, not the recurrence guard.** seccompProfile is inherited from the pod-level securityContext when omitted at container level — asserting it per-container in the recurrence guard would couple the test to K8s inheritance semantics rather than the actual fix. The focused test does assert it explicitly for `copy-html` (where the fix added it). Same decision PR #500 made; still correct.

3. **No CRD/RBAC/migration changes.** The fix is a 5-line template addition matching the main `frontend` container's existing securityContext. Verified the migration Job (the only other pod the chart renders with init-style containers) is already compliant: pod-level `seccompProfile.type: RuntimeDefault` at `migration-job.yaml` + container `capabilities.drop: [ALL]`.

---

## Assumptions stated and validated (Rule 7)

1. *PR #500's fix logic is correct and only the paths were stale.* Validated by reading the PR diff: exactly `capabilities.drop: [ALL]` + `seccompProfile.type: RuntimeDefault` added to the copy-html initContainer. The current `helm/templates/frontend-deployment.yaml` copy-html initContainer is identical to the pre-fix `charts/llmsafespaces/templates/frontend-deployment.yaml` copy-html initContainer (modulo the path rename).
2. *The bug is still present on main.* Validated by `grep -B 1 -A 12 "name: copy-html" helm/templates/frontend-deployment.yaml` — the securityContext has only `allowPrivilegeEscalation`, `runAsNonRoot`, `runAsUser`. No `capabilities`, no `seccompProfile`.
3. *copy-html is the only initContainer in the chart.* Validated by `grep -rn "initContainers:" helm/templates/` — one match, in frontend-deployment.yaml. The recurrence guard is preventive, not redundant.
4. *The migration Job is already PSA-compliant.* Validated by reading `helm/templates/migration-job.yaml` — pod-level `seccompProfile.type: RuntimeDefault` and container `capabilities.drop: [ALL]` are both set.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, fixed):** Initial test only checked `capabilities.drop`. Reviewed: PSA `restricted` also requires `seccompProfile` (default is `Unconfined` if omitted, which violates restricted). The pod-level securityContext does set it, so it's inherited — but defense-in-depth + matching the main container's explicit set is the project convention. Added `seccompProfile` assertion to the focused test and the same field to the template.
- **Phase 1 finding (real, fixed):** The recurrence guard initially only walked `containers`, missing `initContainers` — the exact bug class PR #500 caught and the exact gap that let #468 slip in the first place. Replaced inline walk with `allContainersAndInitContainers` helper that walks both arrays.
- **Phase 2 false alarm initially considered:** "Does `runAsUser: 101` need to be `runAsGroup: 101` too?" Reviewed: PSA restricted requires `runAsNonRoot` (present) and either `runAsUser != 0` (present, 101) or `runAsGroup != 0`. The main frontend container sets both `runAsUser` and `runAsGroup`; the copy-html initContainer matches PR #500's original fix (which omits `runAsGroup`). The Pod Security Admission controller's restricted profile accepts either. False alarm.
- **Phase 2 false alarm initially considered:** "Will the recurrence guard fail on the relay-router Deployment, which has a sidecar?" Reviewed: `TestF4c` is scoped to the frontend Deployment via `findDeploymentByNameSubstr(docs, "-frontend")`. Other Deployments are out of scope. False alarm.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 80s -race -count=1 -run "TestF4|TestF4b|TestF4c" ./helm/...
  pre-fix:  FAIL (TestF4b, TestF4c both fail at copy-html capabilities)
  post-fix: ok (all F4-family tests pass)
```

---

## Files Modified

- `helm/templates/frontend-deployment.yaml` — added `capabilities.drop: [ALL]` and `seccompProfile.type: RuntimeDefault` to the `copy-html` initContainer's `securityContext`.
- `helm/chart_test.go` — added `initContainerByName` and `allContainersAndInitContainers` helpers; added `TestF4b_FrontendCopyHtmlInitContainer_PSARestricted` and `TestF4c_FrontendAllContainersDropAllCapabilities`.

---

## Next Steps

1. Open this PR.
2. Closes #468.
3. Credits PR #500 (closed stale) for the original fix logic and test design.

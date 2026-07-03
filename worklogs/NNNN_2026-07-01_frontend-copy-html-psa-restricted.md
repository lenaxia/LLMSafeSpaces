# Worklog: Frontend copy-html initContainer PSA restricted fix (#468)

**Date:** 2026-07-01
**Session:** Fix the copy-html initContainer so the frontend Deployment schedules under a PSA `restricted` namespace.
**Status:** Complete

---

## Objective

Resolve #468: the `copy-html` initContainer in
`charts/llmsafespaces/templates/frontend-deployment.yaml` dropped no
capabilities and set no container-level seccompProfile, so deploying the
chart into a namespace with
`pod-security.kubernetes.io/enforce: restricted` rejected the pod with
`unrestricted capabilities (container "copy-html" must set
securityContext.capabilities.drop=["ALL"])` and the frontend Deployment
never reached Ready.

---

## Work Completed

### TDD regression tests (written first, confirmed red)

Added to `charts/llmsafespaces/chart_test.go`:

- `initContainerByName` helper (sibling to the existing `containerByName`),
  because `containerByName` only walks `containers`, not `initContainers`.
- `TestF4b_FrontendCopyHtmlInitContainer_PSARestricted` — targeted
  regression asserting the copy-html initContainer sets
  `capabilities.drop=["ALL"]`, `seccompProfile.type=RuntimeDefault`, and
  `allowPrivilegeEscalation: false`.
- `TestF4c_FrontendAllContainersDropAllCapabilities` — broad recurrence
  guard iterating every container AND initContainer in the frontend pod,
  asserting each drops ALL capabilities.

Red phase confirmed: both tests failed against the unmodified template
(`copy-html initContainer must set capabilities (#468)` /
`frontend container "copy-html" must set capabilities`).

### Template fix (green phase)

`charts/llmsafespaces/templates/frontend-deployment.yaml`: added
`capabilities.drop: [ALL]` and `seccompProfile.type: RuntimeDefault` to
the `copy-html` initContainer's securityContext, matching the main
`frontend` container's existing securityContext.

Green phase confirmed: both new tests pass; the pre-existing
`TestF3_MCPSecurityContext` and `TestF4_FrontendReadOnlyRootFilesystem`
still pass.

---

## Key Decisions

- **seccompProfile is added explicitly at container level even though it
  is inherited from the pod-level securityContext.** Rationale: defense-
  in-depth and consistency with the main `frontend` container, which also
  sets it explicitly; the issue asked for it. The pod-level
  `seccompProfile.type: RuntimeDefault` (frontend-deployment.yaml) already
  covers initContainers via inheritance, so the actual blocking PSA error
  was the missing `capabilities.drop` only — confirmed by the admission
  error message in the issue. The decision to set it explicitly does not
  change effective behavior; it makes the manifest self-documenting.

- **Two tests, not six.** The issue's `/fix` bot run (which never pushed a
  branch — see investigation) proposed six tests. Per Rule 4 (not over-
  engineered), a focused regression + one broad recurrence guard is the
  proportional coverage; the broad guard prevents the same class of bug
  on any future container/initContainer added to the frontend pod.

---

## Assumptions (stated and validated per Rule 7)

1. **The blocking PSA error was capabilities only.** Validated: the
   admission error in the issue text names `capabilities` exclusively;
   pod-level seccompProfile already covers initContainers by inheritance.
2. **The migration Job flagged in the issue is already compliant.**
   Validated by reading `templates/migration-job.yaml`: pod-level
   `seccompProfile.type: RuntimeDefault` (lines 32-33) and container
   `capabilities.drop: [ALL]` (lines 44-46) are both present. No change
   needed there.
3. **No other workload template is missing PSA fields.** Validated
   indirectly: the broad guard `TestF4c` iterates all frontend
   containers+initContainers and passes; the existing `TestF3_MCPSecurityContext`
   covers MCP. (Full audit of all 8 templates was done by the earlier
   `/fix` bot run per the issue comments.)

---

## Adversarial self-review (Rule 11)

- **Gap: does the broad guard assert seccompProfile too?** No, only
  capabilities drop. Reason: seccompProfile is inherited from pod-level,
  so asserting it per-container would assert redundant/implicit state and
  couple the test to inheritance semantics. The focused test asserts it
  for copy-html (the explicit fix). Not a real gap — false alarm,
  documented.
- **Could the helper shadow an existing `initContainerByName`?** Checked:
  no prior definition (`grep` found none). Not a collision.
- **Helm render order nondeterminism?** Tests parse rendered YAML and
  query by name, matching the established pattern (chart_test.go:24-27).
  No string matching on raw output. Robust.

---

## Blockers

None.

---

## Tests Run

```
go test -run 'TestF4b|TestF4c|TestF4|TestF3' -v ./charts/llmsafespaces/...   # PASS (red before fix, green after)
go test -timeout 180s -short ./charts/llmsafespaces/...                       # PASS (12.9s, full chart suite)
go vet ./charts/llmsafespaces/...                                            # clean
gofmt -l ./charts/llmsafespaces/                                            # clean (no files listed)
```

helm v3.16.3 on PATH (rendered via `helm template` subprocess, same path
the Makefile and operators use).

---

## Next Steps

- Open PR for this branch, address automated review, merge.
- Proceed to #476 (chart image template digest pinning) — next quick win
  in the burn-down.

---

## Files Modified

- `charts/llmsafespaces/templates/frontend-deployment.yaml` — added
  `capabilities.drop: [ALL]` + `seccompProfile` to copy-html initContainer.
- `charts/llmsafespaces/chart_test.go` — added `initContainerByName`
  helper + `TestF4b_FrontendCopyHtmlInitContainer_PSARestricted` +
  `TestF4c_FrontendAllContainersDropAllCapabilities`.
- `worklogs/NNNN_2026-07-01_frontend-copy-html-psa-restricted.md` — this entry.

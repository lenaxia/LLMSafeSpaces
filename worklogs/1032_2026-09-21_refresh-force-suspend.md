# Worklog: refresh-compute force path (#1505 — user consent bypasses the #761 drain)

**Date:** 2026-09-21
**Session:** The owner's Refresh Compute stopped working on this multi-agent pod: the #761 drain gate defers pod deletion while sessions are busy-and-progressing, and an orchestration pod runs busy sessions around the clock — the drain never finds quiet. Owner decision: the user-initiated refresh/suspend action IS the force path; the UI warning is the consent.
**Status:** Complete

---

## Objective

Refresh Compute (and the user suspend endpoint) must delete the pod immediately despite busy in-flight sessions; every automated suspend source keeps the polite drain.

---

## Work Completed

### Force channel (annotation, transient, self-invalidating)
- `AnnotationForceRecycle = "llmsafespaces.dev/force-recycle"` (`pkg/apis/llmsafespaces/v1/workspace_types.go`), documented in-code.
- TWO path-scoped values, disjoint by construction: a generation number (`"42"`) applies to the refresh-compute recycle observing exactly that restartGeneration; `"suspend"` applies to one user-initiated suspend pass. A generation value can never force a suspend and vice versa (pinned: TestForce_MarkerValuesArePathScoped).
- Honored → cleared in the SAME reconcile pass (`clearForceRecycleAnnotation`, clearSuspendRequest shape; the caller's in-memory object is resourceVersion-synced so the following Status().Update doesn't conflict — the conflict was a real red test finding).

### Controller (controller/internal/workspace/force_recycle.go + the two drain sites)
- handleActive generation-recycle: exact-generation marker match → skip drain, noteForcedRecycle (SessionDrainUserForced event + WorkspaceDrainForcedTotal metric), clear marker; mismatched/stale markers inert.
- handleSuspending: `"suspend"` marker → same force treatment; automated suspends (org, idle, max-active, spec-timeout — none set the marker) keep the #761 drain.

### API
- `RefreshWorkspaceCompute` stamps the marker with the post-increment generation in the same CRD write.
- `SuspendWorkspace` (polite, automated callers unchanged) vs `SuspendWorkspaceForce` (the /suspend endpoint only) — shared `suspendWorkspace(ctx, uid, id, force)`; the force variant stamps `"suspend"`. Interface + mock extended.
- Endpoint contract documented in-code (refresh-compute route comment) and in the OpenAPI spec description (force semantics, in-flight turns cut, warning-is-consent); `make -C sdks sdk-check` green.

### TDD (the issue's mandated matrix)
- Controller: forced refresh + busy → deletes FIRST pass (the live repro), generation observed, marker cleared, statusz NEVER consulted; stale marker → drains; plain bump → drains; forced user suspend + busy → one-pass delete + Suspended; automated suspend → drains; cross-value isolation. 6 tests, all red-first observed (the marker-clear conflict surfaced here).
- API: refresh stamps generation-keyed marker (service pin); Force variant stamps `"suspend"`, plain variant stamps nothing (service pins); the /suspend ROUTE wires to Force — router pin asserts Force called once and polite NEVER called.

## Key Decisions

1. **Annotation over a spec field**: transient by nature (honored once, cleared by the controller), self-invalidating when stale (generation-keyed), and needs no CRD schema bump — matches the house annotation conventions (requested-at, last-activity-at, relay staging).
2. **Two disjoint marker values** instead of a bool: path-scoping falls out of string inequality — no cross-path force is expressible by accident.
3. **Neutralize/max-active/idle keep SuspendWorkspace** (polite): they have no consent surface; the issue's constraint 1 verbatim.
4. **The in-flight-turn death is documented at every layer it touches**: the route comment (server-side contract), the OpenAPI description (client contract), and the SessionDrainUserForced event (the object's audit history) — the UI warning's counterparts.

## Assumptions → validation record (Rule 7)

- "The repro hangs in the generation-recycle drain, not handleSuspending" → verified by reading the refresh endpoint's actual path (Active → RestartGeneration++ → handleActive drain) and the owner's symptom (refresh, not suspend); the suspend path got the same treatment because the issue's constraint text named it and the same multi-agent hang applies.
- "The UI warns before Refresh Compute" → the owner decision states it (binding); the sidebar refresh mutation path exists (frontend/src/components/layout/Sidebar.tsx:104).
- "statusz is never consulted on the forced path" → pinned (0 calls asserted in the repro test).
- "Marker clear + status update don't conflict" → first red run FAILED with exactly that conflict; fixed by RV-syncing (evidence the tests bite).

## Blockers

None.

## Tests Run

- `go test ./controller/internal/workspace/ ./api/internal/services/workspace/ ./api/internal/server/ ./controller/...` — all ok (controller suite 80s).
- `make -C sdks sdk-check` — green.

## Next Steps

1. Reviewer mutation surfaces: delete the force branch → repro test fails (deferred requeue); route to the polite variant → router pin fails; drop the marker stamp → API pins fail.
2. Post-merge: the owner's refresh works again on this pod; the SessionDrainUserForced event should appear once per forced refresh (watch the event stream).

## Files Modified

- `pkg/apis/llmsafespaces/v1/workspace_types.go` (AnnotationForceRecycle)
- `controller/internal/workspace/force_recycle.go` (new) + `force_recycle_test.go` (new, 6 tests)
- `controller/internal/workspace/phase_active.go`, `phase_suspend.go` (the two drain sites)
- `api/internal/services/workspace/workspace_service.go` (marker stamp + Force variant)
- `api/internal/interfaces/interfaces.go`, `api/internal/mocks/workspace.go`
- `api/internal/server/router.go` (route + contract comment), `router_workspace_test.go` (route pin)
- `api/internal/server/mcp_router_integration_test.go` (mock)
- `sdks/openapi.yaml` (description)
- `worklogs/1032_2026-09-21_refresh-force-suspend.md` (this file)

## r2 rework (post-#1510, post-r1) — the suspend-force layer is GONE

Adjudication: #1510 (#1507) merged as squash ee787444 — suspend is bounded-immediate for EVERY caller (the pod's termination grace is the graceful window; there is no drain on the suspend path at all). A suspend force marker atop that is vestigial by construction, and r1's findings 1/2 (MCP tool silently inheriting force; the UI suspend consent premise being false — no dialog exists) independently kill the route flip. Orchestrator GO: drop the layer, keep the refresh machinery.

Dropped (resolves r1 findings 1, 2, 3, and 4 by removal):
- `SuspendWorkspaceForce` + the `suspendWorkspace(ctx, ..., force)` plumbing + the `"suspend"` marker stamp (service), the `/suspend` route flip + the "CONSented" typo comment (router), the interface method, the error-swallowing mock (r1 finding 3 — removed rather than fixed), the MCP integration-test stub line (r1 finding 1 — the MCP tool now shares the single bounded SuspendWorkspace path like every caller), and `ForceValueSuspend` + the suspend doc halves (controller, types).
- Rebase: phase_suspend.go resolves to main's #1510 shape — the branch's `"suspend"` bypass sat on a drain call that no longer exists. The three suspend-side controller tests (forced-suspend-immediate / automated-suspend-drains / cross-value-isolation) were RED post-rebase (run recorded) — suspend semantics are owned by phase_suspend_1507_test.go now; the matrix drops to refresh-scoped pins.
- r1 finding 4 (suspend-then-refresh marker overwrite): unconstructible — only refresh stamps.

Kept + added:
- The generation-keyed refresh marker end-to-end: API stamp → handleActive exact-generation bypass → same-pass clear (RV-synced) → SessionDrainUserForced event.
- NEW PIN: TestForce_ClearAnnotationFailureRequeuesWithoutDeletion — injected first-Update failure → Requeue (not RequeueAfter), pod KEPT, marker retained for the retry. Mutation-verified: ignoring the clear error fails the pin (red reproduced, then restored).
- NEW ASSERTION: exactly one SessionDrainUserForced event on the forced pass; zero on the polite passes (r1's missing-test ask).
- Inverted wiring pin: TestSuspendRoute_UsesSuspendWorkspace — /suspend routes to the single SuspendWorkspace, called once.
- e2e row: local/issue-1505-refresh-busy-e2e.sh + 5 shape pins (busy-before-refresh ordering, Active-budget + NEW-pod + PVC-retained verdicts, fail-closed R2 with BOTH the negative drain-defer grep and the POSITIVE forced-bypass grep, WS_BASE isolation, and the reason-string↔controller-constant match pin that keeps the greps honest). Same static-delivery disposition as the #1507 row: standalone script, first recorded execution rides the #1456 wiring lane.
- Corrected the "polite drain" phrasing everywhere it mentioned suspend (openapi refresh description, router contract comment, force_recycle.go header, phase_active.go comment): the polite drain survives on the AUTOMATED recycle paths (restart-generation bumps, arch drift, password self-heal); suspend has no drain since #1510.

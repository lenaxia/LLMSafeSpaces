# Worklog: refresh-compute force path (#1505 — user consent bypasses the #761 drain)

**Date:** 2026-09-21
**Session:** The owner's Refresh Compute stopped working on this multi-agent pod: the #761 drain gate defers pod deletion while sessions are busy-and-progressing, and an orchestration pod runs busy sessions around the clock — the drain never finds quiet. Owner decision: the user-initiated refresh/suspend action IS the force path; the UI warning is the consent. *(r2 correction: the "suspend" half of that decision was superseded by #1510/#1507 — suspend is bounded-immediate for every caller and has no drain to bypass; only the refresh half survives.)*
**Status:** Complete

---

## Objective

Refresh Compute (and the user suspend endpoint) must delete the pod immediately despite busy in-flight sessions; every automated suspend source keeps the polite drain.

> **SUPERSEDED (r2 rework):** the objective is now refresh-only — Refresh Compute must delete the pod immediately despite busy in-flight sessions; automated pod-recycle paths keep the polite drain; suspend shares #1510's single bounded-grace path (no drain, no force variant). The suspend-force half below was dropped; see the r2 section.

---

---

## Work Completed

> **SUPERSEDED by the r2 rework below** — everything suspend-scoped in
> this section (`"suspend"` marker value, SuspendWorkspaceForce, the
> handleSuspending bypass, the 6-test matrix) was DROPPED post-#1510.
> The r2 section is the current truth. This record stands unmodified
> per the append-only rule.

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

> r1 list superseded — interfaces.go, mocks/workspace.go,
> mcp_router_integration_test.go, and phase_suspend.go are NO LONGER in
> the diff (the dropped layer); the controller test count is 4 (r2) → 6
> (r3). Current diff (r3):
>
> - `pkg/apis/llmsafespaces/v1/workspace_types.go` (AnnotationForceRecycle + three-site clear invariant)
> - `controller/internal/workspace/force_recycle.go` (new) + `force_recycle_test.go` (new, 6 tests)
> - `controller/internal/workspace/phase_active.go` (Active-recycle bypass + clear),
>   `phase_creating.go` (gen-observe clear), `recovery.go` (Failed gen-observe clear)
> - `api/internal/services/workspace/workspace_service.go` (refresh stamp; no Force variant),
>   `workspace_refresh_test.go` (stamps/never-stamps pins)
> - `api/internal/server/router.go` (refresh contract comment), `router_workspace_test.go` (route pin)
> - `sdks/openapi.yaml` (refresh description)
> - `local/issue-1505-refresh-busy-e2e.sh` + `local/issue_1505_script_test.go` (new)
> - `worklogs/NNNN_2026-09-21_refresh-force-suspend.md` (this file — sentinel until the bot numbers it)

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
- e2e row: local/issue-1505-refresh-busy-e2e.sh + 5 shape pins (busy-before-refresh ordering, Active-budget + NEW-pod + PVC-retained verdicts, fail-closed R2 with BOTH the negative drain-defer grep and the POSITIVE forced-bypass grep, WS_BASE isolation, and the reason-string↔controller-constant match pin that keeps the greps honest).

## r3 — the three r2 findings

- **Finding 1 (invariant false on suspended-refresh / Failed-recovery): FIXED IN CODE.** The generation is observed at THREE sites; the clear existed only at the Active recycle. Added the clear at the Creating gen-observe (phase_creating.go) and the Failed-recovery gen-observe (recovery.go, placed before the Status().Update so the clear's RV-sync holds), both gated on marker presence, both requeue-on-clear-failure (the phase_active shape). RED-first: TestForce_SuspendedRefreshMarkerClearedAtCreatingObserve and TestForce_FailedRecoveryClearsMarker both failed pre-fix (run recorded), green post-fix. The invariant comments at all three claim sites (workspace_types.go, force_recycle.go ×2, workspace_service.go) now state the three-site truth.
- **Finding 4 (worklog self-numbered 1032, colliding with main's bot-assigned 1032_playwright-flake-investigation): FIXED.** My error — I kept the rebase-assigned number instead of the NNNN_ sentinel (the rule my own session notes carry). Renamed to NNNN_; the bot assigns at merge. Stale r1 artifacts (Files Modified list, 6-test count, r1 prose) corrected above under SUPERSEDED banners.
- **Finding 2 (execution gate): CORRECTED, not claimed.** My r2 PR-body claim that this row shares "the same static-delivery disposition as the APPROVED #1507 row" was FALSE characterization — the #1507/#1510 approval closed its execution gate with a REVIEWER-side execution of the script UNMODIFIED on a real kind cluster ("exit 0"); the static part was only the nightly wiring. This row has ZERO recorded executions by anyone. The PR body is corrected; the closure path requested is the same reviewer-side execution the #1507 precedent set (their runner has docker/kind/kubectl), with the #1456 nightly-wiring lane as the post-merge durability follow-up either way.
- Corrected the "polite drain" phrasing everywhere it mentioned suspend (openapi refresh description, router contract comment, force_recycle.go header, phase_active.go comment): the polite drain survives on the AUTOMATED recycle paths (restart-generation bumps, arch drift, password self-heal); suspend has no drain since #1510.

## r4 — the row's verdict fixed after the reviewer's live execution

The reviewer executed local/issue-1505-refresh-busy-e2e.sh UNMODIFIED on a real kind cluster built from 3676acf4 (their transcript: in-flight turn real, R1 Active in 16s vs the never-completing repro, R2 forced-bypass logged with reason restart_generation_user_forced, PVC retained, SessionDrainUserForced event on the CR, marker cleared, zero drain-defer lines) — and the row still exited 1, on MY verdict: "NEW pod" asserted `NEW_POD != OLD_POD`, but podName() is deterministic per workspace UID (constants.go: workspaceName + uid[:8]) — every in-place recycle recreates the pod under the SAME NAME. The verdict was unsatisfiable by construction; it would have permanently reded the nightly lane. Second instance of the r7-predicate bug class (an assertion that can never pass, shipped with a claim that it does) — caught this time by the reviewer's execution, which is exactly why execution is the gate.

Fix: the verdict now asserts `status.restartCount` — the identity that actually changes on a replacement (the reviewer's run: 0→1). Empty-field guards on both reads (jsonpath returns empty-with-exit-0; bare `|| echo 0` never fires — OLD_RC is regex-coerced, NEW_RC regex-gated). Shape pins updated (`restartCount ${OLD_RC} → ${NEW_RC}`, `restartCount did not bump`); local pins + bash -n green. PR body's "NEW pod" phrase corrected. A green re-run of the unmodified row closes the gate.

## r5 — the two doc defects; the row is execution-green

The reviewer's gate-closing kind run at f5ad4e30 PASSED: the row unmodified, restartCount 0→1 live (rc=1 gen=2 observed=1), the guards exercised, one recycle exactly. The two r5 findings were doc-accuracy only, both fixed above: the stale "NEW pod" phrasing in the pin-test comment (the one file the r4 purge missed) and the r1-era header/Objective suspend claims, now bracketed with superseding corrections per the append-only rule. No script changes — no re-execution required per the verdict note.

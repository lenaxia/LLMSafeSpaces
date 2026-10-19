# Worklog: #1746 — agentdDelivery updatePolicy: workspace pods must not roll on platform upgrade

**Date:** 2026-10-19
**Session:** Implementation lane, issue #1746 (agentd updates apply on user refresh, not platform upgrade).
**Status:** First PR complete: policy + status + forceRoll + docs + N-1 compat arm, red-first per convention.

## Objective

Implement the owner ruling (2026-10-19): platform upgrades must not roll workspace pods; agentd digest changes apply at the next natural pod recreation. Deliver `agentdDelivery.updatePolicy` (Rolling | OnRefresh, default OnRefresh) on the Workspace CRD + controller, the `forceRoll` escape hatch, `agentdEffectiveDigest`/`agentdPendingDigest` status fields through the API, operator/architecture docs, and the N-1 compat test arm for the worker-tool seam (`/internal/v1/automation/board/*`).

## Ground truth established before writing code

- Issue #1746 + the #1724 incident thread: the suspected "digest bump → spec-hash drift → fleet recreation" mechanism does NOT exist (no spec-hash reconciler; the overlay digest enters the pod spec only at build time). The lazy roll was already the de-facto behavior; #1746 makes it an explicit, configurable POLICY with visibility. `Rolling` is therefore NEW opt-in behavior (drain-aware proactive roll), not "today's behavior" as the issue summary loosely words it — the acceptance criteria (no recreation under OnRefresh; immediate roll under forceRoll) are the binding contract.
- The #1724 pins (`upgrade_rollout_kill_test.go`) pin OnRefresh-shaped behavior unconditionally: my default-policy resolution (spec → instance flag → OnRefresh) keeps them green untouched — they are the default-posture kill-tests.
- The deletion-path inventory guard (`TestUpgrade_DeletionPathInventory`) is (file, function)-granular and DID catch the new deletion site (in `reconcileAgentdDelivery`, not inlined in `handleActive`) — classified there with the drain ruling: Rolling defers via #761 (`agentd_update_roll`); forceRoll is consent-bypassed (`agentd_update_roll_forced`, the #1505 posture).

## Design decisions (with alternatives)

1. **Digest identity = the digest PORTION only** (`sha256:<64hex>`), not the full image ref. A Renovate repo/tag re-pin of the same digest is not drift — comparing full refs would show false pending/roll on tag-only bumps. Status fields store the digest portion.
2. **forceRoll BYPASSES the drain** (immediate), while `Rolling` is drain-aware. Rationale: the hatch exists for security-critical fixes where waiting behind busy sessions (progress-bound up to 60m) defeats the purpose; the operator setting the flag declares consent, mirroring #1505's user-consent force marker. Both paths carry distinct metric reasons + events, so blast radius is attributable.
3. **forceRoll is level-triggered** (drift-gated no-op when converged), and the controller never clears it — spec is operator/API-owned (single-writer discipline); the docs instruct clearing after use. An edge-triggered auto-clearing hatch would need a spec write from the controller (anti-pattern here).
4. **Policy resolution chain**: spec.agentdDelivery.updatePolicy → controller flag (`--agentd-update-policy`, Helm `controller.agentdDelivery.updatePolicy`) → OnRefresh. Empty spec (all pre-existing workspaces) inherits the instance default — the migration path is a no-op. No kubebuilder materialization concerns: absent objects stay absent.
5. **Where the roll hooks handleActive**: after the arch-drift recycle (so a pending natural recreation isn't double-triggered), before the agentd integrity detectors (a stale-digest roll may itself fix a verify failure). OnRefresh bookkeeping rides handleActive's existing final Status().Update; the deferral branch persists status itself before returning (the early return would otherwise drop the write). Builds record effective/pending in handleCreating right after pod create.
6. **The N-1 compat arm is a wire-freeze matrix, not a version-bump test**: real N-1 binaries aren't runnable in-repo, so the skew surface is the WIRE FORM. `pkg/board/worker_compat.go` freezes each still-supported agentd release's exact requests (method, path+query, stamped body); the API test replays every frozen set against the CURRENT seam (real resolve chain), and the agentd test pins the live client to emit exactly the newest set. Neither side can drift alone; changes are additive (new set or /internal/v2). Fixture workspace IDs are UUIDs (production shape); the SA-principal fake pins to the same ID.
7. **Chart flag renders only when set** (empty = controller default OnRefresh): keeps `TestAgentdDelivery_ImageOnlyRendersSingleFlag`'s exactly-one-`--agentd-*`-flag contract intact for the Renovate form, and adds render-time validation (`must be Rolling or OnRefresh`) so a typo'd value fails install rather than boot.

## Work completed

- **Red-first commit (57230a60)**: all pins before behavior. 7 controller arms + 2 API arms red (assertion output quoted in the commit message); compat arms green by design (regression pins); inert plumbing (types, chart CRD lockstep, deepcopy, flag wiring, DTO fields) shipped in the same commit so the red is behavioral, not a compile failure. The unused-helper lint findings in the interim state were resolved by moving the digest helpers to the behavior commit.
- **CRD**: `spec.agentdDelivery` (`updatePolicy` enum Rolling|OnRefresh default OnRefresh, `forceRoll` bool default false; operator-facing like `runtimeClass`), status `agentdEffectiveDigest`/`agentdPendingDigest`; `helm/crds/workspace.yaml` in lockstep (repolint CRD-drift green); deepcopy regenerated via `make deepcopy`.
- **Controller** (`agentd_update_policy.go`, hooks in `phase_active.go`/`phase_creating.go`): policy resolution, digest-portion identity, drift status bookkeeping, drain-aware Rolling roll, consent-bypassed forceRoll, `AgentdUpdateForcedRoll` event, metric reasons `agentd_update_roll` / `agentd_update_roll_forced`; `--agentd-update-policy` flag + startup validation (`ValidateAgentdUpdatePolicy`) + `SetupControllers` wiring.
- **API**: `WorkspaceStatusResult.agentdEffectiveDigest`/`agentdPendingDigest` mapped from the CRD in `GetWorkspaceStatus`.
- **Helm**: `controller.agentdDelivery.updatePolicy` value (documented; empty = OnRefresh), template flag + render-time validation; chart pins (`agentd_update_policy_chart_test.go`) incl. the exactly-one-flag contract preservation.
- **Compat arms**: `pkg/board/worker_compat.go` (frozen v0.39.0 wire set + additive-only maintenance rule), `api/internal/handlers/board_worker_compat_1746_test.go` (replay + guard-outcome classes + route-version pin), `pkg/agent/opencode/loopback_board_wire_1746_test.go` (exact-emission pin).
- **Docs**: `docs/operator/agentd-delivery.md` (Update policy section: matrix, kubectl/helm examples, drift visibility, N-1 discipline), `docs/operator/storage.md` (platform-delivered volumes section pointing at the policy), `docs/architecture/components.md` (controller ownership bullet + the versioned board seam contract row). CHANGELOG `[Unreleased]`.

## Verification

- `go test ./controller/internal/workspace/` green (full package incl. the #1724/#761 suites); policy arms: OnRefresh no-roll busy+idle with pending recorded; application at natural recreation with pending cleared; Rolling deferred→released with drain metric + rebuilt digest; instance-default Rolling + spec override; forceRoll immediate with forced metric + event + rebuild; forceRoll no-drift no-op; build records effective.
- `go test ./api/internal/services/workspace/ ./api/internal/handlers/ ./pkg/agent/opencode/ ./pkg/board/ ./pkg/apis/... ./helm/` green (helm on PATH; full chart suite with real renders).
- `make helm-render` renders clean (with `--kube-version 1.35.0` per chart floor; the bare target's default kube-version predates the chart floor — pre-existing, CI passes the flag).
- Fake-client lesson (test-infra, not product): `r.Update` on a stale local object conflicts after any reconcile (the #1724 suite's pattern updates specs BEFORE the first reconcile); mid-test spec changes re-GET fresh objects first.

## Disclosed limits / follow-ups

- `Rolling` rolls one reconcile pass per workspace with the drain's stall bound — it is NOT a fleet-wide orchestrated rollout (no concurrency budget across workspaces; each workspace's drain defers independently). Documented in the values comment; an orchestrated fleet budget would be new design.
- The instance-level "critical-release marker" (issue design bullet 3, "optionally") is NOT implemented — forceRoll is the delivered per-workspace hatch; an instance-wide critical marker is additive follow-up design if the owner wants it.
- session-register joins the compat matrix when #1691 lands (issue notes it); the frozen-set structure is ready for the append.
- Dashboard rendering of "agentd X pending — applies on next refresh" is the API field's consumer; no frontend change in this PR (the field is the contract).

## r2 (review round 2, PR #1754 CHANGES_REQUESTED → fixes)

Two blocking findings, both fixed:

1. **CRD `default: OnRefresh` defeated the inheritance chain (Finding 1).** Kubernetes defaulting fills defaults for properties of a PRESENT object, so any write materializing `spec.agentdDelivery` without an explicit policy — including this PR's own documented forceRoll kubectl patch — got `updatePolicy: OnRefresh` stamped server-side; from then on `resolveAgentdUpdatePolicy` never consulted the instance flag (silently, permanently, surviving the forceRoll clear). My design-decision 4 above ("absent objects stay absent") was half right — absent FIELDS WITHIN a present object do default; fake-client tests cannot see this (no CRD defaulting). Fix: dropped the default from the kubebuilder marker and the chart CRD (empty = inherit, resolved in code — the one place the whole chain is visible); red-first pin `agentd_delivery_crd_semantics_test.go` committed failing first (commit 4f16f3e0), then the fix; docs state the no-default contract. `forceRoll`'s `default: false` is kept — false is the zero value, a materialized false is behaviorally identical to absent.
2. **Load-bearing hook ordering unpinned (Missing test 1).** The policy pass runs BEFORE `detectAgentdVerificationFailure` so a Rolling workspace with a verify-failing pod (exit 81) rolls (the new digest may fix it) instead of parking forever on the level-triggered detector. Added `TestAgentdUpdatePolicy_Rolling_RollWinsOverVerifyFailure`; mutation-validated (hook moved below the detectors → the pin FAILS with "the Rolling roll must proceed despite the verify-failure exit"; reverted, green).

Non-blocking notes acknowledged in the review reply: forceRoll's event/metric fire before the status write (re-fires per 409 retry — mirrors the existing `noteForcedRecycle` shape, left convention-consistent); the CHANGELOG header URL drive-by edit reverted to the original bytes.

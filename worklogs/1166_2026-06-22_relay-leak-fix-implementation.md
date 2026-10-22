# Worklog: EC2-Leak Fix — Tag-Based Adoption + Orphan Detector

**Date:** 2026-06-22
**Session:** Implement the proper fix for the EC2-leak-on-Status-conflict bug discovered in worklog 0473. Both adoption + orphan detector approach (defense in depth) per user direction.
**Status:** PR #344 open and reviewed. Code complete; addressing review findings.

---

## Objective

Worklog 0473 surfaced a real production bug: two EC2 instances were alive 14h after the controller logged "all relay VMs destroyed" for them. Root cause: `provisionRelay` creates the EC2 successfully but `r.Status().Update(ctx, relay)` fails with optimistic-concurrency conflict, leaving the InstanceID unrecorded. Future reconciles re-provision (creating duplicates), and `handleDeletion` only iterates `Status.Instances` — original VMs forever orphaned.

This session: ship the production fix.

---

## Design

The fix is **defense in depth**. Three independent paths, each catches a different failure mode:

### 1. Tag-based adoption (reconcileFleet pre-pass)

Before provisioning, list cloud VMs filtered by this CR's UID + provider. If a tagged VM exists for a spec provider that has no Status entry, **adopt** it (synthesize a `RelayInstanceStatus`) instead of calling `Provision` again. Catches the same-reconcile-cycle leak.

Also refreshes `PublicIP` on existing Status entries when the listing has a fresh IP — handles the "adopted while pending, IP attached later" path.

### 2. Deletion tag sweep (handleDeletion)

After the existing `Status.Instances` destroy loop, list cloud VMs by spec providers/regions and destroy any tagged with this CR's UID that aren't already-known and aren't terminated. Catches Status-conflict-orphans at delete time.

The `alreadyProcessed` set (from `Status.Instances` IDs) prevents double-destroy of VMs that the Status loop just terminated but the cloud still reports as running due to eventual consistency.

### 3. OrphanDetector (periodic, leader-only)

`manager.Runnable` that lists every cloud VM tagged `managed-by=llmsafespaces-relay` across all relevant regions and destroys any whose owner UID is not in the cluster's active CR set.

Uses a **CR-list → VM-list → CR-list sandwich pattern** to avoid the race where a freshly-created CR's VM would be misclassified as orphan. The race window analysis is in the method-level doc comment; pinned by `TestOrphanDetector_RaceAvoidance_NewCRAddedDuringSweep`.

`NeedLeaderElection()` returns true so multi-replica controllers don't race to destroy the same orphans. Default 5-minute interval.

**Empty-OwnerUID safety**: legacy / pre-fix VMs (no UID tag) are intentionally NOT auto-destroyed — operator must audit and clean up manually.

---

## Implementation

### Driver tagging contract (driver.go, aws_driver.go, oci_driver.go)

- `ProvisionRequest.OwnerUID` and `.Provider` fields. Drivers tag every provisioned VM with `inferencerelay-uid=<CR.UID>` and `inferencerelay-provider=<provider>` on top of the existing `managed-by=llmsafespaces-relay`.
- `VMInstance.OwnerUID` and `.Provider` populated by `ListInstances` from the cloud's tags.
- Tag constants centralized in `driver.go` (`TagOwnerUID`, `TagProvider`, `TagManagedBy`, `TagManagedByValue`).
- OCI `ListInstances` previously returned all VMs in the compartment; now filters by `managed-by` and uses the passed-in `region` (was incorrectly using `cfg.Region`). Also fetches `PublicIP` per-instance via `getPublicIP` so adoption produces a working endpoint.

### Reconciler changes (reconciler.go)

- `provisionRelay` now passes `OwnerUID` (from `relay.UID`) and `Provider` to the driver.
- New `adoptOrphanedInstances` helper runs as a pre-pass in `reconcileFleet`. Returns extra duplicates to destroy after the main loop completes.
- `handleDeletion` extended with the tag sweep + `alreadyProcessed` filter.

### OrphanDetector (orphan_detector.go, new)

Standalone runnable. Wired into `SetupRelayController` alongside the per-CR reconciler, sharing the same `Drivers` map.

---

## Tests

Total: 18 new tests across 3 files.

### `adoption_test.go` (9 tests)
- `TestAdoption_StatusUpdateConflict_RecoversWithoutDuplicate` — the headline fix
- `TestAdoption_DuplicateTaggedVMs_DestroysExtras` — 1 adopted, N-1 destroyed
- `TestAdoption_DifferentUID_NotAdopted` — cross-CR isolation
- `TestAdoption_TerminatedTaggedVM_NotAdopted` — terminal state skip
- `TestProvisionRequest_TagsContainOwnerUID` — wire contract
- `TestHandleDeletion_TagSweep_DestroysOrphans` — deletion-side recovery
- `TestAdoption_OCIStyle_EmptyPublicIP_AdoptsThenRefreshesNextCycle` — OCI pending-IP flow
- `TestAdoption_MultiProvider_AdoptsCorrectSlot` — AWS+OCI in one CR
- `TestHandleDeletion_StatusInstancesAndTaggedOrphans_NoDoubleDestroy` — `alreadyProcessed` filter

### `oci_driver_test.go` (1 table-driven test, 4 cases)
- `TestOCIProvisionTags` — pure FreeformTags shape across full / no-UID / no-provider / empty inputs

### `orphan_detector_test.go` (12 tests)
- 11 from initial commit (no-op happy path, keep matching, destroy unmatched, legacy untagged skip, terminated skip, list-error per-driver isolation, destroy-error continuation, multi-region multi-CR sweep, operator region override, leader-election, runnable lifecycle)
- `TestOrphanDetector_RaceAvoidance_NewCRAddedDuringSweep` — the sandwich pattern verification

---

## Review Findings Addressed

PR #344 review (commit-by-commit):

1. **OCI ListInstances ignored region parameter** — fixed: now uses `region` arg, falls back to `cfg.Region` only if empty
2. **OCI ListInstances missing PublicIP** — fixed: fetches per-instance via `getPublicIP` for running/pending VMs (skip for terminated/stopped to avoid dead API calls)
3. **handleDeletion double-destroy of Status.Instances IDs** — fixed: `alreadyProcessed` set
4. **Missing test: OCI adoption with empty PublicIP** — added `TestAdoption_OCIStyle_EmptyPublicIP_AdoptsThenRefreshesNextCycle`
5. **Missing test: multi-provider adoption** — added `TestAdoption_MultiProvider_AdoptsCorrectSlot`
6. **Missing test: handleDeletion with both Status entries AND tagged orphans** — added `TestHandleDeletion_StatusInstancesAndTaggedOrphans_NoDoubleDestroy`
7. **Stale `provisionRelay` doc comment (WG public key reference)** — fixed: removed pre-existing stale duplicate
8. **Missing worklog 0474** — this file
9. **Adopted entry without IP refresh path** — fixed: existing-Status path now refreshes `PublicIP` from cloud listing when empty

Open / acknowledged but not addressed in this PR:
- **OrphanDetector interval not Helm-configurable** — acknowledged as Phase 2 work. The runnable accepts an `Interval` field; just no plumbing through `SetupRelayController` yet. Easy follow-up.

---

## Key Decisions

- **Both adoption + detector**, not just one. Adoption recovers in the same reconcile cycle (best UX). Detector catches everything else. The combination is robust to controller crashes mid-flow, conflicts at multiple points, and pre-existing leaked VMs from earlier controller versions.

- **Empty-UID VMs are NEVER auto-destroyed**. The detector logs them at V(1) and skips. Operators must audit and clean up manually. Reasoning: a pre-fix-version VM (or third-party VM that happens to have `managed-by=llmsafespaces-relay`) might exist; auto-destroying could be catastrophic. Once enough time passes after this fix ships, we can remove the empty-UID skip and rely entirely on the UID tag.

- **CR-list → VM-list → CR-list sandwich**, not just one CR-list. Without the second list, a freshly-created CR's VM could be classified as orphan during a sweep. The sandwich proves: any VM seen by the sweep was either (a) created before the first CR-list — in which case the CR is in the first list, or (b) created between the first CR-list and VM-list — in which case the second CR-list captures the CR (a CR can't be deleted between Provision and the second CR-list without the per-CR finalizer running first, which would have destroyed the VM).

- **Per-CR adoption uses `existingByProvider` map mutation** so the existing main loop doesn't need restructuring. Adopt path writes a synthesized `RelayInstanceStatus` into the map; main loop sees it as already-known.

---

## Adversarial Self-Review

1. **What if the same VM is tagged with two different UIDs (impossible, but check)?** — AWS tag keys are unique; OCI FreeformTags are a `map[string]string` so unique. Not a real concern.

2. **What if the controller is upgrading and replicas are running both old and new code?** — Old replicas don't tag (no `OwnerUID` field on `ProvisionRequest`); they'd produce empty-UID VMs. New replicas' detector skips empty-UID. Old replicas' deletion path doesn't have the tag sweep, so it's unchanged behavior. Compatible.

3. **What if `ListInstances` returns an instance with the new UID tag but the controller's `Drivers` map doesn't have that provider (e.g. provider was dropped from spec)?** — `maybeDestroyOrphan` checks `driver := d.Drivers[provider]; if driver == nil { return }`. Skipped. The Status loop's spec-removed-provider path catches it earlier.

4. **What if multiple OrphanDetectors run simultaneously (no leader election)?** — `NeedLeaderElection()` is true. controller-runtime guarantees only the leader runs.

5. **What if `Reconcile` is in flight, calls Provision, EC2 created at T1, then OrphanDetector starts a sweep at T2?**
   - T1 < T2: VM exists at VM-list time. CR was created before T1 (Reconcile only fires after CR-create). Both CR-list passes capture the CR. No false destroy.
   - T2 < T1 < T3 (T3 = end of Reconcile): VM doesn't exist at VM-list time. Even if CR-list passes don't capture the CR (impossible if T0 < T2), no VM in list to consider.
   - The sandwich proof in the doc comment covers this.

6. **What if the cluster has 1000 InferenceRelay CRs?** — `client.List` returns them all in one call. `len(activeUIDs) = 1000`. Map lookup O(1). Fine.

7. **What if a region has hundreds of tagged VMs?** — `ListInstances` returns all of them. For OCI, that's hundreds of `getPublicIP` calls per sweep — slow but bounded. Could optimize with batch list; out of scope for now.

8. **What if a driver's `Destroy` is non-idempotent (calling on already-terminated returns error)?** — AWS `TerminateInstances` is idempotent. OCI's terminate is also idempotent. Other drivers added in future would need to follow this contract.

---

## Blockers

None.

---

## Tests Run

```bash
go build ./...                                                  # pass
go test -timeout 60s ./controller/internal/relay/               # pass
go test -timeout 240s -short ./...                              # pass (all green)
make lint                                                       # 0 issues
```

Total: 21 tests added in this PR (9 adoption + 1 OCI tag + 12 orphan detector — including the headline race-avoidance test).

---

## Next Steps

1. Wait for re-review on PR #344
2. After merge: deploy and run live verification
3. Live tests:
   - Apply InferenceRelay CR
   - Verify EC2 is tagged with `inferencerelay-uid` + `inferencerelay-provider` (via aws cli)
   - Force a Status conflict (patch Status from another writer to bump resourceVersion mid-reconcile, then check no duplicate created)
   - Delete CR, verify EC2 terminated within 30s (Status loop) or via tag sweep
   - Pre-create an EC2 with `inferencerelay-uid=fake-uid` and `managed-by=llmsafespaces-relay`, verify OrphanDetector destroys it within 5 min
4. Worklog 0475 with live verification evidence

---

## Files Modified

| File | Change |
|---|---|
| `controller/internal/relay/driver.go` | New `OwnerUID` + `Provider` fields on `ProvisionRequest` and `VMInstance`; tag constants |
| `controller/internal/relay/aws_driver.go` | Tag instances on RunInstances; populate VM fields from tags in ListInstances |
| `controller/internal/relay/oci_driver.go` | Same as AWS via FreeformTags; fix region parameter; fetch PublicIP per instance; `ociProvisionTags` helper |
| `controller/internal/relay/reconciler.go` | `adoptOrphanedInstances` helper + caller; PublicIP refresh path; `handleDeletion` tag sweep with alreadyProcessed; cleanup of stale provisionRelay doc |
| `controller/internal/relay/orphan_detector.go` | New: periodic detector with sandwich race-avoidance |
| `controller/internal/controller/controller.go` | Wire OrphanDetector via `mgr.Add(detector)` |
| `controller/internal/relay/adoption_test.go` | New: 9 adoption tests |
| `controller/internal/relay/oci_driver_test.go` | New: tag shape table test |
| `controller/internal/relay/orphan_detector_test.go` | New: 12 detector tests including race avoidance |
| `controller/internal/relay/reconciler_test.go` | Extended `stubDriver` with `provisionCalls` + `listInstances` + `listErr` |
| `worklogs/0498_2026-06-22_relay-leak-fix-implementation.md` | This file |

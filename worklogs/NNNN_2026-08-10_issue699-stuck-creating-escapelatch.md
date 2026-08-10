# Worklog: Stuck-Creating escape hatch + FailedMount auto-recovery (issue #699)

**Date:** 2026-08-10
**Session:** Diagnose a hung workspace in prod, root-cause the platform and cluster layers, fix both platform-side defects with regression tests.
**Status:** Complete

---

## Objective

Workspace `5c25e2ef-3f07-48f9-ae50-9769382e6da8` was hung in `Creating` on the prod cluster. Diagnose why, prevent data loss on the PVC, root-cause both the platform and cluster layers, and implement architecturally-correct fixes with regression tests.

---

## Root Causes (Proven)

### Cluster layer (Longhorn + CSI) — data-loss amplifier

`worker-02` (recently provisioned, 7d17h) has chronic engine-image instability: `engine-image-ei-a4d05f02-bqtnc` had **215 restarts** and `engine-image-ei-ff1cedad-5vbdm` had **192 restarts** (15h window). worker-00's same pods (63d old) had **0 restarts**. Both NVMe disks report SMART `PASSED`, 0 MediaErrors — not a hardware fault.

Each engine-image bounce breaks I/O to volumes whose engine is on worker-02. The data-loss chain:

1. engine-image pod bounces (~1/hr)
2. block-device I/O to volumes on worker-02 fails transiently
3. CSI `NodeStageVolume` calls `blkid` to detect the existing filesystem
4. `blkid` read of sector 0 fails with I/O error → returns "no filesystem"
5. CSI proceeds to `mkfs.ext4` to "format the empty device" — would overwrite user data
6. mkfs fails on the same I/O errors → only reason data survived
7. Longhorn `robustness=healthy` throughout (probe = process liveness, not data integrity)

Kubelet re-queued `NodeStageVolume` every ~30s with backoff; 9+ mkfs retries over 4+ minutes before manual halt.

### Platform defect 1 — `spec.suspend` silently ignored outside Active

The tri-state `suspend` field (`pkg/apis/llmsafespaces/v1/workspace_types.go:181`) was documented as Active-only and honored only by `handleActive` (`controller/internal/workspace/phase_active.go:41`). The API layer explicitly rejects non-Active with a 409 (`api/internal/services/workspace/workspace_service.go:699`), but a raw `kubectl patch workspace <name> -p '{"spec":{"suspend":true}}'` is accepted by etcd with no validation, no event, no condition. The controller silently swallows the request.

Operator's first-line defense (`spec.suspend=true`) was a silent no-op, leaving cluster-wide controller scale-to-0 as the only halt.

### Platform defect 2 — FN3b recovery guard missed FailedMount pods

`controller/internal/workspace/phase_creating.go` FN3b ("Scheduled but stuck") used a `len(ContainerStatuses)==0 && len(InitContainerStatuses)==0` predicate. A FailedMount pod has non-zero status entries (each container in pure `Waiting` state), so the predicate never fired. Observed on prod:

```
initContainerStatuses: 2 (both Waiting, PodInitializing)
containerStatuses:     1 (Waiting, PodInitializing)
PodScheduled=True, all other conditions False
```

The predicate expressed the wrong concept: "kubelet allocated zero status objects" instead of the intent "kubelet has not made progress on any container."

---

## Work Completed

### Incident response (cluster)

- Scaled `deploy/llmsafespaces-controller` to 0 to halt the reconciliation loop (only available kill switch at the time)
- Deleted the stuck pod to stop mkfs retries
- Verified PVC data preserved on the worker-00 Longhorn replica (stopped, `failedAt: ""`, `active: true`)
- Confirmed volume detached cleanly post-incident (`state=detached`, `robustness=unknown`)

### Issue filed

[#699](https://github.com/lenaxia/LLMSafeSpaces/issues/699) with full evidence, both root-cause analyses, and proposed fixes. Two comments capture the Longhorn root-cause and the implementation summary.

### Defect 2 fix — minimal predicate swap

- Added `noContainerHasEverStarted(pod)` helper in `controller/internal/workspace/classification.go`. Checks the actual signal: whether any container has `State.Running`, `State.Terminated`, or non-nil `LastTerminationState`. A container the kubelet has never reached (FailedMount) has only `State.Waiting` and nil `LastTerminationState`.
- FN3b predicate in `phase_creating.go` swapped from `len(...)==0` to `noContainerHasEverStarted(pod)`. Comment updated with issue #699 reference.
- No changes to `PodObservation` or `classifyFailure` — FN3b hardcodes `FailureClassInfrastructure` and doesn't route through `classifyFailure`.

**Abstraction level choice (Rule 12):** one consumer today. Pure function on `*corev1.Pod` in the classification module, not a new `PodObservation` field. Promote if a second consumer appears.

### Defect 1 fix — generalize suspend to all pre-Suspended phases

- New `suspendFromPreActive` helper in `controller/internal/workspace/phase_suspend.go`:
  - Computes pod name from workspace name + UID (matches `handleSuspending` convention; `Status.PodName` may be stale or unset)
  - Deletes any existing pod → detaches volume → halts CSI mkfs retries
  - Transitions directly to Suspended (no Suspending phase — no agent to drain)
  - Clears recovery state (parity with `handleSuspending` US-24.8 F22)
  - Does NOT touch `WorkspacesRunning` gauge (only Inc'd on Creating→Active; pre-Active never Inc'd)
  - DOES `Dec` `WorkspacesInRecovery` if applicable
  - Clears `spec.suspend` per the US-23.3 ack contract (status update first, then spec clear — per `clearSuspendRequest` doc)
- Wired into `handleCreating` at the very top (before pod creation — immediate halt)
- Wired into `handlePending` after the finalizer (so cleanup runs on delete) but before PVC creation (don't allocate resources we're about to park)
- Updated `Suspend` field doc in `workspace_types.go` to reflect generalized semantics

**Abstraction level choice:** rather than adding a new `spec.abort` field (more API surface, subtle distinction, same end-state), recognized that `Suspended` is already the universal "PVC safe, pod deleted, resume on demand" state. Single concept, symmetric with resume.

### Regression tests (TDD)

Wrote tests first (confirmed RED), then implemented to make them GREEN:

| Test | Validates |
|---|---|
| `TestReconcile_Creating_PodStuck_FailedMountShape_EntersRecovery` | Exact prod pod shape (2 init + 1 container, all Waiting, age > timeout) now enters Infrastructure recovery + deletes pod |
| `TestReconcile_Creating_PodStuck_FailedMountShape_BelowTimeout_NoRecovery` | False-positive guard: same shape, age < timeout, no recovery |
| `TestReconcile_Creating_PodStuck_FailedMountShape_RunningContainer_NoRecovery` | Misclassification guard: any container ever Running/Terminated → not stuck |
| `TestReconcile_Creating_SpecSuspendTrue_TransitionsToSuspended` | Suspend in Creating parks in Suspended, clears field, PVC remains Bound |
| `TestReconcile_Creating_SpecSuspendTrue_DeletesExistingPod` | Suspend in Creating deletes existing pod (halts CSI mkfs) |
| `TestReconcile_Pending_SpecSuspendTrue_TransitionsToSuspended` | Suspend in Pending parks in Suspended, clears field |

**Pre-existing test corrected:** `TestHandleCreating_ScheduledPendingWithInit_NoRecovery` in `recovery_wiring_test.go` was codifying the bug — its fixture had an init container in pure `Waiting` state (never started) but the comment said "init containers running." Updated the fixture to match the stated intent (init container `Terminated: Completed`), which is the genuine "in progress, not stuck" case.

---

## Key Decisions

1. **Scope: fix both platform defects, defer cluster-side.** The platform-side fixes (auto-recovery + escape hatch) are in our control and would have prevented the data-loss window. The cluster-side root cause (worker-02 engine-image instability, CSI mkfs-on-blid-failure) is tracked in #699 as separate ops work — out of scope for this PR.

2. **`noContainerHasEverStarted` over `StuckWaiting` field.** Initial draft added a `StuckWaiting` field to `PodObservation` and routed it through `classifyFailure`. Reverted — the field had one consumer, and FN3b doesn't use `classifyFailure` (it hardcodes `FailureClassInfrastructure`). A pure helper is the right shape. See Rule 12 (containment before abstraction).

3. **Generalize `spec.suspend` rather than add `spec.abort`.** `Suspended` is already the universal "PVC safe, pod deleted, resume on demand" state. The field's intent already covers the pre-Active case; the original Active-only scoping was an artifact of where the API path was first implemented, not a semantic boundary. Single concept, symmetric with resume.

4. **Direct Pending/Creating→Suspended, not via Suspending.** The Suspending phase exists to drain a running agent. Pre-Active workspaces have no agent to drain; routing through Suspending would add a no-op phase transition. Direct transition is simpler and matches the actual semantics.

5. **Defer API-layer `SuspendWorkspace` 409 relaxation.** The API path still rejects non-Active suspend with a 409. Controller honoring raw spec writes is the unblock for operators; relaxing the API path is a separate UX decision (what should the API return for suspend-on-Creating — 202 accepted? synchronous?) and is noted in #699 for follow-up.

---

## Assumptions Stated + Validated

1. **`WorkspacesRunning` gauge is only Inc'd on Creating→Active.** Validated by reading `phase_creating.go:153` — the only Inc site. Pre-Active suspend never reached that line, so no Dec is needed in `suspendFromPreActive`. ✓
2. **`WorkspacesInRecovery` is Inc'd regardless of phase.** Validated by reading `recovery_policy.go:139` (`recordRecoveryMetrics`, called from `enterRecovery`). Must Dec in `suspendFromPreActive` if in recovery. ✓
3. **`clearSuspendRequest` ordering: status update first, then spec clear.** Validated by reading `suspend_request.go:27-31` doc + the existing `handleSuspending`/`handleSuspended` call order. ✓
4. **`Status.PodName` may be stale or unset in Creating.** Validated by reading `phase_creating.go:122` — `PodName` is set after pod creation, and a stuck pod may have been created by a previous reconcile cycle. Compute name from UID instead. ✓
5. **Direct pod delete halts CSI mkfs retries.** Validated live — pod delete on prod caused Longhorn to detach the volume, ending the NodeStageVolume retry loop. ✓

---

## Blockers

None for the platform-side fixes. Cluster-side (worker-02 engine-image instability) is tracked as separate ops work in #699.

---

## Tests Run

- `go test -timeout 60s ./controller/internal/workspace/ -run 'TestReconcile_Creating_PodStuck_FailedMountShape|TestReconcile_Creating_SpecSuspendTrue|TestReconcile_Pending_SpecSuspendTrue'` — 6 new tests, RED→GREEN
- `go test -timeout 120s ./controller/internal/workspace/` — all pass (including the corrected `TestHandleCreating_ScheduledPendingWithInit_NoRecovery`)
- `go test -timeout 180s ./controller/... ./pkg/apis/...` — all pass
- `go vet ./controller/... ./pkg/apis/...` — clean
- `gofmt -l` on all 7 changed files — clean
- `golangci-lint` — not runnable in this environment (binary absent); CI will cover

---

## Next Steps

1. **Cluster recovery (separate from this PR):** investigate worker-02 engine-image restart loop (dmesg, kernel/PCIe/NVme driver errors), consider upgrading to `6.18.18-talos` to match worker-00. Consider cordoning worker-02 until root-caused.
2. **Restore prod:** scale controller back to 1, recover the workspace's PVC from the worker-00 Longhorn replica.
3. **API-layer follow-up:** decide whether `SuspendWorkspace` API should accept Creating/Pending (202 accepted) or remain 409. Tracked in #699.
4. **Longhorn upstream:** file a bug — `blkid` returning nothing on I/O error should be treated as "unknown state, refuse to format," not "empty, format it."
5. **Optional defense-in-depth:** surface a `SuspendRequestIgnored` condition when `spec.suspend` is set in a phase that won't honor it (moot now that all pre-Suspended phases honor it, but the pattern is worth keeping for future fields).

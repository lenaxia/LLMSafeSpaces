// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// harness_binding.go — the bind-once harness lifecycle at the
// controller tier (owner ruling 2026-11-16: workspaces do NOT switch
// harnesses; one harness per workspace, bound once, immutable
// forever; the 2026-11-15 prelim's switch-with-recreate design was
// REJECTED — no drift arm, no sweep, no switch window exists here).
//
// Exactly two responsibilities:
//
//   - LEGACY BACKFILL: a workspace with spec.harness == "" and NO
//     AnnotationHarnessUnbound marker predates the binding ruling;
//     under the old CRD schema default it has only ever run opencode.
//     It is stamped to an explicit "opencode" on first observe (a
//     ""→opencode bind, which webhook 7a admits), making the
//     "every workspace is explicitly bound" invariant true
//     retroactively with zero behavior change. Idempotent and
//     repeatable across controller restarts: born-unbound workspaces
//     carry the marker and are never stamped.
//
//   - UNBOUND GATE: a born-unbound workspace (marker present,
//     spec.harness == "") deliberately gets NO pod while it waits for
//     the picker's choice — the pod's topology is harness-class, so
//     nothing is built until the bind (first session or explicit
//     PUT /harness) sets spec.harness. The PVC is still created and
//     retained (handlePending is harness-agnostic): storage is
//     never stranded by the bind, and the bind itself never
//     recreates anything (there was no pod to recreate).

// bornUnbound reports whether the workspace was intentionally created
// unbound (the frontend-picker path): spec.harness == "" AND the
// discriminator marker is present. A marker-less empty harness is a
// legacy object (pre-binding-ruling), NOT born-unbound.
func bornUnbound(ws *v1.Workspace) bool {
	return ws.Spec.Harness == "" && ws.Annotations[v1.AnnotationHarnessUnbound] == "true"
}

// reconcileHarnessBinding runs before phase dispatch on every
// non-deleting workspace reconcile. It returns handled=true when the
// reconcile should stop after this pass (backfill stamp landed —
// requeue to re-read the bound object; nothing else may proceed on a
// half-observed spec write).
func (r *WorkspaceReconciler) reconcileHarnessBinding(ctx context.Context, ws *v1.Workspace) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if ws.Spec.Harness != "" {
		// Bound. A stale True binding-pending condition (set while the
		// workspace waited unbound, bound since) is cleared to False —
		// an Active workspace must not advertise a pending choice.
		r.clearHarnessBindingPending(ws)
		return false, ctrl.Result{}, nil
	}

	if !bornUnbound(ws) {
		// Legacy pre-binding-ruling object: stamp explicit opencode
		// (the value it has always run). The webhook admits
		// ""→opencode for anyone; a concurrent first-session bind of a
		// different harness on this same object cannot exist (the API
		// tier refuses non-opencode binds without the marker), so the
		// only race is another writer — resolved by resourceVersion
		// retry, and if the object is meanwhile bound the pass below
		// re-reads and no-ops.
		logger.Info("Legacy unbound workspace; backfilling spec.harness=opencode (bind-once invariant)")
		if err := r.backfillLegacyHarness(ctx, ws); err != nil {
			// Not a webhook denial in practice (only ""→opencode
			// here); treat any failure as retryable.
			return true, ctrl.Result{Requeue: true}, err
		}
		return true, ctrl.Result{Requeue: true}, nil
	}

	// Born-unbound: wait for the bind. No pod is built in this state;
	// the gate in handleCreating enforces it phase-locally too
	// (defense in depth for restores/resumes that land in Creating
	// without passing through here).
	return false, ctrl.Result{}, nil
}

// backfillLegacyHarness stamps spec.harness=opencode on a legacy
// (marker-less, empty-harness) workspace. Fresh-read + conflict-retry
// (the clearForceRecycleAnnotation shape): never writes from a stale
// cache; a mid-flight bind by another writer wins and this pass
// no-ops (bound is bound — the invariant holds either way).
func (r *WorkspaceReconciler) backfillLegacyHarness(ctx context.Context, ws *v1.Workspace) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var fresh v1.Workspace
		nn := types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}
		if err := r.Get(ctx, nn, &fresh); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if fresh.Spec.Harness != "" || fresh.Annotations[v1.AnnotationHarnessUnbound] == "true" {
			// Someone bound it (or it is born-unbound after all):
			// nothing to stamp.
			ws.ResourceVersion = fresh.ResourceVersion
			return nil
		}
		fresh.Spec.Harness = v1.HarnessOpencode
		if err := r.Update(ctx, &fresh); err != nil {
			return err
		}
		ws.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// clearHarnessBindingPending flips a True HarnessBindingPending
// condition to False (reason Bound) on the in-memory object; callers
// persist it with their next Status().Update. No-op when the
// condition is absent or already False.
func (r *WorkspaceReconciler) clearHarnessBindingPending(ws *v1.Workspace) {
	for i := range ws.Status.Conditions {
		c := &ws.Status.Conditions[i]
		if c.Type == v1.WorkspaceConditionHarnessBindingPending && c.Status == "True" {
			c.Status = "False"
			c.Reason = "HarnessBound"
			c.Message = "harness bound; workspace is harness-immutable for its lifetime"
		}
	}
}

// gateUnboundCreating is the handleCreating-local enforcement of the
// unbound gate: a born-unbound workspace never builds a pod. Sets the
// HarnessBindingPending condition + status message once (idempotent),
// and returns true when the caller must stop the reconcile pass (no
// pod build, no phase transition — the spec write of the bind is what
// re-arms the phase machine via the watch).
func (r *WorkspaceReconciler) gateUnboundCreating(ctx context.Context, ws *v1.Workspace) (bool, error) {
	if !bornUnbound(ws) {
		return false, nil
	}
	logger := log.FromContext(ctx)

	// Idempotence probe (setCondition returns no changed signal): a
	// pending condition already True with the parked message means
	// this pass writes nothing and never hot-requeues — the bind's
	// spec write is the wakeup event.
	if harnessBindingPendingIsParked(ws) && ws.Status.Message == "waiting for harness selection" {
		return true, nil
	}
	r.setCondition(ws, v1.WorkspaceConditionHarnessBindingPending, "True", "HarnessUnbound",
		"waiting for harness selection (bind-once): no pod is built until the harness is chosen at first session or via the bind API")
	ws.Status.Message = "waiting for harness selection"
	logger.Info("Workspace is unbound (bind-once); parking in Creating without a pod",
		"harness", "", "marker", v1.AnnotationHarnessUnbound)
	if err := r.Status().Update(ctx, ws); err != nil {
		recordStatusUpdateConflictOnError("handleCreating_harness_unbound", err)
		return true, err
	}
	return true, nil
}

// harnessBindingPendingIsParked reports whether the
// HarnessBindingPending condition is already True with the parked
// reason (the steady unbound state).
func harnessBindingPendingIsParked(ws *v1.Workspace) bool {
	for i := range ws.Status.Conditions {
		c := &ws.Status.Conditions[i]
		if c.Type == v1.WorkspaceConditionHarnessBindingPending {
			return c.Status == "True" && c.Reason == "HarnessUnbound"
		}
	}
	return false
}

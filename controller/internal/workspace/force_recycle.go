// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// force_recycle.go — the #1505 user-consent force path for pod deletion.
//
// Problem: the #761 drain gate defers every controller-initiated pod
// deletion while sessions are busy-and-progressing. A multi-agent
// orchestration pod runs busy sessions around the clock, so the
// user-initiated Refresh Compute action never finds quiet — the owner's
// live repro: refresh hangs in the drain forever.
//
// Owner decision (#1505): the USER-INITIATED Refresh Compute action IS
// the force path — the UI warning shown before the action is the
// consent. The drain stays for every automated path (org suspension,
// idle auto-suspend, max-active eviction, spec-timeout) where no
// warning precedes the deletion and surprise data-loss is unacceptable.
// (The suspend path needs no force since #1510/#1507: handleSuspending
// deletes the pod unconditionally, bounded by the termination grace.)
//
// Mechanism: the API stamps a transient annotation on the Workspace
// when — and only when — it serves a user-consented action:
//
//   - RefreshWorkspaceCompute: AnnotationForceRecycle = "<generation>"
//     (the post-increment restartGeneration), honored by handleActive's
//     generation-recycle branch only while observing THAT generation.
//
// The annotation is controller-cleared the moment it is honored, so it
// cannot leak into a later automated action. Automated callers never
// set it. Stale values self-invalidate: a generation-keyed value stops
// matching once the generation is observed; any other value is inert.

// userForcedRecycle reports whether the user-consented force
// annotation is present with the given expected value.
func userForcedRecycle(ws *v1.Workspace, expected string) bool {
	val, ok := ws.Annotations[v1.AnnotationForceRecycle]
	return ok && val == expected
}

// noteForcedRecycle emits the force event + metric so the bypass is
// always visible in the object's history (the audit counterpart of the
// UI's pre-action warning).
func (r *WorkspaceReconciler) noteForcedRecycle(ctx context.Context, ws *v1.Workspace, reason string) {
	logger := log.FromContext(ctx)
	logger.Info("user-consented force: bypassing session drain (#1505)",
		"reason", reason, "annotation", v1.AnnotationForceRecycle)
	metrics.WorkspaceDrainForcedTotal.WithLabelValues(reason).Inc()
	if r.Recorder != nil {
		r.Recorder.Eventf(ws, corev1.EventTypeNormal, "SessionDrainUserForced",
			"pod deletion forced (%s): user-consented action bypassed the session drain; in-flight turns were cut", reason)
	}
}

// clearForceRecycleAnnotation removes the force annotation (retry on
// conflict, same shape as clearSuspendRequest). Called in the same
// reconcile pass that honors the force so the marker never survives
// into a later lifecycle action. The caller's in-memory object is
// resourceVersion-synced to the update so a following Status().Update
// on that object does not conflict.
func (r *WorkspaceReconciler) clearForceRecycleAnnotation(ctx context.Context, ws *v1.Workspace) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var fresh v1.Workspace
		nn := types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}
		if err := r.Get(ctx, nn, &fresh); err != nil {
			return err
		}
		if _, present := fresh.Annotations[v1.AnnotationForceRecycle]; !present {
			ws.ResourceVersion = fresh.ResourceVersion
			return nil
		}
		delete(fresh.Annotations, v1.AnnotationForceRecycle)
		if err := r.Update(ctx, &fresh); err != nil {
			return err
		}
		ws.ResourceVersion = fresh.ResourceVersion
		return nil
	})
}

// forcedGenerationValue formats the refresh-path annotation value: the
// generation the force applies to.
func forcedGenerationValue(gen int64) string {
	return strconv.FormatInt(gen, 10)
}

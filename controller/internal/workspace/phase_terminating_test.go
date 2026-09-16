// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ctrMetrics "github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// wsForTerminate builds a workspace in the Terminating phase with the finalizer
// and a named PVC, matching the production pre-delete state.
func wsForTerminate(name string) *v1.Workspace {
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PVCName = "workspace-" + name
	return ws
}

// TestHandleTerminating_DeletesPVCAndPasswordSecret — the existing
// TestReconcile_Terminating_CleansUp only asserts phase + finalizer removal; it
// does NOT verify the PVC and password Secret are actually deleted. Without
// this, a regression that skips the Delete calls would leak resources silently
// (the finalizer would still be removed, masking the leak).
// Value: prevents PVC/Secret leaks after workspace deletion. Failure mode:
// orphaned PVC (user data retained indefinitely) or Secret (credential leak).
// Expected: PVC and password Secret are gone (NotFound) after reconcile.
func TestHandleTerminating_DeletesPVCAndPasswordSecret(t *testing.T) {
	ws := wsForTerminate("ws-del")
	pvc := makeBoundPVC("workspace-ws-del", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-del", "default")
	r := reconcilerFor(t, ws, pvc, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-del", "default"))
	require.NoError(t, err)

	// PVC must be deleted.
	gotPVC := &corev1.PersistentVolumeClaim{}
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "workspace-ws-del", Namespace: "default"}, gotPVC)
	assert.True(t, apierrors.IsNotFound(err),
		"PVC must be deleted on terminate, got err=%v", err)

	// Password Secret must be deleted.
	gotSecret := &corev1.Secret{}
	err = r.Get(context.Background(),
		types.NamespacedName{Name: passwordSecretName("ws-del"), Namespace: "default"}, gotSecret)
	assert.True(t, apierrors.IsNotFound(err),
		"password Secret must be deleted on terminate, got err=%v", err)
}

// TestHandleTerminating_NoPVCName_SkipsPVCDelete — when Status.PVCName is empty
// (workspace never got past Pending), handleTerminating must not attempt a PVC
// delete with an empty name (which would be a no-op Delete on a nameless
// object). The guard at phase_terminating.go:31 handles this.
// Value: prevents a malformed delete request on nameless PVC. Failure mode:
// error or panic on empty-name delete. Expected: no error, finalizer removed.
func TestHandleTerminating_NoPVCName_SkipsPVCDelete(t *testing.T) {
	ws := wsForTerminate("ws-nopvc")
	ws.Status.PVCName = "" // never created a PVC
	pwSecret := makePasswordSecret("ws-nopvc", "default")
	r := reconcilerFor(t, ws, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-nopvc", "default"))
	require.NoError(t, err, "empty PVCName must not cause a delete error")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-nopvc", Namespace: "default"}, updated))
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer,
		"finalizer must still be removed when PVCName is empty")
}

// TestHandleTerminating_PVCAlreadyGone — idempotent delete: if the PVC is
// already absent (e.g. deleted out-of-band), handleTerminating must treat
// NotFound as success and proceed to remove the finalizer.
// Value: a workspace must not get stuck in Terminating because its PVC was
// already cleaned up. Failure mode: stuck Terminating on re-reconcile.
// Expected: no error, finalizer removed, phase Terminated.
func TestHandleTerminating_PVCAlreadyGone(t *testing.T) {
	ws := wsForTerminate("ws-gone")
	// No PVC seeded — simulates already-deleted state.
	pwSecret := makePasswordSecret("ws-gone", "default")
	r := reconcilerFor(t, ws, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-gone", "default"))
	require.NoError(t, err, "missing PVC must be treated as already-deleted, not an error")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-gone", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase)
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer)
}

// TestHandleDeletion_NoFinalizer_IsNoOp — handleDeletion short-circuits when
// the workspace has no finalizer (phase_terminating.go:85-87). This is the
// idempotent path for a workspace that was already fully cleaned up.
// Value: prevents re-running cleanup on an already-finalized workspace.
// Failure mode: spurious resource operations on a workspace past its lifecycle.
// Expected: no error, no phase change, no status write.
//
// Called directly (not via Reconcile) because the fake client refuses to seed
// a workspace with a deletionTimestamp but no finalizers (K8s invariant);
// handleDeletion itself only checks the finalizer, so this is a faithful test
// of the early-return branch.
func TestHandleDeletion_NoFinalizer_IsNoOp(t *testing.T) {
	ws := makeWorkspace("ws-noop", "default", v1.WorkspacePhaseTerminated)
	// No finalizer set, no deletionTimestamp needed — handleDeletion only
	// checks ContainsFinalizer.
	r := reconcilerFor(t, ws)

	result, err := r.handleDeletion(context.Background(), ws)
	require.NoError(t, err)
	assert.False(t, result.Requeue, "no-finalizer path must not requeue")

	// Phase must be unchanged — handleDeletion returned before touching status.
	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-noop", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase,
		"phase must be unchanged when no finalizer is present")
}

// TestHandleTerminating_G36_DeletesCredentialsSecret is the G36
// regression: handleTerminating must delete the workspace-creds-* Secret
// in addition to the workspace-pw-* Secret it already deletes. The
// cleanupFailedWorkspaceSecrets primitive (secrets.go:33) already knows
// how to delete both; this test pins the wiring so a future refactor
// that removes the call would fail.
//
// Pre-fix: workspace-creds-* persisted indefinitely after workspace
// deletion. The Secret carries per-workspace credential material
// (provider config snapshot, agent-config.json inputs); leaving it
// behind is a credential leak + quota cost. (Bug 12 in worklog 0085
// flagged the same shape of leak for the Failed phase; this PR extends
// the fix to graceful termination.)
func TestHandleTerminating_G36_DeletesCredentialsSecret(t *testing.T) {
	ws := wsForTerminate("ws-creds")
	pwSecret := makePasswordSecret("ws-creds", "default")
	credsSecret := makeOwnedSecret(
		fmt.Sprintf("workspace-creds-%s", "ws-creds"), "default")
	r := reconcilerFor(t, ws, pwSecret, credsSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-creds", "default"))
	require.NoError(t, err)

	// Password Secret must be deleted (existing behavior, locked by
	// TestHandleTerminating_DeletesPVCAndPasswordSecret).
	gotPw := &corev1.Secret{}
	err = r.Get(context.Background(),
		types.NamespacedName{Name: passwordSecretName("ws-creds"), Namespace: "default"}, gotPw)
	assert.True(t, apierrors.IsNotFound(err),
		"password Secret must be deleted on terminate, got err=%v", err)

	// G36: credentials Secret must ALSO be deleted.
	gotCreds := &corev1.Secret{}
	err = r.Get(context.Background(),
		types.NamespacedName{
			Name:      fmt.Sprintf("workspace-creds-%s", "ws-creds"),
			Namespace: "default",
		}, gotCreds)
	assert.True(t, apierrors.IsNotFound(err),
		"G36 REGRESSION: workspace-creds-* Secret must be deleted on terminate, got err=%v", err)
}

// TestHandleTerminating_G36_DoesNotDeleteOtherWorkspaceSecrets
// confirms the cleanup is scoped — only THIS workspace's secrets are
// deleted, not another workspace's. Without this, a regression in the
// secret-name construction (e.g. dropping the workspace.Name suffix)
// could mass-delete unrelated secrets.
func TestHandleTerminating_G36_DoesNotDeleteOtherWorkspaceSecrets(t *testing.T) {
	ws := wsForTerminate("ws-mine")
	pwSecret := makePasswordSecret("ws-mine", "default")
	// Another workspace's creds secret — must survive.
	otherCreds := makeOwnedSecret(
		fmt.Sprintf("workspace-creds-%s", "ws-other"), "default")
	r := reconcilerFor(t, ws, pwSecret, otherCreds)

	_, err := r.Reconcile(context.Background(), reqFor("ws-mine", "default"))
	require.NoError(t, err)

	got := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{
			Name:      otherCreds.Name,
			Namespace: "default",
		}, got),
		"cleanup must not delete another workspace's secrets")
}

// --- #772: PVC delete is best-effort; owner-reference GC is the backstop ---

// reconcilerForTerminateWithInterceptor builds a reconciler whose fake client
// applies the given interceptor funcs — used to simulate persistent API errors
// on the delete path. Overlay delivery pins are irrelevant on the terminating
// path (no pod is built), matching the bare reconciler used by
// TestGaugeDrift_Terminating_StatusUpdateFailure_NoDecrement.
func reconcilerForTerminateWithInterceptor(t *testing.T, funcs interceptor.Funcs, objs ...runtime.Object) *WorkspaceReconciler {
	t.Helper()
	scheme := testScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&v1.Workspace{}).
		WithInterceptorFuncs(funcs).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}
	r.Recorder = record.NewFakeRecorder(16)
	return r
}

// requireNoWorkspaceEvent asserts the FakeRecorder emitted nothing.
func requireNoWorkspaceEvent(t *testing.T, r *WorkspaceReconciler) {
	t.Helper()
	rec, ok := r.Recorder.(*record.FakeRecorder)
	require.True(t, ok, "test must wire a FakeRecorder")
	select {
	case e := <-rec.Events:
		t.Fatalf("unexpected event emitted: %s", e)
	default:
	}
}

// TestHandleTerminating_PVCDeleteError_BestEffort_StillTerminates is the #772
// regression: a PERSISTENT PVC delete error (RBAC denial; a PVC with a stuck
// CSI finalizer returning conflicts on every reconcile — the Longhorn
// node-loss case) must not wedge the workspace in Terminating forever. The
// PVC carries a controller owner reference to the Workspace
// (phase_pending.go SetControllerReference), so K8s GC owns the residual
// cleanup once the finalizer is removed. Expected: no error, phase
// Terminated, finalizer removed, and a warning Event + metric making the
// delegated cleanup explicit to operators.
func TestHandleTerminating_PVCDeleteError_BestEffort_StillTerminates(t *testing.T) {
	ws := wsForTerminate("ws-pvcerr")
	pvc := makeBoundPVC("workspace-ws-pvcerr", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-pvcerr", "default")

	deleteErr := apierrors.NewConflict(
		schema.GroupResource{Group: "", Resource: "persistentvolumeclaims"},
		"workspace-ws-pvcerr",
		errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	r := reconcilerForTerminateWithInterceptor(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return deleteErr
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, ws, pvc, pwSecret)

	before := testutil.ToFloat64(ctrMetrics.WorkspacePVCCleanupDelegatedTotal.WithLabelValues("Conflict"))
	_, err := r.Reconcile(context.Background(), reqFor("ws-pvcerr", "default"))
	require.NoError(t, err,
		"a persistent PVC delete error must NOT wedge termination — cleanup is delegated to owner-reference GC")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-pvcerr", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase,
		"workspace must reach Terminated despite the PVC delete error")
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer,
		"finalizer must be removed so the Workspace object (and via owner-ref GC, the PVC) can be collected")

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case e := <-rec.Events:
		assert.Contains(t, e, "Warning", "event must be a warning")
		assert.Contains(t, e, string(v1.ReasonPVCCleanupDelegated), "event reason must name the delegated cleanup")
		assert.Contains(t, e, "workspace-ws-pvcerr", "event must name the PVC")
		assert.Contains(t, e, "garbage collection", "event must state the GC fallback")
		assert.Contains(t, e, "finalizer", "event must make the residual (reclaim blocked until the PVC's own finalizers clear) explicit")
	default:
		t.Fatal("expected a warning event on the Workspace when the PVC delete fails")
	}

	assert.Equal(t, before+1.0,
		testutil.ToFloat64(ctrMetrics.WorkspacePVCCleanupDelegatedTotal.WithLabelValues("Conflict")),
		"delegated-cleanup metric must increment once per occurrence")
}

// TestHandleTerminating_PVCDeleteError_ReconcileDoesNotRequeueOnError pins
// the wedge fix itself: the reconcile returns nil error (no infinite
// error-requeue loop burning the workqueue on a permanently failing delete).
// The previous reconcile's error return is what kept the finalizer in place.
func TestHandleTerminating_PVCDeleteError_ReconcileDoesNotRequeueOnError(t *testing.T) {
	ws := wsForTerminate("ws-pvcloop")
	pwSecret := makePasswordSecret("ws-pvcloop", "default")

	deleteErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "persistentvolumeclaims"},
		"workspace-ws-pvcloop",
		errors.New("pvc protection"))
	r := reconcilerForTerminateWithInterceptor(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				return deleteErr
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, ws, pwSecret)

	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(context.Background(), reqFor("ws-pvcloop", "default"))
		require.NoError(t, err, "iteration %d: persistent Forbidden must not error the reconcile", i)
	}

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-pvcloop", Namespace: "default"}, updated))
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer,
		"after the first reconcile the finalizer is gone — later reconciles are no-ops, not an error loop")
}

// TestHandleTerminating_PVCDeleteSucceeds_NoWarningEvent locks the quiet
// paths: a successful PVC delete emits no Event (operator noise discipline —
// events are reserved for the delegated-cleanup residual).
func TestHandleTerminating_PVCDeleteSucceeds_NoWarningEvent(t *testing.T) {
	ws := wsForTerminate("ws-quiet")
	pvc := makeBoundPVC("workspace-ws-quiet", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-quiet", "default")
	r := reconcilerFor(t, ws, pvc, pwSecret)
	r.Recorder = record.NewFakeRecorder(16)

	_, err := r.Reconcile(context.Background(), reqFor("ws-quiet", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-quiet", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase)
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer)
	requireNoWorkspaceEvent(t, r)
}

// TestHandleTerminating_PVCNotFound_SilentNoEvent: NotFound stays silent
// (the PVC is already gone — the desired end state), extending
// TestHandleTerminating_PVCAlreadyGone with the no-event assertion.
func TestHandleTerminating_PVCNotFound_SilentNoEvent(t *testing.T) {
	ws := wsForTerminate("ws-gone-quiet")
	pwSecret := makePasswordSecret("ws-gone-quiet", "default")
	r := reconcilerFor(t, ws, pwSecret)
	r.Recorder = record.NewFakeRecorder(16)

	_, err := r.Reconcile(context.Background(), reqFor("ws-gone-quiet", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-gone-quiet", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase)
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer)
	requireNoWorkspaceEvent(t, r)
}

// TestHandleTerminating_StatusUpdateError_StillReturned pins that the
// best-effort PVC delete does not mask downstream failures: a status update
// error still propagates so controller-runtime requeues with backoff
// (semantics unchanged from before #772; the PVC is present and deletes
// cleanly here, isolating the status path).
func TestHandleTerminating_StatusUpdateError_StillReturned(t *testing.T) {
	ws := wsForTerminate("ws-staterr")
	pvc := makeBoundPVC("workspace-ws-staterr", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-staterr", "default")

	updateErr := errors.New("simulated status update failure")
	r := reconcilerForTerminateWithInterceptor(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				return updateErr
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	}, ws, pvc, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-staterr", "default"))
	require.ErrorIs(t, err, updateErr,
		"status update errors must still propagate (requeue) after the PVC delete became best-effort")
}

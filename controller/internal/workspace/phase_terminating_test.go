// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

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

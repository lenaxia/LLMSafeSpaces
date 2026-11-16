// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// newHarnessBindingReconciler builds a reconciler over the fake
// client with the Workspace status subresource enabled (the standard
// controller-test shape in this package).
func newHarnessBindingReconciler(t *testing.T, objs ...ctrlclient.Object) (*WorkspaceReconciler, ctrlclient.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Workspace{}).
		WithObjects(objs...).
		Build()
	return &WorkspaceReconciler{Client: c, Scheme: scheme}, c
}

// harness_binding_test.go — the controller-tier pins of the bind-once
// harness model (owner ruling 2026-11-16: workspaces do NOT switch
// harnesses; one harness per workspace, bound once, immutable
// forever). Two behaviors live here and nowhere else: the LEGACY
// BACKFILL (marker-less empty harness → explicit opencode, the
// retroactive invariant) and the UNBOUND GATE (a born-unbound
// workspace never builds a pod). There is deliberately NO
// harness-drift arm, NO board sweep, and NO switch window — the
// rejected prelim's machinery does not exist (its absence is the
// point; see reports/specs/harness-switch-*-design.md addenda).

func unboundTestWorkspace() *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ws-bind",
			Namespace:   "default",
			Annotations: map[string]string{v1.AnnotationHarnessUnbound: "true"},
		},
		Spec:   v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "u1"}},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating},
	}
}

func legacyTestWorkspace() *v1.Workspace {
	ws := unboundTestWorkspace()
	ws.Annotations = map[string]string{} // marker-less: pre-ruling object
	ws.Status.Phase = v1.WorkspacePhaseActive
	return ws
}

// The invariant pin: every legacy (marker-less, empty-harness)
// workspace is stamped to explicit opencode on first observe —
// "backfill marks all existing workspaces bound-opencode" — and the
// pass requeues so nothing else proceeds on the half-observed write.
func TestReconcileHarnessBinding_LegacyBackfillStampsOpencode(t *testing.T) {
	r, c := newHarnessBindingReconciler(t, legacyTestWorkspace())
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-bind", Namespace: "default"}})
	require.NoError(t, err)
	assert.True(t, res.Requeue, "the stamp pass requeues; the next pass re-reads the bound object")

	var ws v1.Workspace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "ws-bind", Namespace: "default"}, &ws))
	assert.Equal(t, v1.HarnessOpencode, ws.Spec.Harness,
		"the retroactive invariant: existing workspaces are explicitly bound opencode")
}

// Repeatable across restarts and safe by construction: a second pass
// no-ops (bound is bound), and a BOUND pi workspace is never
// touched — the sweep cannot and does not switch harnesses.
func TestReconcileHarnessBinding_NeverOverwritesBound(t *testing.T) {
	pi := legacyTestWorkspace()
	pi.Spec.Harness = v1.HarnessPi
	r, c := newHarnessBindingReconciler(t, pi)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-bind", Namespace: "default"}})
	require.NoError(t, err)
	assert.False(t, res.Requeue)

	var ws v1.Workspace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "ws-bind", Namespace: "default"}, &ws))
	assert.Equal(t, v1.HarnessPi, ws.Spec.Harness, "bound pi is untouched")
}

// The unbound gate: a born-unbound workspace parks in Creating with
// the HarnessBindingPending condition and NO pod — the PVC-bearing
// phases proceed (Pending created it), but nothing harness-class is
// built until the bind.
func TestReconcileHarnessBinding_UnboundParksWithoutPod(t *testing.T) {
	r, c := newHarnessBindingReconciler(t, unboundTestWorkspace())
	ctx := context.Background()

	for i := 0; i < 2; i++ { // second pass: the idempotence pin (no churn)
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ws-bind", Namespace: "default"}})
		require.NoError(t, err, "pass %d", i)

		var ws v1.Workspace
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "ws-bind", Namespace: "default"}, &ws))
		assert.Equal(t, v1.WorkspacePhaseCreating, ws.Status.Phase, "pass %d: parked, not Active", i)
		assert.Empty(t, ws.Spec.Harness, "pass %d: still unbound — nothing writes the choice but the bind", i)
		assert.Equal(t, "true", ws.Annotations[v1.AnnotationHarnessUnbound], "pass %d: marker intact", i)

		var pending *v1.WorkspaceCondition
		for j := range ws.Status.Conditions {
			if ws.Status.Conditions[j].Type == v1.WorkspaceConditionHarnessBindingPending {
				pending = &ws.Status.Conditions[j]
			}
		}
		require.NotNil(t, pending, "pass %d: the binding-pending condition is set", i)
		assert.Equal(t, "True", pending.Status)
		assert.Equal(t, "HarnessUnbound", pending.Reason)

		pods := &corev1.PodList{}
		require.NoError(t, c.List(ctx, pods))
		assert.Empty(t, pods.Items, "pass %d: no pod is built while unbound", i)
	}

	// The release: after the bind's spec write, the gate opens.
	var ws v1.Workspace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "ws-bind", Namespace: "default"}, &ws))
	ws.Spec.Harness = v1.HarnessPi
	require.NoError(t, c.Update(ctx, &ws))

	halt, err := r.gateUnboundCreating(ctx, &ws)
	require.NoError(t, err)
	assert.False(t, halt, "a bound workspace passes the gate; the phase machine builds")
}

// Unit pins for the discriminator: ONLY the marker makes an empty
// harness born-unbound.
func TestBornUnbound(t *testing.T) {
	assert.True(t, bornUnbound(unboundTestWorkspace()))
	assert.False(t, bornUnbound(legacyTestWorkspace()), "legacy marker-less empty = bound opencode, never swept into the gate")
	pi := unboundTestWorkspace()
	pi.Spec.Harness = v1.HarnessPi
	assert.False(t, bornUnbound(pi))
}

// The stale-condition clear: a bound workspace must not advertise a
// pending choice.
func TestClearHarnessBindingPending(t *testing.T) {
	ws := unboundTestWorkspace()
	ws.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionHarnessBindingPending, Status: "True", Reason: "HarnessUnbound",
	}}
	r, _ := newHarnessBindingReconciler(t)
	r.clearHarnessBindingPending(ws)
	assert.Equal(t, "False", ws.Status.Conditions[0].Status)
	assert.Equal(t, "HarnessBound", ws.Status.Conditions[0].Reason)
}

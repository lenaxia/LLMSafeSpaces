// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// #1505: the user-consented force path. The matrix the issue mandates:
//
//   - forced refresh + BUSY sessions  → pod deleted IMMEDIATELY (the repro:
//     a multi-agent pod whose drain never finds quiet)
//   - plain generation bump + BUSY    → still drains (automated paths)
//   - stale generation marker + BUSY  → still drains (self-invalidation)
//   - clear-failure                   → requeue, pod kept (fail-safe)
//   - marker hygiene: honored markers are CLEARED and AUDITED (the
//     SessionDrainUserForced event fires on every forced pass)
//
// The SUSPEND path is out of scope since #1510 (#1507): handleSuspending
// deletes the pod unconditionally with the pod's termination grace as
// the bounded termination — there is no drain to bypass, so no suspend
// marker exists. Suspend semantics are pinned in
// phase_suspend_1507_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// THE REPRO: user hits Refresh Compute on a pod with perpetually-busy
// sessions. The #761 drain would defer forever; the force marker (which
// only RefreshWorkspaceCompute stamps) must bypass it — pod deleted on
// the FIRST reconcile, generation observed, marker cleared, and the
// bypass AUDITED via the SessionDrainUserForced event.
func TestForce_UserRefreshBusyPodDeletesImmediately(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-refresh", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 7
	ws.Status.ObservedRestartGeneration = 6
	ws.Status.PodIP = "127.0.0.1"
	ws.Annotations = map[string]string{v1.AnnotationForceRecycle: "7"}
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, rec := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.NotEqual(t, drainPollInterval, result.RequeueAfter,
		"a forced refresh must NOT requeue on the drain poll interval")

	assert.False(t, podExists(t, r, pod.Name),
		"the forced refresh deletes the pod on the first pass despite busy sessions")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, int64(7), updated.Status.ObservedRestartGeneration, "the forced generation is observed immediately")
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
	assert.NotContains(t, updated.Annotations, v1.AnnotationForceRecycle,
		"the honored marker is cleared — it cannot leak into a later action")
	assert.Equal(t, 0, stub.statuszCalls(),
		"the forced path never consults the drain (statusz untouched)")

	forced := 0
	for _, e := range eventsFrom(rec) {
		if strings.Contains(e, "SessionDrainUserForced") {
			forced++
		}
	}
	assert.Equal(t, 1, forced,
		"the forced pass emits exactly one SessionDrainUserForced audit event")
}

// Stale generation marker: the marker names generation 7 but the
// workspace is recycling to 8 (a later automated bump). Mismatched
// markers are inert — the polite drain applies.
func TestForce_StaleGenerationMarkerStillDrains(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-stale", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 8
	ws.Status.ObservedRestartGeneration = 7
	ws.Status.PodIP = "127.0.0.1"
	ws.Annotations = map[string]string{v1.AnnotationForceRecycle: "7"} // stale: names the ALREADY-observed gen
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, rec := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter,
		"a stale (mismatched-generation) marker must NOT force — the drain applies")
	assert.True(t, podExists(t, r, pod.Name))
	for _, e := range eventsFrom(rec) {
		assert.False(t, strings.Contains(e, "SessionDrainUserForced"),
			"a non-forced pass must not emit the force audit event")
	}
}

// Plain (automated) generation bump: unchanged polite drain — the #761
// contract for everything not user-consented.
func TestForce_PlainGenerationBumpStillDrains(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-plain", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter)
	assert.True(t, podExists(t, r, pod.Name), "no marker → no force")
}

// Clear-failure: if the annotation clear fails (API error), the
// reconcile must requeue WITHOUT deleting the pod — the marker stays,
// so the retry honors the force instead of silently degrading to the
// polite drain on a wedged pod. Fail-safe is "try again", never
// "delete anyway" and never "hang".
func TestForce_ClearAnnotationFailureRequeuesWithoutDeletion(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-clearfail", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 4
	ws.Status.ObservedRestartGeneration = 3
	ws.Status.PodIP = "127.0.0.1"
	ws.Annotations = map[string]string{v1.AnnotationForceRecycle: "4"}
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")

	// Fail exactly the FIRST Workspace Update — on the force path that
	// is clearForceRecycleAnnotation's write. Everything after (status
	// writes) succeeds, proving the branch returns before deletion.
	updates := 0
	failFirst := interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, isWS := obj.(*v1.Workspace); isWS {
				updates++
				if updates == 1 {
					return errors.New("injected clear failure")
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	}
	r, _ := reconcilerWithInterceptor(t, failFirst, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err, "the requeue branch swallows the error and requeues")
	assert.True(t, result.Requeue, "clear failure must requeue (retry the force pass)")
	assert.Zero(t, result.RequeueAfter)
	assert.True(t, podExists(t, r, pod.Name),
		"the pod must NOT be deleted when the clear fails — retry, don't half-apply")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Contains(t, updated.Annotations, v1.AnnotationForceRecycle,
		"the marker survives a failed clear so the retry still honors the force")
}

// reconcilerWithInterceptor mirrors reconcilerForDrain but injects
// client interceptors (fault injection) into the fake client.
func reconcilerWithInterceptor(t *testing.T, f interceptor.Funcs, objs ...runtime.Object) (*WorkspaceReconciler, *record.FakeRecorder) {
	t.Helper()
	r := reconcilerFor(t, objs...) // pins + defaults, then swap the client
	c := fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&v1.Workspace{}).
		WithInterceptorFuncs(f).
		Build()
	r.Client = c
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	return r, rec
}

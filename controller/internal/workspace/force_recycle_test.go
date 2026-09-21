// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// #1505: the user-consented force path. The matrix the issue mandates:
//
//   - forced refresh + BUSY sessions  → pod deleted IMMEDIATELY (the repro:
//     a multi-agent pod whose drain never finds quiet)
//   - plain generation bump + BUSY    → still drains (automated paths)
//   - forced user suspend + BUSY      → pod deleted IMMEDIATELY, one pass
//   - automated suspend + BUSY        → still drains (#761 unchanged)
//   - marker hygiene: honored markers are CLEARED (never leak into a
//     later automated action); stale generation markers self-invalidate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// THE REPRO: user hits Refresh Compute on a pod with perpetually-busy
// sessions. The #761 drain would defer forever; the force marker (which
// only RefreshWorkspaceCompute stamps) must bypass it — pod deleted on
// the FIRST reconcile, generation observed, marker cleared.
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
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

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
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter,
		"a stale (mismatched-generation) marker must NOT force — the drain applies")
	assert.True(t, podExists(t, r, pod.Name))
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

// User-consented suspend: the marker ("suspend") makes handleSuspending
// delete the pod in ONE pass despite busy sessions.
func TestForce_UserSuspendBusyPodDeletesImmediately(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-suspend", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal
	ws.Annotations = map[string]string{v1.AnnotationForceRecycle: ForceValueSuspend}
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	// First reconcile transitions Active → Suspending.
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	// Second reconcile: handleSuspending honors the force — no defer.
	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.NotEqual(t, drainPollInterval, result.RequeueAfter,
		"a forced suspend must NOT requeue on the drain poll interval")
	assert.False(t, podExists(t, r, pod.Name),
		"the forced suspend deletes the pod despite busy sessions")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Suspend)
	assert.NotContains(t, updated.Annotations, v1.AnnotationForceRecycle,
		"the honored suspend marker is cleared — a LATER automated suspend drains politely")
}

// Automated suspend (no marker): #761 unchanged.
func TestForce_AutomatedSuspendStillDrains(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-auto", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal // no AnnotationForceRecycle
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter, "automated suspend keeps the #761 drain")
	assert.True(t, podExists(t, r, pod.Name))
}

// Cross-value isolation: a generation-keyed marker must not force a
// suspend, and a "suspend" marker must not force a generation recycle.
func TestForce_MarkerValuesArePathScoped(t *testing.T) {
	// Generation value present during a SUSPEND flow → polite drain.
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-force-xval", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal
	ws.Annotations = map[string]string{v1.AnnotationForceRecycle: "9"} // generation form, wrong path
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter,
		"a generation-keyed marker must not force the suspend path")
	assert.True(t, podExists(t, r, pod.Name))
}

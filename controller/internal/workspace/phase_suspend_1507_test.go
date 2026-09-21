// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// #1507: the incident pins. The owner's live repro (workspace
// d8bed486, 2026-09-21): a wedged opencode (69h CPU, session POSTs
// timing out, sessions re-marked busy by the flap loop every ~15s)
// kept the #761 drain deferring FOREVER — busySessions flapped 4↔5
// with progressAge pinned at 0s, so the stall window never aged, and
// Suspending hung 1h+. The busy-check is gone from the suspend path:
// these pins prove a busy/wedged workspace suspends in bounded
// controller passes with the pod deleted and the PVC retained.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// THE FLAP-LOOP REPRO: busy sessions that KEEP flapping (progress
// forever fresh — progressAge 0s, exactly the incident) must not delay
// suspend by a single drain poll. Suspend completes: no deferral
// requeue, pod deleted, phase Suspended.
func TestSuspendBounded_FlappingBusySessionsSuspendImmediately(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-1507-flap", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", types.UID(string(ws.UID)))
	r, _ := reconcilerForDrain(t, ws, pod, pvc)

	// First reconcile: Active → Suspending.
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	// Second reconcile: handleSuspending — busy sessions are NOT
	// consulted; the pod is deleted in THIS pass (previously this was
	// the drainPollInterval deferral that hung for an hour).
	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.NotEqual(t, drainPollInterval, result.RequeueAfter,
		"the busy-session deferral is GONE from the suspend path (#1507)")

	assert.False(t, podExists(t, r, pod.Name), "the pod must be deleted despite busy sessions")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Suspend, "the suspend request is acknowledged")

	// PVC retained: suspend deletes compute, never data.
	pvcFetched := &corev1.PersistentVolumeClaim{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "workspace-" + ws.Name, Namespace: "default"}, pvcFetched),
		"the PVC must survive suspend")
}

// The suspend path never dials the agent — a live statusz stub wired
// to the workspace's PodIP must see ZERO scrapes through a full
// suspend flow with busy sessions.
func TestSuspendBounded_UnreachableAgentSuspendsWithoutConsult(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-1507-unreach", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal
	ws.Status.PodIP = "127.0.0.1" // pointed at a live stub that MUST NOT be consulted
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.NotEqual(t, drainPollInterval, result.RequeueAfter)
	assert.Zero(t, stub.statuszCalls(),
		"the suspend path must not consult statusz at all — suspend no longer depends on agent health (#1507)")
	assert.False(t, podExists(t, r, pod.Name))
}

// The genuinely-unreachable variant: the PodIP points at a port with
// no listener (the deeper incident state — even the scrape times out).
// Suspend completes identically: the path never attempts the dial.
func TestSuspendBounded_DeadAgentSuspendsImmediately(t *testing.T) {
	ws := makeWorkspace("ws-1507-dead", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal
	// Port 1 with no listener: any fetch would fail — irrelevant now.
	ws.Status.PodIP = "127.0.0.1"
	origAdminPort := agentdAdminPort
	agentdAdminPort = 1
	t.Cleanup(func() { agentdAdminPort = origAdminPort })
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.NotEqual(t, drainPollInterval, result.RequeueAfter,
		"a dead agentd cannot block suspend — the path never dials it")
	assert.False(t, podExists(t, r, pod.Name))
	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
}

// The graceful window rides the POD: the deletion's
// terminationGracePeriodSeconds is the bounded graceful opencode
// termination. Default 40s; the flag overrides.
func TestSuspendBounded_GraceRidesThePod(t *testing.T) {
	// Explicit image reference: resolveRuntimeImage needs no
	// RuntimeEnvironment CRD in the fake client.
	ws := makeWorkspace("ws-1507-grace", "default", v1.WorkspacePhasePending)
	ws.Spec.Runtime = "ghcr.io/lenaxia/llmsafespaces/runtimes/base:test"
	r, _ := reconcilerForDrain(t, ws)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(40), *pod.Spec.TerminationGracePeriodSeconds,
		"default grace = agentd's 35s serial shutdown budget + 5s margin")

	r.WorkspaceTerminationGraceSeconds = 120
	pod, err = r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	assert.Equal(t, int64(120), *pod.Spec.TerminationGracePeriodSeconds,
		"the flag overrides the grace (operator-tunable bound)")
}

// The non-suspend drain paths KEEP the #761 gate: the restart-gen
// recycle is a different decision (the agent stays alive; a mid-turn
// recycle is more disruptive) and is out of #1507's scope.
func TestSuspendBounded_RestartGenerationDrainUnchanged(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-1507-rgen", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter,
		"the restart-generation recycle keeps its drain (#1507 scope: suspend only)")
	assert.True(t, podExists(t, r, pod.Name))
}

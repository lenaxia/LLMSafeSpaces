// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// #935 regression tests: the Creating-phase wedge. An init container
// crash-looping forever (deterministic poison-image bug, e.g. the stale
// baked agentd failing on contract-shape MCP metadata) left the pod
// Pending forever — matching none of the existing recovery signals
// (PodFailed/Unschedulable/no-container-started), burning a silent 2s
// hot-requeue, with no event, no condition, no backoff, and a
// restartGeneration bump that didn't restart anything.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// makeInitCrashLoopPod reproduces the incident shape: pod Pending, the
// credential-setup init container Waiting in CrashLoopBackOff (it has
// run and crashed repeatedly — LastTerminationState populated, so
// "no container has ever started" is false and FN3b never matched).
func makeInitCrashLoopPod(name, namespace string, age time.Duration) *corev1.Pod {
	crashWaiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "CrashLoopBackOff",
	}}
	created := metav1.NewTime(time.Now().Add(-age))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: created,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "platform-init", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
				}},
				{Name: "platform-bootstrap", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
				}},
				{Name: "platform-materialize", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
				}},
				// Design 0053 S3: platform-init containers crash-looping
				// is SURFACED (detectPlatformBootFailure — deleting the
				// pod cannot fix a platform bug). The #935 recovery path
				// remains live for user-plane inits: workspace-setup
				// (packages/initScript) is the crash-loop shape here.
				{Name: "workspace-setup", State: crashWaiting,
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason: "Error", ExitCode: 2,
							Message: "materialize: parsing /sandbox-cfg/secrets.json: json: cannot unmarshal array",
						},
					}, RestartCount: 33},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "workspace", State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
				}},
			},
		},
	}
}

// --- Fix 2 (FN3c): init crashloop enters recovery ---

// TestReconcile_Creating_InitCrashLoop_BeyondTimeout_EntersProcessRecovery
// is the #935 primary incident shape: credential-setup crash-looping for
// hours must convert to the standard recovery machinery — pod deleted,
// FailureClassProcess, backoff state set — instead of the silent eternal
// Creating with a 2s hot-requeue.
func TestReconcile_Creating_InitCrashLoop_BeyondTimeout_EntersProcessRecovery(t *testing.T) {
	ws := makeWorkspace("ws-cl-old", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-cl-old"
	expectedPodName := podName("ws-cl-old", string(ws.UID))
	pod := makeInitCrashLoopPod(expectedPodName, "default", 12*time.Hour)
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-cl-old", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-cl-old", Namespace: "default"}, updated))
	assert.Equal(t, string(FailureClassProcess), updated.Status.LastFailureClass,
		"init crashloop is a process failure (classifyFailure's existing mapping)")
	assert.Equal(t, int32(1), updated.Status.ConsecutiveFailures,
		"recovery must engage — no more silent eternal Creating")
	assert.NotNil(t, updated.Status.NextRetryAt, "backoff must gate pod recreation")

	podErr := r.Get(context.Background(),
		types.NamespacedName{Name: expectedPodName, Namespace: "default"}, &corev1.Pod{})
	assert.Error(t, podErr, "crash-looping pod must be deleted so recreation re-pulls images")
}

// TestReconcile_Creating_InitCrashLoop_BelowTimeout_NoRecovery is the
// false-positive guard: a young crash-looping init container (a flaky
// pull, a transient API blip) must not trigger premature recovery.
func TestReconcile_Creating_InitCrashLoop_BelowTimeout_NoRecovery(t *testing.T) {
	ws := makeWorkspace("ws-cl-young", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-cl-young"
	expectedPodName := podName("ws-cl-young", string(ws.UID))
	pod := makeInitCrashLoopPod(expectedPodName, "default", 2*time.Minute)
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-cl-young", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-cl-young", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"init crashloop under the timeout must not enter recovery")
}

// TestReconcile_Creating_PlatformBootFailure_PrecedesCrashloopRecovery
// pins the ordering: a crash-looping PLATFORM boot container (0051
// step-1 world: platform-materialize etc., overlay mode) surfaces via
// detectPlatformBootFailure and must NOT be deleted by the FN3c recovery
// path — deleting the pod cannot fix a platform code bug.
func TestReconcile_Creating_PlatformBootFailure_PrecedesCrashloopRecovery(t *testing.T) {
	ws := makeWorkspace("ws-pb-cl", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-pb-cl"
	expectedPodName := podName("ws-pb-cl", string(ws.UID))

	pod := makeInitCrashLoopPod(expectedPodName, "default", stuckScheduledPendingTimeout+time.Hour)
	// Rename the crash-looping init container (workspace-setup, the last
	// init in the fixture) to a platform boot container — that family is
	// surfaced, not recovered.
	pod.Status.InitContainerStatuses[len(pod.Status.InitContainerStatuses)-1].Name = "platform-materialize"

	r := reconcilerFor(t, ws, pod)
	r.AgentdImage = "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:35a1a5bb35a1a5bb35a1a5bb35a1a5bb35a1a5bb"
	r.AgentdBinarySHA256AMD64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r.AgentdBinarySHA256ARM64 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Precondition: this fixture is one the platform-boot detector owns.
	require.True(t, r.detectPlatformBootFailure(context.Background(), ws, pod),
		"fixture must trigger platform-boot detection for the ordering to be under test")

	_, err := r.Reconcile(context.Background(), reqFor("ws-pb-cl", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-pb-cl", Namespace: "default"}, updated))

	podErr := r.Get(context.Background(),
		types.NamespacedName{Name: expectedPodName, Namespace: "default"}, &corev1.Pod{})
	assert.NoError(t, podErr,
		"platform boot failures are surfaced (condition+event), never pod-deleted")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"platform boot failures skip crashloop recovery by design (0051 step 1)")
}

// --- Fix 3: restartGeneration bump in Creating deletes the pod ---

// TestReconcile_Creating_RestartGenerationBump_DeletesExistingPod: the
// user-facing escape hatches (RestartWorkspace, RefreshWorkspaceCompute)
// document "the controller rebuilds the pod" — in Creating the bump must
// actually delete the existing (possibly crash-looping) pod so the next
// pass rebuilds from current spec (including re-resolving spec.runtime
// for non-pinned refs). The incident's only remediation was kubectl.
func TestReconcile_Creating_RestartGenerationBump_DeletesExistingPod(t *testing.T) {
	ws := makeWorkspace("ws-bump", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-bump"
	ws.Status.ObservedRestartGeneration = 1
	ws.Spec.RestartGeneration = 2
	expectedPodName := podName("ws-bump", string(ws.UID))
	pod := makeInitCrashLoopPod(expectedPodName, "default", 30*time.Minute)
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-bump", "default"))
	require.NoError(t, err)

	podErr := r.Get(context.Background(),
		types.NamespacedName{Name: expectedPodName, Namespace: "default"}, &corev1.Pod{})
	assert.Error(t, podErr,
		"generation bump in Creating must delete the existing pod so the create branch rebuilds")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-bump", Namespace: "default"}, updated))
	assert.Equal(t, int64(2), updated.Status.ObservedRestartGeneration,
		"bump must be observed (no hot loop)")
	assert.Equal(t, int32(1), updated.Status.RestartCount, "RestartCount increments once per bump")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"bump clears recovery state (F19 immediate-retry semantics)")
}

// TestReconcile_Creating_RestartGenerationBump_TerminatingPod_Waits:
// a pod already terminating must not be double-deleted (reuse the
// isPodTerminating guard).
func TestReconcile_Creating_RestartGenerationBump_TerminatingPod_Waits(t *testing.T) {
	ws := makeWorkspace("ws-bump-term", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-bump-term"
	ws.Status.ObservedRestartGeneration = 1
	ws.Spec.RestartGeneration = 2
	expectedPodName := podName("ws-bump-term", string(ws.UID))
	pod := makeInitCrashLoopPod(expectedPodName, "default", 30*time.Minute)
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"example.llmsafespaces.dev/termination-test"}
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-bump-term", "default"))
	require.NoError(t, err)

	// Fake client keeps the pod (deletionTimestamp set, no finalizers to
	// reap it): the bump path must observe it terminating and wait, not
	// attempt a second delete.
	podCheck := &corev1.Pod{}
	assert.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: expectedPodName, Namespace: "default"}, podCheck),
		"terminating pod is left to the reaper")
}

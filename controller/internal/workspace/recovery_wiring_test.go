package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ctrMetrics "github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestMaybeResetConsecutiveFailures_After2Min(t *testing.T) {
	ws := &v1.Workspace{}
	twoMinAgo := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	ws.Status.LastStableAt = &twoMinAgo
	ws.Status.ConsecutiveFailures = 5
	ws.Status.LastFailureClass = string(FailureClassProcess)

	maybeResetConsecutiveFailures(ws)

	assert.Equal(t, int32(0), ws.Status.ConsecutiveFailures)
	assert.Equal(t, "", ws.Status.LastFailureClass)
	assert.Nil(t, ws.Status.LastFailureAt)
	assert.Nil(t, ws.Status.NextRetryAt)
}

func TestMaybeResetConsecutiveFailures_Before2Min(t *testing.T) {
	ws := &v1.Workspace{}
	oneMinAgo := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	ws.Status.LastStableAt = &oneMinAgo
	ws.Status.ConsecutiveFailures = 5

	maybeResetConsecutiveFailures(ws)

	assert.Equal(t, int32(5), ws.Status.ConsecutiveFailures)
}

func TestMaybeResetConsecutiveFailures_NilLastStableAt_StartsClock(t *testing.T) {
	ws := &v1.Workspace{}
	ws.Status.ConsecutiveFailures = 3
	ws.Status.LastStableAt = nil

	maybeResetConsecutiveFailures(ws)

	// Should set LastStableAt to now (start the clock)
	assert.NotNil(t, ws.Status.LastStableAt)
	assert.Equal(t, int32(3), ws.Status.ConsecutiveFailures) // not reset yet
}

func TestMaybeResetConsecutiveFailures_ZeroFailures_NoOp(t *testing.T) {
	ws := &v1.Workspace{}
	ws.Status.ConsecutiveFailures = 0

	maybeResetConsecutiveFailures(ws)

	assert.Nil(t, ws.Status.LastStableAt)
}

func TestNextRetryAtEnforcement(t *testing.T) {
	future := metav1.NewTime(time.Now().Add(30 * time.Second))
	ws := &v1.Workspace{}
	ws.Status.NextRetryAt = &future

	remaining := timeUntilNextRetry(ws)
	assert.True(t, remaining > 0)
	assert.True(t, remaining <= 30*time.Second)
}

func TestNextRetryAtEnforcement_Elapsed(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-10 * time.Second))
	ws := &v1.Workspace{}
	ws.Status.NextRetryAt = &past

	remaining := timeUntilNextRetry(ws)
	assert.Equal(t, time.Duration(0), remaining)
}

func TestNextRetryAtEnforcement_Nil(t *testing.T) {
	ws := &v1.Workspace{}
	ws.Status.NextRetryAt = nil

	remaining := timeUntilNextRetry(ws)
	assert.Equal(t, time.Duration(0), remaining)
}

func TestRestartGeneration_InCreating_BypassesBackoff(t *testing.T) {
	ws := makeWorkspace("ws-restart", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-restart-uid"
	ws.Status.PVCName = "workspace-ws-restart"
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.ConsecutiveFailures = 5
	ws.Status.SafeMode = true
	future := metav1.NewTime(time.Now().Add(5 * time.Minute))
	ws.Status.NextRetryAt = &future

	pvc := makeBoundPVC("workspace-ws-restart", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-restart", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	_, err := r.Reconcile(context.Background(), reqFor("ws-restart", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-restart", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures)
	assert.Nil(t, updated.Status.NextRetryAt)
	assert.False(t, updated.Status.SafeMode)
	assert.Equal(t, int64(2), updated.Status.ObservedRestartGeneration)
	assert.NotEmpty(t, updated.Status.PodName, "pod should be created after restartGeneration bypass")
}

func TestSuspend_ClearsRecoveryState(t *testing.T) {
	ws := makeWorkspace("ws-suspend", "default", v1.WorkspacePhaseSuspending)
	ws.UID = "ws-suspend-uid"
	ws.Status.ConsecutiveFailures = 10
	ws.Status.ControllerRestartCount = 7
	ws.Status.LastFailureClass = string(FailureClassConfiguration)
	now := metav1.Now()
	ws.Status.LastFailureAt = &now
	ws.Status.NextRetryAt = &now
	ws.Status.LastStableAt = &now
	ws.Status.PodIP = "10.0.0.1"

	// ConsecutiveFailures>0 implies a prior WorkspacesInRecovery.Inc via
	// enterRecovery in production; seed the gauge to mirror that invariant.
	ctrMetrics.WorkspacesInRecovery.Inc()

	pod := makeRunningPod(podName("ws-suspend", string(ws.UID)), "default", "10.0.0.1")
	pwSecret := makePasswordSecret("ws-suspend", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-suspend", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-suspend", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures)
	assert.Equal(t, int32(0), updated.Status.ControllerRestartCount, "suspend must clear ControllerRestartCount (US-24.8 F22)")
	assert.Nil(t, updated.Status.NextRetryAt)
	assert.Nil(t, updated.Status.LastFailureAt)
	assert.Nil(t, updated.Status.LastStableAt)
	assert.Equal(t, "", updated.Status.LastFailureClass)
}

func TestHandleCreating_UnschedulablePod_5Min_EntersRecovery(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-unsched", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-unsched-uid"

	// Pod created 6 minutes ago, stuck Unschedulable
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName("ws-unsched", string(ws.UID)),
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-6 * time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: "Unschedulable",
				},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	_, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Name: "ws-unsched", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
	assert.Equal(t, int32(1), updated.Status.ConsecutiveFailures)
	assert.Equal(t, string(FailureClassInfrastructure), updated.Status.LastFailureClass)
	assert.NotNil(t, updated.Status.NextRetryAt)
}

func TestHandleCreating_RestartGenBump_PersistedWhenPodPendingScheduled(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-genpersist", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-genpersist-uid"
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.ConsecutiveFailures = 3

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName("ws-genpersist", string(ws.UID)),
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-30 * time.Second)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	_, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Name: "ws-genpersist", Namespace: "default"}, updated))
	assert.Equal(t, int64(2), updated.Status.ObservedRestartGeneration,
		"ObservedRestartGeneration must be persisted to avoid hot-loop reconcile")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"recovery state must be cleared by restartGeneration bump and persisted")
}

func TestHandleCreating_ScheduledStuckPending_10Min_EntersRecovery(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-stuck", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-stuck-uid"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName("ws-stuck", string(ws.UID)),
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-11 * time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	_, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Name: "ws-stuck", Namespace: "default"}, updated))
	assert.Equal(t, int32(1), updated.Status.ConsecutiveFailures,
		"scheduled pod stuck Pending with no containers for >10min must enter recovery")
	assert.Equal(t, string(FailureClassInfrastructure), updated.Status.LastFailureClass)
	assert.NotNil(t, updated.Status.NextRetryAt)
}

func TestHandleCreating_ScheduledPendingWithInit_NoRecovery(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-init-progress", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-init-progress-uid"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName("ws-init-progress", string(ws.UID)),
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-11 * time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "workspace-dirs", State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
				}},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	_, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Name: "ws-init-progress", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"pod with init containers running must NOT enter stuck-pending recovery")
}

func TestHandleCreating_ScheduledPendingUnderTimeout_NoRecovery(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-soon", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-soon-uid"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName("ws-soon", string(ws.UID)),
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	_, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Name: "ws-soon", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"scheduled pod pending for <10min must NOT enter recovery")
}

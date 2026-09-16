// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	ctrMetrics "github.com/lenaxia/llmsafespaces/controller/internal/metrics"
)

func readGaugeVecValue(t *testing.T, gv *prometheus.GaugeVec, lv ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := gv.WithLabelValues(lv...).Write(m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func readPlainGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := g.(prometheus.Metric).Write(m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func gaugeDelta(t *testing.T, runtime, secLevel string, fn func()) float64 {
	t.Helper()
	before := readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, runtime, secLevel)
	fn()
	after := readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, runtime, secLevel)
	return after - before
}

func activeWorkspaceWithPod(name string) *v1.Workspace {
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "10.0.0.1"
	now := metav1.Now()
	ws.Status.StartTime = &now
	return ws
}

func TestGaugeDrift_Active_RestartGenerationBump_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-rg")
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	pod := makeRunningPod(podName("ws-g-rg", string(ws.UID)), "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-rg", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 on RestartGeneration bump")
}

func TestGaugeDrift_Active_PasswordSecretMissing_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-nopw")
	pod := makeRunningPod(podName("ws-g-nopw", string(ws.UID)), "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-nopw", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 when password secret missing")
}

func TestGaugeDrift_Active_PodMissing_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-podlost")
	r := reconcilerFor(t, ws)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-podlost", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 when pod missing")
}

func TestGaugeDrift_Active_PodTerminating_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-term")
	pod := makeRunningPod(podName("ws-g-term", string(ws.UID)), "default", "10.0.0.1")
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pod.Finalizers = []string{"test-finalizer"}
	pwSecret := makePasswordSecret("ws-g-term", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-term", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 when pod has DeletionTimestamp")
}

func TestGaugeDrift_Active_PodNotRunning_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-pending")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName("ws-g-pending", string(ws.UID)), Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	pwSecret := makePasswordSecret("ws-g-pending", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-pending", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 when pod not Running")
}

func TestGaugeDrift_Active_ArchDrift_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-arch")
	ws.Spec.Architecture = "arm64"
	pod := makeRunningPod(podName("ws-g-arch", string(ws.UID)), "default", "10.0.0.1")
	pod.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	pwSecret := makePasswordSecret("ws-g-arch", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-arch", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 on architecture drift")
}

func TestGaugeDrift_Active_CrashLoopBackOff_Decrements(t *testing.T) {
	ws := activeWorkspaceWithPod("ws-g-crash")
	pod := makeRunningPod(podName("ws-g-crash", string(ws.UID)), "default", "10.0.0.1")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Ready: true,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
		},
	}
	pwSecret := makePasswordSecret("ws-g-crash", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-crash", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 on CrashLoopBackOff")
}

func TestGaugeDrift_Active_AgentUnreachableThreshold_Decrements(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-g-unreach", "default", v1.WorkspacePhaseActive)
	ws.UID = "ws-g-unreach-uid"
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.ConsecutiveHealthFailures = 2

	pod := makeRunningPod(podName("ws-g-unreach", string(ws.UID)), "default", "127.0.0.1")

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdPort = 1
	agentdAdminPort = 1
	defer func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-unreach", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 when agent unreachable beyond threshold")
}

func TestGaugeDrift_Active_AgentUnhealthyThreshold_Decrements(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agentd.HealthzResponse{Healthy: false})
	}))
	defer server.Close()

	scheme := testScheme(t)
	ws := makeWorkspace("ws-g-sick", "default", v1.WorkspacePhaseActive)
	ws.UID = "ws-g-sick-uid"
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.ConsecutiveHealthFailures = 2

	pod := makeRunningPod(podName("ws-g-sick", string(ws.UID)), "default", "127.0.0.1")

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdPort = port
	agentdAdminPort = port
	defer func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-sick", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 when agent unhealthy beyond threshold")
}

func TestGaugeDrift_Terminating_WithPodIP_Decrements(t *testing.T) {
	ws := makeWorkspace("ws-g-term", "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PodIP = "10.0.0.1"
	r := reconcilerFor(t, ws)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-term", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, -1.0, delta, "WorkspacesRunning must decrement by 1 on Terminating with PodIP set")
}

func TestGaugeDrift_Terminating_NoPodIP_NoDecrement(t *testing.T) {
	ws := makeWorkspace("ws-g-noterm", "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PodIP = ""
	r := reconcilerFor(t, ws)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-noterm", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, 0.0, delta, "WorkspacesRunning must NOT decrement on Terminating with empty PodIP")
}

func TestGaugeDrift_Failed_SelfHealToActive_Increments(t *testing.T) {
	ws := makeWorkspace("ws-g-heal", "default", v1.WorkspacePhaseFailed)
	expectedPodName := podName("ws-g-heal", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.1")
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	r := reconcilerFor(t, ws, pod)

	delta := gaugeDelta(t, ws.Spec.Runtime, string(ws.Spec.SecurityLevel), func() {
		_, err := r.Reconcile(context.Background(), reqFor("ws-g-heal", "default"))
		require.NoError(t, err)
	})
	assert.Equal(t, 1.0, delta, "WorkspacesRunning must increment by 1 when Failed self-heals to Active")
}

func TestCreatingActiveCycle_MultiReconcile_NoGaugeDrift(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-cycle", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-cycle-uid"
	ws.Status.PVCName = "workspace-ws-cycle"

	name := podName("ws-cycle", string(ws.UID))
	readyPod := makeRunningPod(name, "default", "10.0.0.1")
	pwSecret := makePasswordSecret("ws-cycle", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, readyPod, pwSecret, rte).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	rt, sl := ws.Spec.Runtime, string(ws.Spec.SecurityLevel)
	ctrMetrics.WorkspacesRunning.Reset()

	ctx := context.Background()
	key := types.NamespacedName{Name: "ws-cycle", Namespace: "default"}

	// Initial Creating→Active: the ready pod drives the gauge to 1 through
	// the real reconcile path (handleCreating Inc).
	_, err := r.Reconcile(ctx, reqFor("ws-cycle", "default"))
	require.NoError(t, err)
	assert.Equal(t, 1.0, readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, rt, sl),
		"initial Creating→Active reconcile must set gauge to 1")

	const cycles = 5
	for i := int64(1); i <= cycles; i++ {
		// Active→Creating: bump RestartGeneration (persisted to the fake
		// client so Reconcile observes it) then reconcile. handleActive
		// deletes the pod and Dec's the gauge.
		cur := &v1.Workspace{}
		require.NoError(t, fc.Get(ctx, key, cur))
		cur.Spec.RestartGeneration = i
		require.NoError(t, fc.Update(ctx, cur))

		_, err := r.Reconcile(ctx, reqFor("ws-cycle", "default"))
		require.NoError(t, err)
		assert.Equal(t, 0.0, readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, rt, sl),
			"cycle %d: Active→Creating reconcile must decrement gauge to 0", i)

		// Simulate kubelet having rebuilt a fresh, ready pod (handleActive
		// deleted the prior one on the RestartGeneration bump).
		require.NoError(t, fc.Create(ctx, makeRunningPod(name, "default", "10.0.0.1")))

		// Creating→Active: the ready pod reconciles back to Active via
		// handleCreating and Inc's the gauge.
		_, err = r.Reconcile(ctx, reqFor("ws-cycle", "default"))
		require.NoError(t, err)
		assert.Equal(t, 1.0, readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, rt, sl),
			"cycle %d: Creating→Active reconcile must increment gauge back to 1", i)
	}

	assert.Equal(t, 1.0, readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, rt, sl),
		"after %d full Active↔Creating reconcile cycles the gauge must read 1 (no drift)", cycles)
}

func TestGaugeDrift_Terminating_StatusUpdateFailure_NoDecrement(t *testing.T) {
	scheme := testScheme(t)
	ws := makeWorkspace("ws-g-rollback", "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PodIP = "10.0.0.1"

	updateErr := errors.New("simulated status update failure")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws).
		WithStatusSubresource(&v1.Workspace{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					return updateErr
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	rt, sl := ws.Spec.Runtime, string(ws.Spec.SecurityLevel)
	ctrMetrics.WorkspacesRunning.Reset()
	ctrMetrics.WorkspacesRunning.WithLabelValues(rt, sl).Inc() // workspace was Active and counted in the gauge

	_, err := r.Reconcile(context.Background(), reqFor("ws-g-rollback", "default"))
	require.ErrorIs(t, err, updateErr)

	assert.Equal(t, 1.0, readGaugeVecValue(t, ctrMetrics.WorkspacesRunning, rt, sl),
		"gauge must NOT decrement when Status().Update fails — otherwise the retry double-decrements")
}

// --- US-24.11 recovery metrics integration tests ---

func TestReconcile_RecoverySuccess_DecrementsInRecoveryGauge(t *testing.T) {
	ws := makeWorkspace("ws-rec-dec", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-rec-dec-uid"
	ws.Status.PVCName = "workspace-ws-rec-dec"
	ws.Status.ConsecutiveFailures = 2
	ws.Status.LastFailureClass = "Process"
	past := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	ws.Status.LastFailureAt = &past

	name := podName("ws-rec-dec", string(ws.UID))
	readyPod := makeRunningPod(name, "default", "10.0.0.1")
	pwSecret := makePasswordSecret("ws-rec-dec", "default")
	pvc := makeBoundPVC("workspace-ws-rec-dec", "default", ws.UID)
	r := reconcilerFor(t, ws, readyPod, pwSecret, pvc)

	ctrMetrics.WorkspacesInRecovery.Set(1)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rec-dec", "default"))
	require.NoError(t, err)

	assert.Equal(t, 0.0, readPlainGaugeValue(t, ctrMetrics.WorkspacesInRecovery),
		"WorkspacesInRecovery must Dec to 0 after recovery success")
}

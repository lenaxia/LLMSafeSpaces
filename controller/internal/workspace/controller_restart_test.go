// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	ctrMetrics "github.com/lenaxia/llmsafespaces/controller/internal/metrics"
)

// readCounterValue reads a labelless counter's current value.
func readCounterValue(t *testing.T, c interface {
	Write(*dto.Metric) error
}) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// --- C1: maybeResetConsecutiveFailures must reset ControllerRestartCount
// independently of ConsecutiveFailures (US-24.8 spec lines 13-17). ---

func TestMaybeResetConsecutiveFailures_ControllerRestartCountOnly_After2Min(t *testing.T) {
	ws := &v1.Workspace{}
	threeMinAgo := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	ws.Status.LastStableAt = &threeMinAgo
	ws.Status.ConsecutiveFailures = 0
	ws.Status.ControllerRestartCount = 3

	maybeResetConsecutiveFailures(ws)

	assert.Equal(t, int32(0), ws.Status.ControllerRestartCount, "ControllerRestartCount must reset after the stability window even when ConsecutiveFailures==0")
	assert.Nil(t, ws.Status.LastStableAt)
}

func TestMaybeResetConsecutiveFailures_ControllerRestartCountOnly_Before2Min(t *testing.T) {
	ws := &v1.Workspace{}
	oneMinAgo := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	ws.Status.LastStableAt = &oneMinAgo
	ws.Status.ControllerRestartCount = 3

	maybeResetConsecutiveFailures(ws)

	assert.Equal(t, int32(3), ws.Status.ControllerRestartCount, "must not reset before the stability window elapses")
}

func TestMaybeResetConsecutiveFailures_ControllerRestartCountOnly_NilLastStableAt_StartsClock(t *testing.T) {
	ws := &v1.Workspace{}
	ws.Status.ControllerRestartCount = 3
	ws.Status.LastStableAt = nil

	maybeResetConsecutiveFailures(ws)

	assert.NotNil(t, ws.Status.LastStableAt, "must start the stability clock on first healthy reconcile")
	assert.Equal(t, int32(3), ws.Status.ControllerRestartCount, "must not reset on the clock-starting reconcile")
}

// --- C4 + C5: health-check restart increments and metrics. ---
//
// The shared harness wires an httptest health endpoint, overrides the
// controller's package-level port/interval vars, and stands up a reconciler
// one health failure away from the restart threshold.

func setupUnhealthyHealthReconciler(t *testing.T, ws *v1.Workspace) (*WorkspaceReconciler, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agentd.HealthzResponse{Healthy: false})
	}))

	scheme := testScheme(t)
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), ws.Namespace, ws.Status.PodIP)
	pwSecret := makePasswordSecret(ws.Name, ws.Namespace)

	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdPort = port
	agentdAdminPort = port
	t.Cleanup(func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
		server.Close()
	})

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod, pwSecret).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	return &WorkspaceReconciler{Client: fc, Scheme: scheme}, server
}

func activeWorkspaceForHealthCheck(name string) *v1.Workspace {
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseActive)
	ws.UID = types.UID(name + "-uid")
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.ConsecutiveHealthFailures = healthCheckFailureThreshold - 1
	return ws
}

// TestControllerRestart_IncrementsRestartCount (US-24.7 AC 1)
func TestControllerRestart_IncrementsRestartCount(t *testing.T) {
	ws := activeWorkspaceForHealthCheck("ws-cr-restart")
	r, _ := setupUnhealthyHealthReconciler(t, ws)

	before := ws.Status.RestartCount
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, before+1, updated.Status.RestartCount, "RestartCount must increment on health-check restart")
}

// TestControllerRestart_IncrementsControllerRestartCount (US-24.7 AC 1)
func TestControllerRestart_IncrementsControllerRestartCount(t *testing.T) {
	ws := activeWorkspaceForHealthCheck("ws-cr-count")
	r, _ := setupUnhealthyHealthReconciler(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, int32(1), updated.Status.ControllerRestartCount, "ControllerRestartCount must increment on health-check restart")
}

// TestControllerRestart_DoesNotIncrementConsecutiveFailures (US-24.7 AC 2)
func TestControllerRestart_DoesNotIncrementConsecutiveFailures(t *testing.T) {
	ws := activeWorkspaceForHealthCheck("ws-cr-nofail")
	r, _ := setupUnhealthyHealthReconciler(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures, "health-check restart must NOT increment ConsecutiveFailures")
}

// TestControllerRestart_EmitsControllerRestartsMetric (C5)
func TestControllerRestart_EmitsControllerRestartsMetric(t *testing.T) {
	ws := activeWorkspaceForHealthCheck("ws-cr-metric")
	r, _ := setupUnhealthyHealthReconciler(t, ws)

	before := readCounterValue(t, ctrMetrics.WorkspaceControllerRestartsTotal)
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	after := readCounterValue(t, ctrMetrics.WorkspaceControllerRestartsTotal)

	assert.Greater(t, after, before, "WorkspaceControllerRestartsTotal must increment on health-check restart")
}

// --- C4 + M6: health-check restarts no longer escalate to a SafeMode
// signal (retired by #760). The persistent-unreachability escalation
// path now only exists for classified failure episodes
// (enterRecovery); health-check restarts bump the counters and the
// recovery-exhaustion coverage lives in recovery_exhaustion_test.go. ---

func TestControllerRestart_5Consecutive_NoEscalationSignal(t *testing.T) {
	// #760: the restart path must not write any escalation state — the
	// derived RecoveryExhausted condition only comes from classified
	// failure episodes crossing their per-class threshold.
	ws := activeWorkspaceForHealthCheck("ws-cr-noesc")
	ws.Status.ControllerRestartCount = 4
	r, _ := setupUnhealthyHealthReconciler(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, int32(5), updated.Status.ControllerRestartCount)
	assert.Nil(t, recoveryExhaustedCondition(updated),
		"health-check restarts are not classified failures and must not set the RecoveryExhausted condition")
}

// --- M7: restartGeneration bump clears ControllerRestartCount (US-24.7 AC 5). ---

func TestRestartGeneration_InCreating_ClearsControllerRestartCount(t *testing.T) {
	ws := makeWorkspace("ws-rg-crc", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-rg-crc-uid"
	ws.Status.PVCName = "workspace-ws-rg-crc"
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.ControllerRestartCount = 4
	ws.Status.ConsecutiveFailures = 5
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})

	pvc := makeBoundPVC("workspace-ws-rg-crc", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-rg-crc", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rg-crc", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-rg-crc", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ControllerRestartCount, "restartGeneration bump must clear ControllerRestartCount")
	assert.Nil(t, recoveryExhaustedCondition(updated), "restartGeneration bump must clear the derived RecoveryExhausted condition")
}

// TestRestartGeneration_InCreating_ClearsLastStableAt guards an adversarial
// finding surfaced while fixing C1/M7: the restartGeneration bump clears
// ConsecutiveFailures and ControllerRestartCount but previously left
// LastStableAt stale. A stale LastStableAt would cause the NEXT failure's
// maybeResetConsecutiveFailures to see an elapsed stability window and
// prematurely forgive the new failure. The bump is a fresh start, so the
// stability clock must reset with the rest of the recovery state.
func TestRestartGeneration_InCreating_ClearsLastStableAt(t *testing.T) {
	ws := makeWorkspace("ws-rg-lsa", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-rg-lsa-uid"
	ws.Status.PVCName = "workspace-ws-rg-lsa"
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.ConsecutiveFailures = 5
	stale := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.LastStableAt = &stale

	pvc := makeBoundPVC("workspace-ws-rg-lsa", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-rg-lsa", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rg-lsa", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-rg-lsa", Namespace: "default"}, updated))
	assert.Nil(t, updated.Status.LastStableAt, "restartGeneration bump must clear the stale stability clock")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures)
}

// --- H5: suspend must Dec WorkspacesInRecovery and clear ControllerRestartCount
// (US-24.8 F22) — and clear the derived RecoveryExhausted condition (#760). ---

func TestSuspend_InRecovery_DecountersInRecoveryGauge_AndClearsRestartCount(t *testing.T) {
	ws := makeWorkspace("ws-susp-crc", "default", v1.WorkspacePhaseSuspending)
	ws.UID = "ws-susp-crc-uid"
	ws.Status.ConsecutiveFailures = 4
	ws.Status.ControllerRestartCount = 3
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})
	now := metav1.Now()
	ws.Status.LastFailureAt = &now

	pod := makeRunningPod(podName("ws-susp-crc", string(ws.UID)), "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	// In production, ConsecutiveFailures>0 implies a prior WorkspacesInRecovery.Inc
	// via enterRecovery. Seed the gauge to mirror that invariant.
	ctrMetrics.WorkspacesInRecovery.Inc()
	gaugeBefore := readPlainGaugeValue(t, ctrMetrics.WorkspacesInRecovery)

	_, err := r.Reconcile(context.Background(), reqFor("ws-susp-crc", "default"))
	require.NoError(t, err)

	gaugeAfter := readPlainGaugeValue(t, ctrMetrics.WorkspacesInRecovery)
	assert.Equal(t, gaugeBefore-1, gaugeAfter, "WorkspacesInRecovery must Dec by 1 when suspending an in-recovery workspace")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-susp-crc", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ControllerRestartCount, "suspend must clear ControllerRestartCount (US-24.8 F22)")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures)
	assert.Nil(t, recoveryExhaustedCondition(updated), "suspend must clear the derived RecoveryExhausted condition (#760)")
}

// TestSuspend_NotInRecovery_DoesNotDecrementGauge: a healthy workspace
// suspending must not drive the gauge negative.
func TestSuspend_NotInRecovery_DoesNotDecrementGauge(t *testing.T) {
	ws := makeWorkspace("ws-susp-ok", "default", v1.WorkspacePhaseSuspending)
	ws.UID = "ws-susp-ok-uid"
	// ConsecutiveFailures == 0 → never Inc'd the recovery gauge.
	pod := makeRunningPod(podName("ws-susp-ok", string(ws.UID)), "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	gaugeBefore := readPlainGaugeValue(t, ctrMetrics.WorkspacesInRecovery)
	_, err := r.Reconcile(context.Background(), reqFor("ws-susp-ok", "default"))
	require.NoError(t, err)
	gaugeAfter := readPlainGaugeValue(t, ctrMetrics.WorkspacesInRecovery)

	assert.Equal(t, gaugeBefore, gaugeAfter, "suspending a non-recovery workspace must not touch WorkspacesInRecovery")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrMetrics "github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// recoveryExhaustedCondition returns the workspace's RecoveryExhausted
// condition, or nil when absent.
func recoveryExhaustedCondition(ws *v1.Workspace) *v1.WorkspaceCondition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == v1.WorkspaceConditionRecoveryExhausted {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

// exhaustedCounterValue reads the package-level exhaustion counter for a
// class (the counter recordRecoveryMetrics writes in production).
func exhaustedCounterValue(t *testing.T, class FailureClass) float64 {
	t.Helper()
	return readCounterValue(t, ctrMetrics.WorkspaceRecoveryExhaustedTotal.WithLabelValues(string(class)))
}

// seedExhaustedEpisode stamps a workspace as one failure away from
// exhaustion for the given class, mirroring what N prior enterRecovery
// calls would have persisted.
func seedExhaustedEpisode(ws *v1.Workspace, class FailureClass) {
	policy := recoveryPolicies[class]
	ws.Status.ConsecutiveFailures = policy.ExhaustionAfter - 1
	ws.Status.LastFailureClass = string(class)
	now := metav1.Now()
	ws.Status.LastFailureAt = &now
}

func TestEnterRecovery_ExhaustionCrossing_SetsCondition(t *testing.T) {
	tests := []struct {
		class FailureClass
	}{
		{FailureClassInfrastructure},
		{FailureClassResource},
		{FailureClassProcess},
		{FailureClassConfiguration},
	}
	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			ws := makeWorkspace("ws-exhaust-"+string(tt.class), "default", v1.WorkspacePhaseCreating)
			seedExhaustedEpisode(ws, tt.class)
			r := reconcilerFor(t, ws)

			_, err := r.enterRecovery(context.Background(), ws, tt.class)
			require.NoError(t, err)

			updated := &v1.Workspace{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))

			cond := recoveryExhaustedCondition(updated)
			require.NotNil(t, cond, "condition must be set when %s failures cross the threshold", tt.class)
			assert.Equal(t, "True", cond.Status)
			assert.Equal(t, v1.ReasonRecoveryExhausted, cond.Reason)
			assert.Contains(t, cond.Message, "spec.suspend", "message must carry the operator remedy (#699)")
			assert.Contains(t, cond.Message, string(tt.class), "message must name the failure class")
			assert.Contains(t, cond.Message, "backoff retries continue", "message must state retries are not halted")
		})
	}
}

// TestEnterRecovery_InfrastructureEscalates is the integration pin for
// the #760 Longhorn silent-loop: an infrastructure failure loop must
// produce an operator-visible signal. Before #760 the Infrastructure
// class had threshold 0 and retried forever with NO condition, event,
// or metric.
func TestEnterRecovery_InfrastructureEscalates(t *testing.T) {
	ws := makeWorkspace("ws-exhaust-infra", "default", v1.WorkspacePhaseCreating)
	seedExhaustedEpisode(ws, FailureClassInfrastructure)
	r := reconcilerFor(t, ws)
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec

	before := exhaustedCounterValue(t, FailureClassInfrastructure)

	_, err := r.enterRecovery(context.Background(), ws, FailureClassInfrastructure)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	require.NotNil(t, recoveryExhaustedCondition(updated), "infra exhaustion must set the condition")
	assert.Equal(t, float64(1), exhaustedCounterValue(t, FailureClassInfrastructure)-before,
		"infra exhaustion must increment the alertable counter")

	events := eventsFrom(rec)
	assert.True(t, hasEvent(events, v1.ReasonRecoveryExhausted),
		"infra exhaustion must emit a warning event, got %v", events)
}

func TestEnterRecovery_BelowThreshold_NoConditionNoEventNoCounter(t *testing.T) {
	ws := makeWorkspace("ws-exhaust-below", "default", v1.WorkspacePhaseCreating)
	ws.Status.ConsecutiveFailures = 1
	r := reconcilerFor(t, ws)
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec

	before := exhaustedCounterValue(t, FailureClassProcess)

	_, err := r.enterRecovery(context.Background(), ws, FailureClassProcess)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Nil(t, recoveryExhaustedCondition(updated), "below-threshold failure must not set the condition")
	assert.Equal(t, float64(0), exhaustedCounterValue(t, FailureClassProcess)-before)
	assert.Empty(t, eventsFrom(rec))
}

func TestEnterRecovery_NoDoubleFire_WhileAlreadyExhausted(t *testing.T) {
	ws := makeWorkspace("ws-exhaust-double", "default", v1.WorkspacePhaseCreating)
	seedExhaustedEpisode(ws, FailureClassProcess)
	r := reconcilerFor(t, ws)
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec

	// First crossing: the episode is flagged.
	_, err := r.enterRecovery(context.Background(), ws, FailureClassProcess)
	require.NoError(t, err)

	afterFirst := exhaustedCounterValue(t, FailureClassProcess)
	firstEvents := eventsFrom(rec)
	assert.True(t, hasEvent(firstEvents, v1.ReasonRecoveryExhausted))

	// The episode continues failing — the condition persists but the
	// Event and counter must NOT fire again.
	_, err = r.enterRecovery(context.Background(), ws, FailureClassProcess)
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	require.NotNil(t, recoveryExhaustedCondition(updated), "condition must persist for the episode")
	assert.Equal(t, float64(0), exhaustedCounterValue(t, FailureClassProcess)-afterFirst,
		"counter must not double-fire while already exhausted")
	assert.Empty(t, eventsFrom(rec), "no second event while already exhausted")
}

// TestEnterRecovery_NoDoubleFire_OnClassSwitch pins the episode
// semantics when the failure class changes mid-episode: the counters are
// class-agnostic (ConsecutiveFailures), so the crossed condition stands
// until the recovery state resets — a class switch does not re-fire.
func TestEnterRecovery_NoDoubleFire_OnClassSwitch(t *testing.T) {
	ws := makeWorkspace("ws-exhaust-switch", "default", v1.WorkspacePhaseCreating)
	seedExhaustedEpisode(ws, FailureClassProcess)
	r := reconcilerFor(t, ws)
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec

	_, err := r.enterRecovery(context.Background(), ws, FailureClassProcess)
	require.NoError(t, err)
	afterProcess := exhaustedCounterValue(t, FailureClassProcess)
	afterConfig := exhaustedCounterValue(t, FailureClassConfiguration)

	// The next failure classifies as Configuration; ConsecutiveFailures
	// (8) is above Configuration's threshold (3), but the episode is
	// already flagged — no re-fire for the new class.
	_, err = r.enterRecovery(context.Background(), ws, FailureClassConfiguration)
	require.NoError(t, err)

	assert.Equal(t, float64(0), exhaustedCounterValue(t, FailureClassProcess)-afterProcess)
	assert.Equal(t, float64(0), exhaustedCounterValue(t, FailureClassConfiguration)-afterConfig,
		"class switch mid-episode must not re-fire the counter for the new class")
}

func TestEnterRecovery_ExhaustionEvent_ReasonAndRemedy(t *testing.T) {
	ws := makeWorkspace("ws-exhaust-event", "default", v1.WorkspacePhaseCreating)
	seedExhaustedEpisode(ws, FailureClassConfiguration)
	r := reconcilerFor(t, ws)
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec

	_, err := r.enterRecovery(context.Background(), ws, FailureClassConfiguration)
	require.NoError(t, err)

	events := eventsFrom(rec)
	require.Len(t, events, 1, "exactly one event on the crossing")
	assert.Contains(t, events[0], "Warning")
	assert.Contains(t, events[0], v1.ReasonRecoveryExhausted)
	assert.Contains(t, events[0], "spec.suspend", "event must carry the operator remedy (#699)")
}

// --- Derived-clearing: every ConsecutiveFailures reset clears the condition. ---

func TestClearRecoveryState_ClearsConditionAndCounters(t *testing.T) {
	ws := makeWorkspace("ws-clear", "default", v1.WorkspacePhaseCreating)
	seedExhaustedEpisode(ws, FailureClassProcess)
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})
	future := metav1.NewTime(time.Now().Add(time.Minute))
	ws.Status.NextRetryAt = &future

	clearRecoveryState(ws)

	assert.Equal(t, int32(0), ws.Status.ConsecutiveFailures)
	assert.Equal(t, "", ws.Status.LastFailureClass)
	assert.Nil(t, ws.Status.LastFailureAt)
	assert.Nil(t, ws.Status.NextRetryAt)
	assert.Nil(t, ws.Status.LastStableAt)
	assert.Nil(t, recoveryExhaustedCondition(ws),
		"the derived condition must not outlive the counters it is derived from")
}

func TestMaybeResetConsecutiveFailures_ClearsCondition_AfterStabilityWindow(t *testing.T) {
	ws := makeWorkspace("ws-reset", "default", v1.WorkspacePhaseActive)
	seedExhaustedEpisode(ws, FailureClassProcess)
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})
	threeMinAgo := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	ws.Status.LastStableAt = &threeMinAgo

	maybeResetConsecutiveFailures(ws)

	assert.Equal(t, int32(0), ws.Status.ConsecutiveFailures)
	assert.Nil(t, recoveryExhaustedCondition(ws),
		"recovery success (2-minute stability window) must clear the condition")
}

func TestMaybeResetConsecutiveFailures_BeforeWindow_ConditionStays(t *testing.T) {
	ws := makeWorkspace("ws-reset-soon", "default", v1.WorkspacePhaseActive)
	seedExhaustedEpisode(ws, FailureClassProcess)
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})
	oneMinAgo := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	ws.Status.LastStableAt = &oneMinAgo

	maybeResetConsecutiveFailures(ws)

	assert.NotNil(t, recoveryExhaustedCondition(ws),
		"condition must persist until the stability window resets the counters")
	assert.Equal(t, recoveryPolicies[FailureClassProcess].ExhaustionAfter-1, ws.Status.ConsecutiveFailures)
}

// TestRestartGeneration_InCreating_ClearsRecoveryExhaustedCondition: the
// operator's explicit retry (the escape hatch of last resort, #935)
// starts a fresh episode — the derived condition must drop with the
// counters.
func TestRestartGeneration_InCreating_ClearsRecoveryExhaustedCondition(t *testing.T) {
	ws := makeWorkspace("ws-rg-exhaust", "default", v1.WorkspacePhaseCreating)
	ws.UID = "ws-rg-exhaust-uid"
	ws.Status.PVCName = "workspace-ws-rg-exhaust"
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.ConsecutiveFailures = 7
	ws.Status.LastFailureClass = string(FailureClassProcess)
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})

	pvc := makeBoundPVC("workspace-ws-rg-exhaust", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-rg-exhaust", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python:3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rg-exhaust", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-rg-exhaust", Namespace: "default"}, updated))
	assert.Nil(t, recoveryExhaustedCondition(updated),
		"restartGeneration bump must clear the RecoveryExhausted condition with the rest of the recovery state")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures)
}

// TestSuspend_ClearsRecoveryExhaustedCondition: suspend clears recovery
// state for a fresh start on resume — the derived condition goes with
// it (the pre-#760 "preserve SafeMode across suspend" behavior existed
// to feed a TTL carve-out in handleSuspended that was never built).
func TestSuspend_ClearsRecoveryExhaustedCondition(t *testing.T) {
	ws := makeWorkspace("ws-susp-exhaust", "default", v1.WorkspacePhaseSuspending)
	ws.UID = "ws-susp-exhaust-uid"
	ws.Status.ConsecutiveFailures = 6
	ws.Status.LastFailureClass = string(FailureClassResource)
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionRecoveryExhausted, Status: "True", Reason: v1.ReasonRecoveryExhausted,
	})
	now := metav1.Now()
	ws.Status.LastFailureAt = &now

	pod := makeRunningPod(podName("ws-susp-exhaust", string(ws.UID)), "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-susp-exhaust", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-susp-exhaust", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
	assert.Nil(t, recoveryExhaustedCondition(updated),
		"suspend must clear the RecoveryExhausted condition with the rest of the recovery state")
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures)
}

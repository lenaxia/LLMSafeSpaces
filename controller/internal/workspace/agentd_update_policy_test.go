// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_update_policy_test.go — #1746 reconcile pins for
// agentdDelivery.updatePolicy (Rolling | OnRefresh) + forceRoll,
// extending the #761/#1724 drain fixtures (session_drain_test.go,
// upgrade_rollout_kill_test.go).
//
// Product posture (owner ruling 2026-10-19): a platform upgrade must
// NOT roll workspace pods — the user owns when their sessions are
// interrupted; agentd updates apply at the next user-initiated compute
// refresh (or any other natural pod recreation). OnRefresh is the
// DEFAULT; Rolling and forceRoll are opt-ins.
//
// Pins:
//   - OnRefresh (default, and spec-pinned): a desired-digest change
//     NEVER deletes a running pod (busy or idle); it records
//     status.agentdPendingDigest and leaves agentdEffectiveDigest on
//     the running pod's digest. The next natural recreation (here: a
//     user-consented restart-generation bump) applies the new digest
//     and CLEARS the pending field.
//   - Rolling: digest drift rolls the pod THROUGH the #761 drain —
//     deferred behind busy sessions (requeue at drainPollInterval, no
//     kill), released on idle; the rebuilt pod carries the new digest.
//   - forceRoll: digest drift reconciles IMMEDIATELY under OnRefresh —
//     the drain is bypassed (operator-declared consent, the #1505
//     posture), the forced metric moves, the rebuilt pod carries the
//     new digest.
//   - The instance-level default (--agentd-update-policy) is consulted
//     when the spec is absent, and the spec OVERRIDES it.
//
// Red-first (worklog): committed before the controller logic; the
// behavioral arms failed then with "expected phase Creating, got Active"
// / "pod still exists" / empty agentdPendingDigest (assertion output
// quoted in the commit message).

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// The digest PORTIONS of the #1724 fixtures (upgradeDigestA/B in
// upgrade_rollout_kill_test.go carry full image refs; the status fields
// record the digest identity only — a repo:tag re-pin of the SAME
// digest is not drift).
var (
	digestPortionA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestPortionB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// makePolicyWorkspace builds the Active workspace + OLD-digest overlay
// pod + password secret — the state every healthy workspace is in the
// moment the controller restarts with digest B (applyOverlayB).
func makePolicyWorkspace(t *testing.T, name string, busy bool) (*v1.Workspace, *corev1.Pod, *corev1.Secret, *statuszStub) {
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseActive)
	start := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &start
	ws.Status.PodIP = "127.0.0.1"
	pod := makeUpgradedOverlayPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSec := makePasswordSecret(ws.Name, "default")

	var stub *statuszStub
	if busy {
		stub = &statuszStub{resp: busyStatusz(100)}
	} else {
		stub = &statuszStub{resp: idleStatusz()}
	}
	startStatuszAgent(t, stub)
	return ws, pod, pwSec, stub
}

// getWs fetches the workspace fresh from the fake client.
func getWs(t *testing.T, r *WorkspaceReconciler, name string) *v1.Workspace {
	t.Helper()
	ws := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, ws))
	return ws
}

// --- policy resolution ------------------------------------------------------

func TestAgentdUpdatePolicy_ResolutionDefaultsAndOverrides(t *testing.T) {
	r := &WorkspaceReconciler{}
	ws := &v1.Workspace{}

	// No spec, no instance default: the product posture.
	assert.Equal(t, v1.AgentdUpdatePolicyOnRefresh, r.resolveAgentdUpdatePolicy(ws),
		"empty everywhere resolves to OnRefresh (the #1746 product default)")

	// Instance-level pin (--agentd-update-policy) applies when the spec
	// is silent.
	r.AgentdUpdatePolicyDefault = v1.AgentdUpdatePolicyRolling
	assert.Equal(t, v1.AgentdUpdatePolicyRolling, r.resolveAgentdUpdatePolicy(ws),
		"the instance-level default is consulted when the workspace spec is absent")

	// The per-workspace spec overrides the instance default.
	ws.Spec.AgentdDelivery = &v1.AgentdDeliverySpec{UpdatePolicy: v1.AgentdUpdatePolicyOnRefresh}
	assert.Equal(t, v1.AgentdUpdatePolicyOnRefresh, r.resolveAgentdUpdatePolicy(ws),
		"spec.agentdDelivery.updatePolicy overrides the instance default")
	ws.Spec.AgentdDelivery = &v1.AgentdDeliverySpec{UpdatePolicy: v1.AgentdUpdatePolicyRolling}
	assert.Equal(t, v1.AgentdUpdatePolicyRolling, r.resolveAgentdUpdatePolicy(ws))
}

// --- OnRefresh: the default posture -----------------------------------------

// TestAgentdUpdatePolicy_OnRefresh_NoRoll_RecordsPending: with the
// default policy (absent spec AND absent instance flag), a desired
// digest change must not touch a running pod with BUSY sessions — and,
// distinct from the #1724 pins, must now RECORD the drift on status:
// agentdPendingDigest = desired, agentdEffectiveDigest = the running
// pod's digest.
func TestAgentdUpdatePolicy_OnRefresh_NoRoll_RecordsPending(t *testing.T) {
	ws, pod, pwSec, _ := makePolicyWorkspace(t, "ws-policy-onrefresh-busy", true)
	r, rec := reconcilerForDrain(t, ws, pod, pwSec)
	applyOverlayB(r)

	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		require.NoError(t, err)
		assert.True(t, podExists(t, r, pod.Name),
			"reconcile %d: OnRefresh must not delete a running pod on digest drift", i)
	}

	updated := getWs(t, r, ws.Name)
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase)
	assert.Equal(t, digestPortionB, updated.Status.AgentdPendingDigest,
		"the desired digest is recorded on status — 'agentd X pending, applies on next refresh'")
	assert.Equal(t, digestPortionA, updated.Status.AgentdEffectiveDigest,
		"the effective digest is what the RUNNING pod was built with")

	// No drain machinery may engage under OnRefresh.
	for _, e := range eventsFrom(rec) {
		assert.NotContains(t, e, "SessionDrain",
			"OnRefresh digest drift must not fire any drain event, got: %s", e)
	}
}

// TestAgentdUpdatePolicy_OnRefresh_IdleAlsoUntouched_RecordsPending:
// laziness holds even idle — no churn, drift still recorded.
func TestAgentdUpdatePolicy_OnRefresh_IdleAlsoUntouched_RecordsPending(t *testing.T) {
	ws, pod, pwSec, _ := makePolicyWorkspace(t, "ws-policy-onrefresh-idle", false)
	r, _ := reconcilerForDrain(t, ws, pod, pwSec)
	applyOverlayB(r)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	assert.True(t, podExists(t, r, pod.Name), "an idle pod is not churned under OnRefresh")
	updated := getWs(t, r, ws.Name)
	assert.Equal(t, digestPortionB, updated.Status.AgentdPendingDigest)
	assert.Equal(t, digestPortionA, updated.Status.AgentdEffectiveDigest)
}

// TestAgentdUpdatePolicy_OnRefresh_AppliesAtNaturalRecreation: the
// pending digest applies at the next natural pod recreation — here a
// user-consented restart-generation bump (compute refresh) after the
// busy turn ends — and the pending field CLEARS on the rebuilt pod.
func TestAgentdUpdatePolicy_OnRefresh_AppliesAtNaturalRecreation(t *testing.T) {
	ws, pod, pwSec, stub := makePolicyWorkspace(t, "ws-policy-onrefresh-apply", true)
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", ws.UID)
	r, _ := reconcilerForDrain(t, ws, pod, pwSec, rte, pvc)
	applyOverlayB(r)

	// Post-upgrade: pending recorded, pod untouched.
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	require.Equal(t, digestPortionB, getWs(t, r, ws.Name).Status.AgentdPendingDigest)

	// User refresh (compute refresh): generation bump while busy → the
	// #761 drain defers BOTH the recycle and the agentd update.
	ws.Spec.RestartGeneration = 2
	require.NoError(t, r.Update(context.Background(), ws))
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	require.True(t, podExists(t, r, pod.Name), "busy turn: the refresh's recycle defers (the agentd update rides it)")

	// Turn ends → recycle proceeds → rebuilt pod carries digest B and
	// the status converges (pending cleared, effective = B).
	stub.set(idleStatusz())
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name), "idle + generation bump: the natural recreation proceeds")

	rebuilt := &corev1.Pod{}
	require.Eventually(t, func() bool {
		_, _ = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		return r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, rebuilt) == nil
	}, 5*time.Second, 100*time.Millisecond, "handleCreating must rebuild the pod")

	assert.Equal(t, upgradeDigestB, agentdVolumeRef(rebuilt),
		"the recreated pod carries the pending digest — the refresh applies it")
	updated := getWs(t, r, ws.Name)
	assert.Equal(t, digestPortionB, updated.Status.AgentdEffectiveDigest,
		"the effective digest follows the rebuilt pod")
	assert.Empty(t, updated.Status.AgentdPendingDigest,
		"converged: no pending digest once the pod runs the desired one")
}

// --- Rolling: the opt-in ------------------------------------------------------

// TestAgentdUpdatePolicy_Rolling_RollsThroughDrain: spec-pinned Rolling
// rolls on digest drift but ONLY through the #761 drain — busy sessions
// defer (no kill, drain-deferred metric with the agentd_update_roll
// reason, pending visible during the deferral), idle releases.
func TestAgentdUpdatePolicy_Rolling_RollsThroughDrain(t *testing.T) {
	ws, pod, pwSec, stub := makePolicyWorkspace(t, "ws-policy-rolling", true)
	ws.Spec.AgentdDelivery = &v1.AgentdDeliverySpec{UpdatePolicy: v1.AgentdUpdatePolicyRolling}
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", ws.UID)
	r, rec := reconcilerForDrain(t, ws, pod, pwSec, rte, pvc)
	applyOverlayB(r)

	deferredBefore := counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonAgentdUpdateRoll)

	// Busy: deferred — pod alive, requeue at the drain cadence, drift on
	// status visible while the roll waits.
	res, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.True(t, podExists(t, r, pod.Name), "Rolling defers the roll behind busy sessions (#761)")
	assert.Equal(t, drainPollInterval, res.RequeueAfter, "the roll parks at the drain poll cadence")
	assert.Equal(t, deferredBefore+1, counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonAgentdUpdateRoll),
		"the deferral is attributed to the agentd_update_roll drain reason")
	updated := getWs(t, r, ws.Name)
	assert.Equal(t, digestPortionB, updated.Status.AgentdPendingDigest,
		"the pending digest is visible while the roll is deferred")
	assert.True(t, hasEvent(eventsFrom(rec), "SessionDrainDeferred"),
		"the deferral emits the standard drain event")

	// Idle: released — pod deleted, phase Creating, rebuilt with B.
	stub.set(idleStatusz())
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name), "idle: the Rolling policy's roll proceeds")
	updated = getWs(t, r, ws.Name)
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)

	rebuilt := &corev1.Pod{}
	require.Eventually(t, func() bool {
		_, _ = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		return r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, rebuilt) == nil
	}, 5*time.Second, 100*time.Millisecond, "handleCreating must rebuild the pod")
	assert.Equal(t, upgradeDigestB, agentdVolumeRef(rebuilt), "the rolled pod carries the new digest")

	updated = getWs(t, r, ws.Name)
	assert.Equal(t, digestPortionB, updated.Status.AgentdEffectiveDigest)
	assert.Empty(t, updated.Status.AgentdPendingDigest, "converged after the roll")
}

// TestAgentdUpdatePolicy_InstanceDefaultRolling: --agentd-update-policy
// Rolling (no per-workspace spec) rolls drain-aware — the instance pin
// without a spec override.
func TestAgentdUpdatePolicy_InstanceDefaultRolling(t *testing.T) {
	ws, pod, pwSec, stub := makePolicyWorkspace(t, "ws-policy-instance-rolling", true)
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", ws.UID)
	r, _ := reconcilerForDrain(t, ws, pod, pwSec, rte, pvc)
	r.AgentdUpdatePolicyDefault = v1.AgentdUpdatePolicyRolling
	applyOverlayB(r)

	deferredBefore := counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonAgentdUpdateRoll)
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.True(t, podExists(t, r, pod.Name), "instance Rolling defers behind busy sessions")
	assert.Equal(t, deferredBefore+1, counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonAgentdUpdateRoll))

	// Spec OnRefresh overrides the instance default: no roll on the
	// next drift observation (pod survives the deferred window intact
	// until the spec says otherwise).
	stub.set(idleStatusz())
	ws.Spec.AgentdDelivery = &v1.AgentdDeliverySpec{UpdatePolicy: v1.AgentdUpdatePolicyOnRefresh}
	require.NoError(t, r.Update(context.Background(), ws))
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.True(t, podExists(t, r, pod.Name),
		"a spec flip to OnRefresh stops the roll — the instance default never overrides the workspace")
}

// --- forceRoll: the escape hatch ----------------------------------------------

// TestAgentdUpdatePolicy_ForceRoll_Immediate: forceRoll under OnRefresh
// reconciles the pod IMMEDIATELY on digest drift — the drain is
// bypassed (busy sessions are cut; the operator set the flag), the
// forced metric moves with the agentd_update_roll reason family, and
// the rebuilt pod carries the new digest.
func TestAgentdUpdatePolicy_ForceRoll_Immediate(t *testing.T) {
	ws, pod, pwSec, _ := makePolicyWorkspace(t, "ws-policy-forceroll", true)
	ws.Spec.AgentdDelivery = &v1.AgentdDeliverySpec{
		UpdatePolicy: v1.AgentdUpdatePolicyOnRefresh,
		ForceRoll:    true,
	}
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", ws.UID)
	r, rec := reconcilerForDrain(t, ws, pod, pwSec, rte, pvc)
	applyOverlayB(r)

	forcedBefore := counterValue(t, metrics.WorkspaceDrainForcedTotal, drainReasonAgentdForceRoll)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name),
		"forceRoll + digest drift: the pod reconciles NOW — busy sessions do not defer it")
	assert.Equal(t, forcedBefore+1, counterValue(t, metrics.WorkspaceDrainForcedTotal, drainReasonAgentdForceRoll),
		"the forced roll is attributed to the agentd force-roll reason")
	assert.True(t, hasEvent(eventsFrom(rec), "AgentdUpdateForcedRoll"),
		"the forced roll emits its own event reason (operator-visible consent record)")

	updated := getWs(t, r, ws.Name)
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)

	rebuilt := &corev1.Pod{}
	require.Eventually(t, func() bool {
		_, _ = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		return r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, rebuilt) == nil
	}, 5*time.Second, 100*time.Millisecond, "handleCreating must rebuild the pod")
	assert.Equal(t, upgradeDigestB, agentdVolumeRef(rebuilt), "the forced roll lands the new digest")

	updated = getWs(t, r, ws.Name)
	assert.Equal(t, digestPortionB, updated.Status.AgentdEffectiveDigest)
	assert.Empty(t, updated.Status.AgentdPendingDigest)
}

// TestAgentdUpdatePolicy_ForceRoll_NoDriftIsNoop: forceRoll with a
// MATCHING digest does nothing — the hatch is level-triggered on drift,
// not a standing recycle order.
func TestAgentdUpdatePolicy_ForceRoll_NoDriftIsNoop(t *testing.T) {
	ws, pod, pwSec, _ := makePolicyWorkspace(t, "ws-policy-forceroll-noop", false)
	ws.Spec.AgentdDelivery = &v1.AgentdDeliverySpec{ForceRoll: true}
	r, _ := reconcilerForDrain(t, ws, pod, pwSec)
	// No applyOverlayB: desired == the pod's digest A.

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.True(t, podExists(t, r, pod.Name), "no drift: forceRoll is a no-op")
	assert.Equal(t, v1.WorkspacePhaseActive, getWs(t, r, ws.Name).Status.Phase)
}

// --- effective-digest bookkeeping on plain builds ------------------------------

// TestAgentdUpdatePolicy_EffectiveDigestRecordedOnBuild: a fresh
// Creating→Active build records the effective digest of the built pod
// and never carries a pending digest while converged.
func TestAgentdUpdatePolicy_EffectiveDigestRecordedOnBuild(t *testing.T) {
	ws, pod, pwSec, _ := makePolicyWorkspace(t, "ws-policy-build-record", false)
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", ws.UID)
	r, _ := reconcilerForDrain(t, ws, pod, pwSec, rte, pvc)
	applyOverlayB(r)

	// Recreate through a generation bump (idle: immediate).
	ws.Spec.RestartGeneration = 2
	require.NoError(t, r.Update(context.Background(), ws))
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name))

	rebuilt := &corev1.Pod{}
	require.Eventually(t, func() bool {
		_, _ = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		return r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, rebuilt) == nil
	}, 5*time.Second, 100*time.Millisecond)

	updated := getWs(t, r, ws.Name)
	assert.Equal(t, digestPortionB, updated.Status.AgentdEffectiveDigest,
		"the build records the built pod's digest as effective")
	assert.Empty(t, updated.Status.AgentdPendingDigest,
		"a build under the current desired digest leaves no pending")
}

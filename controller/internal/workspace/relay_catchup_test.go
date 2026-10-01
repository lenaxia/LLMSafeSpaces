// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// relay_catchup_test.go — the EXISTING-workspace catch-up pins (issue
// #1614's verification path): relay handoff staging is LEVEL-TRIGGERED
// from the Creating/Active phase handlers, not tied to any event a
// pre-arming workspace never re-fires (the #1597 creation-binding class
// was the charter's prime hypothesis — disproven here and live: after
// the #1611 API-side un-break, workspace d8bed486 converged from a
// 5-day permanent fallback to token delivery within minutes, with no
// workspace event, purely on its next Active reconciles).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestRelayStaging_CatchUp_ConvergesPreArmingActiveWorkspace: a
// workspace that predates the relay-only arming (no handoff Secret, no
// staged-state annotations, already Active with a running pod) converges
// on its FIRST reconcile under an armed controller — the catch-up
// semantics the strict-mode flip depends on. Driven through
// Reconcile/handleActive (the production trigger), not a direct
// reconcileRelayStaging call.
func TestRelayStaging_CatchUp_ConvergesPreArmingActiveWorkspace(t *testing.T) {
	pubSec, _ := makePubSecret(t, 4)
	ws := activeWorkspaceWithPod("ws-catchup")
	require.Empty(t, ws.Annotations[relayStagedProvidersAnnotation], "fixture: no prior staging state (pre-arming)")
	pod := makeRunningPod(podName("ws-catchup", string(ws.UID)), "default", "10.0.0.1")
	pw := makePasswordSecret("ws-catchup", "default")

	src := &fakeProviderSource{providers: []secrets.LLMProviderData{
		openaiPD("openai", "sk-catchup-live"),
	}}
	r := stagingReconciler(t, src, &fakeRouterClient{}, nil, pubSec, ws, pod, pw)

	// Pre-arming ground truth: no handoff Secret exists.
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: handoffSecretName("ws-catchup")}, &corev1.Secret{})
	require.Error(t, err, "fixture: the pre-arming workspace has no staged handoff")

	_, err = r.Reconcile(context.Background(), reqFor("ws-catchup", "default"))
	require.NoError(t, err)

	// Converged: handoff staged with the minted token, envelope sealed,
	// conditions True — one reconcile, no workspace event required.
	ho := getSecret(t, r, "default", handoffSecretName("ws-catchup"))
	h := decodeHandoff(t, ho.Data[relayHandoffDataKey])
	require.Len(t, h.Providers, 1)
	assert.Equal(t, "openai", h.Providers[0].ProviderSlug)
	assert.NotEmpty(t, h.Providers[0].Token, "the catch-up pass mints the token")
	assert.NotEmpty(t, h.Revision)

	getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-catchup", "openai"))

	stored := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-catchup", Namespace: "default"}, stored))
	staged := conditionOf(stored, v1.WorkspaceConditionCredentialsStaged)
	require.NotNil(t, staged)
	assert.Equal(t, "True", staged.Status)
	assert.Equal(t, v1.ReasonCredentialsStaged, staged.Reason)

	// And the second reconcile is a no-op steady state (change-gated
	// persistence): the same handoff bytes survive untouched.
	mintsBefore := func() int {
		return len(h.Providers)
	}
	_, err = r.Reconcile(context.Background(), reqFor("ws-catchup", "default"))
	require.NoError(t, err)
	ho2 := getSecret(t, r, "default", handoffSecretName("ws-catchup"))
	assert.Equal(t, ho.Data[relayHandoffDataKey], ho2.Data[relayHandoffDataKey],
		"steady state: the cached unexpired token is retained, not re-minted")
	assert.Equal(t, mintsBefore(), 1)
}

// TestRelayStaging_TriggerIsLevelTriggeredInBothPhaseHandlers: the
// source-shape pin — handleCreating and handleActive both invoke
// reconcileRelayStaging, positioned BEFORE the lifecycle branches (pod
// build in Creating; restart-generation in Active), so a pre-arming
// workspace converges on its regular reconcile cadence (requeueActive
// 15s / requeueCreating 2s) with no event dependency. Deleting either
// call site — or moving it behind an early-return branch — fails here.
func TestRelayStaging_TriggerIsLevelTriggeredInBothPhaseHandlers(t *testing.T) {
	for _, tc := range []struct {
		file    string
		fun     string
		marker  string // the lifecycle branch the staging pass must precede
		marker2 string
	}{
		{"phase_creating.go", "func (r *WorkspaceReconciler) handleCreating", "buildPod(ctx, workspace)", ""},
		{"phase_active.go", "func (r *WorkspaceReconciler) handleActive", "workspace.Spec.RestartGeneration > workspace.Status.ObservedRestartGeneration", ""},
	} {
		raw, err := os.ReadFile(tc.file)
		require.NoError(t, err, "%s unreadable", tc.file)
		src := string(raw)
		fnStart := strings.Index(src, tc.fun)
		require.GreaterOrEqual(t, fnStart, 0, "%s not found in %s", tc.fun, tc.file)
		callIdx := strings.Index(src[fnStart:], "reconcileRelayStaging(ctx, workspace)")
		require.GreaterOrEqual(t, callIdx, 0, "the staging pass must be called from %s", tc.fun)
		markerIdx := strings.Index(src[fnStart:], tc.marker)
		require.GreaterOrEqual(t, markerIdx, 0)
		assert.Less(t, callIdx, markerIdx,
			"the staging pass in %s must run before the %q lifecycle branch", tc.fun, tc.marker)
	}
}

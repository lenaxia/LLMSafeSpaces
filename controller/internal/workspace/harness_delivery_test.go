// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// harness_delivery_test.go — S5 harness-selection plumbing pins
// (pi-scoping §6 item 2):
//
//   - The golden pod-spec diff is EMPTY for default workspaces: a
//     harness-omitted workspace and an explicit harness=opencode
//     workspace render byte-identical pod specs, in both pod
//     topologies — the zero-behavior-change constraint, pinned as a
//     marshaled-spec equality so ANY divergence (env order, volume
//     order, a stray field) fails the diff.
//   - The supervisor subcommand follows spec.harness:
//     supervise-opencode (default and explicit) vs supervise-pi.
//   - validateWorkspaceHarness refuses loud: pi without the sidecar
//     topology, a harness with no delivery configured, and a harness
//     whose overlay image is unpinned.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- golden zero-diff (the S5 constraint) -----------------------------------

// TestHarnessDelivery_DefaultWorkspaceGoldenZeroDiff: the recorded S5
// constraint — spec.harness="" (the CRD default, every pre-S5 Workspace)
// and spec.harness="opencode" (the explicit form) produce IDENTICAL pod
// specs. Byte-level over the marshaled spec; the opencode-branch
// overlay invariants themselves are pinned by the unchanged S1-era
// opencode_overlay_test.go suite.
func TestHarnessDelivery_DefaultWorkspaceGoldenZeroDiff(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		r := reconcilerWithAllOverlays(t, sidecar)

		wsDefault := newWorkspaceForSecurity(t) // harness omitted
		podDefault, err := r.buildPod(context.Background(), wsDefault)
		require.NoError(t, err)

		wsExplicit := newWorkspaceForSecurity(t)
		wsExplicit.Spec.Harness = v1.HarnessOpencode
		podExplicit, err := r.buildPod(context.Background(), wsExplicit)
		require.NoError(t, err)

		defJSON, err := json.Marshal(podDefault)
		require.NoError(t, err)
		explicitJSON, err := json.Marshal(podExplicit)
		require.NoError(t, err)

		require.JSONEq(t, string(defJSON), string(explicitJSON),
			"golden pod-spec diff MUST be empty: harness=%q vs harness=%q (sidecar=%v)",
			wsDefault.Spec.Harness, wsExplicit.Spec.Harness, sidecar)
		require.Equal(t, string(defJSON), string(explicitJSON),
			"the marshaled specs must be byte-identical, not merely deeply equal")

		// Non-triviality: the compared specs actually carry the opencode
		// overlay (an empty-vs-empty pass would vacuously "succeed").
		require.Contains(t, string(defJSON), `"opencode"`,
			"the golden spec must contain the opencode overlay wiring")
	}
}

// --- supervisor subcommand ---------------------------------------------------

func TestHarnessSupervisorSubcommand(t *testing.T) {
	cases := []struct {
		harness v1.HarnessType
		want    string
	}{
		{"", "supervise-opencode"},
		{v1.HarnessOpencode, "supervise-opencode"},
		{v1.HarnessPi, "supervise-pi"},
	}
	for _, tc := range cases {
		ws := newWorkspaceForSecurity(t)
		ws.Spec.Harness = tc.harness
		require.Equal(t, tc.want, harnessSupervisorSubcommand(ws),
			"harness=%q", tc.harness)
	}
}

// TestHarnessDelivery_PodSubcommandFollowsSpec: the BUILT sidecar pod's
// main container args carry the harness-selected supervisor subcommand.
func TestHarnessDelivery_PodSubcommandFollowsSpec(t *testing.T) {
	r := reconcilerWithAllOverlays(t, true)

	ocWs := newWorkspaceForSecurity(t)
	ocPod, err := r.buildPod(context.Background(), ocWs)
	require.NoError(t, err)
	require.Equal(t, []string{"supervise-opencode"}, ocPod.Spec.Containers[0].Args)

	piWs := piHarnessWorkspace(t)
	piPod, err := r.buildPod(context.Background(), piWs)
	require.NoError(t, err)
	require.Equal(t, []string{"supervise-pi"}, piPod.Spec.Containers[0].Args)
}

// --- validateWorkspaceHarness refusals ---------------------------------------

func TestValidateWorkspaceHarness_Refusals(t *testing.T) {
	// pi requires the sidecar topology — supervise-pi is a PID-1
	// supervisor; the single-container --supervise role is
	// opencode-shaped.
	r := reconcilerWithAllOverlays(t, false)
	err := r.validateWorkspaceHarness(piHarnessWorkspace(t))
	require.ErrorContains(t, err, "requires the agentd sidecar topology")

	// With the sidecar on, the same workspace validates.
	r.AgentdSidecarEnabled = true
	require.NoError(t, r.validateWorkspaceHarness(piHarnessWorkspace(t)))

	// Default-harness workspaces validate in both topologies.
	for _, sidecar := range []bool{false, true} {
		r.AgentdSidecarEnabled = sidecar
		require.NoError(t, r.validateWorkspaceHarness(newWorkspaceForSecurity(t)),
			"sidecar=%v", sidecar)
	}

	// A harness with no delivery entry (a future/unknown harness on a
	// two-harness controller) is refused loud, never half-built.
	unknown := newWorkspaceForSecurity(t)
	unknown.Spec.Harness = v1.HarnessType("claude-code")
	err = r.validateWorkspaceHarness(unknown)
	require.ErrorContains(t, err, "no delivery configured")

	// A harness whose overlay image is unpinned (a programmatically
	// constructed reconciler missing the flag) is refused loud.
	unpinned := reconcilerWithAllOverlays(t, true)
	unpinned.PiImage = ""
	err = unpinned.validateWorkspaceHarness(piHarnessWorkspace(t))
	require.ErrorContains(t, err, "requires its overlay delivery")
}

// --- assembled system: full Reconcile for a spec.harness=pi workspace ---------
//
// The review finding: buildPod-level assertions alone do not prove the
// wiring survives the WHOLE path (validation, defaulting, secret/PVC
// preconditions, sidecar injection). This leg drives the real
// Reconcile from PhaseCreating and asserts the CREATED pod object.

func TestHarnessDelivery_Assembled_PiWorkspaceReconcile(t *testing.T) {
	ws := makeWorkspace("ws-pi-assembled", "default", v1.WorkspacePhaseCreating)
	ws.Spec.Harness = v1.HarnessPi
	ws.Status.PVCName = "workspace-ws-pi-assembled"
	pvc := makeBoundPVC("workspace-ws-pi-assembled", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-pi-assembled", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)
	// pi requires the sidecar topology (validateWorkspaceHarness).
	r.AgentdSidecarEnabled = true

	_, err := r.Reconcile(context.Background(), reqFor("ws-pi-assembled", "default"))
	require.NoError(t, err)

	pod := &corev1.Pod{}
	err = r.Get(context.Background(), types.NamespacedName{Name: podName("ws-pi-assembled", string(ws.UID)), Namespace: "default"}, pod)
	require.NoError(t, err, "the pi workspace's pod must be created")

	// The supervisor subcommand follows spec.harness through the FULL
	// reconcile path, not just buildPod.
	require.Equal(t, []string{"supervise-pi"}, pod.Spec.Containers[0].Args)

	// Exactly one harness overlay: pi wired, opencode absent.
	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.Image != nil {
			volNames[v.Name] = true
		}
	}
	assert.True(t, volNames["pi"], "the pi overlay volume is wired")
	assert.False(t, volNames["opencode"], "the opencode overlay volume is absent on a pi workspace")

	mainEnv := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		mainEnv[e.Name] = e.Value
	}
	require.Equal(t, piAgentDirMountPath, mainEnv[agentd.EnvPiAgentDir],
		"the shared pi agent-dir coordinate rides the main container")

	// The sidecar is a NATIVE sidecar (the last init container,
	// restartPolicy Always) and carries the harness selection
	// (AGENTD_HARNESS=pi — absent means opencode on a mixed fleet).
	require.NotEmpty(t, pod.Spec.InitContainers, "sidecar topology: the native sidecar init container must exist")
	sidecar := pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1]
	sidecarEnv := map[string]string{}
	for _, e := range sidecar.Env {
		sidecarEnv[e.Name] = e.Value
	}
	assert.Equal(t, "pi", sidecarEnv[agentd.EnvAgentdHarness],
		"the sidecar's harness branch keys on the shared env name (pkg/agentd.EnvAgentdHarness)")
}

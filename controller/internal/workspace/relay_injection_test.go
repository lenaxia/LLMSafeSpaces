// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Tests for relay baseURL injection into workspace pods (Epic 42 fleet).
// The controller injects INFERENCE_RELAY_BASEURL as an env var on the
// main container when InferenceRelayURL is configured. agentd reads this
// at startup and rewrites opencode's provider config to route free-tier
// inference through the self-hosted relay fleet. The URL itself is the
// only configuration — the fleet uses per-VM tokens managed by the
// router, never a path-segment secret.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

func init() { opencode.Register() }

// TestBuildPod_RelayBaseURL_NotInjectedWhenEmpty verifies that when
// InferenceRelayURL is empty (the chart default), INFERENCE_RELAY_BASEURL
// is not added — agentd then no-ops the relay injector and opencode
// calls https://opencode.ai/zen/v1 directly.
func TestBuildPod_RelayBaseURL_NotInjectedWhenEmpty(t *testing.T) {
	ws := makeWorkspace("ws-no-relay", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-no-relay"
	pvc := makeBoundPVC("workspace-ws-no-relay", "default", ws.UID)
	pw := makePasswordSecret("ws-no-relay", "default")
	rte := makeRuntimeEnv("python-3.11")

	r := reconcilerFor(t, ws, pvc, pw, rte)
	r.InferenceRelayURL = ""

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod)

	main := mainContainer(pod)
	require.NotNil(t, main)
	for _, e := range main.Env {
		assert.NotEqual(t, "INFERENCE_RELAY_BASEURL", e.Name,
			"INFERENCE_RELAY_BASEURL must not be set when InferenceRelayURL is empty")
	}
}

// TestBuildPod_RelayBaseURL_Injected verifies that when InferenceRelayURL
// is set, INFERENCE_RELAY_BASEURL equals the URL verbatim — the fleet
// uses per-VM tokens, not path-segment secrets, so no URL rewriting.
func TestBuildPod_RelayBaseURL_Injected(t *testing.T) {
	ws := makeWorkspace("ws-relay", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-relay"
	pvc := makeBoundPVC("workspace-ws-relay", "default", ws.UID)
	pw := makePasswordSecret("ws-relay", "default")
	rte := makeRuntimeEnv("python-3.11")

	r := reconcilerFor(t, ws, pvc, pw, rte)
	r.InferenceRelayURL = "https://relay.example.test/"

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	main := mainContainer(pod)
	require.NotNil(t, main)
	assert.Equal(t, "https://relay.example.test/", getEnv(main, "INFERENCE_RELAY_BASEURL"))
}

// --- helpers ---

func mainContainer(pod *corev1.Pod) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func getEnv(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

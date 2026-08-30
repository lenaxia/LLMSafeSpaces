// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// #887 D5.1 pod-spec delivery modes for the admin-mux bearer token.
//
//	File mode (Secret carries the distinct `admin-token` key):
//	  init installs /sandbox-cfg/admin-token mode 0400; main container
//	  gets AGENTD_ADMIN_TOKEN_FILE only — the token never enters env.
//
//	Legacy mode (no key — pre-upsert Secret race):
//	  AGENTD_ADMIN_TOKEN env, exactly as before. The token equals the
//	  password there, which is unavoidable for legacy pods; no NEW pod
//	  is built in this mode once upsert converges.

func buildPodForTokenTest(t *testing.T, pwSecret *corev1.Secret) (*corev1.Pod, *corev1.Container) {
	t.Helper()
	ws := makeWorkspace(pwSecret.Name[len("workspace-pw-"):], "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"
	rte := makeRuntimeEnv("base")
	r := reconcilerFor(t, pwSecret, makeBoundPVC(ws.Status.PVCName, "default", ws.UID), rte)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var main *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			main = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, main)
	return pod, main
}

func envVar(cont *corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range cont.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestPodSpec_AdminTokenFileMode(t *testing.T) {
	sec := makePasswordSecret("ws-filemode", "default")
	sec.Data["admin-token"] = []byte("distinct-admin-token")

	pod, main := buildPodForTokenTest(t, sec)

	// File env present; token env GONE (the leak this PR closes).
	fileVar, ok := envVar(main, "AGENTD_ADMIN_TOKEN_FILE")
	require.True(t, ok, "file mode must set AGENTD_ADMIN_TOKEN_FILE")
	assert.Equal(t, "/sandbox-cfg/admin-token", fileVar.Value)
	_, tokenVar := envVar(main, "AGENTD_ADMIN_TOKEN")
	assert.False(t, tokenVar, "AGENTD_ADMIN_TOKEN must NOT be set in file mode — it rides opencode's env into every tool process")

	// The admin-token file install moved into the agentd init-fs
	// subcommand (design 0053 S3 — the bash heredoc is deleted); the
	// controller-side invariant is that the projected Secret reaches
	// the platform-init container that performs the install. The 0400
	// mode itself is pinned by cmd/workspace-agentd init-fs tests.
	var platformInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "platform-init" {
			platformInit = &pod.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, platformInit)
	var pwMount *corev1.VolumeMount
	for i := range platformInit.VolumeMounts {
		if platformInit.VolumeMounts[i].Name == "pw-secret" {
			pwMount = &platformInit.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, pwMount, "platform-init must mount the password Secret (init-fs installs admin-token from it)")
	require.True(t, pwMount.ReadOnly)

	// Probes carry the DISTINCT token.
	for _, p := range []*corev1.Probe{main.ReadinessProbe, main.StartupProbe} {
		require.NotNil(t, p)
		var hdr string
		for _, h := range p.HTTPGet.HTTPHeaders {
			if h.Name == "Authorization" {
				hdr = h.Value
			}
		}
		assert.Equal(t, "Bearer distinct-admin-token", hdr,
			"probes must authenticate with the distinct admin token in file mode")
	}
}

func TestPodSpec_AdminTokenLegacyEnvMode(t *testing.T) {
	sec := makePasswordSecret("ws-legacy-env", "default") // no admin-token key

	_, main := buildPodForTokenTest(t, sec)

	tokenVar, ok := envVar(main, "AGENTD_ADMIN_TOKEN")
	require.True(t, ok, "legacy mode keeps the env delivery (pre-upsert Secret)")
	require.NotNil(t, tokenVar.ValueFrom, "legacy token env is Secret-sourced")
	_, fileVar := envVar(main, "AGENTD_ADMIN_TOKEN_FILE")
	assert.False(t, fileVar, "file env is not set in legacy mode")

	for _, p := range []*corev1.Probe{main.ReadinessProbe, main.StartupProbe} {
		require.NotNil(t, p)
		var hdr string
		for _, h := range p.HTTPGet.HTTPHeaders {
			if h.Name == "Authorization" {
				hdr = h.Value
			}
		}
		assert.True(t, strings.HasPrefix(hdr, "Bearer "),
			"legacy probes still authenticate (with the password value, as before)")
	}
}

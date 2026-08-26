//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Envtest integration for design 0051 sidecar migration step 1
// (integration level L2): the PLATFORM INIT CONTAINER SPECS against a
// real Kubernetes API server, plus the boot-failure visibility path
// through the real status subresource.
//
// What the API server adds over the unit suite's structural asserts:
// real admission of the platform-init container (uid 1000 + non-root +
// RO rootfs + the image-volume mount), and real status subresource
// persistence for the BootReady condition the incident class (eternal
// reason-less Creating, 2026-08-25) now reports through.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestPlatformInit
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestEnvtestPlatformInit_SpecAdmitted: the overlay-mode pod (legacy and
// sidecar variants) must be admitted by a real API server — the whole
// platform init chain in one shot.
func TestEnvtestPlatformInit_SpecAdmitted(t *testing.T) {
	cfg := startEnvtest(t)
	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		sidecar bool
	}{
		{"legacy overlay chain", false},
		{"sidecar mode", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newWorkspaceForSecurity(t)
			r := reconcilerWithAgentd(t)
			if tc.sidecar {
				r.AgentdSidecarEnabled = true
			}

			pod, err := r.buildPod(context.Background(), ws)
			require.NoError(t, err)
			pod.Namespace = "default"
			require.NoError(t, dyn.Create(context.Background(), pod))

			got := &corev1.Pod{}
			require.NoError(t, dyn.Get(context.Background(), client.ObjectKey{Name: pod.Name, Namespace: "default"}, got))

			names := []string{}
			for _, c := range got.Spec.InitContainers {
				names = append(names, c.Name)
			}
			require.Equal(t, "platform-init", names[0], "platform-init is first")
			require.NotContains(t, names, "credential-setup")
			require.NotContains(t, names, "workspace-dirs")
			if tc.sidecar {
				require.NotContains(t, names, "platform-bootstrap")
				require.NotContains(t, names, "platform-materialize")
				require.Equal(t, "agentd", names[len(names)-1])
			} else {
				require.Equal(t, "platform-bootstrap", names[len(names)-2])
				require.Equal(t, "platform-materialize", names[len(names)-1])
			}
		})
	}
}

// TestEnvtestPlatformInit_BootFailureConditionPersists: a crash-looping
// platform container must surface as BootReady=False THROUGH the real
// status subresource — the 2026-08-25 incident class (eternal Creating,
// no reason) reports as a queryable condition.
func TestEnvtestPlatformInit_BootFailureConditionPersists(t *testing.T) {
	cfg := startEnvtest(t)
	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	r := reconcilerWithAgentdSidecar(t)

	ws := newWorkspaceForSecurity(t)
	require.NoError(t, dyn.Create(context.Background(), ws))

	pod := makeSidecarModePod(ws, "platform-init", 1,
		"init-fs: password source: read /mnt/secrets/password/password: no such file")
	pod.Namespace = "default"
	require.NoError(t, dyn.Create(context.Background(), pod))

	require.True(t, r.detectPlatformBootFailure(context.Background(), ws, pod))
	require.NoError(t, r.Status().Update(context.Background(), ws))

	fetched := &v1.Workspace{}
	require.NoError(t, dyn.Get(context.Background(), client.ObjectKey{Name: ws.Name, Namespace: ws.Namespace}, fetched))
	var cond *v1.WorkspaceCondition
	for i := range fetched.Status.Conditions {
		if fetched.Status.Conditions[i].Type == v1.WorkspaceConditionBootReady {
			cond = &fetched.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond, "BootReady must persist through the status subresource")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonPlatformBootFailed), cond.Reason)
	require.Contains(t, cond.Message, "password")
}

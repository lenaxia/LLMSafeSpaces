// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// #1758 (the #1744 maintainer addendum): the regenerable Go cache
// economy moves OFF the workspace PVC onto a dedicated disk-backed
// emptyDir that dies with the pod. Evidence: 55%→93% in minutes under
// 5 concurrent Go lanes, ~5G/hour cache regrowth, 5 manual sweeps in
// one evening — GOCACHE defaulted into $HOME/.cache/go-build and
// GOTMPDIR into /tmp, both PVC subPaths (every writable surface agents
// get is PVC-backed; agents cannot self-relocate).
//
// r2 hardening: the fixture carries InitScript so buildPod CREATES the
// workspace-setup init (the security_test.go:108-110 precedent — a
// bare newWorkspaceForSecurity pod has NO workspace-setup, and the
// init-side assertions would run vacuously), and the test REQUIRES the
// init's presence so the pin cannot silently degrade again.
func TestGoCacheEconomy_RelocatedToEmptyDir(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	ws.Spec.InitScript = "true" // buildPod only creates workspace-setup with packages/initScript
	pod, err := reconcilerPinned(t).buildPod(context.Background(), ws)
	require.NoError(t, err)
	main := mainContainerOf(t, pod)

	setup := false
	for _, c := range pod.Spec.InitContainers {
		if c.Name == "workspace-setup" {
			setup = true
		}
	}
	require.True(t, setup, "fixture must create the workspace-setup init (r2: without it the init-side assertions are vacuous)")

	// ENV: GOCACHE/GOTMPDIR point at the emptyDir, not the PVC-backed
	// defaults ($HOME/.cache/go-build, /tmp).
	for name, want := range map[string]string{
		"GOCACHE":  "/sandbox-cache/go-build",
		"GOTMPDIR": "/sandbox-cache/gotmp",
	} {
		v, ok := envVar(main, name)
		require.True(t, ok, "#1758: %s must be platform env — the Go defaults land on the PVC (the fill incident)", name)
		assert.Equal(t, want, v.Value, name)
	}

	// VOLUME: disk-backed emptyDir with the 8Gi SizeLimit. Disk-backed
	// on purpose (the addendum): build caches need capacity, not RAM —
	// unlike sandbox-cfg/sandbox-runtime which are credential tmpfs.
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "build-cache" {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "#1758: the build-cache emptyDir volume must exist")
	require.NotNil(t, vol.EmptyDir, "build-cache must be an emptyDir (dies with the pod — correct for caches)")
	assert.Empty(t, vol.EmptyDir.Medium, "disk-backed by design — Memory medium would trade the PVC for node RAM")
	require.NotNil(t, vol.EmptyDir.SizeLimit, "SizeLimit bounds the cache economy per pod")
	assert.Equal(t, resource.MustParse("8Gi"), *vol.EmptyDir.SizeLimit, "the addendum's 8Gi bound")

	// MOUNTS: both subPaths on EVERY container that runs platform
	// tooling — the main container (agent builds) and the
	// workspace-setup init (package installs hit GOCACHE too, and its
	// root filesystem is read-only: without the mount go install
	// cannot create the cache dir and the init fails).
	wantMounts := []struct{ mountPath, subPath string }{
		{"/sandbox-cache/go-build", "go-build"},
		{"/sandbox-cache/gotmp", "gotmp"},
	}
	tooling := map[string]bool{"workspace": true, "workspace-setup": true}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		if !tooling[c.Name] {
			continue
		}
		for _, w := range wantMounts {
			found := false
			for _, m := range c.VolumeMounts {
				if m.Name == "build-cache" && m.MountPath == w.mountPath {
					found = true
					assert.Equal(t, w.subPath, m.SubPath, "%s: %s subPath", c.Name, w.mountPath)
					assert.False(t, m.ReadOnly, "%s: %s must be RW (it is a cache)", c.Name, w.mountPath)
				}
			}
			assert.True(t, found, "#1758: %s must mount build-cache at %s", c.Name, w.mountPath)
		}
	}
}

// PIN (sidecar mode survives the overlay): applyAgentdSidecar mutates
// the main container (env overlay, mounts) — the build-cache mounts and
// GOCACHE/GOTMPDIR env must survive that mutation in sidecar mode too:
// the supervisor (main container) is where the engine and the agent
// tooling run.
func TestGoCacheEconomy_SurvivesAgentdSidecarOverlay(t *testing.T) {
	pod := buildPodMandatory(t, reconcilerWithAgentdSidecar(t))
	main := mainContainerOf(t, pod)

	v, ok := envVar(main, "GOCACHE")
	require.True(t, ok, "#1758: GOCACHE must survive the sidecar overlay on the main container")
	assert.Equal(t, "/sandbox-cache/go-build", v.Value)
	tmp, ok := envVar(main, "GOTMPDIR")
	require.True(t, ok, "#1758: GOTMPDIR must survive the sidecar overlay too (r2 finding 5)")
	assert.Equal(t, "/sandbox-cache/gotmp", tmp.Value)
	for _, wantPath := range []string{"/sandbox-cache/go-build", "/sandbox-cache/gotmp"} {
		found := false
		for _, m := range main.VolumeMounts {
			if m.Name == "build-cache" && m.MountPath == wantPath {
				found = true
			}
		}
		assert.True(t, found, "#1758: sidecar-mode main container must keep the %s mount", wantPath)
	}
}

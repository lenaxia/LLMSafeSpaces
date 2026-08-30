// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// The platform/ PVC subPath (Epic 69 US-69.2): the sessionstate durable seq
// cursor (ledger at S2). Three topology invariants:
//
//  1. Single-container (legacy + overlay): the main container mounts
//     /platform; the directory is created by workspace-dirs / platform-init
//     running as uid 1000 — the documented weakening (design 0055 M2:
//     crash-ambiguity protection only).
//  2. Sidecar mode: the workspace container does NOT mount it at all
//     (mount topology is the integrity control); the agentd sidecar mounts
//     it RW; a uid-2000 init creates it (ownership follows the writer).
//  3. Existing PVCs: creation is idempotent mkdir — every pod boot runs the
//     init, so pre-platform PVCs gain the directory on the next start.

func containerMount(c *corev1.Container, mountPath string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == mountPath {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

func TestPlatformSubPath_SingleContainerLegacy(t *testing.T) {
	r := reconcilerFor(t)
	pod, err := r.buildPod(context.Background(), newWorkspaceForPodBuilder(t))
	require.NoError(t, err)

	dirs := findInitContainer(pod, "workspace-dirs")
	require.NotNil(t, dirs)
	assert.Contains(t, dirs.Command[2], "/pvc/platform",
		"legacy workspace-dirs must create the platform/ subPath for existing PVCs")

	mainIdx := containerIndexByName(pod.Spec.Containers, mainContainerName)
	require.NotEqual(t, -1, mainIdx)
	main := &pod.Spec.Containers[mainIdx]
	m := containerMount(main, "/platform")
	require.NotNil(t, m, "single-container main container must mount /platform")
	assert.Equal(t, "workspace", m.Name)
	assert.Equal(t, "platform", m.SubPath)
	assert.False(t, m.ReadOnly)
}

func TestPlatformSubPath_SingleContainerOverlay(t *testing.T) {
	r := reconcilerWithAgentd(t)
	pod, err := r.buildPod(context.Background(), newWorkspaceForPodBuilder(t))
	require.NoError(t, err)

	pi := findInitContainer(pod, "platform-init")
	require.NotNil(t, pi)
	assert.Contains(t, strings.Join(pi.Args, " "), "--platform-subpath=create",
		"single-container overlay: platform-init (uid 1000) creates platform/")
	assert.Nil(t, findInitContainer(pod, "platform-dirs"),
		"platform-dirs init is sidecar-mode only")

	mainIdx := containerIndexByName(pod.Spec.Containers, mainContainerName)
	require.NotEqual(t, -1, mainIdx)
	require.NotNil(t, containerMount(&pod.Spec.Containers[mainIdx], "/platform"))
}

func TestPlatformSubPath_SidecarTopology(t *testing.T) {
	r := reconcilerWithAgentdSidecar(t)
	pod, err := r.buildPod(context.Background(), newWorkspaceForPodBuilder(t))
	require.NoError(t, err)

	// (1) The workspace container NEVER mounts platform/ in sidecar mode —
	// uid-1000 user space must not see the authority's state.
	mainIdx := containerIndexByName(pod.Spec.Containers, mainContainerName)
	require.NotEqual(t, -1, mainIdx)
	main := &pod.Spec.Containers[mainIdx]
	assert.Nil(t, containerMount(main, "/platform"),
		"sidecar mode: the workspace container must NOT mount /platform (mount-topology integrity, design 0055 M2)")
	for _, m := range main.VolumeMounts {
		assert.NotEqual(t, "platform", m.SubPath,
			"sidecar mode: platform subPath must not appear under any mount path in uid-1000 space")
	}

	// (2) The sidecar mounts it RW. The agentd sidecar is appended as the
	// last INIT container (design 0051 US-2 native sidecar pattern).
	sc := findInitContainer(pod, "agentd")
	require.NotNil(t, sc, "agentd sidecar must exist")
	m := containerMount(sc, "/platform")
	require.NotNil(t, m, "agentd sidecar must mount /platform")
	assert.Equal(t, "platform", m.SubPath)
	assert.False(t, m.ReadOnly)

	// (3) Creation: platform-init skips; a uid-2000 platform-dirs init owns it.
	pi := findInitContainer(pod, "platform-init")
	require.NotNil(t, pi)
	assert.Contains(t, strings.Join(pi.Args, " "), "--platform-subpath=skip",
		"sidecar mode: platform-init (uid 1000) must NOT create platform/ — ownership follows the writer")

	pd := findInitContainer(pod, "platform-dirs")
	require.NotNil(t, pd, "sidecar mode: the uid-2000 platform-dirs init must exist")
	require.NotNil(t, pd.SecurityContext.RunAsUser)
	assert.Equal(t, int64(2000), *pd.SecurityContext.RunAsUser,
		"platform-dirs runs as the sidecar uid so the directory is owned by the writer")
	assert.Contains(t, strings.Join(pd.Args, " "), "--platform-subpath=only")
}

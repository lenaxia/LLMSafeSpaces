// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// exchange_subpath_test.go — design 0070 §2.4a: the exchange/ PVC
// subPath (the board payload exchange dir).
//
// Why it exists: the 0070 auto-cleanup contract ("a successful board
// file-payload call deletes its payload file") was architecturally
// unsatisfiable in sidecar mode — the sidecar's /workspace mount is
// ReadOnly (design 0055 M2 integrity control, pinned by
// agentd_sidecar_pod_test.go), so agentd's os.Remove hit EROFS on
// EVERY payload file, and the one RW mount the sidecar had (/platform)
// is uid-2000-exclusive — the runtime container's write tool cannot
// create files there. NO path was both runtime-writable and
// sidecar-deletable.
//
// The fix: a purpose-scoped exchange/ subPath, dual-rw BY DESIGN —
// mounted RW in the RUNTIME container (uid 1000, where the agent's
// write tool runs) and RW in the agentd SIDECAR (uid 2000, where the
// cleanup hook runs), created by the existing platform-dirs init
// (sidecar mode) / platform-init (single-container) with mode 0770
// gid 1000 (the pod's shared group — the US-4b rt/ precedent). It
// holds ONLY short-lived board payload files.
//
// Invariants these tests lock in (the ORIGINAL gap these pin, not
// just the new feature — run against the pre-fix controller they are
// RED: no exchange mount exists anywhere):
//
//   1. Sidecar mode: exchange/ mounted RW in BOTH the runtime
//      container and the agentd sidecar, SubPath "exchange".
//   2. The integrity control is NOT weakened: the sidecar's workspace
//      mount stays ReadOnly.
//   3. Single-container mode: the main container mounts exchange/ RW
//      too (the payload contract is mode-uniform), and the platform/
//      single-container weakening is unchanged.
//   4. Creation: sidecar mode → platform-init SKIPS exchange (the
//      uid-2000 platform-dirs init creates it, ownership follows the
//      writer); single-container → platform-init creates it.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// requireExchangeMountRW asserts the dual-rw contract on one container.
func requireExchangeMountRW(t *testing.T, c *corev1.Container, who, why string) {
	t.Helper()
	m := containerMount(c, agentd.ExchangePath)
	require.NotNil(t, m, "%s must mount %s (the payload exchange dir)", who, agentd.ExchangePath)
	assert.Equal(t, "exchange", m.SubPath, "%s: the exchange subPath", who)
	assert.Equal(t, "workspace", m.Name, "%s: the exchange dir lives on the workspace PVC", who)
	assert.False(t, m.ReadOnly, "%s: exchange/ is RW — %s", who, why)
}

func TestExchangeSubPath_SidecarTopology(t *testing.T) {
	r := reconcilerWithAgentdSidecar(t)
	pod, err := r.buildPod(context.Background(), newWorkspaceForPodBuilder(t))
	require.NoError(t, err)

	// (1) Dual-rw: BOTH containers mount exchange/ RW.
	mainIdx := containerIndexByName(pod.Spec.Containers, mainContainerName)
	require.NotEqual(t, -1, mainIdx)
	requireExchangeMountRW(t, &pod.Spec.Containers[mainIdx], "the runtime container",
		"the agent's write tool creates payload files there")

	sc := findInitContainer(pod, "agentd")
	require.NotNil(t, sc, "agentd sidecar must exist")
	requireExchangeMountRW(t, sc, "the agentd sidecar",
		"the 0070 cleanup hook unlinks consumed payload files there")

	// (2) The integrity control is NOT weakened: the sidecar's
	// workspace mount stays ReadOnly (design 0055 M2), and the
	// exchange subPath is the ONLY additional PVC subtree the sidecar
	// gained.
	wsMnt := containerMount(sc, agentd.WorkspacePath)
	require.NotNil(t, wsMnt)
	assert.True(t, wsMnt.ReadOnly,
		"the user's workspace stays read-only to the sidecar — the exchange dir is a purpose-scoped carve-out, not a weakening of the M2 control")
	assert.Equal(t, "workspace", wsMnt.SubPath)

	// (3) Creation: platform-init skips; the uid-2000 platform-dirs
	// init creates it (ownership follows the writer — same contract
	// as platform/).
	pi := findInitContainer(pod, "platform-init")
	require.NotNil(t, pi)
	assert.Contains(t, strings.Join(pi.Args, " "), "--exchange-subpath=skip",
		"sidecar mode: platform-init (uid 1000) must NOT create exchange/ — the uid-2000 platform-dirs init owns it")

	pd := findInitContainer(pod, "platform-dirs")
	require.NotNil(t, pd)
	assert.Contains(t, strings.Join(pd.Args, " "), "--exchange-subpath=only",
		"sidecar mode: the platform-dirs init creates exchange/ alongside platform/")
	require.NotNil(t, pd.SecurityContext.RunAsUser)
	assert.Equal(t, int64(2000), *pd.SecurityContext.RunAsUser,
		"platform-dirs runs as the sidecar uid so the directory is owned by a uid-2000 writer (group-write carries the runtime side)")
}

func TestExchangeSubPath_SingleContainer(t *testing.T) {
	r := reconcilerWithAgentd(t) // overlay delivery on, sidecar OFF
	pod, err := r.buildPod(context.Background(), newWorkspaceForPodBuilder(t))
	require.NoError(t, err)

	mainIdx := containerIndexByName(pod.Spec.Containers, mainContainerName)
	require.NotEqual(t, -1, mainIdx)
	requireExchangeMountRW(t, &pod.Spec.Containers[mainIdx], "the runtime container",
		"the payload contract is mode-uniform (agentd runs in this container here)")

	// Single-container platform/ weakening unchanged (criterion 4:
	// single-container mode is unchanged).
	require.NotNil(t, containerMount(&pod.Spec.Containers[mainIdx], "/platform"),
		"single-container mode keeps its platform/ mount")
	assert.Nil(t, findInitContainer(pod, "platform-dirs"),
		"platform-dirs init stays sidecar-mode only")

	pi := findInitContainer(pod, "platform-init")
	require.NotNil(t, pi)
	assert.Contains(t, strings.Join(pi.Args, " "), "--exchange-subpath=create",
		"single-container mode: platform-init (uid 1000) creates exchange/ — the main container's agentd process is both writer and deleter")
}

func TestExchangeSubPath_SidecarWorkloadMountsReadOnlyGuard(t *testing.T) {
	// The threat-model pin: the exchange mount must not silently become
	// a workspace write surface. The sidecar's ONLY RW PVC mount is the
	// exchange subPath (platform/ excepted — it is uid-2000-exclusive);
	// every other workspace-volume mount in the sidecar is either RO
	// (workspace/) or not a PVC subPath at all (sandbox-runtime is
	// tmpfs).
	r := reconcilerWithAgentdSidecar(t)
	pod, err := r.buildPod(context.Background(), newWorkspaceForPodBuilder(t))
	require.NoError(t, err)

	sc := findInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	for _, m := range sc.VolumeMounts {
		if m.Name != "workspace" {
			continue
		}
		switch m.SubPath {
		case "workspace":
			assert.True(t, m.ReadOnly, "the workspace subPath stays RO in the sidecar")
		case "exchange":
			assert.False(t, m.ReadOnly, "the exchange subPath is the dual-rw carve-out")
		default:
			assert.False(t, m.ReadOnly, "unexpected RW workspace-PVC subPath %q in the sidecar — audit this against design 0055 M2", m.SubPath)
		}
	}
}

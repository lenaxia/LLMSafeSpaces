// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// platform_init_test.go — design 0051 sidecar migration, step 1: the
// platform init containers (TDD: authored before the implementation).
//
// With agentd overlay delivery enabled (#863 — mandatory for sidecar
// mode, already the production configuration), the bash init containers
// (workspace-dirs mkdir + the credential-setup heredoc) are replaced by
// platform init containers running the DIGEST-PINNED agentd image:
//
//	platform-init        → workspace-agentd init-fs
//	                       (uid-1000 PVC prep: dirs, hardened symlink
//	                       farm, password/admin-token install,
//	                       free-models copy — absorbs workspace-dirs)
//	platform-bootstrap   → workspace-agentd bootstrap   (legacy mode only)
//	platform-materialize → workspace-agentd materialize (legacy mode only)
//
// In SIDECAR mode bootstrap+materialize run inside the sidecar's boot
// phase (cmd/workspace-agentd/sidecar_boot.go) — the init sequence stops
// at platform-init, and the bootstrap-token volume mounts on the sidecar
// instead.
//
// Invariants locked here:
//
//   - No shell heredoc anywhere: each container is [binary, subcommand]
//     (the agentd delivery image may be shell-less).
//   - init-fs runs uid 1000 (PVC home owner), PVC root mounted RW at
//     /pvc, hardened security context.
//   - Legacy-no-overlay mode keeps the bash path unchanged (deleted in
//     migration step 5 when the baked binary leaves the base image).
//   - Ordering: platform-init first (it creates the subPath roots every
//     later init and the main container mount), bootstrap+materialize in
//     credential-setup's old slot.

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/require"
)

func platformInitContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// TestPlatformInit_OverlayMode_ReplacesBashInits: overlay delivery on →
// the bash init containers are gone; platform-init carries the
// subcommand form with the agentd image and uid 1000.
func TestPlatformInit_OverlayMode_ReplacesBashInits(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Nil(t, platformInitContainer(pod, "workspace-dirs"),
		"bash workspace-dirs must be absorbed by platform-init in overlay mode")
	require.Nil(t, platformInitContainer(pod, "credential-setup"),
		"bash credential-setup heredoc must be replaced in overlay mode")

	pi := platformInitContainer(pod, "platform-init")
	require.NotNil(t, pi, "platform-init must exist in overlay mode")
	require.Equal(t, testAgentdImage, pi.Image)
	require.Equal(t, []string{agentdMountPath + agentdBinaryRelPath}, pi.Command)
	require.Equal(t, []string{"init-fs", "--platform-subpath=create"}, pi.Args,
		"Epic 69 US-69.2: platform-init creates the platform/ subPath (single-container)")

	// uid 1000 = PVC home owner (symlink farm writes into uid-1000 dirs).
	require.NotNil(t, pi.SecurityContext.RunAsUser)
	require.Equal(t, int64(1000), *pi.SecurityContext.RunAsUser)
	require.True(t, *pi.SecurityContext.RunAsNonRoot)
	require.True(t, *pi.SecurityContext.ReadOnlyRootFilesystem)

	// PVC root (not subPaths — it CREATES them) at /pvc, RW.
	var pvcMount *corev1.VolumeMount
	for i := range pi.VolumeMounts {
		if pi.VolumeMounts[i].MountPath == "/pvc" {
			pvcMount = &pi.VolumeMounts[i]
		}
	}
	require.NotNil(t, pvcMount, "platform-init mounts the PVC root at /pvc")
	require.Equal(t, "workspace", pvcMount.Name)
	require.False(t, pvcMount.ReadOnly)

	// Ordering: platform-init is the FIRST init container (it creates
	// the subPath roots every later container mounts).
	require.Equal(t, "platform-init", pod.Spec.InitContainers[0].Name)
}

// TestPlatformInit_AgentdVolumeMountedEverywhere: every container whose
// Command references the overlay binary path must mount the `agentd`
// image volume — wireAgentdOverlay wires main+sidecar only, and the
// platform init containers were missed (kind L3 run 06:35 UTC 2026-08-26:
// platform-init CrashLoopBackOff, exit 128, "stat /agentd/usr/local/bin/
// workspace-agentd: no such file or directory"). Found at L3 because no
// lower layer validates mount-vs-command consistency.
func TestPlatformInit_AgentdVolumeMountedEverywhere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sidecar bool
	}{
		{"legacy overlay", false},
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

			for _, c := range pod.Spec.InitContainers {
				if len(c.Command) == 0 || !strings.HasPrefix(c.Command[0], agentdMountPath) {
					continue
				}
				require.NotNil(t, sidecarVolumeMount(&c, "agentd"),
					"%s runs the overlay binary but does not mount the agentd volume", c.Name)
			}
			for _, c := range pod.Spec.Containers {
				if len(c.Command) == 0 || !strings.HasPrefix(c.Command[0], agentdMountPath) {
					continue
				}
				require.NotNil(t, sidecarVolumeMount(&c, "agentd"),
					"%s runs the overlay binary but does not mount the agentd volume", c.Name)
			}
		})
	}
}

// TestPlatformInit_OverlayMode_FreeModelsMountedWithRelay: the catalog
// copy rides platform-init's mount set exactly when relay is on (the
// legacy freemodels tests pin the bash path; this pins the overlay one).
func TestPlatformInit_OverlayMode_FreeModelsMountedWithRelay(t *testing.T) {
	ws := newWorkspaceForSecurity(t)

	r := reconcilerWithAgentd(t)
	r.InferenceRelayURL = "http://relay.llmsafespaces.svc:8080"
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	pi := platformInitContainer(pod, "platform-init")
	require.NotNil(t, pi)
	require.NotNil(t, sidecarVolumeMount(pi, "free-models"),
		"platform-init mounts the free-models catalog when relay is on")

	r2 := reconcilerWithAgentd(t)
	pod2, err := r2.buildPod(context.Background(), ws)
	require.NoError(t, err)
	pi2 := platformInitContainer(pod2, "platform-init")
	require.NotNil(t, pi2)
	require.Nil(t, sidecarVolumeMount(pi2, "free-models"),
		"no free-models mount when relay is off (volume absent)")
}

// TestPlatformInit_LegacyOverlayMode_ChainsBootstrapMaterialize: overlay
// on, sidecar OFF → bootstrap and materialize run as their own init
// containers after platform-init (credential-setup's old slot), no
// shell, bootstrap token mounted on the bootstrap container.
func TestPlatformInit_LegacyOverlayMode_ChainsBootstrapMaterialize(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	bs := platformInitContainer(pod, "platform-bootstrap")
	require.NotNil(t, bs, "legacy overlay mode keeps bootstrap as an init container")

	mz := platformInitContainer(pod, "platform-materialize")
	require.NotNil(t, mz, "legacy overlay mode keeps materialize as an init container")
	require.Equal(t, []string{"materialize"}, mz.Args)

	// Bootstrap carries the projected SA token and drives the
	// subcommand's pinned flag contract.
	var tokMount *corev1.VolumeMount
	for i := range bs.VolumeMounts {
		if bs.VolumeMounts[i].MountPath == "/var/run/bootstrap" {
			tokMount = &bs.VolumeMounts[i]
		}
	}
	require.NotNil(t, tokMount, "platform-bootstrap mounts the projected SA token")
	require.True(t, tokMount.ReadOnly)
	require.Equal(t, []string{"bootstrap", "--workspace-id", ws.Name}, bs.Args)

	// Materialize keeps the pre-boot relay env (cold-start optimization).
	require.NotNil(t, sidecarEnvVar(mz, "XDG_DATA_HOME"),
		"platform-materialize carries XDG_DATA_HOME (preBootAuthJSONPath)")

	// Ordering: init → bootstrap → materialize, contiguously after any
	// workspace-setup init.
	names := make([]string, 0, len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	require.Equal(t, "platform-init", names[0])
	require.Equal(t, "platform-bootstrap", names[len(names)-2])
	require.Equal(t, "platform-materialize", names[len(names)-1])
}

// TestPlatformInit_SidecarMode_InitOnly_BootstrapMovesToSidecar: sidecar
// mode → no bootstrap/materialize init containers; the sidecar carries
// the projected token mount and the API URL for its boot phase.
func TestPlatformInit_SidecarMode_InitOnly_BootstrapMovesToSidecar(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Nil(t, platformInitContainer(pod, "platform-bootstrap"),
		"sidecar mode absorbs bootstrap into the sidecar boot phase")
	require.Nil(t, platformInitContainer(pod, "platform-materialize"),
		"sidecar mode absorbs materialize into the sidecar boot phase")
	require.NotNil(t, platformInitContainer(pod, "platform-init"),
		"platform-init (uid-1000 PVC prep) still runs in sidecar mode")

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)

	var tokMount *corev1.VolumeMount
	for i := range sc.VolumeMounts {
		if sc.VolumeMounts[i].MountPath == "/var/run/bootstrap" {
			tokMount = &sc.VolumeMounts[i]
		}
	}
	require.NotNil(t, tokMount, "sidecar mounts the projected SA bootstrap token")
	require.True(t, tokMount.ReadOnly)

	require.NotNil(t, sidecarEnvVar(sc, "LLMSAFESPACE_API_URL"),
		"sidecar boot phase needs the API URL")
	require.NotNil(t, sidecarEnvVar(sc, "LLMSAFESPACES_ENRICHER_CACHE_DIR"),
		"enricher cache must leave the PVC home (sidecar does not mount /home/sandbox)")

	// Cross-uid write profile (US-4b): the sidecar materializes as uid
	// 2000 while the consumers (supervisor buildEnvFrom, entrypoint
	// source, ssh, git) run as uid 1000 — outputs must be 0640 (shared
	// gid 1000 bridge).
	crossUID := sidecarEnvVar(sc, "LLMSAFESPACES_CROSS_UID_FILES")
	require.NotNil(t, crossUID, "sidecar must carry the cross-uid write profile flag")
	require.Equal(t, "1", crossUID.Value)
}

// TestPlatformInit_Step2_SupervisorCommandBypass: sidecar mode sets the
// main container Command to the overlay supervisor binary — the baked
// entrypoint (runtime-image platform logic, the Gap-2 stale-base supply)
// is bypassed entirely. Its env work moves into the pod spec:
// OPENCODE_CONFIG, XDG_DATA_HOME, the event-system flag, and the server
// password via secretKeyRef (was: file read in bash).
func TestPlatformInit_Step2_SupervisorCommandBypass(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	main := &pod.Spec.Containers[0]
	require.Equal(t, []string{agentdMountPath + agentdBinaryRelPath}, main.Command,
		"sidecar mode: main runs the overlay supervisor, not the baked entrypoint")
	require.Equal(t, []string{"supervise-opencode"}, main.Args)
	require.NotContains(t, main.Command[0], "entrypoint-opencode.sh")

	envEq := func(name, val string) {
		t.Helper()
		e := sidecarEnvVar(main, name)
		require.NotNil(t, e, "%s env must move from the entrypoint into the pod spec", name)
		require.Equal(t, val, e.Value)
	}
	// US-4b: sidecar-mode opencode reads agent-config.json from the RO
	// agentd-config volume, not /sandbox-runtime.
	envEq("OPENCODE_CONFIG", agentdConfigMountPath+"/agent-config.json")
	envEq("XDG_DATA_HOME", "/workspace/.local")
	envEq("OPENCODE_EXPERIMENTAL_EVENT_SYSTEM", "true")

	pw := sidecarEnvVar(main, "OPENCODE_SERVER_PASSWORD")
	require.NotNil(t, pw, "OPENCODE_SERVER_PASSWORD must come from the Secret now")
	require.NotNil(t, pw.ValueFrom, "password via secretKeyRef, not the old file read")
	require.Equal(t, "password", pw.ValueFrom.SecretKeyRef.Key)
	require.Equal(t, passwordSecretName(ws.Name), pw.ValueFrom.SecretKeyRef.Name)

	// Mode marker retained (identity; markers read it).
	require.NotNil(t, sidecarEnvVar(main, "AGENTD_SIDECAR_MODE"))
}

// TestPlatformInit_Step2_LegacyMode_KeepsBakedEntrypoint: sidecar OFF →
// the baked entrypoint stays (it performs verify+select, secrets-env
// sourcing, and the --supervise branch for single-container mode). It is
// the migration step-5 deletion target, unchanged until then.
func TestPlatformInit_Step2_LegacyMode_KeepsBakedEntrypoint(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t) // overlay on, sidecar off

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	main := &pod.Spec.Containers[0]
	require.Equal(t, []string{"/usr/local/bin/entrypoint-opencode.sh"}, main.Command)
	require.Nil(t, sidecarEnvVar(main, "OPENCODE_SERVER_PASSWORD"),
		"legacy mode: the entrypoint reads the password file, not the pod spec")
}

// TestPlatformInit_LegacyNoOverlay_KeepsBashPath: no overlay delivery →
// the bash init containers remain (migration step 5 deletes this path
// together with the baked binary).
func TestPlatformInit_LegacyNoOverlay_KeepsBashPath(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.NotNil(t, platformInitContainer(pod, "workspace-dirs"),
		"legacy no-overlay mode keeps the bash workspace-dirs init")
	cred := platformInitContainer(pod, "credential-setup")
	require.NotNil(t, cred, "legacy no-overlay mode keeps the bash credential-setup init")
	require.Nil(t, platformInitContainer(pod, "platform-init"),
		"no platform-init without the agentd delivery image")
}

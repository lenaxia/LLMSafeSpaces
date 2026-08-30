// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// us4b_mount_relocations_test.go — design 0051 US-4b (owner ruling
// 2026-08-21, amendment #1018): stores split by CONSUMER.
//
// Invariants these tests lock in (sidecar mode):
//
//   - Two NEW Memory-medium emptyDirs: `agentd-config` (agent-config.json
//     + allowed-dirs.json; RW sidecar, RO workspace container — integrity
//     by mount, V3) and `agentd-secrets` (secrets-env, admin-prompt.md,
//     last-reload-secrets.json; sidecar-ONLY — absent from uid-1000 space
//     by mount topology, V2).
//   - The sidecar carries the path env overrides so its writers/readers
//     (ConfigWriter, reload handler, healthz) target the relocated
//     stores; LLMSAFESPACES_CROSS_UID_FILES=1 arms the materializer's
//     cross-uid modes (rt/* re-materialized by uid 2000, consumed by
//     uid-1000 tools via the shared gid 1000).
//   - The workspace container gets OPENCODE_CONFIG=/agentd-config/
//     agent-config.json (opencode reads the config from the RO mount)
//     and NEVER sees agentd-secrets.
//   - The credential-setup init mounts both volumes RW (bootstrap writes
//     admin-prompt/allowed-dirs there; materialize writes the config base
//     + secrets-env + boot reload cache) and carries AGENTD_SIDECAR_MODE
//     so the script's sidecar branch runs (chmod 0770 rt dirs — reset()
//     unlink needs group-write; bootstrap gains the relocated --out flags).
//   - `sandbox-runtime` keeps rt/* tool-consumed (class C) and the shared
//     restart marker; rt/* modes stay 0700 in single-container mode.
//   - Disabled (default): NO new volumes, NO new mounts, NO new env —
//     single-container behavior unchanged.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

const (
	us4bAgentdConfigVolume  = "agentd-config"
	us4bAgentdSecretsVolume = "agentd-secrets"
)

func buildPodUS4B(t *testing.T, sidecar bool) *corev1.Pod {
	t.Helper()
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)
	if sidecar {
		r.AgentdSidecarEnabled = true
	}
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	return pod
}

func podVolumeByName(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// --- enabled: volumes -------------------------------------------------------

func TestUS4B_Enabled_AgentdConfigVolume(t *testing.T) {
	pod := buildPodUS4B(t, true)

	vol := podVolumeByName(pod, us4bAgentdConfigVolume)
	require.NotNil(t, vol, "agentd-config volume must exist in sidecar mode")
	require.NotNil(t, vol.EmptyDir, "agentd-config must be an emptyDir")
	require.Equal(t, corev1.StorageMediumMemory, vol.EmptyDir.Medium,
		"agentd-config must be Memory-medium (at-rest invariant, ruling: emptyDir-only)")
	require.NotNil(t, vol.EmptyDir.SizeLimit, "agentd-config needs a SizeLimit (tmpfs is RAM)")
	require.Positive(t, vol.EmptyDir.SizeLimit.Value(), "SizeLimit must be positive")
}

func TestUS4B_Enabled_AgentdSecretsVolume(t *testing.T) {
	pod := buildPodUS4B(t, true)

	vol := podVolumeByName(pod, us4bAgentdSecretsVolume)
	require.NotNil(t, vol, "agentd-secrets volume must exist in sidecar mode")
	require.NotNil(t, vol.EmptyDir, "agentd-secrets must be an emptyDir")
	require.Equal(t, corev1.StorageMediumMemory, vol.EmptyDir.Medium,
		"agentd-secrets must be Memory-medium (at-rest invariant, ruling: emptyDir-only)")
	require.NotNil(t, vol.EmptyDir.SizeLimit, "agentd-secrets needs a SizeLimit (tmpfs is RAM)")
}

// --- enabled: mount matrix --------------------------------------------------

func TestUS4B_Enabled_SidecarMountsBothRW(t *testing.T) {
	pod := buildPodUS4B(t, true)
	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)

	cfg := sidecarVolumeMount(sc, us4bAgentdConfigVolume)
	require.NotNil(t, cfg, "sidecar must mount agentd-config")
	require.Equal(t, "/agentd-config", cfg.MountPath)
	require.False(t, cfg.ReadOnly, "sidecar WRITES agent-config.json + allowed-dirs.json")

	sec := sidecarVolumeMount(sc, us4bAgentdSecretsVolume)
	require.NotNil(t, sec, "sidecar must mount agentd-secrets")
	require.Equal(t, "/agentd-secrets", sec.MountPath)
	require.False(t, sec.ReadOnly, "sidecar WRITES secrets-env/admin-prompt/reload-cache")
}

func TestUS4B_Enabled_WorkspaceMountConfigRO_NoSecrets(t *testing.T) {
	pod := buildPodUS4B(t, true)
	main := &pod.Spec.Containers[0]

	cfg := sidecarVolumeMount(main, us4bAgentdConfigVolume)
	require.NotNil(t, cfg, "workspace container must mount agentd-config (opencode reads it)")
	require.Equal(t, "/agentd-config", cfg.MountPath)
	require.True(t, cfg.ReadOnly,
		"agent-config.json integrity is a MOUNT fact (V3: rename-over impossible)")

	require.Nil(t, sidecarVolumeMount(main, us4bAgentdSecretsVolume),
		"agentd-secrets must NEVER be mounted in the workspace container (V2)")
}

func TestUS4B_Enabled_SandboxRuntimeStillShared(t *testing.T) {
	pod := buildPodUS4B(t, true)
	main := &pod.Spec.Containers[0]
	sc := sidecarInitContainer(pod, "agentd")

	for _, c := range []*corev1.Container{main, sc} {
		m := sidecarVolumeMount(c, "sandbox-runtime")
		require.NotNil(t, m, "%s keeps the shared sandbox-runtime (rt/* class C + marker)", c.Name)
		require.False(t, m.ReadOnly, "%s needs sandbox-runtime RW", c.Name)
	}
}

// --- enabled: env overrides -------------------------------------------------

func TestUS4B_Enabled_SidecarPathEnv(t *testing.T) {
	pod := buildPodUS4B(t, true)
	sc := sidecarInitContainer(pod, "agentd")

	expect := map[string]string{
		"LLMSAFESPACES_AGENT_CONFIG_PATH": "/agentd-config/agent-config.json",
		"LLMSAFESPACES_SECRETS_ENV_PATH":  "/agentd-secrets/secrets-env",
		"LLMSAFESPACES_RELOAD_CACHE_PATH": "/agentd-secrets/last-reload-secrets.json",
		"LLMSAFESPACES_ADMIN_PROMPT_PATH": "/agentd-secrets/admin-prompt.md",
		"LLMSAFESPACES_ALLOWED_DIRS_PATH": "/agentd-config/allowed-dirs.json",
		"LLMSAFESPACES_CROSS_UID_FILES":   "1",
	}
	for name, val := range expect {
		ev := sidecarEnvVar(sc, name)
		require.NotNil(t, ev, "sidecar env must carry %s", name)
		require.Equal(t, val, ev.Value, "%s", name)
	}
}

func TestUS4B_Enabled_WorkspaceOpenCodeConfigEnv(t *testing.T) {
	pod := buildPodUS4B(t, true)
	main := &pod.Spec.Containers[0]

	ev := sidecarEnvVar(main, "OPENCODE_CONFIG")
	require.NotNil(t, ev, "workspace container must point opencode at the relocated config")
	require.Equal(t, "/agentd-config/agent-config.json", ev.Value)
}

func TestUS4B_Enabled_CredentialSetupInitSidecarEnv(t *testing.T) {
	// Sidecar-migration step 1: overlay+sidecar pods have NO bash
	// credential-setup init (platform-init + the sidecar's boot phase own
	// boot). The successor surface for the US-4b relocation contract is
	// the SIDECAR container's own env — this pins it there.
	pod := buildPodUS4B(t, true)
	require.Nil(t, sidecarInitContainer(pod, "credential-setup"),
		"overlay+sidecar pods run platform-init, not the bash cred script")
	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)

	expect := map[string]string{
		"LLMSAFESPACES_AGENT_CONFIG_PATH": "/agentd-config/agent-config.json",
		"LLMSAFESPACES_SECRETS_ENV_PATH":  "/agentd-secrets/secrets-env",
		"LLMSAFESPACES_RELOAD_CACHE_PATH": "/agentd-secrets/last-reload-secrets.json",
		// The boot files the sidecar materializes are read across the
		// uid split → 0640 at materialize.
		"LLMSAFESPACES_CROSS_UID_FILES": "1",
	}
	for name, val := range expect {
		ev := sidecarEnvVar(sc, name)
		require.NotNil(t, ev, "sidecar env must carry %s", name)
		require.Equal(t, val, ev.Value, "%s", name)
	}
	for _, vol := range []string{us4bAgentdConfigVolume, us4bAgentdSecretsVolume} {
		m := sidecarVolumeMount(sc, vol)
		require.NotNil(t, m, "sidecar must mount %s (its boot phase writes there)", vol)
		require.False(t, m.ReadOnly, "sidecar writes %s", vol)
	}
}

// --- enabled: script branch -------------------------------------------------

func credSetupScriptFor(t *testing.T, sidecar bool) string {
	t.Helper()
	// Sidecar-migration step 1: the bash cred script exists only in
	// legacy-no-overlay pods now (overlay pods run platform-init); the
	// script branch pins below validate the SCRIPT, so extract from a
	// legacy pod regardless of the sidecar parameter.
	pod := buildPodUS4B(t, sidecar)
	var src *corev1.Container
	if cred := sidecarInitContainer(pod, "credential-setup"); cred != nil {
		src = cred
	} else {
		// Overlay pod: build a legacy pod for the script text.
		ws := newWorkspaceForSecurity(t)
		r := reconcilerFor(t)
		legacy, err := r.buildPod(context.Background(), ws)
		require.NoError(t, err)
		src = sidecarInitContainer(legacy, "credential-setup")
	}
	require.NotNil(t, src)
	require.Len(t, src.Command, 3)
	return src.Command[2]
}

// --- disabled: single-container regression pins ------------------------------

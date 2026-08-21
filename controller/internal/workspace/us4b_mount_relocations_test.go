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
	pod := buildPodUS4B(t, true)
	cred := sidecarInitContainer(pod, "credential-setup")
	require.NotNil(t, cred)

	expect := map[string]string{
		"AGENTD_SIDECAR_MODE":             "1",
		"LLMSAFESPACES_AGENT_CONFIG_PATH": "/agentd-config/agent-config.json",
		"LLMSAFESPACES_SECRETS_ENV_PATH":  "/agentd-secrets/secrets-env",
		"LLMSAFESPACES_RELOAD_CACHE_PATH": "/agentd-secrets/last-reload-secrets.json",
		// The init's boot files (secrets-env, reload cache) are read by
		// the uid-2000 sidecar across the split → 0640 at materialize.
		"LLMSAFESPACES_CROSS_UID_FILES": "1",
	}
	for name, val := range expect {
		ev := sidecarEnvVar(cred, name)
		require.NotNil(t, ev, "credential-setup env must carry %s in sidecar mode", name)
		require.Equal(t, val, ev.Value, "%s", name)
	}

	for _, vol := range []string{us4bAgentdConfigVolume, us4bAgentdSecretsVolume} {
		m := sidecarVolumeMount(cred, vol)
		require.NotNil(t, m, "credential-setup must mount %s (bootstrap/materialize write there)", vol)
		require.False(t, m.ReadOnly, "credential-setup writes %s", vol)
	}
}

// --- enabled: script branch -------------------------------------------------

func credSetupScriptFor(t *testing.T, sidecar bool) string {
	t.Helper()
	pod := buildPodUS4B(t, sidecar)
	cred := sidecarInitContainer(pod, "credential-setup")
	require.NotNil(t, cred)
	require.Len(t, cred.Command, 3)
	return cred.Command[2]
}

func TestUS4B_Enabled_CredScriptSidecarBranch(t *testing.T) {
	script := credSetupScriptFor(t, true)

	require.Contains(t, script, "chmod 0770 /sandbox-runtime/rt",
		"sidecar mode must group-write rt/ so the uid-2000 sidecar's reset() can unlink")
	require.Contains(t, script, "--admin-prompt-out /agentd-secrets/admin-prompt.md",
		"bootstrap must write admin-prompt.md to the sidecar-only volume")
	require.Contains(t, script, "--allowed-dirs-out /agentd-config/allowed-dirs.json",
		"bootstrap must write allowed-dirs.json to agentd-config")
}

// --- disabled: single-container regression pins ------------------------------

func TestUS4B_Disabled_NoRelocations(t *testing.T) {
	pod := buildPodUS4B(t, false)

	require.Nil(t, podVolumeByName(pod, us4bAgentdConfigVolume), "no agentd-config volume when disabled")
	require.Nil(t, podVolumeByName(pod, us4bAgentdSecretsVolume), "no agentd-secrets volume when disabled")

	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		require.Nil(t, sidecarVolumeMount(c, us4bAgentdConfigVolume), "init %s must not mount agentd-config when disabled", c.Name)
		require.Nil(t, sidecarVolumeMount(c, us4bAgentdSecretsVolume), "init %s must not mount agentd-secrets when disabled", c.Name)
		require.Nil(t, sidecarEnvVar(c, "AGENTD_SIDECAR_MODE"), "init %s must not carry sidecar-mode env when disabled", c.Name)
		require.Nil(t, sidecarEnvVar(c, "LLMSAFESPACES_AGENT_CONFIG_PATH"), "init %s must not carry relocation env when disabled", c.Name)
	}
	main := &pod.Spec.Containers[0]
	require.Nil(t, sidecarVolumeMount(main, us4bAgentdConfigVolume), "main must not mount agentd-config when disabled")
	require.Nil(t, sidecarEnvVar(main, "OPENCODE_CONFIG"),
		"single-container mode must not set OPENCODE_CONFIG (entrypoint default stands)")
}

func TestUS4B_Disabled_CredScriptKeepsDefaultModes(t *testing.T) {
	script := credSetupScriptFor(t, false)

	// The guarded branch may exist in the generated text, but the default
	// path must keep 0700 rt dirs (uid-1000-only writers) and the bare
	// bootstrap invocation. exec-level behavior pins live in
	// us4b_cred_script_exec_test.go.
	require.Contains(t, script, "chmod 700 /sandbox-runtime/rt/ssh /sandbox-runtime/rt/secrets",
		"single-container mode keeps rt dirs 0700")
	require.Contains(t, script,
		"else\n  workspace-agentd bootstrap --workspace-id \"$WORKSPACE_ID\" --api-url \"$LLMSAFESPACE_API_URL\"\nfi",
		"the default (non-sidecar) bootstrap branch stays the bare, unchanged invocation")
}

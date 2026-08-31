// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// cross_uid_boot_matrix_test.go — US-70.1 R3 (design 0057): the standing
// cross-uid credential matrix for the POD-SPEC side of the boot path.
//
// Rule (epic #1158, normative): every credential/file crossing uids in
// the boot path is enumerated here — writer uid, reader uid, medium,
// expected outcome. A new crossing is ADDED to the matrix, never fixed
// ad hoc. The agentd-side file-mode rows live in
// cmd/workspace-agentd/cross_uid_boot_matrix_test.go (the CROSS_UID_FILES
// machinery); this file owns the pod wiring: which container holds which
// credential and which volumes reach uid-1000 space.
//
// A2 (validated here): the credential the supervisor presents at the
// spawn-time pull — the §D1 carve-out workspace password — is wired into
// the MAIN container env by the controller (OPENCODE_SERVER_PASSWORD),
// i.e. readable by the supervisor's uid at spawn time by construction.
// The §D1 control-plane credential (agentdPassword) and the admin bearer
// stay OUT of uid-1000 space entirely (design 0051 D1 — that invariant
// outranks the literal A2 wording, which predates the credential choice;
// see the worklog's assumptions register).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestCrossUIDBootMatrix_PodSpec(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	main := &pod.Spec.Containers[0]
	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)

	t.Run("A2 pull credential: OPENCODE_SERVER_PASSWORD in main env (uid-1000, env-only)", func(t *testing.T) {
		pw := sidecarEnvVar(main, "OPENCODE_SERVER_PASSWORD")
		require.NotNil(t, pw, "the supervisor's spawn-time pull credential must be wired into the main container env")
		require.NotNil(t, pw.ValueFrom, "delivered via Secret reference, never inline")
		require.Equal(t, "password", pw.ValueFrom.SecretKeyRef.Key,
			"the §D1 carve-out workspace password — the same uid-1000-legitimate credential class as /v1/mcp")
	})

	t.Run("§D1 agentdPassword: sidecar env-only, never in uid-1000 space", func(t *testing.T) {
		require.NotNil(t, sidecarEnvVar(sc, "AGENTD_CONTROL_PLANE_PASSWORD"),
			"the sidecar holds the control-plane credential (env, uid-2000 space)")
		require.Nil(t, sidecarEnvVar(main, "AGENTD_CONTROL_PLANE_PASSWORD"),
			"the control-plane credential must NEVER reach the main container (design 0051 D1)")
	})

	t.Run("admin bearer: sidecar env-only, never in uid-1000 space", func(t *testing.T) {
		require.NotNil(t, sidecarEnvVar(sc, "AGENTD_ADMIN_TOKEN"))
		require.Nil(t, sidecarEnvVar(main, "AGENTD_ADMIN_TOKEN"))
		require.Nil(t, sidecarEnvVar(main, "AGENTD_ADMIN_TOKEN_FILE"),
			"the /sandbox-cfg/admin-token file is uid-1000-owned 0400 and deliberately not wired to the main container in sidecar mode")
	})

	t.Run("sidecar workspace password: env-only (the /sandbox-cfg/password 0600/uid-1000 file is unreadable at uid 2000)", func(t *testing.T) {
		require.NotNil(t, sidecarEnvVar(sc, "AGENTD_SIDECAR_PASSWORD"),
			"the sidecar's copy of the workspace password arrives via env — the 0600 uid-1000 file is denied cross-uid")
	})

	t.Run("agentd-secrets volume (staged secrets-env, reload cache, admin prompt): NEVER mounted in uid-1000 space", func(t *testing.T) {
		require.Nil(t, sidecarVolumeMount(main, "agentd-secrets"),
			"uid-1000 must not read the staged delta store or any sidecar-secret artifact (I9)")
		require.NotNil(t, sidecarVolumeMount(sc, "agentd-secrets"),
			"the sidecar owns the store")
	})

	t.Run("agentd-config: RO in the main container (opencode reads, never writes)", func(t *testing.T) {
		m := sidecarVolumeMount(main, "agentd-config")
		require.NotNil(t, m)
		require.True(t, m.ReadOnly, "integrity by mount (US-4b): uid-1000 gets read-only agent-config.json")
	})

	t.Run("sandbox-cfg: RO for the sidecar (bootstrap artifacts are uid-1000-owned)", func(t *testing.T) {
		m := sidecarVolumeMount(sc, "sandbox-cfg")
		require.NotNil(t, m)
		require.True(t, m.ReadOnly)
	})
}

// TestCrossUIDBootMatrix_MainContainerCredentialSet pins the exact
// credential surface of the main container (uid-1000 space): the pull
// credential and the platform env ONLY — anything beyond the carve-out
// set appearing here is a matrix breach.
func TestCrossUIDBootMatrix_MainContainerCredentialSet(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentdSidecar(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	main := &pod.Spec.Containers[0]

	banned := []string{
		"AGENTD_CONTROL_PLANE_PASSWORD",
		"AGENTD_ADMIN_TOKEN",
		"AGENTD_ADMIN_TOKEN_FILE",
		"AGENTD_SIDECAR_PASSWORD",
	}
	for _, name := range banned {
		require.Nil(t, sidecarEnvVar(main, name),
			"uid-1000 space must never hold %s (design 0051 D1)", name)
	}
	require.NotNil(t, sidecarEnvVar(main, "OPENCODE_SERVER_PASSWORD"))
}

// TestCrossUIDBootMatrix_SidecarAdminTokenEnvOnly: in sidecar mode the
// sidecar's env is the SOLE bearer channel. Two structural facts pin
// that: (a) the bash credential-setup init (the only admin-token FILE
// installer) is not built for sidecar pods — overlay delivery is
// mandatory for the sidecar, and the overlay path defers boot to the
// sidecar; (b) the base builder's env wiring for the main container
// (env mode for legacy Secrets, AGENTD_ADMIN_TOKEN_FILE for distinct
// tokens) is stripped by applyAgentdSidecar — uid-1000 space gets
// neither the env nor a file reference.
func TestCrossUIDBootMatrix_SidecarAdminTokenEnvOnly(t *testing.T) {
	sec := makePasswordSecret("ws-matrix-token", "default")
	sec.Data["admin-token"] = []byte("distinct-admin-token")

	ws := makeWorkspace(sec.Name[len("workspace-pw-"):], "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-" + ws.Name
	ws.Spec.Runtime = "base"

	r := reconcilerFor(t, sec, makeBoundPVC(ws.Status.PVCName, "default", ws.UID), makeRuntimeEnv("base"))
	r.AgentdImage = testAgentdImage
	r.AgentdBinarySHA256AMD64 = testAgentdSHAAMD64
	r.AgentdBinarySHA256ARM64 = testAgentdSHAARM64
	r.AgentdSidecarEnabled = true

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Nil(t, initContainerByName(pod, credentialSetupContainerName),
		"sidecar pods must not build the bash credential-setup init — the only admin-token FILE installer")

	sc := sidecarInitContainer(pod, "agentd")
	require.NotNil(t, sc)
	require.NotNil(t, sidecarEnvVar(sc, "AGENTD_ADMIN_TOKEN"),
		"the sidecar's env delivery is the sole bearer channel in sidecar mode")

	main := &pod.Spec.Containers[0]
	require.Nil(t, sidecarEnvVar(main, "AGENTD_ADMIN_TOKEN_FILE"),
		"the distinct-token file reference must be stripped from uid-1000 space (R3 matrix finding)")
	require.Nil(t, sidecarEnvVar(main, "AGENTD_ADMIN_TOKEN"))
}

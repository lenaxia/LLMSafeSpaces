// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// platform_init.go — design 0051 sidecar migration, step 1: the
// platform init containers, run from the digest-pinned agentd image
// (#863 overlay delivery) instead of the runtime base.
//
// Replaces, in overlay mode, BOTH bash init containers:
//
//   - workspace-dirs (mkdir -p /pvc/{workspace,home,tmp}) → platform-init
//   - credential-setup (the heredoc: symlink farm, password install,
//     free-models copy, bootstrap, materialize) → platform-init for the
//     filesystem half; platform-bootstrap + platform-materialize for the
//     credential half in legacy single-container mode, or the SIDECAR's
//     boot phase (cmd/workspace-agentd/sidecar_boot.go) in sidecar mode.
//
// Motivation (incident 2026-08-25): the bash heredoc executed
// /bin/sh and workspace-agentd from the RUNTIME image — a user-plane
// artifact on its own release cadence. A factory-built base carrying a
// pre-#871 agentd crash-looped Init:Error on contract-shape MCP
// metadata. Platform boot logic now ships in the platform artifact;
// base-image staleness can only yield an old Python, never a boot
// crash.
//
// Shape: every container is [binary, subcommand] — no shell anywhere,
// so the delivery image may be shell-less. uid 1000 (the PVC home
// owner); hardened security context identical to the inits it replaces.

import (
	corev1 "k8s.io/api/core/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// buildPlatformInit returns the init-fs container: uid-1000 PVC prep
// (subPath roots, hardened symlink farm, password/admin-token install,
// free-models copy — see cmd/workspace-agentd/init_fs.go).
func (r *WorkspaceReconciler) buildPlatformInit(relayOn bool) corev1.Container {
	trueVal := true
	falseVal := false
	uid1000 := int64(1000)

	mounts := []corev1.VolumeMount{
		// The overlay binary itself (image volume; Command references
		// agentdMountPath — kind L3 2026-08-26 caught its absence).
		{Name: "agentd", MountPath: agentdMountPath, ReadOnly: true},
		// PVC ROOT (not subPaths — this container creates them).
		{Name: "workspace", MountPath: "/pvc"},
		{Name: "sandbox-cfg", MountPath: "/sandbox-cfg"},
		{Name: "sandbox-runtime", MountPath: "/sandbox-runtime"},
		{Name: "pw-secret", MountPath: "/mnt/secrets/password", ReadOnly: true},
	}
	if relayOn {
		mounts = append(mounts, corev1.VolumeMount{
			Name: "free-models", MountPath: "/mnt/freemodels", ReadOnly: true,
		})
	}

	return corev1.Container{
		Name:    "platform-init",
		Image:   r.AgentdImage,
		Command: []string{agentdMountPath + agentdBinaryRelPath},
		Args:    []string{"init-fs"},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			RunAsUser:                &uid1000,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}

// buildPlatformBootstrap returns the legacy-mode bootstrap init: fetch
// decrypted secrets from the API with the projected SA token. In
// sidecar mode this runs inside the sidecar's boot phase instead
// (sidecar_boot.go) — buildPod omits the container there.
func (r *WorkspaceReconciler) buildPlatformBootstrap(workspace *v1.Workspace) corev1.Container {
	trueVal := true
	falseVal := false
	uid1000 := int64(1000)

	return corev1.Container{
		Name:    "platform-bootstrap",
		Image:   r.AgentdImage,
		Command: []string{agentdMountPath + agentdBinaryRelPath},
		// Subcommand contract is pinned by cmd/workspace-agentd tests:
		// --workspace-id is a required flag (no env fallback);
		// --api-url falls back to LLMSAFESPACE_API_URL, passed as env
		// so the spec carries one coordinate for it.
		Args: []string{"bootstrap", "--workspace-id", workspace.Name},
		Env: []corev1.EnvVar{
			{Name: "LLMSAFESPACE_API_URL", Value: r.APIServiceURL},
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			RunAsUser:                &uid1000,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "agentd", MountPath: agentdMountPath, ReadOnly: true},
			{Name: "sandbox-cfg", MountPath: "/sandbox-cfg"},
			{Name: "sandbox-runtime", MountPath: "/sandbox-runtime"},
			{Name: "bootstrap-token", MountPath: "/var/run/bootstrap", ReadOnly: true},
		},
	}
}

// buildPlatformMaterialize returns the legacy-mode materialize init:
// apply the fetched batch to the tmpfs credential tree and pre-render
// the relay provider block before opencode boots.
func (r *WorkspaceReconciler) buildPlatformMaterialize(relayBaseURL string) corev1.Container {
	trueVal := true
	falseVal := false
	uid1000 := int64(1000)

	env := []corev1.EnvVar{
		// #401 review fix: XDG_DATA_HOME must match the main container's
		// so preBootAuthJSONPath reads the same symlink opencode will.
		{Name: "XDG_DATA_HOME", Value: "/workspace/.local"},
	}
	if relayBaseURL != "" {
		env = append(env, corev1.EnvVar{Name: "INFERENCE_RELAY_BASEURL", Value: relayBaseURL})
	}

	return corev1.Container{
		Name:    "platform-materialize",
		Image:   r.AgentdImage,
		Command: []string{agentdMountPath + agentdBinaryRelPath},
		Args:    []string{"materialize"},
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			RunAsUser:                &uid1000,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "agentd", MountPath: agentdMountPath, ReadOnly: true},
			{Name: "sandbox-cfg", MountPath: "/sandbox-cfg"},
			{Name: "sandbox-runtime", MountPath: "/sandbox-runtime"},
			{Name: "workspace", MountPath: "/workspace", SubPath: "workspace"},
			{Name: "workspace", MountPath: "/home/sandbox", SubPath: "home"},
		},
	}
}

// buildPasswordSecretVolume is the pw-secret volume shared by the
// legacy credential-setup init and the overlay platform-init.
func buildPasswordSecretVolume(workspace *v1.Workspace) corev1.Volume {
	return corev1.Volume{
		Name: "pw-secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: passwordSecretName(workspace.Name)},
		},
	}
}

// buildBootstrapTokenVolume is the projected per-workspace SA token
// (audience-scoped, 600s) consumed by bootstrap — in the legacy init or
// on the sidecar in sidecar mode.
func buildBootstrapTokenVolume() corev1.Volume {
	tokenTTL := int64(600)
	return corev1.Volume{
		Name: "bootstrap-token",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path:              "token",
						ExpirationSeconds: &tokenTTL,
						Audience:          bootstrapAudience,
					},
				}},
			},
		},
	}
}

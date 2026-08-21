// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_sidecar.go — US-2 (design 0051 §D1/§4a): native-sidecar split.
//
// When AgentdSidecarEnabled, buildPod appends an `agentd` native sidecar
// (init container with restartPolicy Always — KEP-753, chart floor 1.35)
// running the SAME digest-pinned agentd image (#863: single-artifact
// provenance) with `--sidecar`, at uid 2000 / gid 1000 (gid 1000 = the
// workspace group: shared-file reads across the split stay possible).
//
// Ordering (the #857 stamp-before-opencode-reads guarantee): the sidecar
// is the LAST init container, after credential-setup's materialize. A
// native sidecar's startup probe gates the main container — opencode
// starts only after the sidecar is serving, and the sidecar serves only
// after ensureBootAgentConfig stamped the platform blocks (the stamp is
// synchronous before its muxes). Net effect: opencode's first config
// read observes the completed config, exactly as in single-container
// mode where main() stamps before startManagedProcess.
//
// Credentials cross via the sidecar's own ENV (uid-2000 space): the
// 0600/0400 uid-1000 files under /sandbox-cfg are unreadable cross-uid.
// Env delivery is safe in THIS container because the sidecar spawns no
// children — the no-env rule exists for opencode's env inheritance, which
// does not apply here. US-3 swaps the password key for the distinct
// agentdPassword; US-2 keeps the workspace password.

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	agentdSidecarContainerName = "agentd"
	// SidecarRestartMarkerEnv wires LLMSAFESPACES_RESTART_MARKER_PATH on
	// both containers so the sidecar's and the supervisor's marker writes
	// land on one cross-uid-readable file (0640 + shared gid 1000).
	SidecarRestartMarkerEnv = agentd.SidecarRestartMarkerPath
)

// ValidateAgentdSidecar is the exported startup guard used by controller
// main. Wraps validateAgentdSidecarConfig.
func ValidateAgentdSidecar(sidecarEnabled bool, agentdImage string) error {
	return validateAgentdSidecarConfig(sidecarEnabled, agentdImage)
}

// validateAgentdSidecarConfig enforces the startup contract: the sidecar
// runs the delivery image, so enabling the split without #863 delivery
// configured is a startup error, not a runtime pod failure.
func validateAgentdSidecarConfig(sidecarEnabled bool, agentdImage string) error {
	if !sidecarEnabled {
		return nil
	}
	if agentdImage == "" {
		return fmt.Errorf("agentdSidecar: --agentd-sidecar requires --agentd-image (the sidecar runs the digest-pinned delivery artifact; baked-in delivery cannot supply the second container)")
	}
	return nil
}

// buildAgentdSidecarContainer returns the native sidecar container.
// adminToken is the resolved admin-mux bearer (empty when the Secret
// predates the #933 upsert): the readiness probe must carry it because
// the sidecar's /v1/readyz is bearer-gated whenever the token exists.
// Called only when AgentdSidecarEnabled (validated at startup).
func (r *WorkspaceReconciler) buildAgentdSidecarContainer(workspace *v1.Workspace, adminToken string) corev1.Container {
	trueVal := true
	falseVal := false
	uid2000 := int64(2000)
	gid1000 := int64(1000)

	env := []corev1.EnvVar{
		{Name: "WORKSPACE_ID", Value: workspace.Name},
		// Credentials via env only — uid-2000 space (see file header).
		{Name: "AGENTD_SIDECAR_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: passwordSecretName(workspace.Name)},
				Key:                  "password",
			},
		}},
		{Name: "AGENTD_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: passwordSecretName(workspace.Name)},
				Key:                  "admin-token",
			},
		}},
		// US-3 (§D1 per-endpoint table): the control-plane Basic secret —
		// reload-secrets, agent/reload, workflow/* accept this OR the
		// workspace password (mixed-generation window). Delivered env-only
		// to the SIDECAR (uid-2000 space, no child inherits it); the main
		// container is deliberately NOT wired — this secret must never
		// exist in uid-1000 space. Upsert-once in ensurePasswordSecret
		// guarantees the key before any sidecar build (Q3).
		{Name: "AGENTD_CONTROL_PLANE_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: passwordSecretName(workspace.Name)},
				Key:                  "agentdPassword",
			},
		}},
		// Shared restart-reason marker (cross-uid file, 0640, gid 1000).
		{Name: "LLMSAFESPACES_RESTART_MARKER_PATH", Value: SidecarRestartMarkerEnv},
		// auth.json discovery must match the main container's XDG home.
		{Name: "XDG_DATA_HOME", Value: "/workspace/.local"},
	}
	if r.InferenceRelayURL != "" {
		env = append(env, corev1.EnvVar{Name: "INFERENCE_RELAY_BASEURL", Value: r.InferenceRelayURL})
	}

	return corev1.Container{
		Name:           agentdSidecarContainerName,
		Image:          r.AgentdImage,
		Command:        []string{agentdMountPath + agentdBinaryRelPath, "--sidecar"},
		RestartPolicy:  &alwaysRestart,
		Env:            env,
		StartupProbe:   sidecarBootProbe(),
		LivenessProbe:  sidecarLivenessProbe(),
		ReadinessProbe: sidecarReadinessProbe(adminToken),
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			RunAsUser:                &uid2000,
			RunAsGroup:               &gid1000,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "agentd", MountPath: agentdMountPath, ReadOnly: true},
			{Name: "sandbox-cfg", MountPath: "/sandbox-cfg", ReadOnly: true},
			{Name: "sandbox-runtime", MountPath: "/sandbox-runtime"},
			{Name: "workspace", MountPath: agentd.WorkspacePath, SubPath: "workspace", ReadOnly: true},
		},
	}
}

// alwaysRestart is the KEP-753 marker making an init container a native
// sidecar: it stays running for the pod's lifetime and does not gate
// later init containers on EXIT — the startup probe below does the
// gating instead.
var alwaysRestart = corev1.ContainerRestartPolicyAlways

// sidecarBootProbe is the #857 ordering gate: the main container cannot
// start until the sidecar answers healthz, and the sidecar answers only
// after the boot-time agent-config stamp. Budget mirrors the main
// container's startup probe (5s × 36 = 3min) — the stamp reads files on
// tmpfs and is fast, but the sidecar image pull counts against this on a
// cold node.
func sidecarBootProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/v1/healthz",
				Port: intstr.FromInt(agentd.AgentdAdminPort),
			},
		},
		InitialDelaySeconds: 2, PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 36,
	}
}

func sidecarLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/v1/healthz",
				Port: intstr.FromInt(agentd.AgentdAdminPort),
			},
		},
		// A wedged sidecar restarts the SIDECAR only — the workspace
		// container (opencode + supervisor) keeps running.
		InitialDelaySeconds: 15, PeriodSeconds: 10, TimeoutSeconds: 10, FailureThreshold: 8,
	}
}

func sidecarReadinessProbe(adminToken string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/v1/readyz",
				Port: intstr.FromInt(agentd.AgentdAdminPort),
				// F1.4.2 parity with the main container's probes: readyz
				// is bearer-gated when the admin token is resolved (#933).
				// Without the header the probe 401s FOREVER — the sidecar
				// runs (startup/liveness use the open /v1/healthz) but the
				// pod never goes Ready. Found by the L3 kind run
				// (32421411899): sidecar gate readyz_first_200 fired at
				// +9s while kubelet's probe kept failing.
				HTTPHeaders: func() []corev1.HTTPHeader {
					if adminToken == "" {
						return nil
					}
					return []corev1.HTTPHeader{
						{Name: "Authorization", Value: "Bearer " + adminToken},
					}
				}(),
			},
		},
		InitialDelaySeconds: 2, PeriodSeconds: 5, TimeoutSeconds: 5, FailureThreshold: 12,
	}
}

// applyAgentdSidecar mutates the pod for sidecar mode: appends the
// native sidecar as the last init container and switches the main
// container to supervisor mode (entrypoint branch env + kernel-level
// TCP liveness). adminToken is buildPod's resolved admin-mux bearer
// (empty for legacy Secrets) — the sidecar's readiness probe needs it.
// No-op when disabled.
func (r *WorkspaceReconciler) applyAgentdSidecar(pod *corev1.Pod, workspace *v1.Workspace, adminToken string) {
	if !r.AgentdSidecarEnabled {
		return
	}

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, r.buildAgentdSidecarContainer(workspace, adminToken))

	main := &pod.Spec.Containers[0]
	main.Env = append(main.Env,
		corev1.EnvVar{Name: "AGENTD_SIDECAR_MODE", Value: "1"},
		// The supervisor's crash-path and socket-restart markers land on
		// the same cross-uid-readable file the sidecar reads at boot.
		corev1.EnvVar{Name: "LLMSAFESPACES_RESTART_MARKER_PATH", Value: SidecarRestartMarkerEnv},
	)
	// Liveness in sidecar mode: HTTP healthz is served by the SIDECAR
	// (shared netns) — pointing the WORKSPACE container's liveness at it
	// would restart opencode+supervisor whenever the sidecar wedges,
	// which is precisely backwards. TCP on opencode's own port is the
	// kernel-level answer for "is the supervised child alive": refused
	// beyond the startup budget = restart the workspace container.
	main.LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt(agentd.AgentPort),
			},
		},
		InitialDelaySeconds: 15, PeriodSeconds: 10, TimeoutSeconds: 10, FailureThreshold: 8,
	}
}

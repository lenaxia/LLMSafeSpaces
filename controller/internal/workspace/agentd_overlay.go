// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd overlay delivery (#863).
//
// workspace-agentd ships as a digest-pinned image volume mounted read-only
// at /agentd, replacing the binary baked into runtimes/base. This keeps
// "the image is the workspace" (user deps + distro stay in the factory
// image) while platform-internal code delivers on platform cadence: any
// pod (re)creation — launch, resume, controller restart — pulls the
// current pinned digest.
//
// Security model: the mount MUST be readOnly. Init containers run once per
// pod, not per container; an RW mount would let the main container
// overwrite the binary and have the tampered copy executed on the next
// container restart within the same pod. The entrypoint additionally
// verifies the binary's sha256 against the pod-spec env pins (immutable
// post-create) and refuses to exec on mismatch — no silent fallback, which
// would be a downgrade attack.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Entrypoint exit codes (runtimes/base/tools/entrypoints/entrypoint-common.sh).
// Distinct from generic failures so the controller can distinguish agentd
// integrity outcomes from any other crash.
const (
	agentdExitVerifyFailed   int32 = 81
	agentdExitOverlayMissing int32 = 82
)

const (
	agentdVolumeName    = "agentd"
	agentdMountPath     = "/agentd"
	agentdBinaryRelPath = "/usr/local/bin/workspace-agentd"
	agentdOverlayEnvKey = "AGENTD_IMAGE_VOLUME"
)

// podHasAgentdOverlay reports whether this specific pod was built with
// the overlay wiring (the controller sets AGENTD_IMAGE_VOLUME=1 only in
// the same branch that adds the volume + verify pins). Pre-existing
// pods from before delivery was enabled do not carry it.
func podHasAgentdOverlay(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != "workspace" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == agentdOverlayEnvKey && e.Value == "1" {
				return true
			}
		}
	}
	return false
}

var (
	metricsAgentdVerifyFailures = metrics.WorkspaceAgentdVerifyFailuresTotal
	sha256HexRe                 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestRe                    = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// agentdOverlayEnabled reports whether image-volume agentd delivery is on.
func (r *WorkspaceReconciler) agentdOverlayEnabled() bool {
	return r.AgentdImage != ""
}

// wireAgentdOverlay adds the agentd image volume, the read-only mount, and
// the verification env pins to the main container and volume list when
// overlay delivery is enabled. No-op in legacy mode.
func (r *WorkspaceReconciler) wireAgentdOverlay(mainContainer *corev1.Container, volumes *[]corev1.Volume) {
	if !r.agentdOverlayEnabled() {
		return
	}
	*volumes = append(*volumes, corev1.Volume{
		Name: agentdVolumeName,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  r.AgentdImage,
				PullPolicy: corev1.PullIfNotPresent,
			},
		},
	})
	mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, corev1.VolumeMount{
		Name:      agentdVolumeName,
		MountPath: agentdMountPath,
		ReadOnly:  true,
	})
	mainContainer.Env = append(mainContainer.Env,
		corev1.EnvVar{Name: agentdOverlayEnvKey, Value: "1"},
		corev1.EnvVar{Name: "LLMSAFESPACES_AGENTD_BINARY", Value: agentdMountPath + agentdBinaryRelPath},
		corev1.EnvVar{Name: "LLMSAFESPACES_AGENTD_SHA256_AMD64", Value: r.AgentdBinarySHA256AMD64},
		corev1.EnvVar{Name: "LLMSAFESPACES_AGENTD_SHA256_ARM64", Value: r.AgentdBinarySHA256ARM64},
	)
}

// AgentdDeliveryConfig is the #863 overlay-delivery configuration passed
// from controller main (flags) to the reconciler. Zero value = legacy
// baked-in mode.
type AgentdDeliveryConfig struct {
	Image             string
	BinarySHA256AMD64 string
	BinarySHA256ARM64 string
}

// ValidateAgentdDelivery is the exported startup guard used by controller
// main. Wraps validateAgentdDeliveryConfig.
func ValidateAgentdDelivery(image, amd64, arm64 string) error {
	return validateAgentdDeliveryConfig(image, amd64, arm64)
}

// validateAgentdDeliveryConfig enforces the all-or-nothing contract at
// controller startup: image (digest-pinned) + both per-arch binary hashes,
// or nothing at all (legacy baked-in mode).
func validateAgentdDeliveryConfig(image, amd64, arm64 string) error {
	if image == "" && amd64 == "" && arm64 == "" {
		return nil
	}
	if image == "" {
		return fmt.Errorf("agentd delivery: --agentd-image is required when binary sha256 flags are set")
	}
	if !digestRe.MatchString(image) {
		return fmt.Errorf("agentd delivery: --agentd-image must be digest-pinned (@sha256:<64 hex>), got %q — a floating tag defeats both reproducibility and the entrypoint verify contract", image)
	}
	// Hashes are OPTIONAL overrides (break-glass). The normal path is
	// image-only: the per-arch binary sha256s are resolved from the
	// image index annotations at startup (single Renovate-updatable
	// coordinate — see agentd_pins.go). If ANY hash flag is given, both
	// must be given and well-formed, so a manual pin is always complete.
	if amd64 == "" && arm64 == "" {
		return nil
	}
	if amd64 == "" || arm64 == "" {
		return fmt.Errorf("agentd delivery: binary sha256 flags are per-image overrides — set BOTH --agentd-binary-sha256-amd64 and --agentd-binary-sha256-arm64, or neither (annotations resolve them)")
	}
	if !sha256HexRe.MatchString(amd64) || !sha256HexRe.MatchString(arm64) {
		return fmt.Errorf("agentd delivery: binary sha256 flags must be exactly 64 hex characters")
	}
	return nil
}

// detectAgentdVerificationFailure inspects the running pod's container
// statuses for an agentd verify failure (exit 81/82 in the current or last
// terminated state). On detection it records the condition, emits one
// warning event and one metric increment per failure episode, and returns
// true — the caller must then skip the crashloop recovery path (a digest
// mismatch cannot be fixed by restarting the pod).
//
// Idempotency: an episode ends when the condition transitions away from
// the failure reason (pod recreated with a good binary or a corrected
// pin). Repeated reconciles of the same episode do not re-emit.
func (r *WorkspaceReconciler) detectAgentdVerificationFailure(ctx context.Context, ws *v1.Workspace, pod *corev1.Pod) bool {
	if !r.agentdOverlayEnabled() || !podHasAgentdOverlay(pod) {
		return false
	}

	term := latestTerminatedState(pod)
	if term == nil {
		return false
	}

	var reason string
	var outcome string
	switch term.ExitCode {
	case agentdExitVerifyFailed:
		reason = v1.ReasonAgentdVerificationFailed
		outcome = "verify_failed"
	case agentdExitOverlayMissing:
		reason = v1.ReasonAgentdOverlayMissing
		outcome = "overlay_missing"
	default:
		return false
	}

	msg := term.Message
	if msg == "" {
		msg = fmt.Sprintf("agentd integrity failure: exit code %d", term.ExitCode)
	}

	prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionAgentdVerified)
	alreadyReported := prev != nil && prev.Status == "False" && prev.Reason == string(reason)

	r.setCondition(ws, v1.WorkspaceConditionAgentdVerified, "False", string(reason), msg)

	if !alreadyReported {
		metricsAgentdVerifyFailures.WithLabelValues(outcome, pod.Spec.NodeName, digestVersionLabel(r.AgentdImage)).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, string(reason),
				"agentd verification failure (node %s, image %s, restart #%d): %s", pod.Spec.NodeName, r.AgentdImage, termToRestartCount(pod, term), msg)
		}
		log.FromContext(ctx).Error(nil, "agentd verification failed — should-never-fire signal, page and investigate",
			"workspace", ws.Name, "pod", pod.Name, "node", pod.Spec.NodeName, "exitCode", term.ExitCode, "detail", msg)
	}
	return true
}

// markAgentdVerified sets the positive condition once a pod in overlay
// mode is observed running without a verify failure. Cheap and idempotent.
// Gated on the POD carrying the overlay, not just the controller flag:
// during a rollout window (delivery enabled, pre-existing pods still on
// the baked binary), flag-only gating stamped AgentdVerified=True on
// pods that never ran the entrypoint verification — a false positive on
// a security-relevant condition (live-cluster finding, #863 validation).
// Same gate protects detection below from misreading an unrelated
// exit-81 on a legacy pod.
func (r *WorkspaceReconciler) markAgentdVerified(pod *corev1.Pod, ws *v1.Workspace) {
	if !r.agentdOverlayEnabled() || !podHasAgentdOverlay(pod) {
		return
	}
	if prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionAgentdVerified); prev != nil && prev.Status == "True" {
		return
	}
	r.setCondition(ws, v1.WorkspaceConditionAgentdVerified, "True", string(v1.ReasonAgentdVerified),
		"agentd binary verified against pod-spec digest pin")
}

// latestTerminatedState returns the first container status carrying a
// terminated state. Deliberate single-attribution semantics: one
// terminated state = one exit code = one overlay-artifact attribution
// per failure episode. On a dual-overlay pod with two failed containers
// in one episode, only the first is attributed — dual simultaneous
// failure is one root cause (e.g. a node or registry outage), and the
// second artifact's failure surfaces on the next episode after the
// first is fixed. Do not "fix" this into a fan-out without redesigning
// the one-condition-per-episode contract.
func latestTerminatedState(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.State.Terminated != nil {
			return cs.State.Terminated
		}
		if cs.LastTerminationState.Terminated != nil {
			return cs.LastTerminationState.Terminated
		}
	}
	return nil
}

func termToRestartCount(pod *corev1.Pod, term *corev1.ContainerStateTerminated) int32 {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.State.Terminated == term || cs.LastTerminationState.Terminated == term {
			return cs.RestartCount
		}
	}
	return 0
}

func conditionOfTypeLocal(ws *v1.Workspace, ct v1.WorkspaceConditionType) *v1.WorkspaceCondition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == ct {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

// digestVersionLabel derives a stable, low-cardinality version identity
// from a digest-pinned image ref (the 12 hex after "sha256:"). Used as a
// metric label so failures can be grouped per rollout.
func digestVersionLabel(image string) string {
	i := strings.LastIndex(image, "sha256:")
	if i < 0 || len(image)-i-7 < 12 {
		return "unknown"
	}
	return image[i+7 : i+7+12]
}

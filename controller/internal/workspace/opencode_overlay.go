// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// opencode overlay delivery (design 0053 §4.2).
//
// opencode ships as a digest-pinned image volume mounted read-only at
// /opencode on the WORKSPACE container — the supervisor that spawns
// opencode lives there, so neither the agentd sidecar nor any init
// container mounts it. Independent pin from agentdDelivery by design:
// opencode moves on the upstream-validation cadence, agentd on the
// platform release cadence — bundling them would couple every agentd
// rollback to an opencode rollforward (design 0053 §5).
//
// Security model mirrors #863: the mount MUST be readOnly, and the
// supervisor verifies the binary's sha256 against the pod-spec env pins
// (immutable post-create) before spawn, refusing on mismatch — no
// silent fallback. Exit codes 83/84 are distinct from agentd's 81/82 so
// a dual-overlay pod's failure attribution never crosses artifacts.

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Supervisor exit codes (workspace container). Distinct from agentd's
// 81/82 so the controller can attribute an integrity failure to exactly
// one overlay artifact on a dual-overlay pod.
const (
	opencodeExitVerifyFailed   int32 = 83
	opencodeExitOverlayMissing int32 = 84
)

const (
	opencodeVolumeName    = "opencode"
	opencodeMountPath     = "/opencode"
	opencodeBinaryRelPath = "/usr/local/bin/opencode"
	opencodeOverlayEnvKey = "OPENCODE_IMAGE_VOLUME"
)

var metricsOpencodeVerifyFailures = metrics.WorkspaceOpencodeVerifyFailuresTotal

// podHasOpencodeOverlay reports whether this specific pod was built
// with the opencode overlay wiring (the controller sets
// OPENCODE_IMAGE_VOLUME=1 only in the same branch that adds the volume
// + verify pins). Pre-existing pods from before delivery was enabled do
// not carry it.
func podHasOpencodeOverlay(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != "workspace" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == opencodeOverlayEnvKey && e.Value == "1" {
				return true
			}
		}
	}
	return false
}

// wireOpencodeOverlay adds the opencode image volume, the read-only
// mount, and the verification env pins to the workspace container and
// the volume list. Design 0053 §4.5: always on — the pins are validated
// mandatory.
func (r *WorkspaceReconciler) wireOpencodeOverlay(mainContainer *corev1.Container, volumes *[]corev1.Volume) {
	*volumes = append(*volumes, corev1.Volume{
		Name: opencodeVolumeName,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  r.OpencodeImage,
				PullPolicy: corev1.PullIfNotPresent,
			},
		},
	})
	mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, corev1.VolumeMount{
		Name:      opencodeVolumeName,
		MountPath: opencodeMountPath,
		ReadOnly:  true,
	})
	mainContainer.Env = append(mainContainer.Env,
		corev1.EnvVar{Name: opencodeOverlayEnvKey, Value: "1"},
		corev1.EnvVar{Name: "LLMSAFESPACES_OPENCODE_BINARY", Value: opencodeMountPath + opencodeBinaryRelPath},
		corev1.EnvVar{Name: "LLMSAFESPACES_OPENCODE_SHA256_AMD64", Value: r.OpencodeBinarySHA256AMD64},
		corev1.EnvVar{Name: "LLMSAFESPACES_OPENCODE_SHA256_ARM64", Value: r.OpencodeBinarySHA256ARM64},
	)
}

// OpencodeDeliveryConfig is the design 0053 §4.2 overlay-delivery
// configuration passed from controller main (flags) to the reconciler.
// Zero value = opencode stays baked into runtimes/base (S1 default).
type OpencodeDeliveryConfig struct {
	Image             string
	BinarySHA256AMD64 string
	BinarySHA256ARM64 string
}

// ValidateOpencodeDelivery is the exported startup guard used by
// controller main. Wraps validateOpencodeDeliveryConfig.
func ValidateOpencodeDelivery(image, amd64, arm64 string) error {
	return validateOpencodeDeliveryConfig(image, amd64, arm64)
}

// validateOpencodeDeliveryConfig enforces the same all-or-nothing
// contract as the agentd guard at controller startup: image
// (digest-pinned) + both per-arch binary hashes, or nothing at all.
func validateOpencodeDeliveryConfig(image, amd64, arm64 string) error {
	// Design 0053 §4.5: mandatory pin, no baked fallback. Empty image
	// is a fatal configuration error, not the opt-in no-op it was in
	// the S1 interim.
	if image == "" {
		return fmt.Errorf("opencode delivery: --opencode-image is mandatory (design 0053 §4.5 — the base image carries no opencode)")
	}
	if !digestRe.MatchString(image) {
		return fmt.Errorf("opencode delivery: --opencode-image must be digest-pinned (@sha256:<64 hex>), got %q — a floating tag defeats both reproducibility and the supervisor verify contract", image)
	}
	// Hashes are OPTIONAL overrides (break-glass). The normal path is
	// image-only: the per-arch binary sha256s are resolved from the
	// image index annotations at startup (single Renovate-updatable
	// coordinate — see overlay_pins.go). If ANY hash flag is given, both
	// must be given and well-formed, so a manual pin is always complete.
	if amd64 == "" && arm64 == "" {
		return nil
	}
	if amd64 == "" || arm64 == "" {
		return fmt.Errorf("opencode delivery: binary sha256 flags are per-image overrides — set BOTH --opencode-binary-sha256-amd64 and --opencode-binary-sha256-arm64, or neither (annotations resolve them)")
	}
	if !sha256HexRe.MatchString(amd64) || !sha256HexRe.MatchString(arm64) {
		return fmt.Errorf("opencode delivery: binary sha256 flags must be exactly 64 hex characters")
	}
	return nil
}

// detectOpencodeVerificationFailure inspects the running pod's container
// statuses for an opencode verify failure (exit 83/84 in the current or
// last terminated state). On detection it records the condition, emits
// one warning event and one metric increment per failure episode, and
// returns true — the caller must then skip the crashloop recovery path
// (a digest mismatch cannot be fixed by restarting the pod).
//
// Idempotency: an episode ends when the condition transitions away from
// the failure reason (pod recreated with a good binary or a corrected
// pin). Repeated reconciles of the same episode do not re-emit.
//
// Exit codes 83/84 are disjoint from agentd's 81/82: an agentd failure
// on a dual-overlay pod returns false here (and vice versa), so
// attribution never crosses artifacts.
func (r *WorkspaceReconciler) detectOpencodeVerificationFailure(ctx context.Context, ws *v1.Workspace, pod *corev1.Pod) bool {
	if !podHasOpencodeOverlay(pod) {
		return false
	}

	term := latestTerminatedState(pod)
	if term == nil {
		return false
	}

	var reason string
	var outcome string
	switch term.ExitCode {
	case opencodeExitVerifyFailed:
		reason = v1.ReasonOpencodeVerificationFailed
		outcome = "verify_failed"
	case opencodeExitOverlayMissing:
		reason = v1.ReasonOpencodeOverlayMissing
		outcome = "overlay_missing"
	default:
		return false
	}

	msg := term.Message
	if msg == "" {
		msg = fmt.Sprintf("opencode integrity failure: exit code %d", term.ExitCode)
	}

	prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionOpencodeVerified)
	alreadyReported := prev != nil && prev.Status == "False" && prev.Reason == string(reason)

	r.setCondition(ws, v1.WorkspaceConditionOpencodeVerified, "False", string(reason), msg)

	if !alreadyReported {
		metricsOpencodeVerifyFailures.WithLabelValues(outcome, pod.Spec.NodeName, digestVersionLabel(r.OpencodeImage)).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, string(reason),
				"opencode verification failure (node %s, image %s, restart #%d): %s", pod.Spec.NodeName, r.OpencodeImage, termToRestartCount(pod, term), msg)
		}
		log.FromContext(ctx).Error(nil, "opencode verification failed — should-never-fire signal, page and investigate",
			"workspace", ws.Name, "pod", pod.Name, "node", pod.Spec.NodeName, "exitCode", term.ExitCode, "detail", msg)
	}
	return true
}

// markOpencodeVerified sets the positive condition once a pod in
// opencode overlay mode is observed running without a verify failure.
// Cheap and idempotent. Gated on the POD carrying the overlay, not just
// the controller flag: during a rollout window (delivery enabled,
// pre-existing pods still on the baked binary), flag-only gating would
// stamp OpencodeVerified=True on pods that never ran the supervisor
// verification — a false positive on a security-relevant condition (the
// #863 live-cluster finding). Same gate protects detection above from
// misreading an unrelated exit-83 on a legacy pod.
func (r *WorkspaceReconciler) markOpencodeVerified(pod *corev1.Pod, ws *v1.Workspace) {
	if !podHasOpencodeOverlay(pod) {
		return
	}
	if prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionOpencodeVerified); prev != nil && prev.Status == "True" {
		return
	}
	r.setCondition(ws, v1.WorkspaceConditionOpencodeVerified, "True", string(v1.ReasonOpencodeVerified),
		"opencode binary verified against pod-spec digest pin")
}

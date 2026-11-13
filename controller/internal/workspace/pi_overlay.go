// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// pi overlay delivery — the second-harness artifact (design 0053 §5.4:
// "a second agent is a different image at the same mount point, not a
// redesign"; scoping report §2.3 story S1).
//
// pi ships as a digest-pinned image volume mounted read-only at /pi on
// the WORKSPACE container, exactly like the opencode overlay: the
// supervisor that will spawn it lives there, so neither the agentd
// sidecar nor any init container mounts it. Independent pin from
// agentdDelivery and opencodeDelivery by the same design-0053 §5 logic:
// pi moves on its own upstream-validation cadence (~2 releases/week —
// faster than opencode's), and bundling would couple every rollback of
// one artifact to a rollforward of another.
//
// The artifact is a directory TREE, not one file — the trust ruling
// (hash-the-binary + digest-the-tree, the #1416/#1469 precedent) is
// recorded in runtimes/pi/Dockerfile: the BINARY at /pi/pi carries the
// per-arch sha256 pin contract; every other tree member is non-
// executable data covered by the image digest.
//
// Security model mirrors #863/0053: the mount MUST be readOnly, and the
// supervisor verifies the binary's sha256 against the pod-spec env pins
// (immutable post-create) before any harness spawn, refusing on mismatch
// — no silent fallback. Exit codes 85/86 are distinct from agentd's
// 81/82 and opencode's 83/84 so a tri-overlay pod's failure attribution
// never crosses artifacts. (The controller PROCESS also exits 85 for
// "relay staging not armed" — a different process entirely; in-pod
// workspace-container codes and controller-pod codes are disambiguated
// by which pod crashed, and this comment is the treaty line.)

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Supervisor exit codes (workspace container). Distinct from agentd's
// 81/82 and opencode's 83/84 so the controller can attribute an
// integrity failure to exactly one overlay artifact on a tri-overlay
// pod.
const (
	piExitVerifyFailed   int32 = 85
	piExitOverlayMissing int32 = 86
)

const (
	piVolumeName    = "pi"
	piMountPath     = "/pi"
	piBinaryRelPath = "/pi"
	piOverlayEnvKey = "PI_IMAGE_VOLUME"
)

var metricsPiVerifyFailures = metrics.WorkspacePiVerifyFailuresTotal

// podHasPiOverlay reports whether this specific pod was built with the
// pi overlay wiring (the controller sets PI_IMAGE_VOLUME=1 only in the
// same branch that adds the volume + verify pins). Pre-existing pods
// from before the pi overlay landed do not carry it.
func podHasPiOverlay(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != "workspace" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == piOverlayEnvKey && e.Value == "1" {
				return true
			}
		}
	}
	return false
}

// wirePiOverlay adds the pi image volume, the read-only mount, and the
// verification env pins to the workspace container and the volume list.
// Always on once the controller booted with --pi-image (the pin is
// mandatory at startup — no baked fallback ever existed for pi).
//
// The binary path is /pi/pi, NOT a /pi/bin/pi subpath: the tree keeps
// the upstream release layout byte-for-byte because bun's
// --compile-autoload-package-json resolves a package.json NEXT TO the
// executable and the native prebuilds sit at fixed relative paths —
// relocating the binary would silently break runtime resolution (the
// ruling lives in runtimes/pi/Dockerfile).
func (r *WorkspaceReconciler) wirePiOverlay(mainContainer *corev1.Container, volumes *[]corev1.Volume) {
	*volumes = append(*volumes, corev1.Volume{
		Name: piVolumeName,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  r.PiImage,
				PullPolicy: corev1.PullIfNotPresent,
			},
		},
	})
	mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, corev1.VolumeMount{
		Name:      piVolumeName,
		MountPath: piMountPath,
		ReadOnly:  true,
	})
	mainContainer.Env = append(mainContainer.Env,
		corev1.EnvVar{Name: piOverlayEnvKey, Value: "1"},
		corev1.EnvVar{Name: "LLMSAFESPACES_PI_BINARY", Value: piMountPath + piBinaryRelPath},
		corev1.EnvVar{Name: "LLMSAFESPACES_PI_SHA256_AMD64", Value: r.PiBinarySHA256AMD64},
		corev1.EnvVar{Name: "LLMSAFESPACES_PI_SHA256_ARM64", Value: r.PiBinarySHA256ARM64},
	)
}

// PiDeliveryConfig is the pi overlay-delivery configuration passed from
// controller main (flags) to the reconciler. Zero value = the controller
// refuses to start (validatePiDeliveryConfig: pi was never baked into
// runtimes/base, so there is no fallback mode — an empty --pi-image is a
// fatal configuration error on every surface).
type PiDeliveryConfig struct {
	Image             string
	BinarySHA256AMD64 string
	BinarySHA256ARM64 string
}

// ValidatePiDelivery is the exported startup guard used by controller
// main. Wraps validatePiDeliveryConfig.
func ValidatePiDelivery(image, amd64, arm64 string) error {
	return validatePiDeliveryConfig(image, amd64, arm64)
}

// validatePiDeliveryConfig enforces the same all-or-nothing contract as
// the agentd/opencode guards: image (digest-pinned) + both-or-neither
// per-arch binary hashes. For pi there is NO baked fallback — the pin is
// mandatory from day one (design 0053 §4.5 posture applied at
// introduction, not after a base strip: nothing in runtimes/base has
// ever shipped pi).
func validatePiDeliveryConfig(image, amd64, arm64 string) error {
	if image == "" {
		return fmt.Errorf("pi delivery: --pi-image is mandatory (pi was never baked into the base image — there is no fallback mode)")
	}
	if !digestRe.MatchString(image) {
		return fmt.Errorf("pi delivery: --pi-image must be digest-pinned (@sha256:<64 hex>), got %q — a floating tag defeats both reproducibility and the supervisor verify contract", image)
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
		return fmt.Errorf("pi delivery: binary sha256 flags are per-image overrides — set BOTH --pi-binary-sha256-amd64 and --pi-binary-sha256-arm64, or neither (annotations resolve them)")
	}
	if !sha256HexRe.MatchString(amd64) || !sha256HexRe.MatchString(arm64) {
		return fmt.Errorf("pi delivery: binary sha256 flags must be exactly 64 hex characters")
	}
	return nil
}

// detectPiVerificationFailure inspects the running pod's container
// statuses for a pi verify failure (exit 85/86 in the current or last
// terminated state). Same semantics as the opencode detector: records
// the condition, emits one warning event and one metric increment per
// failure episode, and returns true — the caller then skips the
// crashloop recovery path (a digest mismatch cannot be fixed by
// restarting the pod).
//
// Exit codes 85/86 are disjoint from agentd's 81/82 and opencode's
// 83/84: either of those on a tri-overlay pod returns false here (and
// vice versa), so attribution never crosses artifacts.
func (r *WorkspaceReconciler) detectPiVerificationFailure(ctx context.Context, ws *v1.Workspace, pod *corev1.Pod) bool {
	if !podHasPiOverlay(pod) {
		return false
	}

	term := latestTerminatedState(pod)
	if term == nil {
		return false
	}

	var reason string
	var outcome string
	switch term.ExitCode {
	case piExitVerifyFailed:
		reason = v1.ReasonPiVerificationFailed
		outcome = "verify_failed"
	case piExitOverlayMissing:
		reason = v1.ReasonPiOverlayMissing
		outcome = "overlay_missing"
	default:
		return false
	}

	msg := term.Message
	if msg == "" {
		msg = fmt.Sprintf("pi integrity failure: exit code %d", term.ExitCode)
	}

	prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionPiVerified)
	alreadyReported := prev != nil && prev.Status == "False" && prev.Reason == string(reason)

	r.setCondition(ws, v1.WorkspaceConditionPiVerified, "False", string(reason), msg)

	if !alreadyReported {
		metricsPiVerifyFailures.WithLabelValues(outcome, pod.Spec.NodeName, digestVersionLabel(r.PiImage)).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, string(reason),
				"pi verification failure (node %s, image %s, restart #%d): %s", pod.Spec.NodeName, r.PiImage, termToRestartCount(pod, term), msg)
		}
		log.FromContext(ctx).Error(nil, "pi verification failed — should-never-fire signal, page and investigate",
			"workspace", ws.Name, "pod", pod.Name, "node", pod.Spec.NodeName, "exitCode", term.ExitCode, "detail", msg)
	}
	return true
}

// markPiVerified sets the positive condition once a pod in pi overlay
// mode is observed running without a verify failure. Cheap and
// idempotent. Gated on the POD carrying the overlay (the same
// rollout-window protection as markOpencodeVerified — flag-only gating
// would stamp PiVerified=True on pods that never ran the supervisor
// verification).
func (r *WorkspaceReconciler) markPiVerified(pod *corev1.Pod, ws *v1.Workspace) {
	if !podHasPiOverlay(pod) {
		return
	}
	if prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionPiVerified); prev != nil && prev.Status == "True" {
		return
	}
	r.setCondition(ws, v1.WorkspaceConditionPiVerified, "True", string(v1.ReasonPiVerified),
		"pi binary verified against pod-spec digest pin")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// boot_failure.go — design 0051 sidecar migration, step 1: platform
// boot-phase failure visibility.
//
// The incident class this serves (2026-08-25): a platform boot bug
// (stale agentd aborting on contract-shape MCP metadata) presented as a
// workspace eternal in Creating with no reason on the Workspace object
// — discoverable only by reading pod logs. Post-migration the same bug
// crash-loops a PLATFORM container (platform-init, platform-bootstrap,
// platform-materialize, or the agentd sidecar's boot phase); this
// detection mirrors the #863 verify-failure pattern so the failure
// lands on the Workspace as a condition + one-shot event + metric.
//
// Semantics identical to detectAgentdVerificationFailure: once per
// failure episode (idempotent across reconciles), true return means the
// caller should NOT enter crashloop recovery — deleting the pod cannot
// fix a code or input bug.

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// platformBootContainers are the container names owned by the platform
// boot phase. The main workspace container is deliberately absent —
// its crashes have their own health/recovery paths.
func platformBootContainer(name string) bool {
	switch name {
	case "platform-init", "platform-bootstrap", "platform-materialize", "agentd":
		return true
	}
	return false
}

// detectPlatformBootFailure inspects the pod's init and main container
// statuses for a platform boot container terminated with an error.
// Records BootReady=False, emits one warning event and one metric
// increment per episode, and returns true.
func (r *WorkspaceReconciler) detectPlatformBootFailure(ctx context.Context, ws *v1.Workspace, pod *corev1.Pod) bool {
	if !r.agentdOverlayEnabled() {
		return false
	}

	for i := range pod.Status.InitContainerStatuses {
		if cs := bootFailureStatus(&pod.Status.InitContainerStatuses[i]); cs != nil {
			r.reportPlatformBootFailure(ctx, ws, pod, pod.Status.InitContainerStatuses[i].Name, cs)
			return true
		}
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if !platformBootContainer(cs.Name) {
			continue
		}
		if t := bootFailureStatus(cs); t != nil {
			r.reportPlatformBootFailure(ctx, ws, pod, cs.Name, t)
			return true
		}
	}
	return false
}

// bootFailureStatus returns the terminated-with-error state for a
// platform boot container (current or last), nil when clean.
func bootFailureStatus(cs *corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
		return t
	}
	if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 {
		return t
	}
	return nil
}

func (r *WorkspaceReconciler) reportPlatformBootFailure(ctx context.Context, ws *v1.Workspace, pod *corev1.Pod, container string, term *corev1.ContainerStateTerminated) {
	msg := term.Message
	if msg == "" {
		msg = "platform boot container terminated without a message"
	}

	prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionBootReady)
	alreadyReported := prev != nil && prev.Status == "False" && prev.Reason == string(v1.ReasonPlatformBootFailed) &&
		prev.Message == msg

	r.setCondition(ws, v1.WorkspaceConditionBootReady, "False", string(v1.ReasonPlatformBootFailed),
		container+": "+msg)

	if !alreadyReported {
		metrics.WorkspacePlatformBootFailuresTotal.WithLabelValues(container, pod.Spec.NodeName).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, string(v1.ReasonPlatformBootFailed),
				"platform boot failure (container %s, node %s, exit %d): %s", container, pod.Spec.NodeName, term.ExitCode, msg)
		}
		log.FromContext(ctx).Error(nil, "platform boot phase failed — pod will never become Ready; investigate the condition message",
			"workspace", ws.Name, "pod", pod.Name, "container", container, "exitCode", term.ExitCode, "detail", msg)
	}
}

// markBootReady sets the positive condition once a pod's platform boot
// phase is observed clean. Cheap and idempotent.
func (r *WorkspaceReconciler) markBootReady(pod *corev1.Pod, ws *v1.Workspace) {
	if !r.agentdOverlayEnabled() {
		return
	}
	if prev := conditionOfTypeLocal(ws, v1.WorkspaceConditionBootReady); prev != nil && prev.Status == "True" {
		return
	}
	r.setCondition(ws, v1.WorkspaceConditionBootReady, "True", string(v1.ReasonBootReady),
		"platform boot phase (init-fs/bootstrap/materialize) completed")
}

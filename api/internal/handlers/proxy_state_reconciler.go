// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stateReconciler is US-69.11's replacement for sseWatchReconciler: with
// the SSE tracker retired, the surviving periodic duties are —
//
//   - escalateHungs (D6/#998): the statusz-based unattended-escalation
//     sweep (data source unchanged; only the scheduler moved);
//   - usage-gate arming: every Active workspace gets one busy-gated ABI
//     subscription attempt (the gate itself drops the connection after
//     the idle settle window — arming is cheap and idempotent);
//   - gate teardown for workspaces that left Active.
//
// It deliberately does NOT re-derive session state: display state is the
// pod's projection (contract streams), billing/state bridges hang off the
// usage gates.
func (h *ProxyHandler) stateReconciler(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			// Shutdown race (#903 review): the select above may pick
			// ticker.C even when stopCh is already closed.
			select {
			case <-h.stopCh:
				return
			default:
			}
			if h.phaseSource == nil {
				continue
			}
			phases := h.phaseSource.GetAllKnownPhases()
			watched := make([]string, 0, len(phases))
			for id, phase := range phases {
				if phase == string(phaseActive) {
					h.UsageStream().Open(id)
					watched = append(watched, id)
				} else {
					h.UsageStream().Close(id)
				}
			}
			// Stale activeSess self-heal (the retired tracker's
			// onReconnect duty, re-driven here): statusz reconcile clears
			// sessions idle in the agent but busy in our map.
			func() {
				defer func() {
					if r := recover(); r != nil {
						h.logger.Error("statusz reconcile panicked (isolated; reconciler continues)",
							fmt.Errorf("%v", r), "component", "reconcile")
					}
				}()
				for _, id := range watched {
					podIP := h.statuszPodIP(id)
					if podIP == "" {
						continue
					}
					pw, err := h.getPassword(context.Background(), id)
					if err != nil {
						continue
					}
					h.reconcileSessionState(id, podIP, pw)
				}
			}()
			// D6 (#998): unattended-escalation sweep on the same tick —
			// notify, never execute. Failure-isolated.
			func() {
				defer func() {
					if r := recover(); r != nil {
						h.logger.Error("D6 escalation sweep panicked (isolated; reconciler continues)",
							fmt.Errorf("%v", r), "component", "d6")
					}
				}()
				h.escalateHungs(watched)
			}()
		}
	}
}

// statuszPodIP resolves the pod IP for statusz polls (phase-guarded —
// the retired tracker's resolver, kept for the D6 sweep and the
// reconcile pass).
func (h *ProxyHandler) statuszPodIP(workspaceID string) string {
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return ""
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(context.Background(), workspaceID, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	if workspace.Status.Phase != phaseActive {
		return ""
	}
	return workspace.Status.PodIP
}

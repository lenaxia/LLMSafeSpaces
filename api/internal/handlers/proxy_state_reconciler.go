// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// stateReconciler is US-69.11's replacement for sseWatchReconciler: with
// the SSE tracker retired, the surviving periodic duties are —
//
//   - escalateHungs (D6/#998): the statusz-based unattended-escalation
//     sweep (data source unchanged; only the scheduler moved);
//   - the stale-activeSess statusz self-heal;
//   - gate teardown for workspaces that left Active.
//
// It deliberately does NOT arm gates and does NOT re-derive session
// state: usage gates are ACTIVITY-gated (outbox delivery, adapter write
// ops — the moments a turn may start), so an idle fleet holds ZERO pod
// streams (D1-B's rolling_deploy_no_fanin_storm holds by construction —
// an API deploy with idle pods establishes no upstream connections).
// Display state is the pod's projection (contract streams).
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
					podIP := h.statuszPodIP(context.Background(), id)
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
func (h *ProxyHandler) statuszPodIP(ctx context.Context, workspaceID string) string {
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return ""
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	if workspace.Status.Phase != phaseActive {
		return ""
	}
	return workspace.Status.PodIP
}

// FetchStatuszPublic resolves the workspace pod and fetches statusz
// (admin-facing; phase-guarded). The authority flip's drain signal reads
// the ledger_in_flight field off it.
func (h *ProxyHandler) FetchStatuszPublic(ctx context.Context, workspaceID string) (*agentd.StatuszResponse, error) {
	podIP := h.statuszPodIP(ctx, workspaceID)
	if podIP == "" {
		return nil, fmt.Errorf("no pod IP for %s (not Active)", workspaceID)
	}
	return h.fetchStatusz(ctx, workspaceID, podIP)
}

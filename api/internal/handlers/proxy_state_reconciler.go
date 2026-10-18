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
//   - gate teardown for workspaces that left Active;
//   - #1732: gate RE-ARM for Active workspaces with live sessions in
//     the roll-surviving ws_state store (the api-roll recovery leg —
//     see rearmGatesForLiveSessions).
//
// It deliberately does NOT arm gates BLANKETLY and does NOT re-derive
// session state: usage gates are ACTIVITY-gated (outbox delivery,
// adapter write ops — the moments a turn may start), so an idle fleet
// holds ZERO pod streams (D1-B's rolling_deploy_no_fanin_storm holds by
// construction — an API deploy with idle pods establishes no upstream
// connections). The #1732 re-arm is NOT a blanket: it touches only
// workspaces whose active-session records say a session is LIVE — idle
// workspaces have no records (SREM deletes on empty; the 30-min TTL
// bounds strays), so D1-B's no-fan-in property holds. Display state is
// the pod's projection (contract streams).
//
// The first pass runs IMMEDIATELY (not after one interval): after an
// API roll the new pod starts gate-less, and in-flight sessions generate
// no new routed activity to arm gates the activity way — the wedge the
// 2026-10-17 v0.38.0 incident made visible (#1732).
func (h *ProxyHandler) stateReconciler(interval time.Duration) {
	h.reconcilePass()
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
			h.reconcilePass()
		}
	}
}

// reconcilePass is one reconciler tick (also the startup pass).
func (h *ProxyHandler) reconcilePass() {
	if h.phaseSource == nil {
		return
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
	// #1732: re-arm gates for Active workspaces whose sessions are still
	// live in the roll-surviving store — AFTER an api roll this is the
	// only path that re-establishes the persistent pod streams for
	// sessions already in flight (no new activity is coming to arm them
	// the activity-gated way).
	h.rearmGatesForLiveSessions(watched)
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
			podIP, podErr := h.statuszPodIP(context.Background(), id)
			if podErr != nil || podIP == "" {
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

// rearmGatesForLiveSessions re-arms usage gates for Active workspaces
// that hold live active-session records in the ws_state store (#1732's
// api-roll recovery leg). The store is Redis-backed in production and
// SURVIVES an api roll — the new pod reads the old pod's session truth.
// D1-B is preserved by construction: idle workspaces carry no
// active-session records, so the re-arm opens nothing for them; a stray
// record (a session that ended inside the 30-min TTL) opens a gate that
// the fold's own idle-drop closes after the settle window. Fail-closed:
// a store error reads as "no live sessions" and retries next tick.
func (h *ProxyHandler) rearmGatesForLiveSessions(activeWorkspaces []string) {
	// SKELETON (#1732 pins commit): the pass structure is real; the
	// re-arm action is inert and lands with the fix commit.
	_ = activeWorkspaces
}

// statuszPodIP resolves the pod to poll. The second return distinguishes
// "confirmed no pod" (workspace fetched, phase not Active — resolution
// authority gone) from UNKNOWN states (client construction failure, Get
// error/timeout, Active-with-empty-IP while the pod is creating) which
// are errors: a transient blip resolving a still-hung workspace would
// rewrite live history, so callers must treat errors as
// try-again-later, never as no-pod.
func (h *ProxyHandler) statuszPodIP(ctx context.Context, workspaceID string) (string, error) {
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return "", fmt.Errorf("k8s client: %w", err)
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("workspace get: %w", err)
	}
	if workspace.Status.Phase != phaseActive {
		return "", nil
	}
	if workspace.Status.PodIP == "" {
		// Active with no pod IP yet (pod creating) — transient.
		return "", fmt.Errorf("active workspace %s has empty pod IP", workspaceID)
	}
	return workspace.Status.PodIP, nil
}

// FetchStatuszPublic resolves the workspace pod and fetches statusz
// (admin-facing; phase-guarded). The authority flip's drain signal reads
// the ledger_in_flight field off it.
func (h *ProxyHandler) FetchStatuszPublic(ctx context.Context, workspaceID string) (*agentd.StatuszResponse, error) {
	podIP, err := h.statuszPodIP(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("no pod IP for %s: %w", workspaceID, err)
	}
	if podIP == "" {
		return nil, fmt.Errorf("no pod IP for %s (not Active)", workspaceID)
	}
	return h.fetchStatusz(ctx, workspaceID, podIP)
}

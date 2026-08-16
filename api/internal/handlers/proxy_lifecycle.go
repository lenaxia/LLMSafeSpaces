// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"fmt"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
)

func (h *ProxyHandler) EnableSessionParentResolution() {
	if h.sessionParents != nil {
		return
	}
	h.sessionParents = newSessionParentCache(h.fetchSessionParent)
}

func (h *ProxyHandler) Start() error {
	var startErr error
	h.startOnce.Do(func() {
		h.started = true
		h.stopCh = make(chan struct{})
		h.userBroker = eventbroker.NewUserEventBroker()

		h.activityTracker = activity.NewActivityTracker(h.k8sClient, h.logger, h.namespace)
		if err := h.activityTracker.Start(); err != nil {
			startErr = fmt.Errorf("starting activity tracker: %w", err)
			return
		}

		h.sseTracker = sse.NewTracker(h.httpClient, h.logger, h.onSessionIdle)
		h.sseTracker.SetPasswordGetter(h)
		h.sseTracker.SetPodIPResolver(h.getPodIPForSSE)
		h.sseTracker.SetOnSessionActive(h.onSessionActive)
		h.sseTracker.SetOnRawEvent(h.onRawEvent)
		h.sseTracker.SetOnAgentDied(h.onAgentDied)
		h.sseTracker.SetOnReconnect(h.reconcileSessionState)

		watcher, err := workspace.NewWatcher(h.k8sClient, h.logger, h.namespace, h.onPhaseChange)
		if err != nil {
			_ = h.activityTracker.Stop()
			startErr = fmt.Errorf("creating CRD watcher: %w", err)
			return
		}
		watcher.SetUserBroker(h.userBroker)
		if h.versionSyncCb != nil {
			watcher.SetVersionSyncCallback(h.versionSyncCb)
		}
		if h.workspaceUpdateCb != nil {
			watcher.SetWorkspaceUpdateCallback(h.workspaceUpdateCb)
		}
		if err := watcher.Start(); err != nil {
			_ = h.activityTracker.Stop()
			startErr = fmt.Errorf("starting CRD watcher: %w", err)
			return
		}
		h.watcher = watcher
		h.phaseSource = watcher
		// #902: the seed path alone cannot keep watches alive — prior-phase
		// is Redis-persisted, so post-restart seeds skip arming, and dead
		// watches have no transition event to re-arm them. The reconciler
		// heals missing watches; see its doc comment for scope limits.
		go h.sseWatchReconciler(sseWatchReconcileInterval)
	})
	return startErr
}

// sseWatchReconcileInterval is how often the SSE watch reconciler re-arms
// watches for Active workspaces. Var for tests.
var sseWatchReconcileInterval = 60 * time.Second

// sseWatchReconciler periodically re-arms SSE tracker watches for every
// Active workspace (#902). It heals MISSING watches — a seed skipped via
// Redis-persisted prior-phase, an armed watch whose subscribe goroutine
// exited, a future bug — converting permanent event-blindness into at
// most one interval. Scope limits, stated plainly (#903 review):
//   - It only adds watches (EnsureWatching is an idempotent map check);
//     it never tears down or resets connections.
//   - It cannot see ARMED-BUT-FAILING watches (goroutine alive,
//     connectAndRead failing forever at Warn with backoff — e.g. a stale
//     podIP). That class needs the tracker-state signal from #901
//     (G1/G11), not this reconciler.
func (h *ProxyHandler) sseWatchReconciler(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			// Shutdown race (#903 review): the select above may pick
			// ticker.C even when stopCh is already closed; arming after
			// Stop() would leak a retry goroutine nobody will cancel.
			select {
			case <-h.stopCh:
				return
			default:
			}
			if h.sseTracker == nil || h.phaseSource == nil {
				continue
			}
			phases := h.phaseSource.GetAllKnownPhases()
			watched := make([]string, 0, len(phases))
			for id, phase := range phases {
				if phase == string(phaseActive) {
					h.sseTracker.EnsureWatching(id)
					watched = append(watched, id)
				}
			}
			// #901 G3: refresh upstream-liveness gauges while we're here
			// (no second loop/ticker for the same data).
			sse.RefreshLastEventGauges(watched)
		}
	}
}

func (h *ProxyHandler) Stop() error {
	h.stopOnce.Do(func() {
		if h.stopCh != nil {
			close(h.stopCh)
		}
		if h.sseTracker != nil {
			h.sseTracker.Stop()
		}
		if h.watcher != nil {
			h.watcher.Stop()
		}
		if h.activityTracker != nil {
			_ = h.activityTracker.Stop()
		}
	})
	return nil
}

func (h *ProxyHandler) GetSSETracker() *sse.Tracker {
	return h.sseTracker
}

func (h *ProxyHandler) GetPasswordGetter() interfaces.WorkspacePasswordProvider {
	return h
}

func (h *ProxyHandler) SetAgentStateChecker(c AgentStateChecker) {
	h.agentStateChecker = c
}

func (h *ProxyHandler) SetVersionSyncCallback(cb workspace.VersionSyncCallback) {
	h.versionSyncCb = cb
}

// SetWorkspaceUpdateCallback installs the per-CRD-event callback that
// powers the watcher-driven auto-push of user-DEK secrets after pod
// recreation (worklog 0591). Must be called before Start().
func (h *ProxyHandler) SetWorkspaceUpdateCallback(cb workspace.WorkspaceUpdateCallback) {
	h.workspaceUpdateCb = cb
}

// SetRequestBufferConfig rebuilds the per-workspace request buffer with the
// configured size and timeout. Must be called before Start: request goroutines
// read h.requestBuffer without synchronization, so a late swap would race.
// Values <=0 fall back to the enabled defaults (size 10, timeout 30s) so the
// feature is on unless explicitly constructed disabled — the zero-value config
// must not silently turn buffering off in production.
func (h *ProxyHandler) SetRequestBufferConfig(maxSize int, timeout time.Duration) {
	if h.started {
		panic("SetRequestBufferConfig called after Start — request goroutines may already be reading requestBuffer")
	}
	if maxSize <= 0 {
		maxSize = defaultBufferMaxSize
	}
	h.requestBuffer = newRequestBuffer(maxSize, timeout, defaultBufferPollInterval, h.logger)
}

func (h *ProxyHandler) SetMeteringService(svc interfaces.MeteringService) {
	h.meteringSvc = svc
}

// SetV2QueueShadow injects the Redis-backed shadow marker for V2 queue
// visibility (US-63.10). Must be called before Start(). Pass nil to
// disable the shadow (ListQueue returns empty under V2).
func (h *ProxyHandler) SetV2QueueShadow(s *V2QueueShadow) {
	h.v2Shadow = s
}

// SetV2PendingTracker overrides the V2 pending-session tracker. Used by
// app.go to swap the in-memory tracker for a Redis-backed one when a Redis
// client is available (multi-replica support). Must be called before Start().
func (h *ProxyHandler) SetV2PendingTracker(t v2PendingTracker) {
	if t != nil {
		h.v2Pending = t
	}
}

func (h *ProxyHandler) GetBroker() BrokerPublisher {
	if h.userBroker == nil {
		return nil
	}
	return h.userBroker
}

func (h *ProxyHandler) GetWorkspaceOwner(workspaceID string) string {
	if h.userBroker == nil {
		return ""
	}
	return h.userBroker.WorkspaceOwner(workspaceID)
}

// publishWorkspaceEvent fans out a workspace-scoped SSE event to subscribers.
func (h *ProxyHandler) publishWorkspaceEvent(workspaceID string, evt apitypes.WorkspaceSSEEvent) {
	if h.userBroker != nil {
		h.userBroker.PublishToWorkspace(workspaceID, evt)
	}
}

// publishWorkspaceAndUserEvent delivers an event to BOTH the workspace stream
// (active-view consumers) and the user stream (cross-workspace, replay-buffered).
// Use for low-frequency events that affect global UI state (agent.question,
// agent.permission). The user-stream copy carries WorkspaceID so the frontend
// can route it; the workspace-stream copy does not (implicit for subscribers).
// If the workspace owner is unrecorded, the user-stream publish is skipped
// silently — the workspace-stream publish still fires.
func (h *ProxyHandler) publishWorkspaceAndUserEvent(workspaceID string, evt apitypes.WorkspaceSSEEvent) {
	if h.userBroker == nil {
		return
	}
	h.userBroker.PublishToWorkspace(workspaceID, evt)
	if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		evt.WorkspaceID = workspaceID
		h.userBroker.PublishToUser(userID, evt)
	}
}

func (h *ProxyHandler) GetAllKnownPhases() map[string]string {
	if h.watcher == nil {
		return nil
	}
	return h.watcher.GetAllKnownPhases()
}

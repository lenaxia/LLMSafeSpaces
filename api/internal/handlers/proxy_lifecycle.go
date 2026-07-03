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
		// SSE subscriptions for already-Active workspaces are established
		// by the watcher's seedResourceVersion(), which calls onPhaseChange
		// for each Active workspace it discovers. No post-Start loop needed.

		// Start the stranded-queue sweep last — after all other setup has
		// succeeded. This avoids leaking the goroutine if watcher.Start()
		// fails and the caller doesn't invoke Stop().
		go h.startQueueSweep(h.stopCh)
	})
	return startErr
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

func (h *ProxyHandler) SetMessageQueueService(svc interfaces.MessageQueueService) {
	h.queueSvc = svc
}

func (h *ProxyHandler) GetMessageQueueService() interfaces.MessageQueueService {
	return h.queueSvc
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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func (h *ProxyHandler) onPhaseChange(workspace *v1.Workspace) {
	phase := workspace.Status.Phase

	prior, hadPrior := h.state().GetPriorPhase(context.Background(), workspace.Name)
	h.state().SetPriorPhase(context.Background(), workspace.Name, string(phase))

	if h.userBroker != nil && workspace.Spec.Owner.UserID != "" {
		h.userBroker.RecordWorkspaceOwner(workspace.Name, workspace.Spec.Owner.UserID)
		h.userBroker.PublishToUser(workspace.Spec.Owner.UserID, apitypes.WorkspaceSSEEvent{
			Type:        "workspace.phase",
			WorkspaceID: workspace.Name,
			Phase:       string(phase),
		})
	}

	if h.meteringSvc != nil && workspace.Spec.Owner.UserID != "" {
		// RecordLifecycleEvent is called unconditionally — including on seed calls
		// (prior=="") that fire when the API restarts with already-Active workspaces.
		// Seed calls produce a phantom lifecycle record with from_phase="" and
		// to_phase="Active". This was a deliberate tradeoff: the alternative (guarding
		// with prior!="") silently drops Creating→Active events for workspaces that
		// transition while the API is restarting, which corrupts billing data worse than
		// a phantom record. The metering service is expected to handle from_phase="" as
		// a no-op or a restart-artifact marker.
		if err := h.meteringSvc.RecordLifecycleEvent(
			context.Background(),
			workspace.Name,
			workspace.Spec.Owner.UserID,
			types.OwnerTypeUser,
			prior,
			string(phase),
			workspace.Spec.SecurityLevel,
			time.Now(),
		); err != nil {
			h.logger.Error("Failed to record lifecycle event", err,
				"workspace_id", workspace.Name,
				"phase", string(phase),
			)
		}
	}

	if phase == phaseSuspending || phase == phaseSuspended || phase == phaseTerminating || phase == phaseTerminated {
		h.invalidateCaches(context.Background(), workspace.Name)
		if h.sseTracker != nil {
			h.sseTracker.StopWatching(workspace.Name)
		}
		if phase == phaseTerminated || phase == phaseTerminating {
			h.state().DeletePriorPhase(context.Background(), workspace.Name)

			if h.activityTracker != nil {
				h.activityTracker.Delete(workspace.Name)
			}
		}
		return
	}

	if phase == v1.WorkspacePhaseFailed {
		h.invalidateCaches(context.Background(), workspace.Name)
		return
	}

	if phase == phaseActive {
		// hadPrior==false means this is the first invocation for this
		// workspace in the handler — either a seed call (workspace was
		// already Active on API restart) or a real transition from a
		// phase not yet seen by the handler (e.g. Creating→Active on a
		// new workspace whose Creating event arrived before the handler
		// was aware of it). prior != phaseActive means a real transition
		// into Active (e.g. Creating → Active, Resuming → Active). Both
		// cases require starting the SSE subscription. prior == phaseActive
		// means a watch event with no phase change — only clear cached config.
		if !hadPrior || prior != string(phaseActive) {
			h.invalidateCaches(context.Background(), workspace.Name)
			if h.sseTracker != nil {
				h.sseTracker.StopWatching(workspace.Name)
				h.sseTracker.EnsureWatching(workspace.Name)
			}
		} else {
			h.state().InvalidateWorkspaceConfig(context.Background(), workspace.Name)
		}
	}
}

func (h *ProxyHandler) onSessionIdle(workspaceID, sessionID string) {
	h.removeActiveSession(context.Background(), workspaceID, sessionID)

	if h.userBroker != nil {
		h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "session.status",
			SessionID: sessionID,
			Status:    "idle",
		})
	}

	if h.userBroker != nil {
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "session.status",
				WorkspaceID: workspaceID,
				SessionID:   sessionID,
				Status:      "idle",
			})
		}
	}

	if h.activityTracker != nil {
		h.activityTracker.Record(workspaceID)
	}
	if h.sessionIndex != nil && !h.isSessionDeleted(workspaceID, sessionID) {
		h.sessionIndex.RecordMessage(workspaceID, sessionID, "", time.Now())
		go h.fetchAndPersistTitle(workspaceID, sessionID)
	}
}

func (h *ProxyHandler) onSessionActive(workspaceID, sessionID string) {
	cfg, ok := h.state().GetWorkspaceConfig(context.Background(), workspaceID)
	maxSessions := defaultMaxActiveSessions
	if ok && cfg.MaxActiveSessions > 0 {
		maxSessions = cfg.MaxActiveSessions
	}
	h.checkAndAddActiveSession(context.Background(), workspaceID, sessionID, maxSessions)

	if h.userBroker != nil {
		h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "session.status",
			SessionID: sessionID,
			Status:    "busy",
		})
	}

	if h.userBroker != nil {
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "session.status",
				WorkspaceID: workspaceID,
				SessionID:   sessionID,
				Status:      "busy",
			})
		}
	}
}

func (h *ProxyHandler) onRawEvent(workspaceID, eventType, rawData string) {
	h.state().TouchActiveSessions(context.Background(), workspaceID)

	// Parse the raw event once; all consumers below share the parsed form.
	var parsed interface{}
	if err := json.Unmarshal([]byte(rawData), &parsed); err != nil {
		h.logger.Debug("Failed to parse event for relay", "error", err, "eventType", eventType)
		return
	}

	if h.userBroker != nil {
		h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "agent.event",
			EventType: eventType,
			Data:      parsed,
		})
	}

	if eventType == "session.updated" && h.sessionIndex != nil {
		h.persistTitleFromEvent(workspaceID, rawData)
	}

	if eventType == "session.next.step.ended" {
		h.logger.Debug("onRawEvent: dispatching to persistContextFromEvent",
			"workspaceID", workspaceID, "eventType", eventType)
		h.persistContextFromEvent(workspaceID, rawData)
	}

	// Epic 63 V2 session-queue bridge: synthesize queue.update SSE events
	// from V2 PromptAdmitted/Prompted events. Unconditional under V2.
	h.onV2RawEvent(workspaceID, eventType, rawData)

	if h.dialect != nil {
		h.emitNormalizedInputEvent(workspaceID, eventType, rawData)
	}
}

func (h *ProxyHandler) onAgentDied(workspaceID string) {
	if h.userBroker != nil {
		h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:        "agent_died",
			WorkspaceID: workspaceID,
			Data:        map[string]string{"reason": "unknown"},
		})
		// M2 (worklog 371): also publish via the user channel (which has a
		// replay buffer) so a frontend that reconnects AFTER the agent died
		// still receives the event. publishWorkspaceEvent → PublishToWorkspace
		// has no replay buffer; without this dual-publish (mirroring
		// onSessionIdle/onSessionActive), a reconnecting user sees no warning
		// and believes the workspace is healthy.
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "agent_died",
				WorkspaceID: workspaceID,
				Data:        map[string]string{"reason": "unknown"},
			})
		}
	}
}

func (h *ProxyHandler) emitNormalizedInputEvent(workspaceID, eventType, rawData string) {
	if h.userBroker == nil {
		return
	}
	var envelope struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(rawData), &envelope); err != nil || len(envelope.Properties) == 0 {
		return
	}
	properties := envelope.Properties

	if h.dialect.IsQuestionAsked(eventType) {
		req, err := h.dialect.ParseQuestionRequest(eventType, properties)
		if err != nil {
			h.logger.Warn("Failed to parse question event", "error", err, "workspaceID", workspaceID)
			return
		}
		req.RootSessionID = h.resolveRootSessionID(workspaceID, req.SessionID)
		h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "agent.question",
			SessionID: req.SessionID,
			RequestID: req.ID,
			Data:      req,
		})
	} else if h.dialect.IsQuestionResolved(eventType) {
		var resolution struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(properties, &resolution) //nolint:errcheck // best-effort parse; nil fields produce empty strings in the event
		h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "agent.question.resolved",
			SessionID: resolution.SessionID,
			RequestID: resolution.ID,
			Data: map[string]string{
				"request_id": resolution.ID,
				"session_id": resolution.SessionID,
			},
		})
	} else if h.dialect.IsPermissionAsked(eventType) {
		req, err := h.dialect.ParsePermissionRequest(eventType, properties)
		if err != nil {
			h.logger.Warn("Failed to parse permission event", "error", err, "workspaceID", workspaceID)
			return
		}

		if h.shouldAutoApprovePermissions(context.Background(), workspaceID) {
			go h.autoApprovePermission(workspaceID, req.ID)
			return
		}

		req.RootSessionID = h.resolveRootSessionID(workspaceID, req.SessionID)
		h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "agent.permission",
			SessionID: req.SessionID,
			RequestID: req.ID,
			Data:      req,
		})
	} else if h.dialect.IsPermissionResolved(eventType) {
		var resolution struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Reply     string `json:"reply"`
		}
		_ = json.Unmarshal(properties, &resolution) //nolint:errcheck // best-effort parse; nil fields produce empty strings in the event
		h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "agent.permission.resolved",
			SessionID: resolution.SessionID,
			RequestID: resolution.ID,
			Data: map[string]string{
				"request_id": resolution.ID,
				"session_id": resolution.SessionID,
				"reply":      resolution.Reply,
			},
		})
	}
}

func (h *ProxyHandler) resolveRootSessionID(workspaceID, sessionID string) string {
	if h.sessionParents == nil || sessionID == "" {
		return sessionID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return h.sessionParents.resolveRoot(ctx, workspaceID, sessionID)
}

func (h *ProxyHandler) persistTitleFromEvent(workspaceID, rawData string) {
	var evt struct {
		Properties struct {
			SessionID string `json:"sessionID"`
			Info      struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				ParentID string `json:"parentID"`
			} `json:"info"`
		} `json:"properties"`
	}
	if json.Unmarshal([]byte(rawData), &evt) != nil {
		return
	}
	id := evt.Properties.Info.ID
	if id == "" {
		return
	}
	if h.isSessionDeleted(workspaceID, id) {
		return
	}
	if evt.Properties.Info.Title != "" {
		if err := h.sessionIndex.UpsertTitle(context.Background(), workspaceID, id, evt.Properties.Info.Title); err != nil {
			h.logger.Warn("Failed to upsert session title", "error", err, "workspaceID", workspaceID, "sessionID", id)
		}
	}
	if evt.Properties.Info.ParentID != "" {
		if err := h.sessionIndex.UpsertParent(context.Background(), workspaceID, id, evt.Properties.Info.ParentID); err != nil {
			h.logger.Warn("Failed to upsert session parent", "error", err, "workspaceID", workspaceID, "sessionID", id)
		}
	}
}

func (h *ProxyHandler) persistContextFromEvent(workspaceID, rawData string) {
	if h.sessionIndex == nil {
		return
	}
	var evt struct {
		Properties struct {
			SessionID string `json:"sessionID"`
			Tokens    *struct {
				Input int64 `json:"input"`
				Cache struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(rawData), &evt); err != nil {
		h.logger.Warn("persistContextFromEvent: failed to parse step.ended event",
			"error", err, "workspaceID", workspaceID)
		return
	}
	if evt.Properties.SessionID == "" {
		h.logger.Warn("persistContextFromEvent: step.ended event missing sessionID",
			"workspaceID", workspaceID)
		return
	}
	if evt.Properties.Tokens == nil {
		h.logger.Warn("persistContextFromEvent: step.ended event missing tokens — opencode wire shape may have changed",
			"workspaceID", workspaceID, "sessionID", evt.Properties.SessionID)
		return
	}
	if h.isSessionDeleted(workspaceID, evt.Properties.SessionID) {
		return
	}
	promptTokens := evt.Properties.Tokens.Input +
		evt.Properties.Tokens.Cache.Read +
		evt.Properties.Tokens.Cache.Write
	if err := h.sessionIndex.UpsertContextUsed(context.Background(), workspaceID, evt.Properties.SessionID, promptTokens); err != nil {
		h.logger.Warn("Failed to upsert session context usage", "error", err, "workspaceID", workspaceID, "sessionID", evt.Properties.SessionID)
	}
}

func (h *ProxyHandler) getPodIPForSSE(workspaceID string) string {
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

type queueUpdateData struct {
	Event     string `json:"event"`
	MessageID string `json:"messageID"`
	Error     string `json:"error,omitempty"`
}

func (h *ProxyHandler) publishQueueEvent(workspaceID, sessionID, event, messageID, errMsg string) {
	if h.userBroker == nil {
		return
	}
	data := queueUpdateData{
		Event:     event,
		MessageID: messageID,
	}
	if errMsg != "" {
		data.Error = errMsg
	}
	h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
		Type:      "queue.update",
		SessionID: sessionID,
		Data:      data,
	})
}

// reconcileSessionState is called by the SSE tracker's onReconnect callback
// each time the tracker establishes a new connection to the workspace pod.
// It queries /v1/statusz on the agentd admin port to get the current session
// states and reconciles stale activeSess entries: a session is idle in
// opencode (per statusz) but still marked active in our local activeSess map.
// This happens when opencode dies (OOM/SIGTERM) mid-stream — the
// session.status=idle event is never emitted, so onSessionIdle is never
// called, and our local map keeps the session marked busy forever. Without
// this fix, POST to a stuck session returns 409 Conflict indefinitely
// (until API restart). See incident report 2026-06-16 (sessions
// ses_13076538bffeYtLrhoZ2ccRM1E and ses_130c14344ffeVF52UQ6QGPmB0P stuck
// after pod OOMKill).
//
// Under Epic 63 V2, this also wakes idle sessions with pending
// queue-delivered input that stranded during a pod restart.
func (h *ProxyHandler) reconcileSessionState(workspaceID, podIP, password string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/v1/statusz", podIP, agentd.AgentdAdminPort) //nolint:gosec // G107: internal pod
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.logger.Debug("reconcileSessionState: failed to build statusz request", "workspaceID", workspaceID, "error", err)
		return
	}
	if password != "" {
		req.Header.Set("Authorization", "Bearer "+password)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.logger.Debug("reconcileSessionState: statusz unavailable", "workspaceID", workspaceID, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		h.logger.Debug("reconcileSessionState: unexpected statusz status", "workspaceID", workspaceID, "status", resp.StatusCode)
		return
	}

	var statusz agentd.StatuszResponse
	// 1 MB cap mirrors the #801 fix (proxy_connections.go): statusz embeds
	// one entry per session in opencode's DB; >~55 sessions overflowed the
	// old 16 KB LimitReader, silently no-op'ing this reconcile (#892 D2 —
	// stale activeSess entries persisted client-side as phantom-busy).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&statusz); err != nil {
		h.logger.Debug("reconcileSessionState: failed to decode statusz", "workspaceID", workspaceID, "error", err)
		return
	}

	for _, sess := range statusz.Sessions {
		if sess.Status != "idle" {
			continue
		}

		// Reconcile stale activeSess entries: opencode says idle, but our
		// local map says active. This is the OOM/SIGTERM case — clean up
		// regardless of whether there are queued messages.
		if h.isSessionActive(ctx, workspaceID, sess.ID) {
			h.logger.Info("reconcileSessionState: clearing stale activeSess entry",
				"workspaceID", workspaceID, "sessionID", sess.ID,
				"reason", "session is idle in opencode but marked active locally")
			h.removeActiveSession(ctx, workspaceID, sess.ID)
			// Publish session.status=idle so connected clients update their UI.
			// Without this, browsers showing the session keep their busy
			// indicator until the next page reload.
			if h.userBroker != nil {
				h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
					Type:      "session.status",
					SessionID: sess.ID,
					Status:    "idle",
				})
			}
		}
	}

	// Build the busy-session set for wakeStrandedV2Sessions so it can
	// skip sessions that are mid-turn (prevents concurrent turns — #744 F1).
	busySessions := make(map[string]bool, len(statusz.Sessions))
	for _, sess := range statusz.Sessions {
		if sess.Status != "idle" {
			busySessions[sess.ID] = true
		}
	}

	// US-63.9: wake idle sessions with pending queue-delivered input
	// that stranded during a pod restart. The wake triggers
	// execution.wake → runner.run → drains durable SessionInput rows.
	h.wakeStrandedV2Sessions(ctx, workspaceID, busySessions)
}

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
	"github.com/lenaxia/llmsafespaces/pkg/session"
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
		// into Active (e.g. Creating → Active, Resuming → Active).
		//
		// #902: arm on EVERY Active event, transition or not. Prior-phase
		// state is Redis-backed and survives API restarts, so the
		// post-restart seed used to take the no-transition path and skip
		// arming entirely; watches that later died (pod churn, idle
		// drops) had no re-arm path — workspaces went silently
		// event-blind while sends and turns kept working. EnsureWatching
		// is an idempotent map check — arming when already armed costs
		// nothing, and it must NOT be preceded by StopWatching in the
		// no-transition case or every activity-driven status update
		// would tear down a healthy connection.
		if !hadPrior || prior != string(phaseActive) {
			// Real transition (or first sighting): full cache reset +
			// a fresh tracker connection — the old one targets the
			// previous pod.
			h.invalidateCaches(context.Background(), workspace.Name)
			if h.sseTracker != nil {
				h.sseTracker.StopWatching(workspace.Name)
			}
		} else {
			// No-transition update: only cached config (the original
			// else-branch semantics — do NOT nuke password/parent
			// caches on activity-driven status updates).
			h.state().InvalidateWorkspaceConfig(context.Background(), workspace.Name)
		}
		if h.sseTracker != nil {
			h.sseTracker.EnsureWatching(workspace.Name)
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

	// Live context-usage persistence: the adapter decides which raw agent
	// events carry usage. Non-usage events cost string compares inside the
	// seam — onRawEvent already JSON-parses every event for the broker
	// relay above, so no new per-event parse is introduced for them.
	h.persistContextFromEvent(workspaceID, eventType, rawData)

	// US-65.8 client bridge: translate agent events into CONTRACT events
	// and publish them for browsers/SDKs. Clients consume contract shapes
	// only; the raw agent.event relay above remains for non-client
	// consumers. Title/parent persistence rides the translated session
	// update (the last raw agent-shape parse in platform code retires).
	h.publishClientEvents(workspaceID, eventType, rawData)

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
	// Deprecated raw-parse path retained for the adapter-nil transitional
	// state only; superseded by persistSessionMeta from translated events.
	if h.adapter != nil {
		return
	}
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
	if h.sessionIndex == nil || id == "" || h.isSessionDeleted(workspaceID, id) {
		return
	}
	h.persistSessionMeta(context.Background(), workspaceID, id, evt.Properties.Info.Title, evt.Properties.Info.ParentID)
}

// publishClientEvents is the US-65.8 bridge: contract events to the
// broker, retry detail to the platform session.status channel, and
// title/parent persistence from the translated session update.
func (h *ProxyHandler) publishClientEvents(workspaceID, eventType, rawData string) {
	if h.adapter == nil {
		return
	}
	for _, ce := range h.adapter.ClientEventsFromEvent(eventType, rawData) {
		if ce.Type == session.EventSessionUpdated && ce.Session != nil &&
			h.sessionIndex != nil && !h.isSessionDeleted(workspaceID, ce.Session.ID) {
			h.persistSessionMeta(context.Background(), workspaceID, ce.Session.ID, ce.Session.Title, ce.Session.ParentID)
		}
		h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "session.event",
			SessionID: ce.SessionID,
			Data:      ce,
		})
	}
	if sid, retry, ok := h.adapter.RetryFromEvent(eventType, rawData); ok {
		h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
			Type:      "session.status",
			SessionID: sid,
			Status:    "retry",
			Data:      retry,
		})
	}
}

func (h *ProxyHandler) persistContextFromEvent(workspaceID, eventType, rawData string) {
	if h.sessionIndex == nil {
		return
	}
	if h.adapter == nil {
		h.logger.Debug("persistContextFromEvent: no adapter configured — context usage not persisted",
			"workspaceID", workspaceID)
		return
	}
	sessionID, usage, ok := h.adapter.ContextUsageFromEvent(eventType, rawData)
	if !ok {
		return
	}
	if sessionID == "" || usage == nil {
		// Non-conforming adapter return (interface doc guarantees both
		// non-empty/non-nil on ok=true). Guard the SSE event path against
		// a panic and surface it — a silent skip would hide a seam bug.
		h.logger.Warn("persistContextFromEvent: usage event missing sessionID",
			"workspaceID", workspaceID, "eventType", eventType)
		return
	}
	if h.isSessionDeleted(workspaceID, sessionID) {
		return
	}
	if err := h.sessionIndex.UpsertContextUsed(context.Background(), workspaceID, sessionID, usage.Used); err != nil {
		h.logger.Warn("Failed to upsert session context usage", "error", err, "workspaceID", workspaceID, "sessionID", sessionID)
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
	bearers := h.adminBearerCandidates(ctx, workspaceID, password)
	resp, err := GetWithBearers(ctx, h.httpClient, url, bearers)
	if err != nil {
		h.logger.Warn("reconcileSessionState: statusz unavailable", "workspaceID", workspaceID, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		h.logger.Warn("reconcileSessionState: unexpected statusz status", "workspaceID", workspaceID, "status", resp.StatusCode)
		return
	}

	var statusz agentd.StatuszResponse
	// 1 MB cap mirrors the #801 fix (proxy_connections.go): statusz embeds
	// one entry per session in opencode's DB; >~55 sessions overflowed the
	// old 16 KB LimitReader, silently no-op'ing this reconcile (#892 D2 —
	// stale activeSess entries persisted client-side as phantom-busy).
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&statusz); err != nil {
		h.logger.Warn("reconcileSessionState: failed to decode statusz", "workspaceID", workspaceID, "error", err)
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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const adapterRetryAfterSec = 10

// resolveWorkspaceForAdapter performs the workspace-readiness check
// shared between the adapter path and the legacy proxy path. It fetches
// the workspace CRD, verifies the workspace is Active with a PodIP, and
// enforces the per-workspace connection limit.
//
// Returns the workspace CRD and true on success. On failure, it writes
// the appropriate HTTP error (503 not-ready, 429 connection-limit,
// 404 not-found, 500 internal) and returns nil, false.
func (h *ProxyHandler) resolveWorkspaceForAdapter(c *gin.Context, workspaceID string) (*v1.Workspace, bool) {
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace ID required"})
		return nil, false
	}

	var workspace *v1.Workspace
	if cached, exists := c.Get("workspace"); exists {
		if sb, ok := cached.(*v1.Workspace); ok {
			workspace = sb
		}
	}
	if workspace == nil {
		v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
		if v1Err != nil {
			h.logger.Error("Failed to get LLMSafespacesV1 client", v1Err, "workspaceID", workspaceID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return nil, false
		}
		var err error
		workspace, err = v1Client.Workspaces(h.namespace).Get(c.Request.Context(), workspaceID, metav1.GetOptions{})
		if err != nil {
			h.logger.Error("Failed to get workspace CRD", err, "workspaceID", workspaceID)
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return nil, false
		}
	}

	if workspace.Status.Phase != phaseActive || workspace.Status.PodIP == "" {
		c.Header("Retry-After", fmt.Sprintf("%d", adapterRetryAfterSec))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "workspace not ready",
			"phase":      workspace.Status.Phase,
			"retryAfter": adapterRetryAfterSec,
		})
		return nil, false
	}

	if !h.acquireConnection(workspaceID) {
		c.Header("Retry-After", fmt.Sprintf("%d", adapterRetryAfterSec))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":      "connection limit reached",
			"retryAfter": adapterRetryAfterSec,
		})
		return nil, false
	}

	return workspace, true
}

// checkAdapterSessionLimit enforces the MaxActiveSessions limit for
// write operations. Returns true on success. On failure, writes a 429
// and returns false. Must be called AFTER resolveWorkspaceForAdapter
// succeeded and the connection slot was acquired.
func (h *ProxyHandler) checkAdapterSessionLimit(c *gin.Context, workspace *v1.Workspace, workspaceID, sessionID string) bool {
	if sessionID == "" {
		return true
	}
	maxSessions := int(workspace.Spec.MaxActiveSessions)
	if maxSessions <= 0 {
		maxSessions = defaultMaxActiveSessions
	}
	if !h.checkAndAddActiveSession(c.Request.Context(), workspaceID, sessionID, maxSessions) {
		c.Header("Retry-After", fmt.Sprintf("%d", adapterRetryAfterSec))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":             "active session limit reached",
			"maxActiveSessions": maxSessions,
			"retryAfter":        adapterRetryAfterSec,
		})
		return false
	}
	return true
}

// checkAdapterQuota verifies the caller has not exceeded their LLM
// request quota. Returns true if the request should proceed. On failure,
// writes a 429 and returns false.
func (h *ProxyHandler) checkAdapterQuota(c *gin.Context, workspace *v1.Workspace) bool {
	if h.meteringSvc == nil {
		return true
	}
	return h.checkProxyQuota(c, workspace)
}

// postAdapterSuccess performs the cross-cutting post-success side
// effects shared between the adapter path and the legacy proxy path:
// activity tracking, session-index message recording, and metering.
func (h *ProxyHandler) postAdapterSuccess(c *gin.Context, workspace *v1.Workspace, workspaceID, sessionID string, isWriteOp bool) {
	if h.activityTracker != nil {
		h.activityTracker.Record(workspaceID)
	}

	if h.sessionIndex != nil && sessionID != "" && isWriteOp {
		h.sessionIndex.RecordMessage(workspaceID, sessionID, "", time.Now())
	}

	if h.meteringSvc != nil && workspaceID != "" {
		userID, _ := extractAuth(c)
		if userID != "" && workspace.Labels["llmsafespaces.dev/canary"] != "true" {
			h.meteringSvc.Record(types.UsageEvent{
				IdempotencyKey: fmt.Sprintf("llmreq:%s:%d", workspaceID, time.Now().UnixNano()),
				Owner:          types.BillingOwner{ID: userID, Type: types.OwnerTypeUser},
				ActorID:        userID,
				WorkspaceID:    workspaceID,
				EventType:      "llm_request",
				EventSubtype:   "message",
				Quantity:       1,
				Source:         "api",
				EventTime:      time.Now(),
				RequestContext: map[string]any{
					"ip":         c.ClientIP(),
					"request_id": c.GetString("request_id"),
					"session_id": sessionID,
				},
			})
		}
	}
}

// adapterEnsureSSEWatch arms the busy-gated usage stream for the
// workspace (adapter write ops may start a turn — billing and the state
// bridge must see it; the gate drops itself after the idle window).
func (h *ProxyHandler) adapterEnsureSSEWatch(workspaceID string) {
	if h.agentdTerminus {
		h.UsageStream().Open(workspaceID)
	}
}

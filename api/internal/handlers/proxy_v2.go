// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// v2Client constructs an opencode V2 client targeting the workspace's pod.
// Uses the same pod-IP + password resolution as every other opencode call
// (getPodIPAndPassword). Returns an error if the workspace is not Active.
//
// This file is a known leak in the agent-import boundary (repolint
// agentImportKnownLeaks) — it imports pkg/agent/opencode/ to call the
// concrete V2 client. Retires when US-65.4 migrates the proxy handlers to
// the agent.Adapter interface.
func (h *ProxyHandler) v2Client(ctx context.Context, workspaceID string) (*opencode.Client, error) {
	podIP, password, err := h.getPodIPAndPassword(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	baseURL := fmt.Sprintf("http://%s:%d", podIP, agentd.AgentPort)
	// Do NOT inject h.httpClient — it has no timeout (shared with streaming
	// SSE proxying). V2 calls are fast request-response (POST prompt → 200,
	// POST interrupt → 204); the opencode.NewClient default (10s timeout)
	// is the right fit.
	return opencode.NewClient(baseURL, password, nil), nil
}

// enqueueV2 sends a prompt to opencode's V2 session API with
// delivery:"queue". Emits the same queue.update/enqueued SSE as the V1
// Redis path so the frontend behavior is unchanged (US-63.5 may later
// derive the event from V2's PromptAdmitted; for now the proxy emits it
// on success).
//
// Returns true if the V2 path handled the request (caller should return
// from the gin handler). Returns false if the V2 path was not taken
// (flag off) so the caller falls through to the legacy V1 path.
func (h *ProxyHandler) enqueueV2(c *gin.Context, wid, sid, text string) bool {
	if !h.v2SessionQueueEnabled {
		return false
	}
	client, err := h.v2Client(c.Request.Context(), wid)
	if err != nil {
		h.logger.Error("V2 enqueue: failed to construct client", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach workspace"})
		return true
	}
	resp, err := client.PromptV2(c.Request.Context(), sid, text, opencode.V2DeliveryQueue)
	if err != nil {
		if errors.Is(err, opencode.ErrV2SessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return true
		}
		h.logger.Error("V2 enqueue: PromptV2 failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue message"})
		return true
	}
	h.publishQueueEvent(wid, sid, "enqueued", resp.ID, "")
	c.JSON(http.StatusAccepted, gin.H{"messageID": resp.ID})
	return true
}

// abortV2 sends a non-destructive interrupt to opencode's V2 session API.
// The queued messages survive and drain on the next execution.wake (F8).
// No Redis queue mutation, no dismissed SSE, no flushAndAbortAfterIdle.
//
// Returns true if the V2 path handled the request. Returns false if the
// V2 path was not taken (flag off).
func (h *ProxyHandler) abortV2(c *gin.Context, wid, sid string) bool {
	if !h.v2SessionQueueEnabled {
		return false
	}
	client, err := h.v2Client(c.Request.Context(), wid)
	if err != nil {
		h.logger.Error("V2 abort: failed to construct client", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach workspace"})
		return true
	}
	if err := client.InterruptV2(c.Request.Context(), sid); err != nil {
		h.logger.Error("V2 abort: InterruptV2 failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abort session"})
		return true
	}
	c.Status(http.StatusNoContent)
	return true
}

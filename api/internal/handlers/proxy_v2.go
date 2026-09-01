// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// V2SessionClient is re-exported from pkg/agent (the canonical location).
// Defined as an interface so tests inject a client targeting a dynamic-port
// httptest.Server instead of the hardcoded port 4096.
type V2SessionClient = agent.V2SessionClient

// V2ClientFactory builds a V2SessionClient for the given workspace.
type V2ClientFactory = agent.V2ClientFactory

// SetV2ClientFactory overrides V2 client construction. Used by tests to
// inject a client targeting a dynamic-port httptest.Server.
func (h *ProxyHandler) SetV2ClientFactory(f V2ClientFactory) {
	h.v2ClientFactory = f
}

// SetV2ClientConcreteFactory sets the factory that builds a V2SessionClient
// from a baseURL + password. app.go wires this with opencode.NewClient so
// this file does not import the opencode package.
func (h *ProxyHandler) SetV2ClientConcreteFactory(f func(baseURL, password string) (agent.V2SessionClient, error)) {
	h.v2ClientConcreteFactory = f
}

func (h *ProxyHandler) v2Client(ctx context.Context, workspaceID string) (V2SessionClient, error) {
	if h.v2ClientFactory != nil {
		return h.v2ClientFactory(ctx, workspaceID)
	}
	podIP, password, err := h.getPodIPAndPassword(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	baseURL := fmt.Sprintf("http://%s:%d", podIP, agentd.AgentPort)
	// Use a factory set during wiring (app.go) — this file must not
	// import pkg/agent/opencode.
	if h.v2ClientConcreteFactory != nil {
		return h.v2ClientConcreteFactory(baseURL, password)
	}
	return nil, fmt.Errorf("V2 client factory not configured")
}

// ---------------------------------------------------------------------------
// US-63.5: SSE Event Bridge — V2 events → queue.update
// ---------------------------------------------------------------------------

// Spike-verified V2 event wire types (worklog NNNN_us-63.1-v2-spike, F14):
//
//	session.next.prompt.admitted → queue.update/enqueued
//	session.next.prompted        → queue.update/sent
//
// Both carry properties.{messageID, sessionID, delivery}. Only
// delivery:"queue" inputs are bridged — delivery:"steer" inputs are
// mid-turn injections, not queue entries the frontend tracks as pills.
const (
	v2EventPromptAdmitted = "session.next.prompt.admitted"
	v2EventPrompted       = "session.next.prompted"
)

// ---------------------------------------------------------------------------
// US-63.9: Stranded-Input Recovery — wake on reconnect
// ---------------------------------------------------------------------------

const (
	v2EventStepEnded  = "session.next.step.ended"
	v2EventStepFailed = "session.next.step.failed"
)

// ---------------------------------------------------------------------------
// US-63.3: Enqueue path (delivery:queue)
// ---------------------------------------------------------------------------

// enqueueV2 sends a prompt to opencode's V2 session API with
// delivery:"queue".
//
// Under US-63.5: the queue.update/enqueued SSE event is NO LONGER emitted
// here — it is derived from the V2 PromptAdmitted event in onV2RawEvent.
// This eliminates the race where enqueued fires before opencode has
// actually admitted the input. The response still returns the messageID
// synchronously for callers that need it.
func (h *ProxyHandler) enqueueV2(c *gin.Context, wid, sid, text string) {
	client, err := h.v2Client(c.Request.Context(), wid)
	if err != nil {
		h.logger.Error("V2 enqueue: failed to construct client", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach workspace"})
		return
	}
	resp, err := client.PromptV2(c.Request.Context(), sid, text, agent.V2DeliveryQueue)
	if err != nil {
		if agent.IsSessionNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		h.logger.Error("V2 enqueue: PromptV2 failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue message"})
		return
	}
	// US-63.5: the enqueued pill derives from the outbox's staging event;
	// under the terminus the ledger owns stranded-input recovery.
	c.JSON(http.StatusAccepted, gin.H{"messageID": resp.ID})
}

// ---------------------------------------------------------------------------
// US-63.4: Abort path (non-destructive interrupt)
// ---------------------------------------------------------------------------

// abortV2 sends a non-destructive interrupt to opencode's V2 session API.
// The queued messages survive and drain on the next execution.wake (F8).
func (h *ProxyHandler) abortV2(c *gin.Context, wid, sid string) {
	client, err := h.v2Client(c.Request.Context(), wid)
	if err != nil {
		h.logger.Error("V2 abort: failed to construct client", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach workspace"})
		return
	}
	if err := client.InterruptV2(c.Request.Context(), sid); err != nil {
		h.logger.Error("V2 abort: InterruptV2 failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abort session"})
		return
	}
	c.Status(http.StatusNoContent)
}

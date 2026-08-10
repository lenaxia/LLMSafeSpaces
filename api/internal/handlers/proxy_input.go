// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

var (
	questionIDPattern   = regexp.MustCompile(`^que_[a-zA-Z0-9]+$`)
	permissionIDPattern = regexp.MustCompile(`^per_[a-zA-Z0-9_]+$`)
)

// ListQuestions proxies GET /question to the workspace pod.
func (h *ProxyHandler) ListQuestions(c *gin.Context) {
	if h.dialect == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dialect not configured"})
		return
	}
	h.proxyToWorkspace(c, h.dialect.QuestionListPath(), false, "")
}

// QuestionReply proxies POST /question/:requestID/reply to the workspace pod.
func (h *ProxyHandler) QuestionReply(c *gin.Context) {
	if h.dialect == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dialect not configured"})
		return
	}
	requestID := c.Param("requestID")
	if !questionIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid question request ID format"})
		return
	}
	h.proxyToWorkspace(c, h.dialect.QuestionReplyPath(requestID), false, "")
}

// QuestionReject proxies POST /question/:requestID/reject to the workspace pod.
func (h *ProxyHandler) QuestionReject(c *gin.Context) {
	if h.dialect == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dialect not configured"})
		return
	}
	requestID := c.Param("requestID")
	if !questionIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid question request ID format"})
		return
	}
	h.proxyToWorkspace(c, h.dialect.QuestionRejectPath(requestID), false, "")
}

// ListPermissions proxies GET /permission to the workspace pod.
func (h *ProxyHandler) ListPermissions(c *gin.Context) {
	if h.dialect == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dialect not configured"})
		return
	}
	h.proxyToWorkspace(c, h.dialect.PermissionListPath(), false, "")
}

// PermissionReply proxies POST /permission/:requestID/reply to the workspace pod.
func (h *ProxyHandler) PermissionReply(c *gin.Context) {
	if h.dialect == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "dialect not configured"})
		return
	}
	requestID := c.Param("requestID")
	if !permissionIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission request ID format"})
		return
	}
	h.proxyToWorkspace(c, h.dialect.PermissionReplyPath(requestID), false, "")
}

// emitPendingInputRequests fetches pending questions and permissions from the pod
// and publishes them as synthetic events so reconnecting browsers see them immediately.
//
// Accepts the parent's context (typically `c.Request.Context()` for the
// SSE/snapshot path) so contextcheck is happy and a client disconnect
// cancels the in-flight pod fetch promptly. Internally derives a 5s
// timeout from the parent to keep the bounded-per-call cap.
func (h *ProxyHandler) emitPendingInputRequests(ctx context.Context, workspaceID string) {
	// D9: emit the snapshot-complete marker unconditionally on exit, even on
	// timeout/error. This lets the provider commit (or clear) the workspace's
	// pending set without hanging. On timeout, staging is empty -> the provider
	// commits empty (pending cleared for this workspace).
	defer func() {
		if h.userBroker == nil {
			return
		}
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "agent.input.snapshot_complete",
				WorkspaceID: workspaceID,
			})
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Adapter path (US-65.4): unified ListPending returns both questions
	// and permissions in one call, already typed as session.InputRequest.
	// The legacy path does two separate fetchFromPod calls + dialect-based
	// parsing.
	if h.adapter != nil {
		h.emitPendingViaAdapter(ctx, workspaceID)
		return
	}

	// Legacy path.
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil || workspace.Status.Phase != phaseActive || workspace.Status.PodIP == "" {
		return
	}

	password, err := h.getPassword(ctx, workspaceID)
	if err != nil {
		return
	}

	podIP := workspace.Status.PodIP

	// Fetch and emit pending questions
	if body, err := h.fetchFromPod(ctx, podIP, password, h.dialect.QuestionListPath()); err == nil {
		for _, req := range h.parseQuestionList(body) {
			if h.sessionParents != nil {
				req.RootSessionID = h.sessionParents.resolveRoot(ctx, workspaceID, req.SessionID)
			} else {
				req.RootSessionID = req.SessionID
			}
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.question",
				SessionID: req.SessionID,
				RequestID: req.ID,
				Data:      req,
			})
		}
	}

	// Fetch and emit pending permissions (only if not auto-approving)
	if !h.shouldAutoApprovePermissions(ctx, workspaceID) {
		if body, err := h.fetchFromPod(ctx, podIP, password, h.dialect.PermissionListPath()); err == nil {
			for _, req := range h.parsePermissionList(body) {
				if h.sessionParents != nil {
					req.RootSessionID = h.sessionParents.resolveRoot(ctx, workspaceID, req.SessionID)
				} else {
					req.RootSessionID = req.SessionID
				}
				h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
					Type:      "agent.permission",
					SessionID: req.SessionID,
					RequestID: req.ID,
					Data:      req,
				})
			}
		}
	}
}

// emitPendingViaAdapter uses the Adapter's ListPending to fetch pending
// input requests in a single call, then publishes them as SSE events.
// Converts session.InputRequest to the legacy agent.QuestionRequest /
// agent.PermissionRequest shapes the SSE consumers expect.
func (h *ProxyHandler) emitPendingViaAdapter(ctx context.Context, workspaceID string) {
	pending, err := h.adapter.ListPending(ctx, "", workspaceID, "")
	if err != nil {
		return
	}
	autoApprove := h.shouldAutoApprovePermissions(ctx, workspaceID)

	for _, ir := range pending {
		if ir.Kind == session.InputPermission && autoApprove {
			continue
		}
		rootSession := ir.SessionID
		if h.sessionParents != nil {
			rootSession = h.sessionParents.resolveRoot(ctx, workspaceID, ir.SessionID)
		}
		switch ir.Kind {
		case session.InputQuestion:
			questionInfo := agent.QuestionInfo{
				Question: ir.Question,
				Header:   ir.Header,
				Multiple: ir.Multiple,
				Custom:   ir.Custom,
			}
			for _, o := range ir.Options {
				questionInfo.Options = append(questionInfo.Options,
					agent.QuestionOption{Label: o.Label, Description: o.Description})
			}
			req := &agent.QuestionRequest{
				ID:            ir.ID,
				SessionID:     ir.SessionID,
				RootSessionID: rootSession,
				Questions:     []agent.QuestionInfo{questionInfo},
			}
			if ir.Tool != nil {
				req.Tool = &agent.ToolRef{
					MessageID: ir.Tool.MessageID,
					CallID:    ir.Tool.CallID,
				}
			}
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.question",
				SessionID: ir.SessionID,
				RequestID: ir.ID,
				Data:      req,
			})
		case session.InputPermission:
			req := &agent.PermissionRequest{
				ID:            ir.ID,
				SessionID:     ir.SessionID,
				RootSessionID: rootSession,
				Permission:    ir.Permission,
				Patterns:      ir.Patterns,
				Always:        ir.Always,
			}
			if ir.Tool != nil {
				req.Tool = &agent.ToolRef{
					MessageID: ir.Tool.MessageID,
					CallID:    ir.Tool.CallID,
				}
			}
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.permission",
				SessionID: ir.SessionID,
				RequestID: ir.ID,
				Data:      req,
			})
		}
	}
}

// fetchFromPod makes a GET request to the workspace pod.
func (h *ProxyHandler) fetchFromPod(ctx context.Context, podIP, password, path string) ([]byte, error) {
	url := fmt.Sprintf("http://%s:%d%s", podIP, opencodePort, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(agentd.AuthUsername, password)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.logger.Warn("Failed to fetch pending input requests", "error", err, "path", path)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// parseQuestionList parses the response from GET /question into normalized requests.
func (h *ProxyHandler) parseQuestionList(body []byte) []*agent.QuestionRequest {
	var raw []json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil
	}
	var results []*agent.QuestionRequest
	for _, r := range raw {
		if req, err := h.dialect.ParseQuestionRequest("question.asked", r); err == nil {
			results = append(results, req)
		}
	}
	return results
}

// parsePermissionList parses the response from GET /permission into normalized requests.
func (h *ProxyHandler) parsePermissionList(body []byte) []*agent.PermissionRequest {
	var raw []json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil
	}
	var results []*agent.PermissionRequest
	for _, r := range raw {
		if req, err := h.dialect.ParsePermissionRequest("permission.asked", r); err == nil {
			results = append(results, req)
		}
	}
	return results
}

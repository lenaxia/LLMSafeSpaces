// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// podSessionArchiver is the workspace-service surface the archive
// endpoint delegates to (production: interfaces.WorkspaceService).
type podSessionArchiver interface {
	SetSessionArchived(ctx context.Context, userID, workspaceID, sessionID string, archived bool) error
}

// podSessionProxyAPI is the proxy surface the delete endpoint rides
// (production: *ProxyHandler). HardDeleteSession is the same flow the
// REST DELETE uses — agent side AND platform index both die.
type podSessionProxyAPI interface {
	HardDeleteSession(ctx context.Context, workspaceID, sessionID string) error
	PublishSessionArchived(workspaceID, sessionID string, archived bool)
}

// podSessionLister feeds the archived-set read (production: the
// session-index service).
type podSessionLister interface {
	ListByWorkspace(ctx context.Context, workspaceID string) ([]types.SessionListItem, error)
}

// PodSessionArchiveHandler exposes session archive/unarchive, hard
// delete, and the archived set to THIS workspace's agent (the agentd
// MCP tools, #1627) over the pod-identity auth contract — identical to
// PodWorkspaceRenameHandler/PodAutomationHandler: TokenReview, SA
// name/namespace bound to the workspace, owner resolved server-side.
// A compromised pod gains nothing it didn't already have as its
// owner's agent (the owner can archive/delete via REST today).
type PodSessionArchiveHandler struct {
	tokenReviewer     TokenReviewer
	lookup            bootstrapWorkspaceLookup
	archiver          podSessionArchiver
	proxy             podSessionProxyAPI
	lister            podSessionLister
	expectedNamespace string
	logger            interfaces.LoggerInterface
}

// NewPodSessionArchiveHandler constructs the handler; the
// FromClientset variant wraps the shared TokenReview clientset.
func NewPodSessionArchiveHandler(reviewer TokenReviewer, lookup bootstrapWorkspaceLookup, archiver podSessionArchiver, proxy podSessionProxyAPI, lister podSessionLister, expectedNamespace string) *PodSessionArchiveHandler {
	return &PodSessionArchiveHandler{
		tokenReviewer:     reviewer,
		lookup:            lookup,
		archiver:          archiver,
		proxy:             proxy,
		lister:            lister,
		expectedNamespace: expectedNamespace,
	}
}

// NewPodSessionArchiveHandlerFromClientset is the production constructor.
func NewPodSessionArchiveHandlerFromClientset(clientset kubernetes.Interface, lookup bootstrapWorkspaceLookup, archiver podSessionArchiver, proxy podSessionProxyAPI, lister podSessionLister, expectedNamespace string) *PodSessionArchiveHandler {
	return NewPodSessionArchiveHandler(&k8sTokenReviewer{clientset: clientset}, lookup, archiver, proxy, lister, expectedNamespace)
}

// SetLogger installs the failure-path logger (pod-bootstrap #407 precedent).
func (h *PodSessionArchiveHandler) SetLogger(l interfaces.LoggerInterface) { h.logger = l }

// HasLogger reports logger wiring (app-level guard).
func (h *PodSessionArchiveHandler) HasLogger() bool { return h.logger != nil }

// resolve authenticates the pod and returns the workspace ID + owner
// userID. The workspace arrives in-body on the writes and in-query on
// the archived-set read; writes the failure response itself.
func (h *PodSessionArchiveHandler) resolve(c *gin.Context, workspaceID string) (string, bool) {
	token := extractBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
		return "", false
	}
	username, err := h.tokenReviewer.Review(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, errTokenNotAuthenticated) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token not authenticated"})
			return "", false
		}
		if h.logger != nil {
			h.logger.Error("session-archive: token review failed", err)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "token review failed"})
		return "", false
	}
	if exp, hasExp := unverifiedJWTExp(token); hasExp && time.Now().After(exp.Add(jwtExpiryLeeway)) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
		return "", false
	}
	if workspaceID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID required"})
		return "", false
	}
	saNamespace, saWorkspaceID, principalOK := parseSAPrincipal(username)
	if !principalOK || saWorkspaceID != workspaceID || saNamespace != h.expectedNamespace {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "workspace identity mismatch"})
		return "", false
	}
	ws, err := h.lookup.GetWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("session-archive: lookup failed", err, "workspaceID", workspaceID)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workspace lookup failed"})
		return "", false
	}
	if ws == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return "", false
	}
	return ws.UserID, true
}

// Archive handles POST /internal/v1/session-archive — the agentd
// session_archive MCP tool's platform half.
func (h *PodSessionArchiveHandler) Archive(c *gin.Context) {
	var req struct {
		WorkspaceID string `json:"workspaceID"`
		SessionID   string `json:"sessionID"`
		Archived    *bool  `json:"archived"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.WorkspaceID == "" || req.SessionID == "" || req.Archived == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID, sessionID, and archived (boolean) are required"})
		return
	}
	ownerID, ok := h.resolve(c, req.WorkspaceID)
	if !ok {
		return
	}
	if err := h.archiver.SetSessionArchived(c.Request.Context(), ownerID, req.WorkspaceID, req.SessionID, *req.Archived); err != nil {
		var apiErr *apierrors.APIError
		if errors.As(err, &apiErr) {
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{"error": apiErr.Error()})
			return
		}
		if h.logger != nil {
			h.logger.Error("session-archive: set failed", err, "workspaceID", req.WorkspaceID, "sessionID", req.SessionID)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to set archive state"})
		return
	}
	if h.proxy != nil {
		h.proxy.PublishSessionArchived(req.WorkspaceID, req.SessionID, *req.Archived)
	}
	c.Status(http.StatusNoContent)
}

// Delete handles POST /internal/v1/session-delete — the agentd
// delete_session MCP tool's platform half. HARD DELETE both sides via
// the same flow the REST DELETE rides.
func (h *PodSessionArchiveHandler) Delete(c *gin.Context) {
	var req struct {
		WorkspaceID string `json:"workspaceID"`
		SessionID   string `json:"sessionID"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.WorkspaceID == "" || req.SessionID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID and sessionID are required"})
		return
	}
	if _, ok := h.resolve(c, req.WorkspaceID); !ok {
		return
	}
	if h.proxy == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "delete unavailable"})
		return
	}
	if err := h.proxy.HardDeleteSession(c.Request.Context(), req.WorkspaceID, req.SessionID); err != nil {
		if h.logger != nil {
			h.logger.Error("session-delete: hard delete failed", err, "workspaceID", req.WorkspaceID, "sessionID", req.SessionID)
		}
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "failed to delete session"})
		return
	}
	c.Status(http.StatusNoContent)
}

// ArchivedSet handles GET /internal/v1/session-archived — the archived
// session IDs of this workspace (the agentd session_metadata annotation
// source).
func (h *PodSessionArchiveHandler) ArchivedSet(c *gin.Context) {
	workspaceID := c.Query("workspaceID")
	if _, ok := h.resolve(c, workspaceID); !ok {
		return
	}
	if h.lister == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "session index unavailable"})
		return
	}
	items, err := h.lister.ListByWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("session-archive: list failed", err, "workspaceID", workspaceID)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions"})
		return
	}
	archived := make([]string, 0)
	for _, it := range items {
		if it.Archived {
			archived = append(archived, it.ID)
		}
	}
	c.JSON(http.StatusOK, gin.H{"archived": archived})
}

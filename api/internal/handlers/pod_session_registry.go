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

	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// podSessionRegistry is the registration surface the endpoints below
// ride (production: the *database.Service — the single mint owner,
// design 0066 §4.2 rule 1: agentd reads or replicates, never mints).
type podSessionRegistry interface {
	EnsureSessionRegistered(ctx context.Context, workspaceID, sessionID, harnessType string) (string, error)
}

// podSessionRegistryLister feeds the boot/on-miss replica pulls.
type podSessionRegistryLister interface {
	ListSessionIndex(ctx context.Context, workspaceID string) ([]types.SessionListItem, error)
}

// PodSessionRegistryHandler exposes the short-id registry to THIS
// workspace's agentd over the pod-identity auth contract — identical
// to PodSessionArchiveHandler: TokenReview, SA name/namespace bound
// to the workspace, owner resolved server-side. agentd is a REPLICA:
// it pulls the mapping (boot + on-miss) and asks the control plane to
// register freshly created sessions; it never writes mappings.
type PodSessionRegistryHandler struct {
	tokenReviewer     TokenReviewer
	lookup            bootstrapWorkspaceLookup
	registry          podSessionRegistry
	lister            podSessionRegistryLister
	expectedNamespace string
	logger            interfaces.LoggerInterface
}

// NewPodSessionRegistryHandler constructs the handler; the
// FromClientset variant wraps the shared TokenReview clientset.
func NewPodSessionRegistryHandler(reviewer TokenReviewer, lookup bootstrapWorkspaceLookup, registry podSessionRegistry, lister podSessionRegistryLister, expectedNamespace string) *PodSessionRegistryHandler {
	return &PodSessionRegistryHandler{
		tokenReviewer:     reviewer,
		lookup:            lookup,
		registry:          registry,
		lister:            lister,
		expectedNamespace: expectedNamespace,
	}
}

// NewPodSessionRegistryHandlerFromClientset is the production constructor.
func NewPodSessionRegistryHandlerFromClientset(clientset kubernetes.Interface, lookup bootstrapWorkspaceLookup, registry podSessionRegistry, lister podSessionRegistryLister, expectedNamespace string) *PodSessionRegistryHandler {
	return NewPodSessionRegistryHandler(&k8sTokenReviewer{clientset: clientset}, lookup, registry, lister, expectedNamespace)
}

// SetLogger installs the failure-path logger (pod-bootstrap #407 precedent).
func (h *PodSessionRegistryHandler) SetLogger(l interfaces.LoggerInterface) { h.logger = l }

// HasLogger reports logger wiring (app-level guard).
func (h *PodSessionRegistryHandler) HasLogger() bool { return h.logger != nil }

// List handles GET /internal/v1/session-registry?workspaceID= — the
// agentd replica's boot + on-miss pull: every indexed session of the
// pod's own workspace with its short id (bounded by the list surface).
func (h *PodSessionRegistryHandler) List(c *gin.Context) {
	workspaceID := c.Query("workspaceID")
	if _, ok := h.resolve(c, workspaceID); !ok {
		return
	}
	if h.lister == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "session registry unavailable"})
		return
	}
	items, err := h.lister.ListSessionIndex(c.Request.Context(), workspaceID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("session-registry: list failed", err, "workspaceID", workspaceID)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to list session registry"})
		return
	}
	// The registry pull feeds the agentd replica's phrase→harness-id
	// mapping: `sessionID` is the CANONICAL harness id (InternalID —
	// the post-#1756 wire `ID` is the phrase and would poison the
	// replica phrase→phrase), `shortID` is the phrase. Rows without a
	// minted phrase stay unknown to the replica until registered —
	// the replica's install rejects an empty short id, so they are
	// skipped here rather than shipped as degenerate entries.
	out := make([]gin.H, 0, len(items))
	for _, it := range items {
		if it.ShortID == "" {
			continue
		}
		out = append(out, gin.H{"sessionID": it.InternalID, "shortID": it.ShortID})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": out})
}

// Register handles POST /internal/v1/session-register — the agentd
// create_session tool's platform half: register (mint-and-persist,
// idempotent) one session of the pod's own workspace and return its
// short id. The harness type is the calling pod's agent — opencode,
// the only live adapter today (design 0066 §4.2).
func (h *PodSessionRegistryHandler) Register(c *gin.Context) {
	var req struct {
		WorkspaceID string `json:"workspaceID"`
		SessionID   string `json:"sessionID"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.WorkspaceID == "" || req.SessionID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID and sessionID are required"})
		return
	}
	if err := validateSessionID(req.SessionID); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid sessionID: " + err.Error()})
		return
	}
	if _, ok := h.resolve(c, req.WorkspaceID); !ok {
		return
	}
	if h.registry == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "session registry unavailable"})
		return
	}
	shortID, err := h.registry.EnsureSessionRegistered(c.Request.Context(), req.WorkspaceID, req.SessionID, "opencode")
	if err != nil {
		if h.logger != nil {
			h.logger.Error("session-register: registration failed", err, "workspaceID", req.WorkspaceID, "sessionID", req.SessionID)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to register session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"shortID": shortID})
}

// resolve authenticates the pod and returns the workspace's owner —
// the shared pod-identity contract (TokenReview + SA principal bound
// to this workspace), the PodSessionArchiveHandler.resolve shape.
func (h *PodSessionRegistryHandler) resolve(c *gin.Context, workspaceID string) (string, bool) {
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
			h.logger.Error("session-registry: token review failed", err)
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
			h.logger.Error("session-registry: lookup failed", err, "workspaceID", workspaceID)
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

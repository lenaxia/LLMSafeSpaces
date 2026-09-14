// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
)

// workspaceNameMaxLength matches the workspaces.name column
// (character varying(255), migration 000001).
const workspaceNameMaxLength = 255

// podWorkspaceRenamer renames a workspace under a resolved owner. The
// production wiring passes the workspace Service; the interface exists so
// the handler's tests (and any future caller) do not need the full
// service graph.
type podWorkspaceRenamer interface {
	RenameWorkspace(ctx context.Context, userID, workspaceID, name string) error
}

// PodWorkspaceRenameHandler handles POST /internal/v1/workspace-rename —
// the pod-identity path for the agentd rename_workspace MCP tool. The
// workspace display name lives in PostgreSQL and is owned by the API;
// the pod cannot write it directly, so the agent asks the platform.
//
// Auth mirrors pod-bootstrap exactly: a projected SA token validated via
// K8s TokenReview (no JWT middleware — the agent has no user identity),
// the SA name must be workspace-<workspaceID> in the expected namespace,
// and the workspaceID in the body must match the SA-derived one. A
// compromised pod can therefore only rename its own workspace.
type PodWorkspaceRenameHandler struct {
	tokenReviewer     TokenReviewer
	lookup            bootstrapWorkspaceLookup
	renamer           podWorkspaceRenamer
	expectedNamespace string
}

// NewPodWorkspaceRenameHandler constructs the handler. reviewer is the
// TokenReview-backed validator (production: the same k8sTokenReviewer
// wrapping the API's K8s clientset that pod-bootstrap uses); lookup
// resolves the workspace to its owner (production: the DB service);
// renamer performs the rename (production: the workspace Service).
func NewPodWorkspaceRenameHandler(reviewer TokenReviewer, lookup bootstrapWorkspaceLookup, renamer podWorkspaceRenamer, expectedNamespace string) *PodWorkspaceRenameHandler {
	return &PodWorkspaceRenameHandler{
		tokenReviewer:     reviewer,
		lookup:            lookup,
		renamer:           renamer,
		expectedNamespace: expectedNamespace,
	}
}

// NewPodWorkspaceRenameHandlerFromClientset is the production constructor
// that wraps a kubernetes.Interface into the shared k8sTokenReviewer.
func NewPodWorkspaceRenameHandlerFromClientset(clientset kubernetes.Interface, lookup bootstrapWorkspaceLookup, renamer podWorkspaceRenamer, expectedNamespace string) *PodWorkspaceRenameHandler {
	return NewPodWorkspaceRenameHandler(&k8sTokenReviewer{clientset: clientset}, lookup, renamer, expectedNamespace)
}

// Rename handles POST /internal/v1/workspace-rename.
func (h *PodWorkspaceRenameHandler) Rename(c *gin.Context) {
	token := extractBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
		return
	}

	username, err := h.tokenReviewer.Review(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, errTokenNotAuthenticated) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token not authenticated"})
			return
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "token review failed"})
		return
	}
	// F4b defense-in-depth expiry check — same rationale as pod-bootstrap:
	// TokenReview does not enforce the mint's exp.
	if exp, ok := unverifiedJWTExp(token); ok && time.Now().After(exp.Add(jwtExpiryLeeway)) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
		return
	}

	var req struct {
		WorkspaceID string `json:"workspaceID"`
		Name        string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.WorkspaceID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID required"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if len(name) > workspaceNameMaxLength {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "name exceeds maximum length"})
		return
	}

	// S1: the token's SA identity must be this workspace in this
	// namespace — a pod can only rename its own workspace.
	saNamespace, saWorkspaceID, ok := parseSAPrincipal(username)
	if !ok || saWorkspaceID != req.WorkspaceID || saNamespace != h.expectedNamespace {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "workspace identity mismatch"})
		return
	}

	ws, err := h.lookup.GetWorkspace(c.Request.Context(), req.WorkspaceID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workspace lookup failed"})
		return
	}
	if ws == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	if err := h.renamer.RenameWorkspace(c.Request.Context(), ws.UserID, req.WorkspaceID, name); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to rename workspace"})
		return
	}
	c.Status(http.StatusNoContent)
}

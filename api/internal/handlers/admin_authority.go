// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// AuthorityFlipHandler serves the flip gate's operator endpoints
// (US-69.13, design 0055 M4): the drain-before-flip procedure is
// same-domain — ask the pod's ledger for its unresolved count, wait for
// zero or park the workspace's in-flight outbox entries with an explicit
// mode_transition reason, flip, and on rollback unpark (the ledger
// back-drains via the 0052 path). No cross-store verification anywhere.
type AuthorityFlipHandler struct {
	proxyHandler *ProxyHandler
	logger       pkginterfaces.LoggerInterface
}

func NewAuthorityFlipHandler(proxyHandler *ProxyHandler, logger pkginterfaces.LoggerInterface) *AuthorityFlipHandler {
	return &AuthorityFlipHandler{proxyHandler: proxyHandler, logger: logger}
}

type parkRequest struct {
	WorkspaceID string `json:"workspaceId" binding:"required"`
	Reason      string `json:"reason" binding:"required,max=200"`
}

// Park POST /api/v1/admin/authority/park — holds the workspace's
// in-flight outbox entries with the mode_transition reason. Idempotent.
func (h *AuthorityFlipHandler) Park(c *gin.Context) {
	var req parkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.proxyHandler.GetOutbox() == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "outbox not configured"})
		return
	}
	n, err := h.proxyHandler.GetOutbox().ParkWorkspace(c.Request.Context(), req.WorkspaceID, req.Reason)
	if err != nil {
		h.logger.Error("authority park failed", err, "workspaceId", req.WorkspaceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "park failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"workspaceId": req.WorkspaceID, "parked": n})
}

// Unpark POST /api/v1/admin/authority/unpark — the rollback's drain:
// re-arms exactly the mode_transition parks back to pending.
func (h *AuthorityFlipHandler) Unpark(c *gin.Context) {
	var req parkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.proxyHandler.GetOutbox() == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "outbox not configured"})
		return
	}
	n, err := h.proxyHandler.GetOutbox().UnparkWorkspace(c.Request.Context(), req.WorkspaceID)
	if err != nil {
		h.logger.Error("authority unpark failed", err, "workspaceId", req.WorkspaceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unpark failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"workspaceId": req.WorkspaceID, "unparked": n})
}

// InFlight GET /api/v1/admin/authority/inflight/:workspaceId — the
// drain signal: the pod's unresolved ledger count (ledgered + admitted
// + stalled), read from statusz (the admin deep-introspection surface;
// no ABI change — the ABI is frozen).
func (h *AuthorityFlipHandler) InFlight(c *gin.Context) {
	workspaceID := c.Param("workspaceId")
	sz, err := h.proxyHandler.FetchStatuszPublic(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "statusz read failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"workspaceId": workspaceID, "inFlight": sz.InFlightDeliveries})
}

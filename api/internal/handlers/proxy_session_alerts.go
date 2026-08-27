// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// SetSessionAlerts wires the D6 (#998) alert-persistence service. nil
// (dev/test default) disables persistence — SSE remains the only surface.
func (h *ProxyHandler) SetSessionAlerts(sa interfaces.SessionAlertsService) {
	h.sessionAlerts = sa
}

// GetWorkspaceAlerts returns persisted D6 (#998) hung-session alerts
// for a workspace, newest-first, within the service's retention window
// (24h). Workflow surfaces and reconnecting clients use this to recover
// alerts missed while no SSE stream was attached.
//
// GET /api/v1/workspaces/:id/alerts?limit=50
func (h *ProxyHandler) GetWorkspaceAlerts(c *gin.Context) {
	if h.sessionAlerts == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "alert persistence not configured"})
		return
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit: must be a positive integer"})
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	alerts, err := h.sessionAlerts.ListByWorkspace(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		h.logger.Error("GetWorkspaceAlerts: list failed", err, "workspaceID", c.Param("id"))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list alerts"})
		return
	}
	if alerts == nil {
		alerts = []types.SessionAlert{}
	}
	c.JSON(http.StatusOK, gin.H{"alerts": alerts})
}

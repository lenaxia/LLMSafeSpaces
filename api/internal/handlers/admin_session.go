// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// AdminSessionHandler serves admin-only session recovery endpoints.
//
// ForceAbortSession clears a workspace-scoped session that is stuck in the
// ProxyHandler's active-session set (wsstate.Store.activeSess) after the
// workspace pod has been deleted or become unreachable. It does NOT call the
// opencode proxy — the pod may be gone, by design. It mirrors the local
// cleanup half of ProxyHandler.DeleteSession (proxy_handlers.go:234-268)
// minus the proxy call, plus an audit-log row.
//
// The session-index DB row is deliberately NOT cleaned: force-abort clears
// only the stuck active-session marker, not the session itself. The session
// may still be live in opencode (just not busy). DeleteWorkspace already
// does not clean session_index rows (pre-existing pattern).
//
// Audit-log failure is non-fatal: the force-abort succeeds even if the DB
// INSERT fails, because incident recovery must not be blocked by a DB hiccup
// (differs from AdminDiscardDLQ which returns 500 on audit failure — that
// operation is not incident-recovery-critical).
type AdminSessionHandler struct {
	proxyHandler *ProxyHandler
	db           *sql.DB
	logger       pkginterfaces.LoggerInterface
}

func NewAdminSessionHandler(proxy *ProxyHandler, db *sql.DB, logger pkginterfaces.LoggerInterface) *AdminSessionHandler {
	return &AdminSessionHandler{proxyHandler: proxy, db: db, logger: logger}
}

func (h *AdminSessionHandler) ForceAbortSession(c *gin.Context) {
	workspaceID := c.Param("workspaceId")
	sessionID := c.Param("sessionId")

	if err := validateSessionID(sessionID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspaceId required"})
		return
	}

	if !h.proxyHandler.isSessionActive(c.Request.Context(), workspaceID, sessionID) {
		c.JSON(http.StatusNotFound, gin.H{
			"error":       "session is not currently active (nothing to abort)",
			"sessionId":   sessionID,
			"workspaceId": workspaceID,
		})
		return
	}

	h.proxyHandler.removeActiveSession(c.Request.Context(), workspaceID, sessionID)

	h.proxyHandler.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
		Type:      "session.status",
		SessionID: sessionID,
		Status:    "aborted",
	})

	actorID, _ := extractAuth(c)
	h.logAudit(c.Request.Context(), actorID, workspaceID, sessionID)

	h.logger.Info("admin force-aborted stuck session",
		"workspaceID", workspaceID, "sessionID", sessionID, "actor", actorID)

	c.JSON(http.StatusOK, gin.H{
		"aborted":     true,
		"sessionId":   sessionID,
		"workspaceId": workspaceID,
	})
}

func (h *AdminSessionHandler) logAudit(ctx context.Context, actorID, workspaceID, sessionID string) {
	if h.db == nil {
		return
	}
	metadata, err := json.Marshal(map[string]string{
		"workspaceId": workspaceID,
		"source":      "admin_force_abort",
	})
	if err != nil {
		h.logger.Error("failed to marshal audit metadata", err,
			"workspaceID", workspaceID, "sessionID", sessionID)
		return
	}
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, domain, action, target_id, metadata)
		 VALUES ($1, 'admin', 'session_force_abort', $2, $3)`,
		actorID, sessionID, string(metadata)); err != nil {
		h.logger.Error("failed to write audit log for session force-abort", err,
			"workspaceID", workspaceID, "sessionID", sessionID)
	}
}

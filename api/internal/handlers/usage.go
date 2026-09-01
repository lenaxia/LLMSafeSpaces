// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type UsageHandler struct {
	meteringSvc interfaces.MeteringService
	dbSvc       interfaces.DatabaseService
	db          *sql.DB
}

func NewUsageHandler(meteringSvc interfaces.MeteringService, dbSvc interfaces.DatabaseService) *UsageHandler {
	return &UsageHandler{
		meteringSvc: meteringSvc,
		dbSvc:       dbSvc,
	}
}

func (h *UsageHandler) SetDB(db *sql.DB) {
	h.db = db
}

func (h *UsageHandler) GetUsage(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	from, to, err := parsePeriod(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	owner := types.BillingOwner{ID: userID, Type: types.OwnerTypeUser}
	report, err := h.meteringSvc.GetUsage(c.Request.Context(), owner, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get usage"})
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *UsageHandler) GetWorkspaceUsage(c *gin.Context) {
	userID, _ := extractAuth(c)
	workspaceID := c.Param("id")
	if userID == "" || workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing user or workspace"})
		return
	}

	owned, err := h.dbSvc.CheckResourceOwnership(c.Request.Context(), userID, "workspace", workspaceID)
	if err != nil || !owned {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	from, to, err := parsePeriod(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	owner := types.BillingOwner{ID: userID, Type: types.OwnerTypeUser}
	report, err := h.meteringSvc.GetUsageByWorkspace(c.Request.Context(), owner, workspaceID, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get usage"})
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *UsageHandler) GetQuotaStatus(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	owner := types.BillingOwner{ID: userID, Type: types.OwnerTypeUser}
	statuses, err := h.meteringSvc.GetQuotaStatus(c.Request.Context(), owner)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get quota status"})
		return
	}
	if statuses == nil {
		statuses = []types.QuotaStatus{}
	}
	c.JSON(http.StatusOK, statuses)
}

func (h *UsageHandler) AdminGetUsage(c *gin.Context) {
	ownerID := c.Param("ownerId")
	if ownerID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing owner ID"})
		return
	}

	from, to, err := parsePeriod(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	owner := types.BillingOwner{ID: ownerID, Type: types.OwnerTypeUser}
	report, err := h.meteringSvc.GetUsage(c.Request.Context(), owner, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get usage"})
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *UsageHandler) AdminGetDLQ(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusOK, gin.H{"entries": []interface{}{}})
		return
	}

	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT id, payload, error_message, retry_count, first_failed_at, last_retried_at
		 FROM usage_events_dlq WHERE resolved_at IS NULL ORDER BY id DESC LIMIT 100`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query DLQ"})
		return
	}
	defer func() { _ = rows.Close() }()

	type dlqEntry struct {
		ID            int64           `json:"id"`
		Payload       json.RawMessage `json:"payload"`
		ErrorMessage  string          `json:"errorMessage"`
		RetryCount    int             `json:"retryCount"`
		FirstFailedAt time.Time       `json:"firstFailedAt"`
		LastRetriedAt *time.Time      `json:"lastRetriedAt,omitempty"`
	}

	var entries []dlqEntry
	for rows.Next() {
		var e dlqEntry
		if err := rows.Scan(&e.ID, &e.Payload, &e.ErrorMessage, &e.RetryCount, &e.FirstFailedAt, &e.LastRetriedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read DLQ entries"})
		return
	}
	if entries == nil {
		entries = []dlqEntry{}
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries})
}

func (h *UsageHandler) AdminRetryDLQ(c *gin.Context) {
	id := c.Param("id")
	if h.db == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	var payload json.RawMessage
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT payload FROM usage_events_dlq WHERE id = $1 AND resolved_at IS NULL`, id).Scan(&payload)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "DLQ entry not found or already resolved"})
		return
	}

	var event types.UsageEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to unmarshal event"})
		return
	}

	metaBytes, merr := marshalJSON(event.Metadata)
	if merr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal event metadata"})
		return
	}
	rcBytes, rerr := marshalJSON(event.RequestContext)
	if rerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal request context"})
		return
	}
	_, err = h.db.ExecContext(c.Request.Context(),
		`INSERT INTO usage_events (idempotency_key, owner_id, owner_type, actor_id, workspace_id,
			event_type, event_subtype, quantity, resource_tier, region, metadata, request_context,
			source, event_time, period)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		event.IdempotencyKey, event.Owner.ID, string(event.Owner.Type), event.ActorID, event.WorkspaceID,
		event.EventType, event.EventSubtype, event.Quantity, event.ResourceTier, event.Region,
		metaBytes, rcBytes, event.Source, event.EventTime,
		event.EventTime,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retry event"})
		return
	}

	if _, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE usage_events_dlq SET resolved_at=NOW(), resolution='reprocessed' WHERE id=$1`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark DLQ entry as resolved"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *UsageHandler) AdminDiscardDLQ(c *gin.Context) {
	id := c.Param("id")
	if h.db == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE usage_events_dlq SET resolved_at=NOW(), resolution='discarded' WHERE id=$1 AND resolved_at IS NULL`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to discard DLQ entry"})
		return
	}

	actorID, _ := extractAuth(c)
	if _, aerr := h.db.ExecContext(c.Request.Context(),
		`INSERT INTO audit_log (actor_id, domain, action, target_id, metadata)
		 VALUES ($1, 'billing', 'dlq_discarded', $2, $3)`,
		actorID, id, `{"source":"admin"}`); aerr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write audit log"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *UsageHandler) AdminBillingStatus(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusOK, gin.H{"export_cursors": map[string]interface{}{}, "dlq_size": 0})
		return
	}

	var dlqSize int
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT COUNT(*) FROM usage_events_dlq WHERE resolved_at IS NULL`).Scan(&dlqSize); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query DLQ size"})
		return
	}

	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT provider, last_exported_id, last_exported_at FROM billing_export_cursor`)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"export_cursors": map[string]interface{}{}, "dlq_size": dlqSize})
		return
	}
	defer func() { _ = rows.Close() }()

	cursors := make(map[string]interface{})
	for rows.Next() {
		var provider string
		var lastID int64
		var lastAt time.Time
		if err := rows.Scan(&provider, &lastID, &lastAt); err != nil {
			continue
		}
		cursors[provider] = gin.H{"last_exported_id": lastID, "last_exported_at": lastAt}
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read cursors"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"export_cursors": cursors, "dlq_size": dlqSize})
}

func marshalJSON(v interface{}) ([]byte, error) {
	if v == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(v)
}

func parsePeriod(c *gin.Context) (from, to time.Time, err error) {
	now := time.Now()
	fromStr := c.Query("from")
	toStr := c.Query("to")

	if fromStr != "" {
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from date: must be RFC3339 format (e.g. 2026-06-01T00:00:00Z)")
		}
	}
	if toStr != "" {
		to, err = time.Parse(time.RFC3339, toStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to date: must be RFC3339 format (e.g. 2026-06-30T23:59:59Z)")
		}
	}

	if from.IsZero() {
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	if to.IsZero() {
		to = now
	}
	return from, to, nil
}

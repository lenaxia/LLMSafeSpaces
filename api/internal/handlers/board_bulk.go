// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_bulk.go — #1836's bulk claim/complete REST projection: thin
// adapters over the engine's BulkTaskStart/BulkMoveStage (the same
// §4.2 discipline as every route in board.go — the adapter owns
// binding + presence + vocabulary validation, the engine owns the
// batch semantics: cap, per-item results, partial failure, idempotent
// retry). The per-item result array renders verbatim; batch-level
// guard classes map through renderBoardError like every other board
// write. NO claim tokens ever render on this surface (#1864 ruling).

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardBulkStartRequest is POST /api/v1/me/tasks/bulk-start.
type boardBulkStartRequest struct {
	TaskIDs   []string `json:"taskIds"`
	RequestID string   `json:"requestId"`
}

// StartTasksBulk is the bulk claim (#1836): N manual dispatches in one
// call — each item one full single-task TaskStart (claim + fresh-charter
// spawn, draft∪active epics); an item already in_progress returns done
// (idempotent retry); per-item results, partial failures included.
func (h *BoardHandler) StartTasksBulk(c *gin.Context) {
	var req boardBulkStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if len(req.TaskIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskIds is required (at least one task id)", "code": board.GuardInput})
		return
	}
	results, err := h.board.BulkTaskStart(c.Request.Context(), h.boardActor(c), req.TaskIDs, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results, "count": len(results)})
}

// boardBulkMoveStageRequest is POST /api/v1/me/tasks/bulk-move-stage.
type boardBulkMoveStageRequest struct {
	TaskIDs     []string `json:"taskIds"`
	ToStage     string   `json:"toStage"`
	EvidenceURL string   `json:"evidenceUrl"`
	RequestID   string   `json:"requestId"`
}

// MoveStageBulk is the bulk complete (#1836): one shared target stage
// + evidence across a batch — each item one full single-task MoveStage
// (the guarded stage machine arbitrates every move: Done guard, pause
// guards, claim-token rule, auto-review on →in_review); an item
// already at the target stage returns done (idempotent retry);
// per-item results, partial failures included.
func (h *BoardHandler) MoveStageBulk(c *gin.Context) {
	var req boardBulkMoveStageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if len(req.TaskIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskIds is required (at least one task id)", "code": board.GuardInput})
		return
	}
	if req.ToStage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "toStage is required", "code": board.GuardInput})
		return
	}
	if !boardStages()[req.ToStage] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "toStage must be one of the five stages: " + board.StageVocabulary(), "code": board.GuardInput})
		return
	}
	results, err := h.board.BulkMoveStage(c.Request.Context(), h.boardActor(c), req.TaskIDs, req.ToStage, req.EvidenceURL, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results, "count": len(results)})
}

// boardBulkDeleteRequest is POST /api/v1/me/tasks/bulk-delete.
type boardBulkDeleteRequest struct {
	TaskIDs   []string `json:"taskIds"`
	Reconnect bool     `json:"reconnect"`
	RequestID string   `json:"requestId"`
}

// DeleteTasksBulk is the bulk delete (#1836's delete_task(s) batch):
// N destructive removals in one call — each item one full single-task
// DeleteTask (own transaction + snapshot audit event, orphan events,
// post-commit worker abort for an in-flight row); ONE shared
// reconnect flag across the batch. Per-item results, partial failures
// included; a miss, a foreign row, and an already-deleted row render
// the same not_found item (absence ≡ miss ≡ foreign — never
// disclosed, never a conflict on retry).
func (h *BoardHandler) DeleteTasksBulk(c *gin.Context) {
	var req boardBulkDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if len(req.TaskIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskIds is required (at least one task id)", "code": board.GuardInput})
		return
	}
	results, err := h.board.BulkDeleteTasks(c.Request.Context(), h.boardActor(c), req.TaskIDs, req.Reconnect, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results, "count": len(results)})
}

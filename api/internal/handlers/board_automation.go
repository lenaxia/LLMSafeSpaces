// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_automation.go — the Task Board's internal automation seam (design
// 0067 §4.3c, Lane 1): the SA-token control plane the in-workspace worker
// agent (fresh-charter, ruling R9) speaks. Body passed through verbatim to
// the canonical BoardService — "the platform validates" — with the pod's
// resolved owner as the delegating user (R7) and the agent SURFACE stamped
// so the manager-only guards (pending_input exits, CRUD, cancel) deny here
// by construction.
//
// r1-S1: every action carries the pod's resolved WORKSPACE on the actor —
// the §4.4 caller-workspace binding is structural on this surface (reads
// resolve workspace-scoped; in-flight writes bind to it), so a pod can
// never touch another tenant's tasks even with a leaked token.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardService is the narrow seam the projection adapter needs (§4.2: a
// projection is a thin adapter — no business rules here). Satisfied by
// *board.Service; narrowed for handler testability.
type boardService interface {
	Heartbeat(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, observedProgressAt time.Time) (board.HeartbeatResult, error)
	ReportThrottle(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, retryAfter time.Duration, requestID string) (*board.Task, error)
	MoveStage(ctx context.Context, actor board.Actor, taskID, shortIDOrID, toStage, evidenceURL, requestID string) (*board.Task, error)
	GetTaskInWorkspace(ctx context.Context, workspaceID, taskID, shortIDOrID string) (*board.Task, error)
}

// SetBoardService installs the canonical BoardService (app wiring; the
// nil-service routes are not registered — construction and route
// registration move together in app.go). #1747: the same service carries
// the manager projection the seam's manager routes speak.
func (h *PodAutomationHandler) SetBoardService(svc *board.Service) {
	h.board = svc
	h.boardEngine = svc.Config()
	h.boardMgr = svc
}

// HasBoard reports whether the board service is wired (route gating).
func (h *PodAutomationHandler) HasBoard() bool { return h.board != nil }

// boardActor stamps the delegation: the pod's resolved owner (R7), the
// agent surface, the presented claim token, and the resolved workspace
// (the structural binding, r1-S1).
func (h *PodAutomationHandler) boardActor(ownerID, workspaceID, claimToken string) board.Actor {
	return board.Actor{
		Kind: board.ActorUser, UserID: ownerID, Surface: board.SurfaceAgent,
		ClaimToken: claimToken, WorkspaceID: workspaceID,
	}
}

// BoardHeartbeat is board_heartbeat: evidence-bearing lease renewal
// (#1685 semantics — a heartbeat with no progress evidence does not
// extend). Requires the task's live claim token.
func (h *PodAutomationHandler) BoardHeartbeat(c *gin.Context) {
	raw, workspaceID, ownerID, ok := h.resolve(c)
	if !ok {
		return
	}
	var req struct {
		TaskID             string    `json:"taskId"`
		ShortID            string    `json:"shortId"`
		ClaimToken         string    `json:"claimToken"`
		ObservedProgressAt time.Time `json:"observedProgressAt"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	res, err := h.board.Heartbeat(c.Request.Context(), h.boardActor(ownerID, workspaceID, req.ClaimToken),
		req.TaskID, req.ShortID, req.ObservedProgressAt)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"extended": res.Extended, "leaseExpiresAt": res.LeaseExpiresAt, "lastProgressAt": res.LastProgressAt})
}

// BoardReportThrottle is board_report_throttle: the 429 path — requeue
// without attempt penalty + cooldown on the task and the project's slots.
func (h *PodAutomationHandler) BoardReportThrottle(c *gin.Context) {
	raw, workspaceID, ownerID, ok := h.resolve(c)
	if !ok {
		return
	}
	var req struct {
		TaskID            string `json:"taskId"`
		ShortID           string `json:"shortId"`
		ClaimToken        string `json:"claimToken"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	retryAfter := time.Duration(req.RetryAfterSeconds) * time.Second
	if retryAfter <= 0 {
		retryAfter = h.boardEngine.ThrottleBackoff
	}
	task, err := h.board.ReportThrottle(c.Request.Context(), h.boardActor(ownerID, workspaceID, req.ClaimToken),
		req.TaskID, req.ShortID, retryAfter, "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

// BoardMoveStage is board_move_stage (worker projection): token-guarded
// stage writes. The pending_input EXIT is rejected on this surface — a
// delegated worker is not the human the exit exists for (§4.2).
func (h *PodAutomationHandler) BoardMoveStage(c *gin.Context) {
	raw, workspaceID, ownerID, ok := h.resolve(c)
	if !ok {
		return
	}
	var req struct {
		TaskID      string `json:"taskId"`
		ShortID     string `json:"shortId"`
		ClaimToken  string `json:"claimToken"`
		ToStage     string `json:"toStage"`
		EvidenceURL string `json:"evidenceUrl"`
		RequestID   string `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	task, err := h.board.MoveStage(c.Request.Context(), h.boardActor(ownerID, workspaceID, req.ClaimToken),
		req.TaskID, req.ShortID, req.ToStage, req.EvidenceURL, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

// BoardTaskGet is board_read's item form: the worker's view of a task
// (charter, criteria, blocked-by, lease) — resolved WORKSPACE-SCOPED
// (r1-S1): another tenant's task is a plain 404 with no suggestions.
func (h *PodAutomationHandler) BoardTaskGet(c *gin.Context) {
	_, workspaceID, _, ok := h.resolve(c)
	if !ok {
		return
	}
	task, err := h.board.GetTaskInWorkspace(c.Request.Context(), workspaceID, c.Param("id"), c.Query("shortId"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

// bindBoardBody binds the captured body verbatim (the resolve() capture
// replay discipline).
func bindBoardBody(c *gin.Context, raw []byte, req any) bool {
	if raw == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "request body required"})
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err := c.ShouldBindJSON(req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return false
	}
	return true
}

// renderBoardError maps the BoardService error classes to HTTP (the
// parity matrix pins the same classes per surface).
func renderBoardError(c *gin.Context, err error) {
	switch e := err.(type) {
	case *board.NotFoundError:
		c.JSON(http.StatusNotFound, gin.H{"error": e.Error(), "suggestions": e.Suggestions})
	case *board.GuardError:
		switch e.Code {
		case board.GuardWorkspaceBinding, board.GuardAuthz:
			c.JSON(http.StatusForbidden, gin.H{"error": e.Error(), "code": e.Code})
		default:
			c.JSON(http.StatusConflict, gin.H{"error": e.Error(), "code": e.Code})
		}
	case *board.DependencyError:
		c.JSON(http.StatusConflict, gin.H{"error": e.Error(), "code": e.Code, "suggestions": e.Suggestions})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

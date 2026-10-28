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
	"encoding/json"
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
	RecordVerdict(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, in board.VerdictInput, requestID string) (*board.Task, *board.Verdict, error)
	SetCriteriaMet(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, check board.CriteriaCheck, requestID string) (*board.Task, error)
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

// BoardHeartbeat is board_heartbeat: the lease-nudge RESPONSE channel
// (item 6 amendment, 2026-10-28). Extends ONLY on a row carrying a live
// nudge (the reply the nudge prose demands); the periodic self-report
// discipline is dead — a heartbeat with no nudge records the arrival and
// extends nothing. #1847 future-clamping applies to the stamp. Requires
// the task's live claim token.
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
	c.JSON(http.StatusOK, gin.H{"extended": res.Extended, "leaseExpiresAt": res.LeaseExpiresAt, "lastProgressAt": res.LastProgressAt, "reason": res.Reason})
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

// BoardRecordVerdict is board_record_verdict (worker projection): the
// verdict channel for the SPAWNED reviewer persona (#1947 — the
// reviewer the stage-persona claim-then-spawn mints is a worker
// session; this is its one authorized write). Token-guarded: the
// service requires the in_review row's live claim token (the reviewer's
// own claim from its charter envelope).
func (h *PodAutomationHandler) BoardRecordVerdict(c *gin.Context) {
	raw, workspaceID, ownerID, ok := h.resolve(c)
	if !ok {
		return
	}
	var req struct {
		TaskID      string                 `json:"taskId"`
		ShortID     string                 `json:"shortId"`
		ClaimToken  string                 `json:"claimToken"`
		Verdict     string                 `json:"verdict"`
		Reviewer    string                 `json:"reviewer"`
		EvidenceURL string                 `json:"evidenceUrl"`
		Findings    []board.VerdictFinding `json:"findings"`
		RequestID   string                 `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	task, v, err := h.board.RecordVerdict(c.Request.Context(), h.boardActor(ownerID, workspaceID, req.ClaimToken),
		req.TaskID, req.ShortID,
		board.VerdictInput{
			Verdict:     board.VerdictKind(req.Verdict),
			Findings:    req.Findings,
			Reviewer:    req.Reviewer,
			EvidenceURL: req.EvidenceURL,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": task, "verdict": v})
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

// BoardSetCriteriaMet is the #1946 scoped grant's worker route: the
// reviewer persona records ONE criterion's met flag on its OWN claimed
// row (claim-token-guarded, persona-provenance-guarded at the engine —
// the platform validates; this handler keeps the surface narrow). The
// request vocabulary is deliberately minimal — anything beyond the
// addressing + token + the one check refuses LOUDLY (a reviewer that
// reaches for title/description/criteria-replace fields is told to use
// the manager surface, whose grant it does not hold).
func (h *PodAutomationHandler) BoardSetCriteriaMet(c *gin.Context) {
	raw, workspaceID, ownerID, ok := h.resolve(c)
	if !ok {
		return
	}
	if raw == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "request body required"})
		return
	}
	var fields map[string]json.RawMessage
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err := c.ShouldBindJSON(&fields); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	for k := range fields {
		switch k {
		case "taskId", "shortId", "claimToken", "setCriteriaMet", "requestId", "lsp_injected_session", "workspaceID":
		default:
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "the reviewer grant covers setCriteriaMet only — field \"" + k + "\" is not part of it (title/description/tags/criteria edits ride the manager surface)",
				"code":  board.GuardInput,
			})
			return
		}
	}
	var req struct {
		TaskID         string               `json:"taskId"`
		ShortID        string               `json:"shortId"`
		ClaimToken     string               `json:"claimToken"`
		SetCriteriaMet *board.CriteriaCheck `json:"setCriteriaMet"`
		RequestID      string               `json:"requestId"`
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.SetCriteriaMet == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "setCriteriaMet is required — the reviewer grant records criterion flags {index, met} on your claimed row",
			"code":  board.GuardInput,
		})
		return
	}
	task, err := h.board.SetCriteriaMet(c.Request.Context(), h.boardActor(ownerID, workspaceID, req.ClaimToken),
		req.TaskID, req.ShortID, *req.SetCriteriaMet, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, taskEchoResponse{Task: *task, MutatedCriterion: mutatedCriterion(req.SetCriteriaMet, task)})
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
	case *board.DuplicateError:
		// 200-with-original, never an error (the outbox Duplicate{AcceptedID}
		// contract — a replayed requestId renders the original outcome).
		if e.Result == nil {
			e.Result = map[string]any{"duplicate": true, "event_id": e.EventID, "action": e.Action}
		}
		c.JSON(http.StatusOK, e.Result)
	case *board.NotFoundError:
		c.JSON(http.StatusNotFound, gin.H{"error": e.Error(), "suggestions": e.Suggestions})
	case *board.GuardError:
		switch e.Code {
		case board.GuardWorkspaceBinding, board.GuardAuthz:
			c.JSON(http.StatusForbidden, gin.H{"error": e.Error(), "code": e.Code})
		case board.GuardInput, board.GuardTextRejected: // #1837 P1b: text caps/corruption are caller errors, 400
			c.JSON(http.StatusBadRequest, gin.H{"error": e.Error(), "code": e.Code})
		default:
			c.JSON(http.StatusConflict, gin.H{"error": e.Error(), "code": e.Code})
		}
	case *board.DependencyError:
		c.JSON(http.StatusConflict, gin.H{"error": e.Error(), "code": e.Code, "suggestions": e.Suggestions})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

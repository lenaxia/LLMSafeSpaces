package handlers

// board_wrapup.go — the epic wrap-up review's close + ack surfaces
// (low-level design §§8-9; high-level D6, D7). The wire schema is
// ENGINE-OWNED and closed (verdict pass|gaps; reportUrl required on both
// — it is the close's evidence; gapRefs required non-empty iff gaps;
// notes size-capped; the story verdict's approve|fail is a different
// verb's vocabulary and refuses): these handlers are thin projections
// that bind the body and carry the claim token on the actor — the
// engine arbitrates every guard, on every surface.

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardWrapUpVerdictRequest is the owner-REST body of the close verb.
type boardWrapUpVerdictRequest struct {
	Verdict    board.WrappingVerdict `json:"verdict" binding:"required"`
	ReportURL  string                `json:"reportUrl"`
	GapRefs    []string              `json:"gapRefs"`
	Notes      string                `json:"notes"`
	Reviewer   string                `json:"reviewer" binding:"required"`
	ClaimToken string                `json:"claimToken"`
	RequestID  string                `json:"requestId"`
}

// RecordTaskWrapUpVerdict closes an epic's wrap-up review (route: POST
// /api/v1/me/tasks/:id/wrapup-verdict): PASS completes the epic iff still
// drained (atomically, never a seam re-run); GAPS files the referenced
// gap stories' verdict and holds the epic open by the ordinary drain
// predicate. The claim token rides the actor — the engine's guard is
// unconditional (the schema is engine-owned regardless of reviewer
// role), so even the owner carries the live token or refuses.
func (h *BoardHandler) RecordTaskWrapUpVerdict(c *gin.Context) {
	var req boardWrapUpVerdictRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	actor := h.boardActor(c)
	actor.ClaimToken = req.ClaimToken
	t, e, err := h.board.RecordWrapUpVerdict(c.Request.Context(), actor, "", c.Param("id"),
		board.WrapUpVerdictInput{
			Verdict:   req.Verdict,
			ReportURL: req.ReportURL,
			GapRefs:   req.GapRefs,
			Notes:     req.Notes,
			Reviewer:  req.Reviewer,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t, "epic": e})
}

// boardAckWrapUpRequest is the ack body (the requestID idempotency key;
// the verb itself takes no content — the human said continue).
type boardAckWrapUpRequest struct {
	RequestID string `json:"requestId"`
}

// AckEpicWrapUp is the wrap-up recursion cap's human exit (route: POST
// /api/v1/me/epics/:id/ack-wrapup; manager authz at the engine): reset
// the consecutive-close ledger, clear the ack-wait flag and any parked
// in-flight pointer, and re-drain — an acked, still-drained epic mints
// its next wrap-up immediately.
func (h *BoardHandler) AckEpicWrapUp(c *gin.Context) {
	var req boardAckWrapUpRequest
	_ = c.ShouldBindJSON(&req)
	e, err := h.board.AckWrapUp(c.Request.Context(), h.boardActor(c), c.Param("id"), "", req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"epic": e})
}

// --- The automation seam twins (agentd's worker + manager projections) --------

// BoardRecordWrapUpVerdict is board_record_wrapup_verdict on the worker
// projection: the wrap-up reviewer persona's close — its ONE authorized
// epic-level write, keyed to the in_review claim its charter envelope
// carries (a wrong or missing token refuses at the engine, every
// surface).
func (h *PodAutomationHandler) BoardRecordWrapUpVerdict(c *gin.Context) {
	raw, workspaceID, ownerID, ok := h.resolve(c)
	if !ok {
		return
	}
	var req struct {
		TaskID     string                `json:"taskId"`
		ShortID    string                `json:"shortId"`
		ClaimToken string                `json:"claimToken"`
		Verdict    board.WrappingVerdict `json:"verdict"`
		ReportURL  string                `json:"reportUrl"`
		GapRefs    []string              `json:"gapRefs"`
		Notes      string                `json:"notes"`
		Reviewer   string                `json:"reviewer"`
		RequestID  string                `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	t, e, err := h.board.RecordWrapUpVerdict(c.Request.Context(),
		h.boardActor(ownerID, workspaceID, req.ClaimToken, boardSessionID(c, raw)),
		req.TaskID, req.ShortID,
		board.WrapUpVerdictInput{
			Verdict:   req.Verdict,
			ReportURL: req.ReportURL,
			GapRefs:   req.GapRefs,
			Notes:     req.Notes,
			Reviewer:  req.Reviewer,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t, "epic": e})
}

// BoardAckEpicWrapUp is board_ack_epic_wrapup on the manager projection:
// the human circuit-breaker's exit, workspace-scoped by the pod identity
// (the #1747 manager-action scope — a pod manages its OWN workspace's
// board only).
func (h *PodAutomationHandler) BoardAckEpicWrapUp(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	e, err := h.boardMgr.AckWrapUp(c.Request.Context(),
		h.managerActor(owner, ws, session), c.Param("id"), "", req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"epic": e})
}

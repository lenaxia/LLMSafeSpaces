package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 item 5 (1/2): the structured review verdict surface. The verdict
// CORE (payload validation + guarded transitions + the ordered chain) is
// forge-agnostic and lives in pkg/board; these handlers are the REST
// projection (the forge adapters — PR reviews mapping into verdicts —
// consume the core, never re-implement it).

type boardVerdictRequest struct {
	Verdict     board.VerdictKind      `json:"verdict" binding:"required"`
	Findings    []board.VerdictFinding `json:"findings"`
	Reviewer    string                 `json:"reviewer" binding:"required"`
	EvidenceURL string                 `json:"evidenceUrl"`
	RequestID   string                 `json:"requestId"`
}

// RecordTaskVerdict records a review verdict on a task and applies its
// transition through the guarded stage machine (route: POST
// /api/v1/me/tasks/:id/verdict). A refused transition is a RECORDED
// OUTCOME, not an error: 200 with {task, verdict} where
// verdict.applied=false and verdict.transition="refused:<code>" — the
// chain keeps the evidence, the task stays in its stage. Validation
// failures (payload shape, severity enum, criterion bounds) are 400s;
// the perimeter is the task read's (foreign rows 404).
func (h *BoardHandler) RecordTaskVerdict(c *gin.Context) {
	var req boardVerdictRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	t, v, err := h.board.RecordVerdict(c.Request.Context(), h.boardActor(c), "", c.Param("id"),
		board.VerdictInput{
			Verdict:     req.Verdict,
			Findings:    req.Findings,
			Reviewer:    req.Reviewer,
			EvidenceURL: req.EvidenceURL,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t, "verdict": v})
}

// ListTaskVerdicts renders the task's ordered verdict chain (route: GET
// /api/v1/me/tasks/:id/verdicts) — the record the next claimant's
// charter envelope embeds (PR-B2) and the drawer timeline joins.
func (h *BoardHandler) ListTaskVerdicts(c *gin.Context) {
	chain, err := h.board.TaskVerdicts(c.Request.Context(), h.boardActor(c), "", c.Param("id"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"verdicts": chain})
}

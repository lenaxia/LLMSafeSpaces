// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_automation_config.go — the Task Board config surfaces on the
// internal automation seam: board_get_config (READ, open to any
// in-workspace session — the #1747 §5 repeal; the source-annotated
// merged view at either tier) and board_set_config (the manager-set
// write, tier selector in the body, the config keys themselves passed
// through verbatim — the platform validates, agentd re-models
// nothing). The UI twin of this surface lives ON the board (owner
// ruling 2026-10-28); the agent seam renders the SAME merged view so
// an advisor agent and the board UI read one truth.

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// BoardGetConfig is board_get_config: the effective merged config with
// source annotations (code_default | user_default | project_override),
// per-stage persona provenance included. No projectId → the user
// tier's board-wide defaults; a projectId → that project's view
// (scoped through the caller's workspace binding like every manager
// read). A READ: no claim token, no gate beyond the pod SA resolve.
func (h *PodAutomationHandler) BoardGetConfig(c *gin.Context) {
	_, ws, owner, _, ok := h.resolveManager(c)
	if !ok {
		return
	}
	view, err := h.boardMgr.GetBoardConfigView(c.Request.Context(),
		h.managerActor(owner, ws, ""), "", c.Query("projectId"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// BoardSetConfig is board_set_config: REPLACE-writes one tier's config
// map. The body carries the tier selector ("user" | "project"),
// projectId (required for the project tier, refused for the user
// tier), and the config keys themselves verbatim — every remaining
// key/value pair runs the same key-gate decode as the REST surface
// (unknown keys and malformed values reject 400 here). Absent keys
// revert to inheritance, exactly the REST contract. The response is
// the source-annotated view board_get_config renders — the caller sees
// the merged result, not just the raw echo.
func (h *PodAutomationHandler) BoardSetConfig(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Tier      string `json:"tier"`
		ProjectID string `json:"projectId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	// The config keys are the body MINUS the routing keys: pop them,
	// decode the remainder through the strict key-gate (the same
	// ValidateBoardConfigKey vocabulary the REST adapter runs — the
	// platform validates; nothing outside the vocabulary rides in).
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the config body must be a JSON object of config keys", "code": board.GuardInput})
		return
	}
	for _, k := range []string{"tier", "projectId", "workspaceID", "sessionId"} {
		delete(fields, k)
	}
	in, err := board.DecodeBoardConfigOverride(fields)
	if err != nil {
		renderBoardError(c, err)
		return
	}

	actor := h.managerActor(owner, ws, session)
	switch req.Tier {
	case "user":
		if req.ProjectID != "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "tier=user writes the board-wide defaults — projectId does not ride a user-tier set (drop it, or send tier=project)",
				"code":  board.GuardInput,
			})
			return
		}
		if _, err := h.boardMgr.SetUserBoardConfig(c.Request.Context(), actor, in); err != nil {
			renderBoardError(c, err)
			return
		}
		view, err := h.boardMgr.GetBoardConfigView(c.Request.Context(), actor, "", "")
		if err != nil {
			renderBoardError(c, err)
			return
		}
		c.JSON(http.StatusOK, view)
	case "project":
		if req.ProjectID == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "tier=project requires projectId — the project whose override map this set writes",
				"code":  board.GuardInput,
			})
			return
		}
		if _, err := h.boardMgr.SetProjectBoardConfig(c.Request.Context(), actor, req.ProjectID, "", in); err != nil {
			renderBoardError(c, err)
			return
		}
		view, err := h.boardMgr.GetBoardConfigView(c.Request.Context(), actor, req.ProjectID, "")
		if err != nil {
			renderBoardError(c, err)
			return
		}
		c.JSON(http.StatusOK, view)
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "tier must be \"user\" (the board-wide defaults) or \"project\" (one project's override map)",
			"code":  board.GuardInput,
		})
	}
}

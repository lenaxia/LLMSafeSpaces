// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_config.go — the Task Board Config REST projection (#1832 item
// 6, part 1): the user-scoped board-wide defaults read/write (R7:
// boards are user-level, so "board-wide" = the acting user's boards)
// and the owner-scoped per-project override read/write (both rendering
// the effective merged config).
//
// A thin adapter in the §4.2 sense: the strict key-gate below is the
// adapter's transport validation (unknown keys, malformed values, and
// JSON nulls reject HERE — the cmd/workspace-agentd mcp_server.go
// key-gate discipline; null is never a value: an unset key is ABSENT,
// never null), the service owns the typed belt, and error classes ride
// renderBoardError like every other board surface. Writes are
// REPLACE-semantics: send the full map; absent keys revert to
// inheritance.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// bindBoardConfigOverride binds a config PUT body strictly: the body is
// a raw key/value map; every pair must pass board's key-gate (unknown
// key → GuardInput 400 naming the known set; malformed value →
// GuardInput 400 naming the key; JSON null value → GuardInput 400 —
// omit the key to inherit). An empty body or a top-level null body
// rejects 400 — neither may masquerade as "clear every override".
// Nothing outside the vocabulary reaches the service.
func bindBoardConfigOverride(c *gin.Context) (*board.BoardConfigOverride, bool) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read request body: " + err.Error()})
		return nil, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body required: a config object (send {} to clear every override)"})
		return nil, false
	}
	if string(trimmed) == "null" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body must be a config object, not null (send {} to clear every override)"})
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return nil, false
	}
	o, err := board.DecodeBoardConfigOverride(raw)
	if err != nil {
		renderBoardError(c, err)
		return nil, false
	}
	return o, true
}

// GetUserBoardConfig renders the acting user's effective board-wide
// defaults (route: GET /api/v1/me/board/config — user-scoped surface,
// the R7 ruling on PR #1840: boards are user-level, so board-wide
// defaults are the user's, not the instance's).
func (h *BoardHandler) GetUserBoardConfig(c *gin.Context) {
	cfg, err := h.board.GetUserBoardConfig(c.Request.Context(), h.boardActor(c))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// SetUserBoardConfig REPLACE-writes the acting user's board-wide
// defaults map (route: PUT /api/v1/me/board/config). Send the full
// set: absent keys revert to the code defaults. {} clears every
// override.
func (h *BoardHandler) SetUserBoardConfig(c *gin.Context) {
	in, ok := bindBoardConfigOverride(c)
	if !ok {
		return
	}
	cfg, err := h.board.SetUserBoardConfig(c.Request.Context(), h.boardActor(c), in)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// GetProjectBoardConfig renders one project's raw overrides plus the
// effective merged config (route: GET /api/v1/me/projects/:id/config
// — the service resolves scoped to the acting user; a foreign project
// renders the scoped 404, never disclosed).
func (h *BoardHandler) GetProjectBoardConfig(c *gin.Context) {
	pc, err := h.board.GetProjectBoardConfig(c.Request.Context(), h.boardActor(c), "", c.Param("id"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, pc)
}

// SetProjectBoardConfig REPLACE-writes the project's override map
// (route: PUT /api/v1/me/projects/:id/config — owner perimeter via
// the service's own R7/#1747 guards). Send the full set: absent keys
// revert to inheriting the user's board-wide default. {} clears the
// overrides (full inheritance).
func (h *BoardHandler) SetProjectBoardConfig(c *gin.Context) {
	p, ok := h.ownerProject(c, c.Param("id"))
	if !ok {
		return
	}
	in, ok := bindBoardConfigOverride(c)
	if !ok {
		return
	}
	pc, err := h.board.SetProjectBoardConfig(c.Request.Context(), h.boardActor(c), p.ID, "", in)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, pc)
}

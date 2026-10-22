// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_config.go — the Task Board Config REST projection (#1832 item
// 6, part 1): the admin-scoped global defaults read/write and the
// owner-scoped per-project override read/write (both rendering the
// effective merged config).
//
// A thin adapter in the §4.2 sense: the strict key-gate below is the
// adapter's transport validation (unknown keys reject HERE — the
// cmd/workspace-agentd mcp_server.go key-gate discipline), the service
// owns the typed belt, and error classes ride renderBoardError like
// every other board surface. Writes are REPLACE-semantics: send the
// full map; absent keys revert to inheritance.

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// bindBoardConfigOverride binds a config PUT body strictly: the body is
// a raw key/value map; every pair must pass board's key-gate (unknown
// key → GuardInput 400 naming the known set; malformed value →
// GuardInput 400 naming the key). Nothing outside the vocabulary
// reaches the service.
func bindBoardConfigOverride(c *gin.Context) (*board.BoardConfigOverride, bool) {
	var raw map[string]json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
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

// GetGlobalBoardConfig renders the effective global board defaults
// (route: GET /api/v1/admin/board/config — admin-guarded at the router,
// the instance-settings discipline: board-wide config is instance
// scope).
func (h *BoardHandler) GetGlobalBoardConfig(c *gin.Context) {
	cfg, err := h.board.GetGlobalBoardConfig(c.Request.Context())
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// SetGlobalBoardConfig REPLACE-writes the global defaults map (route:
// PUT /api/v1/admin/board/config; admin-guarded at the router). Send
// the full set: absent keys revert to the code defaults. {} clears
// every override.
func (h *BoardHandler) SetGlobalBoardConfig(c *gin.Context) {
	in, ok := bindBoardConfigOverride(c)
	if !ok {
		return
	}
	cfg, err := h.board.SetGlobalBoardConfig(c.Request.Context(), h.boardActor(c), in)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// GetProjectBoardConfig renders one project's raw overrides plus the
// effective merged config (route: GET /api/v1/me/projects/:id/config —
// owner perimeter via ownerProject, the read-edge discipline).
func (h *BoardHandler) GetProjectBoardConfig(c *gin.Context) {
	p, ok := h.ownerProject(c, c.Param("id"))
	if !ok {
		return
	}
	pc, err := h.board.GetProjectBoardConfig(c.Request.Context(), p.ID, "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, pc)
}

// SetProjectBoardConfig REPLACE-writes the project's override map
// (route: PUT /api/v1/me/projects/:id/config — owner perimeter via
// ownerProject, then the service's own R7/#1747 guards). Send the full
// set: absent keys revert to inheriting the global default. {} clears
// the overrides (full inheritance).
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

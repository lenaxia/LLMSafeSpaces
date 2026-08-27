// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// ResolveHash handles GET /v1/image-factory/resolve/:hash — the public
// content-address lookup powering hash-based re-selection: a user pastes
// a schematic hash (shown on every existing image) and the create form
// recovers the exact selection + base it names.
//
// Any authenticated user may resolve any hash: builds coalesce across
// scopes by design (images are platform-wide artifacts), and a hash is a
// content address over public catalog extension IDs — it reveals nothing
// scope-sensitive (no names, no owners).
func (h *ImageFactoryHandler) ResolveHash(c *gin.Context) {
	hash := c.Param("hash")
	if !imagefactory.IsValidHash(hash) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "invalid hash: expected s- followed by 16 hex characters",
		})
		return
	}
	res, err := h.store.ResolveHash(c.Request.Context(), hash)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no image built with that hash"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve hash"})
		return
	}
	c.JSON(http.StatusOK, res)
}

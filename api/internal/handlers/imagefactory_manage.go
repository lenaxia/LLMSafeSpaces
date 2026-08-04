// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// resolveConfigByHash finds a config by hash, searching scopes in order
// (member → org → platform). Returns the resolved config and the scope it
// was found in. Returns the config even for platform/org scope (read access);
// callers enforce write-authorization separately.
func (h *ImageFactoryHandler) resolveConfigByHash(c *gin.Context) (imagefactory.Config, imagefactory.ConfigScope, bool) {
	hash := c.Param("hash")
	if hash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hash is required"})
		return imagefactory.Config{}, "", false
	}
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return imagefactory.Config{}, "", false
	}
	ctx := c.Request.Context()

	var orgID *string
	if oid, err := h.orgs.GetUserOrgID(ctx, userID); err == nil && oid != "" {
		o := oid
		orgID = &o
	}
	uid := userID

	for _, scope := range []imagefactory.ConfigScope{
		imagefactory.ScopeMember,
		imagefactory.ScopeOrg,
		imagefactory.ScopePlatform,
	} {
		var oidArg, ownerArg *string
		switch scope {
		case imagefactory.ScopeMember:
			ownerArg = &uid
		case imagefactory.ScopeOrg:
			oidArg = orgID
			if oidArg == nil {
				continue
			}
		}
		cfg, err := h.store.GetConfigByHash(ctx, hash, scope, ownerArg, oidArg)
		if err == nil {
			return cfg, scope, true
		}
		if !errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load config"})
			return imagefactory.Config{}, "", false
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
	return imagefactory.Config{}, "", false
}

// DeleteConfig handles DELETE /configs/:hash.
// Only member-scope configs can be deleted by their owner. Org and platform
// configs require admin/owner elevation (not implemented here — return 403).
// Configs with in-flight builds (status=building) cannot be deleted.
func (h *ImageFactoryHandler) DeleteConfig(c *gin.Context) {
	cfg, scope, ok := h.resolveConfigByHash(c)
	if !ok {
		return
	}

	if scope != imagefactory.ScopeMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "only personal configs can be deleted"})
		return
	}

	if cfg.Status == imagefactory.StatusBuilding {
		c.JSON(http.StatusConflict, gin.H{"error": "config has an in-flight build; wait for it to finish"})
		return
	}

	ctx := c.Request.Context()
	if err := h.store.DeleteConfig(ctx, cfg.ID); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete config"})
		return
	}

	c.Status(http.StatusNoContent)
}

type renameConfigRequest struct {
	Name string `json:"name" binding:"required"`
}

// RenameConfig handles PATCH /configs/:hash.
// Only member-scope configs can be renamed by their owner.
func (h *ImageFactoryHandler) RenameConfig(c *gin.Context) {
	cfg, scope, ok := h.resolveConfigByHash(c)
	if !ok {
		return
	}

	if scope != imagefactory.ScopeMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "only personal configs can be renamed"})
		return
	}

	var req renameConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "name must not be empty"})
		return
	}
	if len(name) > 64 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "name must be 64 characters or fewer"})
		return
	}

	ctx := c.Request.Context()
	if err := h.store.RenameConfig(ctx, cfg.ID, name); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
			return
		}
		if errors.Is(err, database.ErrConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "a config with this name already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rename config"})
		return
	}

	updated, err := h.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"id": cfg.ID, "name": name})
		return
	}
	c.JSON(http.StatusOK, updated)
}

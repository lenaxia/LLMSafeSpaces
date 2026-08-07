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
// Member-scope configs can be deleted by their owner. Org-scope configs
// can be deleted by org admins. Platform-scope configs by platform admins.
// Configs with in-flight builds (status=building) cannot be deleted.
func (h *ImageFactoryHandler) DeleteConfig(c *gin.Context) {
	cfg, scope, ok := h.resolveConfigByHash(c)
	if !ok {
		return
	}

	if !h.canMutateScope(c, scope, cfg.OrgID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "you do not have permission to delete this config"})
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
// Member-scope configs can be renamed by their owner. Org-scope configs
// by org admins. Platform-scope configs by platform admins.
func (h *ImageFactoryHandler) RenameConfig(c *gin.Context) {
	cfg, scope, ok := h.resolveConfigByHash(c)
	if !ok {
		return
	}

	if !h.canMutateScope(c, scope, cfg.OrgID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "you do not have permission to rename this config"})
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
		cfg.Name = name
		c.JSON(http.StatusOK, cfg)
		return
	}
	c.JSON(http.StatusOK, updated)
}

// canMutateScope checks whether the caller has permission to mutate a config
// at the given scope. The middleware guards (OrgAdminGuard, AdminGuard) handle
// authorization at the route level for creation; this method provides
// defense-in-depth for the shared delete/rename endpoints, which are mounted
// under the consumer route group (AuthMiddleware only).
//
//   - member: always allowed (resolveConfigByHash already verified ownership
//     by filtering on owner_id = caller's userID).
//   - org: allowed if the caller is an admin of the config's org. Note:
//     resolveConfigByHash only finds org configs the caller is a member of
//     (via GetUserOrgID), so platform admins can only delete/rename org
//     configs from their own org, not other orgs' configs. This is a
//     limitation of resolveConfigByHash, not canMutateScope.
//   - platform: allowed if the caller is a platform admin (role = "admin").
func (h *ImageFactoryHandler) canMutateScope(c *gin.Context, scope imagefactory.ConfigScope, cfgOrgID *string) bool {
	// Platform admin bypass: can mutate any scope.
	if c.GetString("userRole") == "admin" {
		return true
	}
	switch scope {
	case imagefactory.ScopeMember:
		return true
	case imagefactory.ScopeOrg:
		if cfgOrgID == nil {
			return false
		}
		userID := c.GetString("userID")
		ctx := c.Request.Context()
		isAdmin, err := h.orgs.IsOrgAdmin(ctx, *cfgOrgID, userID)
		if err != nil {
			return false
		}
		return isAdmin
	default:
		return false
	}
}

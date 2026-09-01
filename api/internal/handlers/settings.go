// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// SettingsHandler handles admin and user settings API requests.
type SettingsHandler struct {
	instanceSvc *settings.InstanceService
	userSvc     *settings.UserService
}

// NewSettingsHandler creates a new settings handler.
func NewSettingsHandler(instanceSvc *settings.InstanceService, userSvc *settings.UserService) *SettingsHandler {
	return &SettingsHandler{
		instanceSvc: instanceSvc,
		userSvc:     userSvc,
	}
}

// GetAdminSettings returns all instance settings merged with defaults.
func (h *SettingsHandler) GetAdminSettings(c *gin.Context) {
	all, err := h.instanceSvc.GetAll(c.Request.Context())
	if err != nil {
		// Graceful degradation: return schema defaults if DB unavailable
		defaults := make(map[string]any)
		for _, def := range h.instanceSvc.Schema() {
			defaults[def.Key] = def.Default
		}
		c.JSON(http.StatusOK, gin.H{
			"settings":      defaults,
			"schemaVersion": settings.SchemaVersion,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"settings":      all,
		"schemaVersion": settings.SchemaVersion,
	})
}

// GetAdminSettingsSchema returns the full Tier 2 schema definition.
func (h *SettingsHandler) GetAdminSettingsSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"schemaVersion": settings.SchemaVersion,
		"settings":      h.instanceSvc.Schema(),
	})
}

// SetAdminSetting updates a single instance setting.
func (h *SettingsHandler) SetAdminSetting(c *gin.Context) {
	key := c.Param("key")

	var body struct {
		Value json.RawMessage `json:"value" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value is required"})
		return
	}

	var value any
	if err := json.Unmarshal(body.Value, &value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value format"})
		return
	}

	if err := h.instanceSvc.Set(c.Request.Context(), key, value); err != nil {
		if errors.Is(err, settings.ErrReadOnly) {
			c.JSON(http.StatusConflict, gin.H{"error": "setting is managed by Helm; edit the Helm values and upgrade"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key, "value": value})
}

// GetUserSettings returns all user settings merged with defaults.
func (h *SettingsHandler) GetUserSettings(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	all, err := h.userSvc.GetAll(c.Request.Context(), userID)
	if err != nil {
		// Graceful degradation: return schema defaults if DB unavailable
		defaults := make(map[string]any)
		for _, def := range h.userSvc.Schema() {
			defaults[def.Key] = def.Default
		}
		c.JSON(http.StatusOK, gin.H{
			"settings":      defaults,
			"schemaVersion": settings.SchemaVersion,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"settings":      all,
		"schemaVersion": settings.SchemaVersion,
	})
}

// GetUserSettingsSchema returns the full Tier 3 schema definition.
func (h *SettingsHandler) GetUserSettingsSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"schemaVersion": settings.SchemaVersion,
		"settings":      h.userSvc.Schema(),
	})
}

// SetUserSetting updates a single user setting.
func (h *SettingsHandler) SetUserSetting(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	key := c.Param("key")

	var body struct {
		Value json.RawMessage `json:"value" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value is required"})
		return
	}

	var value any
	if err := json.Unmarshal(body.Value, &value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid value format"})
		return
	}

	if err := h.userSvc.Set(c.Request.Context(), userID, key, value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key, "value": value})
}

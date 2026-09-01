// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/prompt"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// promptStore is the data-access surface for prompt CRUD endpoints.
type promptStore interface {
	GetPlatformSetting(ctx context.Context, key types.PlatformSettingKey) (*types.PlatformSetting, error)
	SetPlatformSetting(ctx context.Context, key types.PlatformSettingKey, value json.RawMessage, updatedBy string) error
	GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error)
	SetOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey, value json.RawMessage, updatedBy string) error
	DeleteOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey) error
	GetWorkspacePrompt(ctx context.Context, workspaceID string) (*types.WorkspacePrompt, error)
	SetWorkspacePrompt(ctx context.Context, workspaceID string, prompt string, updatedBy string) error
	GetWorkspaceOrgID(ctx context.Context, workspaceID string) (string, error)
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
	LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, metadata map[string]any) error
}

// PromptHandler handles platform and org prompt CRUD endpoints.
type PromptHandler struct {
	store   promptStore
	svc     *prompt.Service
	authSvc orgAuthService
	logger  policyLogger
}

// NewPromptHandler constructs the handler.
func NewPromptHandler(store promptStore, svc *prompt.Service, authSvc orgAuthService, logger policyLogger) *PromptHandler {
	return &PromptHandler{store: store, svc: svc, authSvc: authSvc, logger: logger}
}

type platformPromptResponse struct {
	Prompt string `json:"prompt"`
}

// GetPlatform handles GET /api/v1/admin/prompt.
func (h *PromptHandler) GetPlatform(c *gin.Context) {
	setting, err := h.store.GetPlatformSetting(c.Request.Context(), types.SettingSysPromptPlatform)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get platform prompt"})
		return
	}
	var promptText string
	if setting != nil {
		_ = json.Unmarshal(setting.Value, &promptText)
	} else {
		// Fresh install: surface the effective default the platform tier
		// delivers, so admins can see and edit-from what workspaces
		// actually receive (an explicitly-cleared saved row still wins).
		promptText = types.DefaultPlatformPrompt
	}
	c.JSON(http.StatusOK, platformPromptResponse{Prompt: promptText})
}

type setPlatformPromptRequest struct {
	Prompt string `json:"prompt" binding:"max=10000"`
}

// SetPlatform handles PUT /api/v1/admin/prompt.
func (h *PromptHandler) SetPlatform(c *gin.Context) {
	var req setPlatformPromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	value, _ := json.Marshal(req.Prompt)
	actorID := h.authSvc.GetUserID(c)

	if err := h.store.SetPlatformSetting(c.Request.Context(), types.SettingSysPromptPlatform, value, actorID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set platform prompt"})
		return
	}
	if err := h.store.LogAuditEvent(c.Request.Context(), "admin", actorID, "prompt.platform.set", "sys_prompt_platform", nil, map[string]any{"length": len(req.Prompt)}); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "prompt.platform.set", "error", err.Error())
	}
	if h.svc != nil {
		h.svc.InvalidatePlatformCache(c.Request.Context())
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

type orgPromptResponse struct {
	Prompt          string `json:"prompt"`
	AllowUserPrompt bool   `json:"allowUserPrompt"`
}

// GetOrg handles GET /api/v1/orgs/:id/prompt.
func (h *PromptHandler) GetOrg(c *gin.Context) {
	orgID := c.Param("id")
	policies, err := h.store.GetOrgPolicies(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get org prompt"})
		return
	}

	resp := orgPromptResponse{}
	for _, p := range policies {
		switch p.Key {
		case types.PolicySysPromptOrg:
			_ = json.Unmarshal(p.Value, &resp.Prompt)
		case types.PolicyAllowUserPrompt:
			_ = json.Unmarshal(p.Value, &resp.AllowUserPrompt)
		}
	}
	c.JSON(http.StatusOK, resp)
}

type setOrgPromptRequest struct {
	Prompt          *string `json:"prompt,omitempty" binding:"omitempty,max=10000"`
	AllowUserPrompt *bool   `json:"allowUserPrompt,omitempty"`
}

// SetOrg handles PUT /api/v1/orgs/:id/prompt.
func (h *PromptHandler) SetOrg(c *gin.Context) {
	orgID := c.Param("id")
	var req setOrgPromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	actorID := h.authSvc.GetUserID(c)

	if req.Prompt != nil {
		value, _ := json.Marshal(*req.Prompt)
		if err := h.store.SetOrgPolicy(c.Request.Context(), orgID, types.PolicySysPromptOrg, value, actorID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set org prompt"})
			return
		}
		if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "prompt.org.set", "sys_prompt_org", map[string]any{"length": len(*req.Prompt)}); err != nil && h.logger != nil {
			h.logger.Warn("audit log emission failed", "action", "prompt.org.set", "error", err.Error())
		}
	}

	if req.AllowUserPrompt != nil {
		value, _ := json.Marshal(*req.AllowUserPrompt)
		if err := h.store.SetOrgPolicy(c.Request.Context(), orgID, types.PolicyAllowUserPrompt, value, actorID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set allow_user_prompt"})
			return
		}
		if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "prompt.toggle", "allow_user_prompt", map[string]any{"value": *req.AllowUserPrompt}); err != nil && h.logger != nil {
			h.logger.Warn("audit log emission failed", "action", "prompt.toggle", "error", err.Error())
		}
	}

	if h.svc != nil {
		h.svc.InvalidateOrgWorkspacesCache(c.Request.Context(), orgID)
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// --- Workspace prompt (user-level custom instructions) ---

type workspacePromptResponse struct {
	Prompt string `json:"prompt"`
}

// GetWorkspacePrompt handles GET /api/v1/workspaces/:id/prompt.
func (h *PromptHandler) GetWorkspacePrompt(c *gin.Context) {
	wsID := c.Param("id")
	wp, err := h.store.GetWorkspacePrompt(c.Request.Context(), wsID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get workspace prompt"})
		return
	}
	prompt := ""
	if wp != nil {
		prompt = wp.Prompt
	}
	c.JSON(http.StatusOK, workspacePromptResponse{Prompt: prompt})
}

type setWorkspacePromptRequest struct {
	Prompt string `json:"prompt" binding:"max=10000"`
}

// userPromptAllowedFromPolicies reports whether member-level prompt/role
// customization is permitted for an org. Defaults to LOCKED (false) when the
// policy is unset — matching the resolution path
// (types.OrgPolicyValues.IsUserPromptAllowed) so a value accepted on write is
// never silently discarded at read. Shared by the prompt and role handlers so
// the default-locked semantics cannot diverge across copies.
func userPromptAllowedFromPolicies(policies []*types.OrgPolicy) bool {
	for _, p := range policies {
		if p.Key == types.PolicyAllowUserPrompt {
			var allowed bool
			if json.Unmarshal(p.Value, &allowed) == nil {
				return allowed
			}
		}
	}
	return false
}

// SetWorkspacePrompt handles PUT /api/v1/workspaces/:id/prompt.
// Respects the org's allow_user_prompt toggle — returns 403 when locked.
func (h *PromptHandler) SetWorkspacePrompt(c *gin.Context) {
	wsID := c.Param("id")
	var req setWorkspacePromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	actorID := h.authSvc.GetUserID(c)

	// Check allow_user_prompt policy
	orgID, err := h.store.GetWorkspaceOrgID(c.Request.Context(), wsID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve workspace org"})
		return
	}
	if orgID != "" {
		policies, err := h.store.GetOrgPolicies(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check org policy"})
			return
		}
		if !userPromptAllowedFromPolicies(policies) {
			c.JSON(http.StatusForbidden, gin.H{"error": "org admin has disabled member prompt customization"})
			return
		}
	}

	if err := h.store.SetWorkspacePrompt(c.Request.Context(), wsID, req.Prompt, actorID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set workspace prompt"})
		return
	}

	if h.svc != nil {
		h.svc.InvalidateWorkspaceCache(c.Request.Context(), wsID)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/policy"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// policyStore is the data-access surface for the policy CRUD endpoints.
type policyStore interface {
	GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error)
	SetOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey, value json.RawMessage, updatedBy string) error
	DeleteOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey) error
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
}

// PolicyHandler handles GET/PUT/DELETE /api/v1/orgs/:id/policies (admin-only).
type PolicyHandler struct {
	store   policyStore
	svc     *policy.Service
	authSvc orgAuthService
	logger  policyLogger
}

type policyLogger interface {
	Warn(msg string, args ...any)
}

// NewPolicyHandler constructs the handler. svc is used for cache invalidation
// after mutations. logger is used to surface audit emission failures.
func NewPolicyHandler(store policyStore, svc *policy.Service, authSvc orgAuthService, logger policyLogger) *PolicyHandler {
	return &PolicyHandler{store: store, svc: svc, authSvc: authSvc, logger: logger}
}

// Get handles GET /api/v1/orgs/:id/policies. Returns all configured policies.
func (h *PolicyHandler) Get(c *gin.Context) {
	orgID := c.Param("id")
	policies, err := h.store.GetOrgPolicies(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get policies"})
		return
	}
	c.JSON(http.StatusOK, policies)
}

// Put handles PUT /api/v1/orgs/:id/policies/:key. Upserts a single policy.
func (h *PolicyHandler) Put(c *gin.Context) {
	orgID := c.Param("id")
	rawKey := c.Param("key")
	userID := h.authSvc.GetUserID(c)

	key := types.OrgPolicyKey(rawKey)
	if !isValidKey(key) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy key"})
		return
	}

	var body json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !isValidValue(key, body) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy value for key"})
		return
	}

	if err := h.store.SetOrgPolicy(c.Request.Context(), orgID, key, body, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set policy"})
		return
	}
	if err := h.store.LogOrgEvent(c.Request.Context(), orgID, userID, "policy.set", string(key), map[string]any{"value": body}); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "policy.set", "orgID", orgID, "error", err.Error())
	}
	if h.svc != nil {
		h.svc.InvalidateCache(c.Request.Context(), orgID)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Delete handles DELETE /api/v1/orgs/:id/policies/:key. Removes a policy
// (reverts to default / unrestricted).
func (h *PolicyHandler) Delete(c *gin.Context) {
	orgID := c.Param("id")
	rawKey := c.Param("key")

	key := types.OrgPolicyKey(rawKey)
	if !isValidKey(key) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy key"})
		return
	}

	if err := h.store.DeleteOrgPolicy(c.Request.Context(), orgID, key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete policy"})
		return
	}
	actorID := h.authSvc.GetUserID(c)
	if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "policy.delete", string(key), nil); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "policy.delete", "orgID", orgID, "error", err.Error())
	}
	if h.svc != nil {
		h.svc.InvalidateCache(c.Request.Context(), orgID)
	}
	c.Status(http.StatusNoContent)
}

func isValidKey(k types.OrgPolicyKey) bool {
	switch k {
	case types.PolicyAllowedModels,
		types.PolicyAllowedProviders,
		types.PolicyMaxWorkspacesPerMember,
		types.PolicyMaxActiveWorkspacesPerMem,
		types.PolicySysPromptOrg,
		types.PolicyAllowUserPrompt,
		types.PolicyAllowUserMcpServers,
		types.PolicyMaxMcpServersPerWorkspace,
		types.PolicyDefaultRuntime:
		return true
	}
	return false
}

// isValidValue validates the JSONB payload shape for the given key.
func isValidValue(key types.OrgPolicyKey, body json.RawMessage) bool {
	switch key {
	case types.PolicyAllowedModels, types.PolicyAllowedProviders:
		var arr []string
		return json.Unmarshal(body, &arr) == nil
	case types.PolicyMaxWorkspacesPerMember, types.PolicyMaxActiveWorkspacesPerMem:
		var n int
		if err := json.Unmarshal(body, &n); err != nil {
			return false
		}
		return n >= 0
	case types.PolicySysPromptOrg:
		var s string
		if err := json.Unmarshal(body, &s); err != nil {
			return false
		}
		return len(s) <= types.MaxPromptPerLevel()
	case types.PolicyAllowUserPrompt:
		var b bool
		return json.Unmarshal(body, &b) == nil
	case types.PolicyAllowUserMcpServers:
		var b bool
		return json.Unmarshal(body, &b) == nil
	case types.PolicyMaxMcpServersPerWorkspace:
		var n int
		if err := json.Unmarshal(body, &n); err != nil {
			return false
		}
		return n >= 0
	case types.PolicyDefaultRuntime:
		var s string
		return json.Unmarshal(body, &s) == nil
	}
	return false
}

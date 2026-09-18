// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// llmProviderResolver is the minimal service surface the internal
// llm-providers endpoint needs. *secrets.SecretService satisfies it.
type llmProviderResolver interface {
	ResolveLLMProviders(ctx context.Context, ownerUserID, workspaceID string) ([]secrets.LLMProviderData, error)
}

// InternalLLMProvidersHandler serves GET
// /api/v1/internal/workspaces/:workspaceID/llm-providers?ownerUserID=<uid> —
// the cluster-internal endpoint the workspace controller's relay staging
// (US-72.3, design 0058 §4.1 hop 1) polls to obtain the decrypted BYO
// provider credentials it seals into llm-relay envelope Secrets.
//
// This endpoint is intentionally NOT behind AuthMiddleware: the controller
// has no user identity. The PRIMARY boundary is a mandatory shared-secret
// header (X-Internal-Token) read from LLMSAFESPACES_INTERNAL_TOKEN — the
// identical contract to InternalOrgStatusHandler (US-43.19 F5): fail-closed
// 403 when unset, 401 on mismatch, constant-time compare. Unlike org-status
// this endpoint RETURNS DECRYPTED CREDENTIAL MATERIAL, so an optional API
// NetworkPolicy L3/L4 carve-out limiting callers to the controller is
// strongly recommended in addition to the token. It MUST never be exposed
// through any ingress. Every successful reveal is audited server-side by
// ResolveLLMProviders ("internal_llm_providers_reveal").
type InternalLLMProvidersHandler struct {
	svc llmProviderResolver
}

// NewInternalLLMProvidersHandler constructs the handler.
func NewInternalLLMProvidersHandler(svc llmProviderResolver) *InternalLLMProvidersHandler {
	return &InternalLLMProvidersHandler{svc: svc}
}

// GetLLMProviders handles GET /api/v1/internal/workspaces/:workspaceID/llm-providers.
func (h *InternalLLMProvidersHandler) GetLLMProviders(c *gin.Context) {
	expected := os.Getenv("LLMSAFESPACES_INTERNAL_TOKEN")
	if expected == "" {
		// Fail closed: no shared secret configured → refuse rather than
		// serve credential material unauthenticated (deployment
		// misconfiguration, not a blocked legitimate caller).
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "internal endpoint not configured"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(c.GetHeader("X-Internal-Token")), []byte(expected)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	workspaceID := c.Param("workspaceID")
	ownerUserID := c.Query("ownerUserID")
	if workspaceID == "" || ownerUserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspaceID and ownerUserID required"})
		return
	}

	providers, err := h.svc.ResolveLLMProviders(c.Request.Context(), ownerUserID, workspaceID)
	if err != nil {
		// The underlying error may carry store detail — never echo it.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "resolve failed"})
		return
	}
	if providers == nil {
		providers = []secrets.LLMProviderData{}
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

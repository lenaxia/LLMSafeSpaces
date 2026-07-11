// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/validation"
)

// WorkspaceEnvService is the caller-shaped subset of SecretService methods
// used by the workspace-env endpoints. *secrets.SecretService satisfies it.
type WorkspaceEnvService interface {
	// GetSecretByName returns the named secret for the user.
	//
	// Contract: a not-found secret is reported as (nil, nil) — NOT
	// (nil, ErrNotFound). SetWorkspaceEnv and DeleteWorkspaceEnv rely on
	// this: they branch on `existing != nil` to decide create-vs-update
	// (and to short-circuit a no-op delete), so any implementation that
	// returned a non-nil error for the absence case would surface as a
	// spurious 500 and break env-var creation. Backed by
	// pg_secret_store.go, which maps pgx.ErrNoRows → (nil, nil).
	GetSecretByName(ctx context.Context, userID, name string) (*secrets.SecretResponse, error)
	UpdateSecret(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, secretID string, req secrets.UpdateSecretRequest) error
	CreateSecret(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, req secrets.CreateSecretRequest) (*secrets.SecretResponse, error)
	AddBindings(ctx context.Context, userID, workspaceID string, secretIDs []string) (secrets.BindingsMutationResult, error)
	GetBindings(ctx context.Context, userID, workspaceID string) (*secrets.BindingsResponse, error)
	DeleteSecret(ctx context.Context, userID, secretID string) error
}

// WorkspaceEnvHandler handles PUT/GET/DELETE /api/v1/workspaces/:id/env.
// Extracted from SecretsHandler (US-29.4) — env-var management is a
// distinct responsibility from secret CRUD, binding management, and model
// selection. Each env var is stored as an env-secret-bound-to-workspace;
// the handler orchestrates create-or-update-by-name + binding management.
type WorkspaceEnvHandler struct {
	svc    WorkspaceEnvService
	logger pkginterfaces.LoggerInterface
}

// NewWorkspaceEnvHandler creates a WorkspaceEnvHandler backed by the given
// service. The service must be non-nil — it is a required dependency
// (US-29.8 principle: fail at construction, not at request time).
func NewWorkspaceEnvHandler(svc WorkspaceEnvService) *WorkspaceEnvHandler {
	return &WorkspaceEnvHandler{svc: svc}
}

// SetLogger installs the logger used to surface non-fatal failures from
// secret create/update/delete operations. Optional; if nil, failures are
// silent (do not leave nil in production).
func (h *WorkspaceEnvHandler) SetLogger(l pkginterfaces.LoggerInterface) {
	h.logger = l
}

func (h *WorkspaceEnvHandler) warn(msg string, fields ...interface{}) {
	if h.logger != nil {
		h.logger.Warn(msg, fields...)
	}
}

// SetWorkspaceEnv handles PUT /api/v1/workspaces/:id/env
//
// Creates or updates env-secret type secrets bound to this workspace.
//
// Concurrency: SetWorkspaceEnv only ADDs bindings (it never removes
// — that's what DeleteWorkspaceEnv is for), so we can use the
// store's AddBindings primitive which holds a workspace-scoped
// advisory lock for the duration of the binding write. Two
// concurrent SetWorkspaceEnv calls on the same workspace serialize
// at the AddBindings step and neither's secrets are lost.
//
// Error handling: every UpdateSecret/CreateSecret/AddBindings
// failure surfaces as 500 with the offending var name. Pre-fix the
// handler returned 204 even when the writes silently failed.
func (h *WorkspaceEnvHandler) SetWorkspaceEnv(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	var req struct {
		Vars map[string]string `json:"vars" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vars map required"})
		return
	}

	// G37: validate every env-var name up front, before creating or
	// updating any secret. Failing fast here avoids partial application
	// (some vars written, one rejected) and avoids persisting a bad name
	// that would then fail at every pod boot until manually deleted.
	// Blocklist (LD_PRELOAD, PATH, PYTHONPATH, etc.) is enforced at both
	// this layer and at materialize-time in pkg/agentd/secrets — defense
	// in depth; the API check is the user-facing gate.
	for varName := range req.Vars {
		if err := validation.ValidateEnvVarName(varName); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid env var name",
				"name":   varName,
				"reason": err.Error(),
			})
			return
		}
	}

	ctx := c.Request.Context()

	newBindings := make([]string, 0, len(req.Vars))
	for varName, value := range req.Vars {
		secretName := fmt.Sprintf("%s-env-%s", workspaceID, strings.ToLower(varName))
		metadata, _ := json.Marshal(map[string]string{"var_name": varName})

		existing, err := h.svc.GetSecretByName(ctx, userID, secretName)
		if err != nil {
			h.warn("SetWorkspaceEnv: GetSecretByName failed",
				"varName", varName, "workspaceID", workspaceID, "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up env var: " + varName})
			return
		}
		if existing != nil {
			if err := h.svc.UpdateSecret(ctx, userID, sessionID, extractMatchedSigningKey(c), existing.ID,
				secrets.UpdateSecretRequest{Value: value}); err != nil {
				h.warn("SetWorkspaceEnv: UpdateSecret failed",
					"varName", varName, "secretID", existing.ID, "error", err.Error())
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update env var: " + varName})
				return
			}
			newBindings = append(newBindings, existing.ID)
			continue
		}
		created, err := h.svc.CreateSecret(ctx, userID, sessionID, extractMatchedSigningKey(c), secrets.CreateSecretRequest{
			Name: secretName, Type: secrets.SecretTypeEnvSecret, Value: value,
			Metadata: metadata,
		})
		if err != nil {
			h.warn("SetWorkspaceEnv: CreateSecret failed",
				"varName", varName, "workspaceID", workspaceID, "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set env var: " + varName})
			return
		}
		newBindings = append(newBindings, created.ID)
	}

	// AddBindings is atomic and idempotent: it adds these secret IDs
	// to the workspace's binding set under a workspace-scoped
	// advisory lock without touching any existing bindings. Two
	// concurrent SetWorkspaceEnv calls on the same workspace
	// serialize at this step rather than racing on a Get-then-Set
	// snapshot (worklog 0094 pass-2 finding O1).
	if _, err := h.svc.AddBindings(ctx, userID, workspaceID, newBindings); err != nil {
		h.warn("SetWorkspaceEnv: AddBindings failed",
			"workspaceID", workspaceID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to commit workspace bindings"})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetWorkspaceEnv handles GET /api/v1/workspaces/:id/env
// Returns env var names (never values) bound to this workspace.
func (h *WorkspaceEnvHandler) GetWorkspaceEnv(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	resp, err := h.svc.GetBindings(c.Request.Context(), userID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get env"})
		return
	}

	vars := []string{}
	for _, b := range resp.Bindings {
		if b.Type == secrets.SecretTypeEnvSecret {
			vars = append(vars, b.Name)
		}
	}
	c.JSON(http.StatusOK, gin.H{"vars": vars})
}

// DeleteWorkspaceEnv handles DELETE /api/v1/workspaces/:id/env/:name
func (h *WorkspaceEnvHandler) DeleteWorkspaceEnv(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	varName := c.Param("name")
	secretName := fmt.Sprintf("%s-env-%s", workspaceID, strings.ToLower(varName))

	existing, err := h.svc.GetSecretByName(c.Request.Context(), userID, secretName)
	if err != nil {
		h.warn("DeleteWorkspaceEnv: GetSecretByName failed",
			"varName", varName, "workspaceID", workspaceID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up env var"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "env var not found"})
		return
	}

	if err := h.svc.DeleteSecret(c.Request.Context(), userID, existing.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete env var"})
		return
	}

	c.Status(http.StatusNoContent)
}

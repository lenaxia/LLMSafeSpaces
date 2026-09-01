// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// orgBindingAndAutoApplyStore is the org-scoped binding + auto-apply interface
// used by OrgCredentialsHandler. Credential CRUD itself is served by the shared
// CredentialStore.
type orgBindingAndAutoApplyStore interface {
	BindCredentialToAllOrgWorkspaces(ctx context.Context, credentialID, orgID string) error
	CreateOrgAutoApply(ctx context.Context, credentialID, orgID string, withinPriority int) error
	ListOrgAutoApply(ctx context.Context, orgID string) ([]*secrets.AutoApplyRule, error)
	DeleteOrgAutoApply(ctx context.Context, credentialID, orgID string) error
}

// OrgCredentialsHandler handles org credential endpoints.
type OrgCredentialsHandler struct {
	credStore CredentialStore
	orgOps    orgBindingAndAutoApplyStore
	provider  secrets.RootKeyProvider
	authSvc   orgAuthService
}

// NewOrgCredentialsHandler creates a new OrgCredentialsHandler.
func NewOrgCredentialsHandler(store CredentialStore, orgOps orgBindingAndAutoApplyStore, provider secrets.RootKeyProvider, authSvc orgAuthService) *OrgCredentialsHandler {
	return &OrgCredentialsHandler{credStore: store, orgOps: orgOps, provider: provider, authSvc: authSvc}
}

type createOrgCredentialRequest struct {
	Name               string         `json:"name"           binding:"required,min=1,max=128"`
	Kind               string         `json:"kind"           binding:"required"`
	Slug               string         `json:"slug"           binding:"required"`
	APIKey             string         `json:"apiKey"         binding:"required"              log:"-"` //nolint:gosec // G117 false positive — field has log:"-" tag, never marshaled to response
	BaseURL            string         `json:"baseURL"`
	ModelAllowlist     []string       `json:"modelAllowlist"`
	ModelContextLimits map[string]int `json:"modelContextLimits"`
	ModelOutputLimits  map[string]int `json:"modelOutputLimits"`
}

type updateOrgCredentialRequest struct {
	Name               *string        `json:"name"`
	Kind               *string        `json:"kind"`
	Slug               *string        `json:"slug"`
	APIKey             *string        `json:"apiKey"          log:"-"` //nolint:gosec // G117 false positive — field has log:"-" tag, never marshaled to response
	BaseURL            *string        `json:"baseURL"`
	ModelAllowlist     []string       `json:"modelAllowlist"`
	ModelContextLimits map[string]int `json:"modelContextLimits"`
	ModelOutputLimits  map[string]int `json:"modelOutputLimits"`
}

// Create handles POST /api/v1/orgs/:id/credentials.
func (h *OrgCredentialsHandler) Create(c *gin.Context) {
	orgID := c.Param("id")
	ctx := c.Request.Context()

	var req createOrgCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Boundary validation against the SDK-class enum and slug regex
	// (Epic 55). Without this, an invalid kind/slug reaches the DB and
	// the CHECK constraint fires as opaque 500 instead of 400.
	if err := secrets.ValidateKind(req.Kind); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "field": "kind"})
		return
	}
	if err := secrets.ValidateSlug(req.Slug); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "field": "slug"})
		return
	}

	if h.provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server key not configured"})
		return
	}

	ciphertext, err := encryptCredentialData(ctx, h.provider.Encrypt, req.Kind, req.Slug, req.APIKey, req.BaseURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode credential"})
		return
	}

	allowlist := req.ModelAllowlist
	if allowlist == nil {
		allowlist = []string{}
	}

	credID := uuid.New().String()
	now := time.Now()
	row := &secrets.CredentialRow{
		ID:                 credID,
		OwnerType:          "org",
		OwnerID:            orgID,
		Name:               req.Name,
		Kind:               req.Kind,
		Slug:               req.Slug,
		Ciphertext:         ciphertext,
		KeyVersion:         secrets.ActiveVersionOf(h.provider),
		ModelAllowlist:     allowlist,
		ModelContextLimits: req.ModelContextLimits,
		ModelOutputLimits:  req.ModelOutputLimits,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if row.ModelContextLimits == nil {
		row.ModelContextLimits = map[string]int{}
	}
	if row.ModelOutputLimits == nil {
		row.ModelOutputLimits = map[string]int{}
	}

	if err := h.credStore.CreateCredential(ctx, "org", orgID, row); err != nil {
		classified := ClassifyPostgresError(err)
		if errors.Is(classified, ErrDuplicateCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "credential with this slug already exists"})
			return
		}
		if errors.Is(classified, ErrCredentialCheckViolation) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential failed validation; kind or slug is invalid"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create credential"})
		return
	}

	// Fetch the freshly-created row so the response reflects the DB-generated
	// timestamps and the stored ciphertext (for baseURL extraction).
	created, err := h.credStore.GetCredential(ctx, "org", orgID, credID)
	if err != nil || created == nil {
		// Credential was stored but unreadable — surface a minimal response.
		c.JSON(http.StatusCreated, CredentialResponse{
			ID: credID, OrgID: orgID, Name: req.Name, Kind: req.Kind, Slug: req.Slug,
			ModelAllowlist:     allowlist,
			ModelContextLimits: req.ModelContextLimits,
			ModelOutputLimits:  req.ModelOutputLimits,
		})
		return
	}

	resp := buildCredentialResponse(ctx, created, h.provider)

	if err := h.orgOps.BindCredentialToAllOrgWorkspaces(ctx, credID, orgID); err != nil {
		resp.BindWarning = "credential created but auto-bind to existing org workspaces failed"
	}

	c.JSON(http.StatusCreated, resp)
}

// List handles GET /api/v1/orgs/:id/credentials.
func (h *OrgCredentialsHandler) List(c *gin.Context) {
	orgID := c.Param("id")
	rows, err := h.credStore.ListCredentials(c.Request.Context(), "org", orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list credentials"})
		return
	}
	// The unified ListCredentials returns ASC (matching admin/user). The org
	// credential list was historically DESC (newest first); preserve that order.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	resp := make([]CredentialResponse, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, buildCredentialResponse(c.Request.Context(), row, h.provider))
	}
	c.JSON(http.StatusOK, resp)
}

// Update handles PUT /api/v1/orgs/:id/credentials/:credID.
func (h *OrgCredentialsHandler) Update(c *gin.Context) {
	orgID := c.Param("id")
	credID := c.Param("credID")
	ctx := c.Request.Context()

	var req updateOrgCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Validate kind/slug if the caller is updating them (Epic 55).
	if req.Kind != nil {
		if err := secrets.ValidateKind(*req.Kind); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "field": "kind"})
			return
		}
	}
	if req.Slug != nil {
		if err := secrets.ValidateSlug(*req.Slug); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "field": "slug"})
			return
		}
	}

	existing, err := h.credStore.GetCredential(ctx, "org", orgID, credID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve credential"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	var newCiphertext []byte
	newKeyVersion := existing.KeyVersion
	// Re-encrypt whenever any field that lives INSIDE the encrypted
	// LLMProviderData blob changes. apiKey and baseURL are obvious; Kind
	// and Slug also live inside the blob (LLMProviderData.Kind/Slug),
	// and the materialize path reads them out as pd.Kind/pd.Slug to
	// determine the SDK adapter and the agent-config.json provider-map
	// key. If the row column changes but the ciphertext stays stale,
	// the rename never reaches the wire format (Epic 55 stale-ciphertext
	// regression — admin handler at admin_provider_credentials.go:283
	// has always included Kind/Slug here for this reason).
	if req.APIKey != nil || req.BaseURL != nil || req.Kind != nil || req.Slug != nil {
		if h.provider == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server key not configured"})
			return
		}

		oldPlaintext, err := h.provider.Decrypt(ctx, existing.Ciphertext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt existing credential"})
			return
		}
		defer zeroBytes(oldPlaintext) // zero on all exit paths (success and failure)
		var pd secrets.LLMProviderData
		if err := json.Unmarshal(oldPlaintext, &pd); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode credential"})
			return
		}
		if req.Kind != nil {
			pd.Kind = *req.Kind
		}
		if req.Slug != nil {
			pd.Slug = *req.Slug
		}
		if req.APIKey != nil {
			pd.APIKey = *req.APIKey
		}
		if req.BaseURL != nil {
			pd.BaseURL = *req.BaseURL
		}
		newPlaintext, err := json.Marshal(pd) //nolint:gosec // G117 false positive — pd contains encrypted credential data
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode credential"})
			return
		}
		newCiphertext, err = h.provider.Encrypt(ctx, newPlaintext)
		zeroBytes(newPlaintext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "re-encryption failed"})
			return
		}
		newKeyVersion++
	}

	// Build the update row. The unified UpdateCredential uses COALESCE so that
	// nil model_allowlist / model_context_limits / model_output_limits / ciphertext
	// mean "don't change"; this preserves the org handler's partial-update
	// semantics. Name/Kind/Slug are applied only when the caller supplied one
	// (empty string leaves the column unchanged via NULLIF). Kind and Slug are
	// passed through as the existing value unless the request overrides them.
	upd := &secrets.CredentialRow{
		ID:             credID,
		OwnerType:      "org",
		OwnerID:        orgID,
		Name:           existing.Name,
		Kind:           existing.Kind,
		Slug:           existing.Slug,
		Ciphertext:     newCiphertext,
		KeyVersion:     newKeyVersion,
		ModelAllowlist: req.ModelAllowlist,
	}
	if req.Name != nil {
		upd.Name = *req.Name
	}
	if req.Kind != nil {
		upd.Kind = *req.Kind
	}
	if req.Slug != nil {
		upd.Slug = *req.Slug
	}
	// modelContextLimits and modelOutputLimits are intentionally NOT pre-normalized:
	// a nil value here must reach the DB as SQL NULL so COALESCE leaves the column
	// unchanged (preserving the org handler's partial-update contract: nil = "don't
	// change", empty map = "clear all"). Only set them when the caller supplied
	// a value.
	upd.ModelContextLimits = req.ModelContextLimits
	upd.ModelOutputLimits = req.ModelOutputLimits

	if err := h.credStore.UpdateCredential(ctx, "org", orgID, credID, upd); err != nil {
		classified := ClassifyPostgresError(err)
		if errors.Is(classified, ErrDuplicateCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "a credential with this slug already exists in this organization"})
			return
		}
		if errors.Is(classified, ErrCredentialCheckViolation) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential failed validation; kind or slug is invalid"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update credential"})
		return
	}

	// Fetch the updated row so the response carries the DB-generated updated_at
	// and the (possibly re-encrypted) ciphertext for baseURL extraction.
	updated, err := h.credStore.GetCredential(ctx, "org", orgID, credID)
	if err != nil || updated == nil {
		c.JSON(http.StatusOK, CredentialResponse{ID: credID, OrgID: orgID})
		return
	}
	c.JSON(http.StatusOK, buildCredentialResponse(ctx, updated, h.provider))
}

// Delete handles DELETE /api/v1/orgs/:id/credentials/:credID.
func (h *OrgCredentialsHandler) Delete(c *gin.Context) {
	orgID := c.Param("id")
	credID := c.Param("credID")
	// The unified DeleteCredential returns pgx.ErrNoRows when no row was affected.
	// Org delete was historically idempotent (204 even if already gone); preserve
	// that by treating "not found" as success.
	if err := h.credStore.DeleteCredential(c.Request.Context(), "org", orgID, credID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.Status(http.StatusNoContent)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete credential"})
		return
	}
	c.Status(http.StatusNoContent)
}

// ProbeModels handles GET /api/v1/orgs/:id/credentials/:credID/models.
// It decrypts the stored credential and calls the provider's /v1/models
// (OpenAI-compatible) to discover available model IDs, merged with any saved
// context limits so the UI can pre-populate the config table.
func (h *OrgCredentialsHandler) ProbeModels(c *gin.Context) {
	orgID := c.Param("id")
	credID := c.Param("credID")
	ctx := c.Request.Context()

	resolveDecrypt := func(_ context.Context) (func(context.Context, []byte) ([]byte, error), string, int) {
		if h.provider != nil {
			return h.provider.Decrypt, "", 0
		}
		return nil, "server key not configured", http.StatusServiceUnavailable
	}
	plaintext, limits, perr := getCredentialForProbe(ctx, h.credStore, "org", orgID, credID, resolveDecrypt)
	if perr != nil {
		c.JSON(perr.status, gin.H{"error": perr.msg})
		return
	}
	defer zeroBytes(plaintext)
	c.JSON(http.StatusOK, probeCredentialModels(ctx, plaintext, limits))
}

// CreateAutoApply handles POST /api/v1/orgs/:id/credentials/:credID/auto-apply.
func (h *OrgCredentialsHandler) CreateAutoApply(c *gin.Context) {
	orgID := c.Param("id")
	credID := c.Param("credID")
	ctx := c.Request.Context()

	cred, err := h.credStore.GetCredential(ctx, "org", orgID, credID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify credential"})
		return
	}
	if cred == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found in this organization"})
		return
	}

	if err := h.orgOps.CreateOrgAutoApply(ctx, credID, orgID, 5); err != nil {
		if errors.Is(ClassifyPostgresError(err), ErrDuplicateCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "auto-apply rule already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create auto-apply rule"})
		return
	}
	c.Status(http.StatusCreated)
}

// ListAutoApply handles GET /api/v1/orgs/:id/credentials/:credID/auto-apply.
func (h *OrgCredentialsHandler) ListAutoApply(c *gin.Context) {
	orgID := c.Param("id")
	ctx := c.Request.Context()

	credID := c.Param("credID")
	if credID != "" {
		cred, err := h.credStore.GetCredential(ctx, "org", orgID, credID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify credential"})
			return
		}
		if cred == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "credential not found in this organization"})
			return
		}
	}

	rules, err := h.orgOps.ListOrgAutoApply(ctx, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list auto-apply rules"})
		return
	}
	if rules == nil {
		rules = []*secrets.AutoApplyRule{}
	}
	c.JSON(http.StatusOK, rules)
}

// DeleteAutoApply handles DELETE /api/v1/orgs/:id/credentials/:credID/auto-apply.
func (h *OrgCredentialsHandler) DeleteAutoApply(c *gin.Context) {
	orgID := c.Param("id")
	credID := c.Param("credID")
	if err := h.orgOps.DeleteOrgAutoApply(c.Request.Context(), credID, orgID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete auto-apply rule"})
		return
	}
	c.Status(http.StatusNoContent)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// CredentialBindingStore abstracts user-only credential↔workspace binding
// operations (the cross-entity concern that only the user handler needs).
type CredentialBindingStore interface {
	BindCredentialToWorkspace(ctx context.Context, credentialID, workspaceID string) error
	// UnbindCredentialFromWorkspace removes an EXPLICIT binding.
	// Returns secrets.ErrAutoBindingProtected for auto-managed bindings.
	UnbindCredentialFromWorkspace(ctx context.Context, credentialID, workspaceID string) error
	// GetCredentialBindingsWithSource returns bindings with source type (explicit vs auto).
	GetCredentialBindingsWithSource(ctx context.Context, credentialID, userID string) ([]secrets.CredentialBindingInfo, error)
	// GetCredentialBindings returns workspace IDs bound to the credential.
	GetCredentialBindings(ctx context.Context, credentialID, userID string) ([]string, error)
	// BindCredentialToAllUserWorkspaces binds a credential to every workspace owned by userID.
	BindCredentialToAllUserWorkspaces(ctx context.Context, credentialID, userID string) error
}

// WorkspaceOwnerChecker verifies workspace ownership for bind operations.
type WorkspaceOwnerChecker func(ctx context.Context, userID, workspaceID string) error

// UserProviderCredentialsHandler handles user-scoped provider credential CRUD.
type UserProviderCredentialsHandler struct {
	store           CredentialStore
	bindings        CredentialBindingStore
	keys            *secrets.KeyService
	keyStore        secrets.KeyStore
	wsOwnerCheck    WorkspaceOwnerChecker
	credStateWriter CredentialStateWriter
}

// NewUserProviderCredentialsHandler creates a new handler. The CRUD store
// provides owner-scoped credential rows; the binding store handles the
// user-only credential↔workspace operations.
func NewUserProviderCredentialsHandler(store CredentialStore, bindings CredentialBindingStore, keys *secrets.KeyService, keyStore secrets.KeyStore) *UserProviderCredentialsHandler {
	return &UserProviderCredentialsHandler{store: store, bindings: bindings, keys: keys, keyStore: keyStore}
}

// SetWorkspaceOwnerChecker installs the ownership verification function.
func (h *UserProviderCredentialsHandler) SetWorkspaceOwnerChecker(fn WorkspaceOwnerChecker) {
	h.wsOwnerCheck = fn
}

// SetCredentialStateWriter installs the reload banner trigger.
func (h *UserProviderCredentialsHandler) SetCredentialStateWriter(w CredentialStateWriter) {
	h.credStateWriter = w
}

// Create handles POST /api/v1/provider-credentials.
//
// NOTE — DEK rotation (L-2 known limitation):
// User credentials are encrypted with the user's DEK at creation time.
// If the user later rotates their password (re-wrapping the DEK), existing
// provider credentials in provider_credentials are NOT re-encrypted, because
// the server cannot access the old DEK without an active session holding it.
// Credentials whose key_version is stale will fail to decrypt after a DEK rotation.
// A future improvement should re-encrypt provider_credentials as part of the
// password-rotation flow.
func (h *UserProviderCredentialsHandler) Create(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" || sessionID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req createAdminCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if strings.TrimSpace(req.Kind) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must not be empty"})
		return
	}
	if strings.TrimSpace(req.Slug) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug must not be empty"})
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.Slug = strings.TrimSpace(req.Slug)
	req.Name = strings.TrimSpace(req.Name)

	// Boundary validation (Epic 55). Surface invalid kind/slug as 400
	// with a field-specific message rather than letting the DB CHECK
	// fire as opaque 500.
	if err := secrets.ValidateKind(req.Kind); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "field": "kind"})
		return
	}
	if err := secrets.ValidateSlug(req.Slug); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "field": "slug"})
		return
	}

	dek, err := h.keys.GetDEK(c.Request.Context(), sessionID, extractMatchedSigningKey(c))
	if err != nil {
		// Issue #593 Option C: surface an actionable message instead
		// of the previous opaque "encryption unavailable". The DEK is
		// unavailable in two common cases — (a) the caller authenticated
		// with an API key created without decrypt_access (the default),
		// or (b) the caller's JWT session has expired and not yet been
		// re-cached. Both have a clear recovery path; tell the caller
		// what it is.
		//
		// Status is 403, not 503: the service is healthy, the caller
		// just lacks the key material this endpoint needs. This matches
		// the secrets-package convention (ErrDEKUnavailable.Status =
		// http.StatusForbidden) used by the other handlers that go
		// through handleSecretError.
		if errors.Is(err, secrets.ErrDEKUnavailable) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "user credential encryption requires a password-authenticated session or an API key created with decryptAccess=true",
				"code":  "dek_unavailable",
			})
			return
		}
		// A non-ErrDEKUnavailable error here is a genuine infrastructure
		// failure (Redis down + rehydrate path returned an unexpected
		// error). 503 is appropriate — the service really is degraded.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "encryption key service unavailable"})
		return
	}

	ciphertext, err := encryptCredentialData(c.Request.Context(), func(_ context.Context, pt []byte) ([]byte, error) { return secrets.EncryptSecret(dek, pt) }, req.Kind, req.Slug, req.APIKey, req.BaseURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode credential"})
		return
	}

	record, err := h.keyStore.GetUserKey(c.Request.Context(), userID)
	if err != nil || record == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user keys not available"})
		return
	}

	now := time.Now()
	row := &secrets.CredentialRow{
		ID:                 uuid.New().String(),
		OwnerID:            userID,
		Name:               req.Name,
		Kind:               req.Kind,
		Slug:               req.Slug,
		Ciphertext:         ciphertext,
		KeyVersion:         record.KeyVersion,
		ModelAllowlist:     req.ModelAllowlist,
		ModelContextLimits: req.ModelContextLimits,
		ModelOutputLimits:  req.ModelOutputLimits,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if row.ModelAllowlist == nil {
		row.ModelAllowlist = []string{}
	}
	if row.ModelContextLimits == nil {
		row.ModelContextLimits = map[string]int{}
	}
	if row.ModelOutputLimits == nil {
		row.ModelOutputLimits = map[string]int{}
	}

	if err := h.store.CreateCredential(c.Request.Context(), "user", userID, row); err != nil {
		classified := ClassifyPostgresError(err)
		if errors.Is(classified, ErrDuplicateCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "credential with this slug already exists"})
			return
		}
		if errors.Is(classified, ErrCredentialCheckViolation) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "credential failed validation; kind or slug is invalid"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store credential"})
		return
	}

	resp := CredentialResponse{
		ID:                 row.ID,
		Name:               row.Name,
		Kind:               row.Kind,
		Slug:               row.Slug,
		ModelAllowlist:     row.ModelAllowlist,
		ModelContextLimits: row.ModelContextLimits,
		ModelOutputLimits:  row.ModelOutputLimits,
		CreatedAt:          row.CreatedAt.Format(time.RFC3339),
		UpdatedAt:          row.UpdatedAt.Format(time.RFC3339),
	}

	// Bind to all existing workspaces (C-2 fix: surface failure via 207 not silent header).
	// Non-transactional with the credential insert by design: partial bind failures are
	// recoverable (SeedWorkspaceCredentials covers new workspaces; user can manually re-bind).
	if bindErr := h.bindings.BindCredentialToAllUserWorkspaces(c.Request.Context(), row.ID, userID); bindErr != nil {
		c.JSON(http.StatusMultiStatus, gin.H{
			"credential":  resp,
			"bindWarning": "credential created but failed to auto-bind to existing workspaces; please bind manually",
		})
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// List handles GET /api/v1/provider-credentials.
func (h *UserProviderCredentialsHandler) List(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	rows, err := h.store.ListCredentials(c.Request.Context(), "user", userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list credentials"})
		return
	}
	resp := make([]CredentialResponse, 0, len(rows))
	for _, row := range rows {
		r := CredentialResponse{
			ID:                 row.ID,
			Name:               row.Name,
			Kind:               row.Kind,
			Slug:               row.Slug,
			ModelAllowlist:     row.ModelAllowlist,
			ModelContextLimits: row.ModelContextLimits,
			ModelOutputLimits:  row.ModelOutputLimits,
			CreatedAt:          row.CreatedAt.Format(time.RFC3339),
			UpdatedAt:          row.UpdatedAt.Format(time.RFC3339),
		}
		if r.ModelAllowlist == nil {
			r.ModelAllowlist = []string{}
		}
		if r.ModelContextLimits == nil {
			r.ModelContextLimits = map[string]int{}
		}
		if r.ModelOutputLimits == nil {
			r.ModelOutputLimits = map[string]int{}
		}
		resp = append(resp, r)
	}
	c.JSON(http.StatusOK, resp)
}

// Get handles GET /api/v1/provider-credentials/:id.
func (h *UserProviderCredentialsHandler) Get(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	row, err := h.store.GetCredential(c.Request.Context(), "user", userID, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get credential"})
		return
	}
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}
	c.JSON(http.StatusOK, CredentialResponse{
		ID:                 row.ID,
		Name:               row.Name,
		Kind:               row.Kind,
		Slug:               row.Slug,
		ModelAllowlist:     row.ModelAllowlist,
		ModelContextLimits: row.ModelContextLimits,
		ModelOutputLimits:  row.ModelOutputLimits,
		CreatedAt:          row.CreatedAt.Format(time.RFC3339),
		UpdatedAt:          row.UpdatedAt.Format(time.RFC3339),
	})
}

// Delete handles DELETE /api/v1/provider-credentials/:id.
// Notifies all workspaces that had this credential bound so running pods
// pick up the revocation on their next secret reload (C-3 fix).
func (h *UserProviderCredentialsHandler) Delete(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	credID := c.Param("id")

	// Snapshot bound workspaces BEFORE the FK cascade removes the bindings.
	boundWSIDs, listErr := h.bindings.GetCredentialBindings(c.Request.Context(), credID, userID)
	if listErr != nil {
		boundWSIDs = nil // non-fatal; worst case pods keep old key until next restart
	}

	// The unified DeleteCredential returns pgx.ErrNoRows when no row was affected.
	// User delete was historically idempotent (204 even if already gone); preserve
	// that by treating "not found" as success.
	if err := h.store.DeleteCredential(c.Request.Context(), "user", userID, credID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.Status(http.StatusNoContent)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete credential"})
		return
	}

	// Signal each previously-bound workspace so the reload banner appears and
	// the next pod restart writes a secrets manifest without this credential.
	if h.credStateWriter != nil {
		for _, wsID := range boundWSIDs {
			_ = h.credStateWriter.MarkCredentialChanged(c.Request.Context(), wsID) //nolint:errcheck // best-effort: controller re-syncs on next reconcile
		}
	}

	c.Status(http.StatusNoContent)
}

// Bind handles POST /api/v1/provider-credentials/:id/bind/:workspaceId.
func (h *UserProviderCredentialsHandler) Bind(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	credID := c.Param("id")
	wsID := c.Param("workspaceId")

	cred, err := h.store.GetCredential(c.Request.Context(), "user", userID, credID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify credential"})
		return
	}
	if cred == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	if h.wsOwnerCheck != nil {
		if err := h.wsOwnerCheck(c.Request.Context(), userID, wsID); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
	}

	if err := h.bindings.BindCredentialToWorkspace(c.Request.Context(), credID, wsID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to bind credential"})
		return
	}

	if h.credStateWriter != nil {
		_ = h.credStateWriter.MarkCredentialChanged(c.Request.Context(), wsID) //nolint:errcheck // best-effort: controller re-syncs on next reconcile
	}

	c.JSON(http.StatusOK, gin.H{"bound": true})
}

// Unbind handles DELETE /api/v1/provider-credentials/:id/bind/:workspaceId.
// Returns 409 Conflict if the binding is auto-managed (H-1 fix: auto-bindings protected).
func (h *UserProviderCredentialsHandler) Unbind(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	credID := c.Param("id")
	wsID := c.Param("workspaceId")

	cred, err := h.store.GetCredential(c.Request.Context(), "user", userID, credID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify credential"})
		return
	}
	if cred == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	if h.wsOwnerCheck != nil {
		if err := h.wsOwnerCheck(c.Request.Context(), userID, wsID); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
	}

	if err := h.bindings.UnbindCredentialFromWorkspace(c.Request.Context(), credID, wsID); err != nil {
		if errors.Is(err, secrets.ErrAutoBindingProtected) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to unbind credential"})
		return
	}

	if h.credStateWriter != nil {
		_ = h.credStateWriter.MarkCredentialChanged(c.Request.Context(), wsID) //nolint:errcheck // best-effort: controller re-syncs on next reconcile
	}

	c.Status(http.StatusNoContent)
}

// ListBindings handles GET /api/v1/provider-credentials/:id/bindings.
// Returns workspace IDs with their binding source type (explicit vs auto) so the
// UI can show which workspaces have user-initiated vs seeded bindings (M-1 fix).
func (h *UserProviderCredentialsHandler) ListBindings(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	credID := c.Param("id")

	cred, err := h.store.GetCredential(c.Request.Context(), "user", userID, credID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify credential"})
		return
	}
	if cred == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	bindings, err := h.bindings.GetCredentialBindingsWithSource(c.Request.Context(), credID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list bindings"})
		return
	}

	wsIDs := make([]string, len(bindings))
	for i, b := range bindings {
		wsIDs[i] = b.WorkspaceID
	}

	c.JSON(http.StatusOK, gin.H{
		"workspaceIds": wsIDs,
		"bindings":     bindings,
	})
}

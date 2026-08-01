// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Epic 53: MCP server CRUD handlers (admin/org/user scope).
//
// One file covers all three scopes because the logic differs only in
// (ownerType, ownerID) resolution and the crypto path. Authz is enforced
// by the router's middleware chain (AdminGuard / OrgAdminGuard / auth+gates).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/billing"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// mcpServerStore is the narrow DB interface the handler depends on.
type mcpServerStore interface {
	CreateMCPServer(ctx context.Context, row *secrets.MCPServerRow) error
	ListMCPServers(ctx context.Context, ownerType, ownerID string) ([]*secrets.MCPServerRow, error)
	GetMCPServer(ctx context.Context, ownerType, ownerID, serverID string) (*secrets.MCPServerRow, error)
	UpdateMCPServer(ctx context.Context, ownerType, ownerID, serverID string, row *secrets.MCPServerRow) error
	DeleteMCPServer(ctx context.Context, ownerType, ownerID, serverID string) error
	CountMCPServersByOwner(ctx context.Context, ownerType, ownerID string) (int, error)
	CountWorkspaceMCPServers(ctx context.Context, workspaceID string) (int, error)
	GetWorkspaceOrgIDForMCP(ctx context.Context, workspaceID string) (string, error)
	GetWorkspaceUserIDForMCP(ctx context.Context, workspaceID string) (string, error)
	BindMCPServerToWorkspace(ctx context.Context, serverID, workspaceID string) error
	UnbindMCPServerFromWorkspace(ctx context.Context, serverID, workspaceID string) error
	CreateMCPServerAutoApply(ctx context.Context, serverID, targetType string, targetID *string) error
	DeleteMCPServerAutoApply(ctx context.Context, serverID, targetType string, targetID *string) error
	ListMCPServerAutoApply(ctx context.Context, serverID string) ([]secrets.MCPAutoApplyRule, error)
	BackfillMCPServerAutoApply(ctx context.Context, serverID string) (int64, error)
}

// mcpOrgChecker is used by the user-scope handler to resolve org membership
// and the allow_user_mcp_servers policy.
type mcpOrgChecker interface {
	GetUserOrgID(ctx context.Context, userID string) (string, error)
	GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error)
	GetUserPlan(ctx context.Context, userID string) (string, error)
}

// mcpAuditLogger is the audit interface for MCP CRUD events.
type mcpAuditLogger interface {
	LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, metadata map[string]any) error
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
}

// mcpSecretPusher pushes new secrets.json to a running pod after a bind/mutation.
// It's a func type so any push implementation (agentpush.Service, test stub)
// can be adapted without a structural interface match on the return type.
type mcpSecretPusher func(ctx context.Context, userID, workspaceID string) error

// MCPServersHandler handles MCP server CRUD for all three scopes.
type MCPServersHandler struct {
	store        mcpServerStore
	orgChecker   mcpOrgChecker
	adminEncrypt secrets.RootKeyProvider
	orgEncrypt   secrets.RootKeyProvider
	keys         *secrets.KeyService // user-scope DEK; nil for admin/org handlers
	keyStore     secrets.KeyStore    // user-scope key version read
	audit        mcpAuditLogger
	pusher       mcpSecretPusher
	logger       mcpLogger
	settings     mcpSettingsReader
	// ownerType is the scope this handler instance serves ("admin"/"org"/"user").
	// Set at construction; used by Bind/Unbind to verify the server belongs to
	// the caller before mutating bindings (prevents cross-tenant tool injection).
	ownerType string
}

// mcpLogger is a minimal logger for non-fatal warnings (audit failures etc).
type mcpLogger interface {
	Warn(msg string, fields ...any)
}

// mcpSettingsReader reads instance settings (for the kill-switch).
type mcpSettingsReader interface {
	GetBool(ctx context.Context, key string) (bool, error)
}

// NewAdminMCPServersHandler creates a handler for platform-admin scope.
func NewAdminMCPServersHandler(store mcpServerStore, provider secrets.RootKeyProvider) *MCPServersHandler {
	return &MCPServersHandler{store: store, adminEncrypt: provider, ownerType: types.MCPServerOwnerAdmin}
}

// NewOrgMCPServersHandler creates a handler for org-admin scope.
func NewOrgMCPServersHandler(store mcpServerStore, provider secrets.RootKeyProvider, oc mcpOrgChecker) *MCPServersHandler {
	return &MCPServersHandler{store: store, orgEncrypt: provider, orgChecker: oc, ownerType: types.MCPServerOwnerOrg}
}

// NewUserMCPServersHandler creates a handler for personal scope. The keys
// and keyStore are required for user-DEK encryption on Create/Update.
func NewUserMCPServersHandler(store mcpServerStore, oc mcpOrgChecker, keys *secrets.KeyService, keyStore secrets.KeyStore) *MCPServersHandler {
	return &MCPServersHandler{store: store, orgChecker: oc, keys: keys, keyStore: keyStore, ownerType: types.MCPServerOwnerUser}
}

// SetAudit installs the audit logger for MCP CRUD events.
func (h *MCPServersHandler) SetAudit(a mcpAuditLogger) { h.audit = a }

// SetSecretPusher installs the secret pusher for live reload after bind/mutation.
func (h *MCPServersHandler) SetSecretPusher(p mcpSecretPusher) { h.pusher = p }

// SetLogger installs a logger for non-fatal warnings.
func (h *MCPServersHandler) SetLogger(l mcpLogger) { h.logger = l }

// SetSettings installs the instance settings reader (for the kill-switch).
func (h *MCPServersHandler) SetSettings(s mcpSettingsReader) { h.settings = s }

// --- Admin (platform) endpoints ---

func (h *MCPServersHandler) AdminList(c *gin.Context) {
	h.list(c, types.MCPServerOwnerAdmin, types.PlatformMcpOwnerID)
}

func (h *MCPServersHandler) AdminCreate(c *gin.Context) {
	h.create(c, types.MCPServerOwnerAdmin, types.PlatformMcpOwnerID, h.adminEncrypt)
}

func (h *MCPServersHandler) AdminGet(c *gin.Context) {
	h.get(c, types.MCPServerOwnerAdmin, types.PlatformMcpOwnerID)
}

func (h *MCPServersHandler) AdminUpdate(c *gin.Context) {
	h.update(c, types.MCPServerOwnerAdmin, types.PlatformMcpOwnerID, h.adminEncrypt)
}

func (h *MCPServersHandler) AdminDelete(c *gin.Context) {
	h.del(c, types.MCPServerOwnerAdmin, types.PlatformMcpOwnerID)
}

// --- Org endpoints ---

func (h *MCPServersHandler) OrgList(c *gin.Context) {
	orgID := c.Param("id")
	h.list(c, types.MCPServerOwnerOrg, orgID)
}

func (h *MCPServersHandler) OrgCreate(c *gin.Context) {
	orgID := c.Param("id")
	// Kill-switch: platform admin can disable org-admin MCP servers globally.
	if !h.orgAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "org-admin MCP server registration is disabled on this instance"})
		return
	}
	h.create(c, types.MCPServerOwnerOrg, orgID, h.orgEncrypt)
}

// orgAdminAllowed checks the mcp.allowOrgAdminServers instance setting.
// Returns true when the setting is absent or true (fail-open default).
func (h *MCPServersHandler) orgAdminAllowed(c *gin.Context) bool {
	if h.settings == nil {
		return true
	}
	allowed, err := h.settings.GetBool(c.Request.Context(), "mcp.allowOrgAdminServers")
	if err != nil {
		return true // fail-open on read error
	}
	return allowed
}

func (h *MCPServersHandler) OrgGet(c *gin.Context) {
	orgID := c.Param("id")
	h.get(c, types.MCPServerOwnerOrg, orgID)
}

func (h *MCPServersHandler) OrgUpdate(c *gin.Context) {
	orgID := c.Param("id")
	h.update(c, types.MCPServerOwnerOrg, orgID, h.orgEncrypt)
}

func (h *MCPServersHandler) OrgDelete(c *gin.Context) {
	orgID := c.Param("id")
	h.del(c, types.MCPServerOwnerOrg, orgID)
}

// --- User (personal) endpoints ---

func (h *MCPServersHandler) UserList(c *gin.Context) {
	userID := c.GetString("userID")
	h.list(c, types.MCPServerOwnerUser, userID)
}

func (h *MCPServersHandler) UserCreate(c *gin.Context) {
	userID := c.GetString("userID")

	// Gate 1: org-membership check. If the user is in an org, the org
	// admin controls the tool surface — personal MCP servers are refused
	// unless the org's allow_user_mcp_servers policy is true.
	orgID, _ := h.orgChecker.GetUserOrgID(c.Request.Context(), userID)
	if orgID != "" {
		policies, _ := h.orgChecker.GetOrgPolicies(c.Request.Context(), orgID)
		if !userMcpAllowedFromPolicies(policies) {
			c.JSON(http.StatusForbidden, gin.H{"error": "org admin has disabled member MCP servers"})
			return
		}
	}

	// Gate 2: plan-tier quota. The quota is read from PlanFeatures and
	// enforced by counting existing user-scope servers.
	planStr, _ := h.orgChecker.GetUserPlan(c.Request.Context(), userID)
	plan := types.OrgPlan(planStr)
	features := billing.GetPlanFeatures(plan)
	maxMcp := features.MaxPersonalMcpServers
	if maxMcp == 0 {
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":   "personal MCP servers are not available on your plan",
			"feature": "personal_mcp_servers",
			"planId":  string(plan),
		})
		return
	}
	if maxMcp > 0 {
		count, err := h.store.CountMCPServersByOwner(c.Request.Context(), types.MCPServerOwnerUser, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check MCP server quota"})
			return
		}
		if count >= maxMcp {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("personal MCP server limit reached (%d/%d)", count, maxMcp)})
			return
		}
	}

	h.create(c, types.MCPServerOwnerUser, userID, nil)
}

func (h *MCPServersHandler) UserGet(c *gin.Context) {
	userID := c.GetString("userID")
	h.get(c, types.MCPServerOwnerUser, userID)
}

func (h *MCPServersHandler) UserUpdate(c *gin.Context) {
	userID := c.GetString("userID")
	h.update(c, types.MCPServerOwnerUser, userID, nil)
}

func (h *MCPServersHandler) UserDelete(c *gin.Context) {
	userID := c.GetString("userID")
	h.del(c, types.MCPServerOwnerUser, userID)
}

// --- shared CRUD ---

func (h *MCPServersHandler) list(c *gin.Context, ownerType, ownerID string) {
	rows, err := h.store.ListMCPServers(c.Request.Context(), ownerType, ownerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list MCP servers"})
		return
	}
	out := make([]types.MCPServerResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, mcpRowToResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"servers": out})
}

func (h *MCPServersHandler) create(c *gin.Context, ownerType, ownerID string, encryptor secrets.RootKeyProvider) {
	var req types.CreateMCPServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if verr := validateMCPServerCreate(&req); verr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": verr.Error()})
		return
	}

	payload := &types.MCPServerSecretPayload{Env: req.Env, Headers: req.Headers}
	plaintext, err := payload.Encode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode MCP secret"})
		return
	}

	var ciphertext []byte
	var keyVersion int
	if ownerType == types.MCPServerOwnerUser {
		// User-scope: encrypt with the session DEK (zero-knowledge, D13).
		sessionID := c.GetString("sessionID")
		if sessionID == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "user MCP server encryption requires a password-authenticated session"})
			return
		}
		if h.keys == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "encryption key service unavailable"})
			return
		}
		dek, err := h.keys.GetDEK(c.Request.Context(), sessionID, extractMatchedSigningKey(c))
		if err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "encryption key unavailable; re-authenticate and retry"})
			return
		}
		ciphertext, err = secrets.EncryptSecret(dek, plaintext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt MCP secret"})
			return
		}
		// User rows stay at key_version=1 (no server KEK involvement, per D13).
		keyVersion = 1
	} else {
		if encryptor == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "master secret not configured"})
			return
		}
		ciphertext, err = encryptor.Encrypt(c.Request.Context(), plaintext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt MCP secret"})
			return
		}
		keyVersion = secrets.ActiveVersionOf(encryptor)
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	now := time.Now()
	row := &secrets.MCPServerRow{
		ID:         uuid.New().String(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		Name:       req.Name,
		Transport:  req.Transport,
		URL:        req.URL,
		Command:    req.Command,
		Args:       req.Args,
		TimeoutMs:  req.TimeoutMs,
		Ciphertext: ciphertext,
		KeyVersion: keyVersion,
		Enabled:    enabled,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := h.store.CreateMCPServer(c.Request.Context(), row); err != nil {
		if isDuplicateKey(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "MCP server with this name already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create MCP server"})
		return
	}

	metrics.RecordMCPServerOp(ownerType, "create")

	// Auto-apply rule if requested.
	if req.AutoApply != nil {
		_ = h.store.CreateMCPServerAutoApply(c.Request.Context(), row.ID, req.AutoApply.TargetType, req.AutoApply.TargetID)
		_, _ = h.store.BackfillMCPServerAutoApply(c.Request.Context(), row.ID)
	}

	// Audit the creation. Best-effort: failure does not affect the response.
	h.auditCreate(c, ownerType, ownerID, row)

	c.JSON(http.StatusCreated, mcpRowToResponse(row))
}

func (h *MCPServersHandler) get(c *gin.Context, ownerType, ownerID string) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	row, err := h.store.GetMCPServer(c.Request.Context(), ownerType, ownerID, serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get MCP server"})
		return
	}
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
		return
	}
	c.JSON(http.StatusOK, mcpRowToResponse(row))
}

func (h *MCPServersHandler) update(c *gin.Context, ownerType, ownerID string, encryptor secrets.RootKeyProvider) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	existing, err := h.store.GetMCPServer(c.Request.Context(), ownerType, ownerID, serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get MCP server"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
		return
	}

	var req types.UpdateMCPServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	row := &secrets.MCPServerRow{
		Name:      derefStr(req.Name),
		URL:       derefStr(req.URL),
		Command:   derefStr(req.Command),
		Args:      req.Args,
		TimeoutMs: req.TimeoutMs,
		Enabled:   existing.Enabled,
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}

	// Re-encrypt if env/headers changed. We must decrypt the existing
	// ciphertext to preserve the unchanged half (env or headers) — the
	// stored bytes are AES-GCM ciphertext, not JSON.
	if req.Env != nil || req.Headers != nil {
		ctx := c.Request.Context()
		if ownerType == types.MCPServerOwnerUser {
			ctx = context.WithValue(ctx, sessionIDKey{}, c.GetString("sessionID"))
		}
		existingPayload, decErr := h.decryptExisting(ctx, ownerType, ownerID, existing, encryptor)
		if decErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read existing MCP secret for merge"})
			return
		}
		env := existingPayload.Env
		if env == nil {
			env = map[string]string{}
		}
		hdrs := existingPayload.Headers
		if hdrs == nil {
			hdrs = map[string]string{}
		}
		if req.Env != nil {
			env = *req.Env
		}
		if req.Headers != nil {
			hdrs = *req.Headers
		}
		payload := &types.MCPServerSecretPayload{Env: env, Headers: hdrs}
		plaintext, err := payload.Encode()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode MCP secret"})
			return
		}
		if ownerType == types.MCPServerOwnerUser {
			sessionID := c.GetString("sessionID")
			if sessionID == "" || h.keys == nil {
				c.JSON(http.StatusForbidden, gin.H{"error": "user MCP server update requires a password-authenticated session"})
				return
			}
			dek, err := h.keys.GetDEK(c.Request.Context(), sessionID, extractMatchedSigningKey(c))
			if err != nil {
				c.JSON(http.StatusForbidden, gin.H{"error": "encryption key unavailable; re-authenticate and retry"})
				return
			}
			ciphertext, encErr := secrets.EncryptSecret(dek, plaintext)
			if encErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt MCP secret"})
				return
			}
			row.Ciphertext = ciphertext
			row.KeyVersion = 1
		} else {
			if encryptor == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "master secret not configured"})
				return
			}
			ct, encErr := encryptor.Encrypt(c.Request.Context(), plaintext)
			if encErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt MCP secret"})
				return
			}
			row.Ciphertext = ct
			row.KeyVersion = secrets.ActiveVersionOf(encryptor)
		}
	}

	if err := h.store.UpdateMCPServer(c.Request.Context(), ownerType, ownerID, serverID, row); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update MCP server"})
		return
	}
	// Copy updated fields to existing for the response. Only overwrite when
	// the request actually provided a value (nil = preserve existing), matching
	// the store's nil-preservation semantics.
	if row.Name != "" {
		existing.Name = row.Name
	}
	if row.URL != "" {
		existing.URL = row.URL
	}
	if row.Command != "" {
		existing.Command = row.Command
	}
	if row.Args != nil {
		existing.Args = row.Args
	}
	if row.TimeoutMs != nil {
		existing.TimeoutMs = row.TimeoutMs
	}
	existing.Enabled = row.Enabled
	existing.UpdatedAt = row.UpdatedAt
	c.JSON(http.StatusOK, mcpRowToResponse(existing))
}

func (h *MCPServersHandler) del(c *gin.Context, ownerType, ownerID string) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	err := h.store.DeleteMCPServer(c.Request.Context(), ownerType, ownerID, serverID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete MCP server"})
		return
	}
	h.auditDelete(c, ownerType, ownerID, serverID)
	metrics.RecordMCPServerOp(ownerType, "delete")
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// auditCreate emits a best-effort audit entry for MCP server creation.
func (h *MCPServersHandler) auditCreate(c *gin.Context, ownerType, ownerID string, row *secrets.MCPServerRow) {
	if h.audit == nil {
		return
	}
	actorID := c.GetString("userID")
	domain := "admin"
	var orgID *string
	if ownerType == types.MCPServerOwnerOrg {
		domain = "org"
		orgID = &ownerID
	}
	meta := map[string]any{"name": row.Name, "transport": row.Transport, "scope": ownerType}
	if ownerType == types.MCPServerOwnerUser {
		domain = "user"
	}
	if err := h.audit.LogAuditEvent(c.Request.Context(), domain, actorID, "mcp_server.create", row.ID, orgID, meta); err != nil && h.logger != nil {
		h.logger.Warn("mcp: audit emission failed", "action", "mcp_server.create", "error", err.Error())
	}
}

// auditDelete emits a best-effort audit entry for MCP server deletion.
func (h *MCPServersHandler) auditDelete(c *gin.Context, ownerType, ownerID, serverID string) {
	if h.audit == nil {
		return
	}
	actorID := c.GetString("userID")
	domain := "admin"
	var orgID *string
	if ownerType == types.MCPServerOwnerOrg {
		domain = "org"
		orgID = &ownerID
	}
	if ownerType == types.MCPServerOwnerUser {
		domain = "user"
	}
	meta := map[string]any{"scope": ownerType}
	if err := h.audit.LogAuditEvent(c.Request.Context(), domain, actorID, "mcp_server.delete", serverID, orgID, meta); err != nil && h.logger != nil {
		h.logger.Warn("mcp: audit emission failed", "action", "mcp_server.delete", "error", err.Error())
	}
}

// --- bindings ---

func (h *MCPServersHandler) Bind(c *gin.Context) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	var body struct {
		WorkspaceID string `json:"workspaceId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify the caller owns the server AND the workspace. Without this,
	// a user on the /me/ route could bind any server to any workspace
	// (cross-tenant tool injection).
	ownerID := h.resolveOwnerID(c)
	if !h.verifyServerOwnership(c, serverID, ownerID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
		return
	}
	// For user-scope binds, verify the caller owns the target workspace.
	// Admin/org scopes are behind AdminGuard/OrgAdminGuard which already
	// verifies authorization.
	if h.ownerType == types.MCPServerOwnerUser {
		wsUserID, err := h.store.GetWorkspaceUserIDForMCP(c.Request.Context(), body.WorkspaceID)
		if err != nil || wsUserID != c.GetString("userID") {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
	}

	// Enforce org policy quota: max_mcp_servers_per_workspace. The bound
	// server count is checked BEFORE the bind; if adding this server would
	// exceed the org's cap, reject with 409. Platform-admin servers (owner_type
	// 'admin') are exempt from org quotas (they are platform policy).
	if h.ownerType != types.MCPServerOwnerAdmin {
		max := h.resolveWorkspaceQuota(c, body.WorkspaceID)
		current, err := h.store.CountWorkspaceMCPServers(c.Request.Context(), body.WorkspaceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check workspace MCP quota"})
			return
		}
		if current >= max {
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("workspace MCP server limit reached (%d/%d)", current, max),
			})
			return
		}
	}

	if err := h.store.BindMCPServerToWorkspace(c.Request.Context(), serverID, body.WorkspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to bind MCP server"})
		return
	}

	// Trigger a live reload so the bound server's tools appear immediately
	// in the running agent. Best-effort: a failure is logged but does not
	// affect the 200 response (the binding is already persisted).
	h.pushReload(c, body.WorkspaceID)

	c.JSON(http.StatusOK, gin.H{"bound": true})
	metrics.RecordMCPBinding("explicit")
}

// resolveOwnerID returns the owner_id this handler instance is scoped to.
// For admin: "_platform". For org: the :id path param. For user: the caller's userID.
func (h *MCPServersHandler) resolveOwnerID(c *gin.Context) string {
	switch h.ownerType {
	case types.MCPServerOwnerAdmin:
		return types.PlatformMcpOwnerID
	case types.MCPServerOwnerOrg:
		return c.Param("id")
	case types.MCPServerOwnerUser:
		return c.GetString("userID")
	}
	return ""
}

// verifyServerOwnership confirms the server exists and belongs to the
// (ownerType, ownerID) scope this handler serves. Returns false when the
// server doesn't exist or belongs to a different owner (404 — route
// existence hiding, matching AdminGuard convention).
func (h *MCPServersHandler) verifyServerOwnership(c *gin.Context, serverID, ownerID string) bool {
	row, err := h.store.GetMCPServer(c.Request.Context(), h.ownerType, ownerID, serverID)
	if err != nil || row == nil {
		return false
	}
	return true
}

// resolveWorkspaceQuota resolves max_mcp_servers_per_workspace from the org
// policy of the workspace's owning org. Returns the default when the workspace
// has no org (personal workspace) or no policy is set.
func (h *MCPServersHandler) resolveWorkspaceQuota(c *gin.Context, workspaceID string) int {
	orgID, err := h.store.GetWorkspaceOrgIDForMCP(c.Request.Context(), workspaceID)
	if err != nil || orgID == "" {
		return types.DefaultMaxMcpServersPerWorkspace
	}
	policies, err := h.orgChecker.GetOrgPolicies(c.Request.Context(), orgID)
	if err != nil {
		return types.DefaultMaxMcpServersPerWorkspace
	}
	for _, p := range policies {
		if p.Key == types.PolicyMaxMcpServersPerWorkspace {
			var n int
			if json.Unmarshal(p.Value, &n) == nil {
				return n
			}
		}
	}
	return types.DefaultMaxMcpServersPerWorkspace
}

// pushReload triggers a secret push to the workspace's running pod so new
// MCP server config reaches the agent without requiring a pod restart.
func (h *MCPServersHandler) pushReload(c *gin.Context, workspaceID string) {
	if h.pusher == nil {
		return
	}
	userID := c.GetString("userID")
	if err := h.pusher(c.Request.Context(), userID, workspaceID); err != nil {
		if h.logger != nil {
			h.logger.Warn("mcp: secret push after bind failed",
				"workspaceID", workspaceID, "error", err.Error())
		}
	}
}

func (h *MCPServersHandler) Unbind(c *gin.Context) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	workspaceID := c.Param("workspaceId")

	// Verify ownership (same as Bind — prevents cross-tenant unbind).
	ownerID := h.resolveOwnerID(c)
	if !h.verifyServerOwnership(c, serverID, ownerID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "MCP server not found"})
		return
	}

	err := h.store.UnbindMCPServerFromWorkspace(c.Request.Context(), serverID, workspaceID)
	if err != nil {
		if errors.Is(err, secrets.ErrAutoBindingProtected) {
			c.JSON(http.StatusConflict, gin.H{"error": "cannot remove auto-managed binding"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to unbind MCP server"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"unbound": true})
}

// --- auto-apply ---

func (h *MCPServersHandler) ListAutoApply(c *gin.Context) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	rules, err := h.store.ListMCPServerAutoApply(c.Request.Context(), serverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list auto-apply rules"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rules": rules})
}

func (h *MCPServersHandler) CreateAutoApply(c *gin.Context) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	var body struct {
		TargetType string  `json:"targetType" binding:"required"`
		TargetID   *string `json:"targetId,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.CreateMCPServerAutoApply(c.Request.Context(), serverID, body.TargetType, body.TargetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create auto-apply rule"})
		return
	}
	_, _ = h.store.BackfillMCPServerAutoApply(c.Request.Context(), serverID)
	c.JSON(http.StatusCreated, gin.H{"created": true})
}

func (h *MCPServersHandler) DeleteAutoApply(c *gin.Context) {
	serverID := c.Param("serverId")
	if serverID == "" {
		serverID = c.Param("id")
	}
	targetType := c.Param("targetType")
	var targetID *string
	if tid := c.Param("targetId"); tid != "" {
		targetID = &tid
	}
	if err := h.store.DeleteMCPServerAutoApply(c.Request.Context(), serverID, targetType, targetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete auto-apply rule"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// --- helpers ---

func validateMCPServerCreate(req *types.CreateMCPServerRequest) error {
	if !types.ValidMCPServerName(req.Name) {
		return fmt.Errorf("invalid name: must match [a-zA-Z0-9][a-zA-Z0-9_-]{0,62}")
	}
	if !types.ValidMCPServerTransport(req.Transport) {
		return fmt.Errorf("invalid transport: must be http, sse, or stdio")
	}
	if req.Transport == types.MCPServerTransportHTTP || req.Transport == types.MCPServerTransportSSE {
		if req.URL == "" {
			return fmt.Errorf("url is required for %s transport", req.Transport)
		}
		if verr := validateMCPURL(req.URL); verr != nil {
			return verr
		}
	}
	if req.Transport == types.MCPServerTransportStdio && req.Command == "" {
		return fmt.Errorf("command is required for stdio transport")
	}
	return nil
}

func validateMCPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("url scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url must have a host")
	}
	// SSRF defense-in-depth: block loopback, link-local, cloud metadata.
	blocked := []string{"127.0.0.1", "localhost", "0.0.0.0", "169.254.169.254", "::1", "[::1]"}
	for _, b := range blocked {
		if strings.EqualFold(host, b) {
			return fmt.Errorf("url host is blocked (loopback/link-local/metadata)")
		}
	}
	return nil
}

func mcpRowToResponse(r *secrets.MCPServerRow) types.MCPServerResponse {
	return types.MCPServerResponse{
		ID:        r.ID,
		Name:      r.Name,
		Transport: r.Transport,
		URL:       r.URL,
		Command:   r.Command,
		Args:      r.Args,
		TimeoutMs: r.TimeoutMs,
		HasSecret: len(r.Ciphertext) > 0,
		Enabled:   r.Enabled,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

func userMcpAllowedFromPolicies(policies []*types.OrgPolicy) bool {
	for _, p := range policies {
		if p.Key == types.PolicyAllowUserMcpServers {
			var allowed bool
			if json.Unmarshal(p.Value, &allowed) == nil {
				return allowed
			}
		}
	}
	return false
}

func isDuplicateKey(err error) bool {
	return strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint")
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// decryptExisting decrypts an MCP server's ciphertext to recover the
// plaintext env/headers payload. Used by update() to merge partial updates
// without destroying the unchanged half. The decryptor is selected by
// ownerType: admin/org via RootKeyProvider, user via session DEK.
func (h *MCPServersHandler) decryptExisting(ctx context.Context, ownerType, ownerID string, row *secrets.MCPServerRow, encryptor secrets.RootKeyProvider) (*types.MCPServerSecretPayload, error) {
	if len(row.Ciphertext) == 0 {
		return &types.MCPServerSecretPayload{}, nil
	}
	var plaintext []byte
	var err error
	switch ownerType {
	case types.MCPServerOwnerAdmin:
		if encryptor == nil {
			return nil, fmt.Errorf("admin provider not configured")
		}
		plaintext, err = encryptor.Decrypt(ctx, row.Ciphertext)
	case types.MCPServerOwnerOrg:
		if encryptor == nil {
			return nil, fmt.Errorf("org provider not configured")
		}
		plaintext, err = encryptor.Decrypt(ctx, row.Ciphertext)
	case types.MCPServerOwnerUser:
		if h.keys == nil {
			return nil, fmt.Errorf("key service not configured")
		}
		// Comma-ok pattern: a future caller may forget to set sessionIDKey.
		sessionID, _ := ctx.Value(sessionIDKey{}).(string)
		if sessionID == "" {
			return nil, fmt.Errorf("session ID required for user-scope decrypt")
		}
		dek, dErr := h.keys.GetDEK(ctx, sessionID, nil)
		if dErr != nil {
			return nil, dErr
		}
		plaintext, err = secrets.DecryptSecret(dek, row.Ciphertext)
	default:
		return nil, fmt.Errorf("unsupported owner_type %q", ownerType)
	}
	if err != nil {
		return nil, err
	}
	return types.DecodeMCPServerSecretPayload(plaintext)
}

// sessionIDKey is a context key for threading the session ID into decryptExisting.
type sessionIDKey struct{}

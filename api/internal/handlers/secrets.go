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
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	pkgerrors "github.com/lenaxia/llmsafespaces/pkg/errors"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// SecretsHandler handles HTTP requests for the secrets API.
type SecretsHandler struct {
	svc              *secrets.SecretService
	pusher           *agentpush.Service
	podIPResolver    PodIPResolver
	logger           pkginterfaces.LoggerInterface
	passwordVerifier PasswordVerifier
	credStateWriter  CredentialStateWriter
	modelCache       ModelCache
}

// OrgPolicyChecker is the minimal interface needed to filter models
// by org policy. The policy.Service implements it.
type OrgPolicyChecker interface {
	GetEffectivePolicy(ctx context.Context, orgID string) (*types.OrgPolicyValues, error)
}

// ModelSelectionRecorder records model selection events for billing/metering.
type ModelSelectionRecorder interface {
	RecordModelSelection(modelID, providerID string)
}

// CredentialStateWriter records that workspace credentials have changed.
// Satisfied by *database.Service.
type CredentialStateWriter interface {
	MarkCredentialChanged(ctx context.Context, workspaceID string) error
}

// SetCredentialStateWriter installs the writer. If nil, MarkCredentialChanged
// is silently skipped (banner won't appear but no crash).
func (h *SecretsHandler) SetCredentialStateWriter(w CredentialStateWriter) {
	h.credStateWriter = w
}

// PodIPResolver looks up the pod IP for a workspace.
type PodIPResolver interface {
	GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error)
}

// PasswordVerifier confirms a user's password against the stored bcrypt
// hash. Used by RevealSecret to enforce a re-authentication gate before
// returning plaintext: a stolen JWT alone must not be sufficient to
// extract every secret. Implementations MUST run constant-time
// comparison (bcrypt.CompareHashAndPassword satisfies this) and MUST
// return a sentinel-typed error rather than the raw bcrypt error so
// the handler can map it to a uniform 403 without leaking timing or
// state information.
type PasswordVerifier interface {
	VerifyPassword(ctx context.Context, userID string, password []byte) error
}

// NewSecretsHandler creates a new SecretsHandler.
func NewSecretsHandler(svc *secrets.SecretService) *SecretsHandler {
	return &SecretsHandler{svc: svc}
}

// SetPasswordVerifier installs the verifier used to confirm the
// caller's password on RevealSecret. If left nil the reveal handler
// rejects every request with 503; this is intentional because shipping
// without password verification is exactly the security theater we
// fixed (validator finding on RevealSecret in worklog 0094 audit).
func (h *SecretsHandler) SetPasswordVerifier(v PasswordVerifier) {
	h.passwordVerifier = v
}

// SetPodIPResolver sets the resolver for looking up pod IPs.
func (h *SecretsHandler) SetPodIPResolver(r PodIPResolver) {
	h.podIPResolver = r
}

// HasPodIPResolver reports whether a PodIPResolver has been configured.
// Used by wiring tests to verify the handler is fully constructed; without
// a resolver the reload-secrets endpoint and the SetBindings auto-push
// silently no-op (Bug 1 + Bug 2 in worklog 0085).
func (h *SecretsHandler) HasPodIPResolver() bool {
	return h.podIPResolver != nil
}

// SetLogger installs the logger used to surface non-fatal failures from
// the bind-time auto-push. Optional; if nil, failures are silent (which
// is exactly Bug 2 in worklog 0085 — do not leave nil in production).
func (h *SecretsHandler) SetLogger(l pkginterfaces.LoggerInterface) {
	h.logger = l
}

// SetModelCache injects the shared model cache so SecretsHandler can evict
// a workspace's cache entry after credential binds (M2-a: replaces the
// former package-level global defaultModelCache).
func (h *SecretsHandler) SetModelCache(c ModelCache) {
	h.modelCache = c
}

// CreateSecret handles POST /api/v1/secrets
func (h *SecretsHandler) CreateSecret(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req secrets.CreateSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	resp, err := h.svc.CreateSecret(c.Request.Context(), userID, sessionID, extractMatchedSigningKey(c), req)
	if err != nil {
		handleSecretError(c, err)
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// ListSecrets handles GET /api/v1/secrets
func (h *SecretsHandler) ListSecrets(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	list, err := h.svc.ListSecrets(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list secrets"})
		return
	}
	if list == nil {
		list = []*secrets.SecretResponse{}
	}

	c.JSON(http.StatusOK, gin.H{"secrets": list})
}

// GetSecret handles GET /api/v1/secrets/:id
func (h *SecretsHandler) GetSecret(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	secretID := c.Param("id")
	resp, err := h.svc.GetSecret(c.Request.Context(), userID, secretID)
	if err != nil {
		handleSecretError(c, err)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// UpdateSecret handles PUT /api/v1/secrets/:id
func (h *SecretsHandler) UpdateSecret(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	secretID := c.Param("id")
	var req secrets.UpdateSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := h.svc.UpdateSecret(c.Request.Context(), userID, sessionID, extractMatchedSigningKey(c), secretID, req); err != nil {
		handleSecretError(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// DeleteSecret handles DELETE /api/v1/secrets/:id
func (h *SecretsHandler) DeleteSecret(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	secretID := c.Param("id")
	if err := h.svc.DeleteSecret(c.Request.Context(), userID, secretID); err != nil {
		handleSecretError(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// RevealSecret handles POST /api/v1/secrets/:id/reveal
// Requires password reconfirmation: a stolen JWT alone must not be
// sufficient to extract every secret. Without a configured
// PasswordVerifier the handler returns 503 — shipping without
// verification is exactly the security theater the validator audit
// flagged. The bcrypt.CompareHashAndPassword call inside the verifier
// is constant-time, so failed-password timing does not differentiate
// from missing-DEK timing in practice.
func (h *SecretsHandler) RevealSecret(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	secretID := c.Param("id")
	var req struct {
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password required to reveal secret"})
		return
	}

	if h.passwordVerifier == nil {
		// Fail closed: refusing to serve reveals without verification
		// is safer than serving them without verification.
		h.warn("RevealSecret blocked: no password verifier configured",
			"userID", userID, "secretID", secretID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "password verification not configured"})
		return
	}
	if err := h.passwordVerifier.VerifyPassword(c.Request.Context(), userID, []byte(req.Password)); err != nil {
		// Uniform 403 regardless of why verification failed (wrong
		// password, user not found, bcrypt error). Do not log the
		// raw error at the request level since it could include
		// bcrypt diagnostic detail; warn-level only.
		h.warn("RevealSecret password verification failed",
			"userID", userID, "secretID", secretID)
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid password"})
		return
	}

	plaintext, err := h.svc.DecryptSecretValue(c.Request.Context(), userID, sessionID, extractMatchedSigningKey(c), secretID)
	if err != nil {
		// Log every reveal failure with full context so operators can correlate
		// user reports with audit log entries. ErrCiphertextDecryptFailed and
		// ErrDEKUnavailable both produce structured audit_log entries via the
		// service layer; this Warn surfaces them in the application log too,
		// which is where most operator alerting hangs off.
		switch {
		case errors.Is(err, secrets.ErrCiphertextDecryptFailed):
			h.warn("RevealSecret: ciphertext decrypt failed (DEK present, ciphertext mismatch — likely DEK rotation without re-encrypt)",
				"userID", userID, "secretID", secretID, "error", err.Error())
		case errors.Is(err, secrets.ErrDEKUnavailable):
			h.warn("RevealSecret: DEK unavailable (session expired or cache flushed; user should re-authenticate)",
				"userID", userID, "secretID", secretID, "sessionID", sessionID, "error", err.Error())
		case errors.Is(err, secrets.ErrSecretNotFound), errors.Is(err, secrets.ErrUserKeysMissing):
			// Expected, lower-severity failures — log at Info to keep Warn
			// dashboards focused on operational issues.
			h.info("RevealSecret: known failure", "userID", userID, "secretID", secretID, "error", err.Error())
		default:
			// Unmapped error — these are the ones operators most need to see.
			h.warn("RevealSecret: unexpected error",
				"userID", userID, "secretID", secretID, "error", err.Error())
		}
		handleSecretError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"value": string(plaintext)})
}

// GetSecretBindings handles GET /api/v1/secrets/:id/bindings
func (h *SecretsHandler) GetSecretBindings(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	secretID := c.Param("id")
	workspaces, err := h.svc.GetBindingsForSecret(c.Request.Context(), userID, secretID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get bindings"})
		return
	}
	if workspaces == nil {
		workspaces = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"workspaces": workspaces})
}

func (h *SecretsHandler) SetBindings(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	var req secrets.SetBindingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	result, err := h.svc.SetBindings(c.Request.Context(), userID, workspaceID, req.SecretIDs)
	if err != nil {
		handleSecretError(c, err)
		return
	}

	if result.LLMProviderAffected && h.credStateWriter != nil {
		if err := h.credStateWriter.MarkCredentialChanged(c.Request.Context(), workspaceID); err != nil {
			if h.logger != nil {
				h.logger.Warn("mark credential changed failed; banner may not appear",
					"workspaceID", workspaceID, "error", err.Error())
			}
		}
	}

	h.pushSecretsToAgent(c, userID, workspaceID)

	c.Status(http.StatusNoContent)
}

// GetBindings handles GET /api/v1/workspaces/:id/bindings
func (h *SecretsHandler) GetBindings(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	resp, err := h.svc.GetBindings(c.Request.Context(), userID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get bindings"})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// ReloadSecrets handles POST /api/v1/workspaces/:id/reload-secrets
// Decrypts bound secrets and pushes them to the running pod's agentd.
//
// Two failure classes get different HTTP status codes:
//
//   - InjectSecrets failures (bad workspaceID, DEK unavailable, wrapped
//     ciphertext corrupted) are mapped by handleSecretError to 400/403/500.
//   - Push transport / agentd failures map to 503/409/502.
//
// Both flow through the same shared agentpush.Service (constructed once
// by the wiring layer, reused across every request) so on-wire behavior
// matches SetBindings and the workspace-service auto-push exactly.
// agentpush wraps InjectSecrets errors with "inject secrets: %w", so we
// can unwrap to recover the typed error for handleSecretError.
func (h *SecretsHandler) ReloadSecrets(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	ctx := agentpush.WithAuth(c.Request.Context(), sessionID, extractMatchedSigningKey(c))
	result, err := h.getPusher().Push(ctx, userID, workspaceID)
	if err != nil {
		// Inject-side failures need the typed-error mapping to 400/403/500.
		// agentpush wraps them with "inject secrets:" so we can unwrap the
		// original SecretService error and route it through the shared
		// handler. This avoids a second, redundant InjectSecrets call in
		// this endpoint.
		if inner := errors.Unwrap(err); inner != nil && strings.HasPrefix(err.Error(), "inject secrets") {
			handleSecretError(c, inner)
			return
		}
		switch {
		case errors.Is(err, agentpush.ErrNoPodIPResolver):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "secret reload not configured"})
		case errors.Is(err, agentpush.ErrNoRunningPod):
			c.JSON(http.StatusConflict, gin.H{"error": "workspace has no running pod"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, reloadResult{Reloaded: result.Reloaded, Restarted: result.Restarted})
}

type reloadResult struct {
	Reloaded  int  `json:"reloaded"`
	Restarted bool `json:"restarted"`
}

// pushSecretsToAgent runs the bind-time live delivery of the latest secret
// snapshot for a workspace.
//
// Epic 35: the durable K8s Secret path (EnsureSecretsManifest) has been
// removed — secretless injection means the init container fetches credentials
// directly from the API at boot. This function now handles ONLY the live HTTP
// push to running pods. Credentials bound during suspend are picked up at the
// next pod boot via the bootstrap endpoint.
//
// SOLID note (PR #407 review pass 2): the push path always calls
// InjectSecrets — there is no branch on sessionID. Both callers (JWT
// auth and API-key auth) flow through the same method because
// InjectSecrets internally degrades user-DEK lookups to skip-with-audit
// when the DEK is unavailable (real session expired, API-key pseudo-
// session, or no session at all).
//
// The live push sends the payload even when it is the empty array '[]' — the
// agent uses this to CLEAR its in-memory secret materialisations. Without
// this an unbind leaves the live pod with stale plaintext until restart.
//
// Delegates to agentpush.Service (extracted for the pod-recreation auto-push
// path in worklog 0589) so both handler-driven pushes and the
// workspace.Service.GetWorkspaceStatus-driven auto-push share one code path.
func (h *SecretsHandler) pushSecretsToAgent(c *gin.Context, userID, workspaceID string) {
	_, sessionID := extractAuth(c)
	ctx := agentpush.WithAuth(c.Request.Context(), sessionID, extractMatchedSigningKey(c))

	_, err := h.getPusher().Push(ctx, userID, workspaceID)
	if err == nil {
		return
	}
	if errors.Is(err, agentpush.ErrNoRunningPod) {
		h.info("reload-secrets skipped: no running pod",
			"workspaceID", workspaceID)
		return
	}
	h.warn("reload-secrets push to agent failed",
		"workspaceID", workspaceID, "error", err.Error())
}

func (h *SecretsHandler) warn(msg string, fields ...interface{}) {
	if h.logger != nil {
		h.logger.Warn(msg, fields...)
	}
}

func (h *SecretsHandler) info(msg string, fields ...interface{}) {
	if h.logger != nil {
		h.logger.Info(msg, fields...)
	}
}

// getPusher returns the injected pusher, or lazily constructs one from
// the handler's own deps if wiring only supplied the individual pieces
// (podIPResolver, modelCache, logger, svc). This lets the handler work
// with either the new "inject an agentpush.Service" wiring OR the
// pre-existing "SetPodIPResolver + SetModelCache" wiring, so the
// migration to the shared pusher can happen without breaking any of the
// dozens of tests that use the older setter-style construction.
func (h *SecretsHandler) getPusher() *agentpush.Service {
	if h.pusher != nil {
		return h.pusher
	}
	opts := []agentpush.Option{}
	if h.podIPResolver != nil {
		opts = append(opts, agentpush.WithPodIPResolver(h.podIPResolver))
	}
	if h.modelCache != nil {
		opts = append(opts, agentpush.WithModelCache(h.modelCache))
	}
	if h.logger != nil {
		opts = append(opts, agentpush.WithLogger(h.logger))
	}
	h.pusher = agentpush.New(h.svc, opts...)
	return h.pusher
}

// SetAgentPusher installs a pre-built agentpush.Service. Preferred over
// SetPodIPResolver + SetModelCache + SetLogger for new call sites, and
// used by app.New to share a single pusher instance across the handler
// and workspace.Service (the pod-recreation auto-push consumer).
func (h *SecretsHandler) SetAgentPusher(p *agentpush.Service) {
	h.pusher = p
}

// GetAuditLog handles GET /api/v1/secrets/audit
func (h *SecretsHandler) GetAuditLog(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	query := secrets.AuditQuery{
		Action:      c.Query("action"),
		SecretID:    c.Query("secretId"),
		WorkspaceID: c.Query("workspaceId"),
		Limit:       100,
	}

	entries, err := h.svc.QueryAudit(c.Request.Context(), userID, query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query audit log"})
		return
	}
	if entries == nil {
		entries = []*secrets.AuditEntry{}
	}

	c.JSON(http.StatusOK, gin.H{"entries": entries})
}

// KeyRotator is the interface needed by the rotation handler.
type KeyRotator interface {
	RotateKeyWithPassword(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) (secrets.RotationResult, error)
	ChangePassword(ctx context.Context, userID, sessionID string, oldPassword, newPassword []byte) error
	ResetWithRecoveryKey(ctx context.Context, userID string, recoveryKeyHex string, newPassword []byte) (string, error)
}

// PasswordHashUpdater updates the user's bcrypt hash in the database.
type PasswordHashUpdater interface {
	UpdatePasswordHash(ctx context.Context, userID string, newPassword []byte) error
}

// SessionRevoker revokes every outstanding JWT for a user. Used by
// ChangePassword (G38) to mirror the password-reset flow's OWASP-mandated
// session invalidation. Best-effort: callers MUST treat an error as
// non-fatal — the password has already been changed cryptographically
// and rollback is not possible.
type SessionRevoker interface {
	RevokeAllUserSessions(ctx context.Context, userID string) error
}

// RotateKeyHandler handles account key management endpoints.
type RotateKeyHandler struct {
	keySvc    KeyRotator
	pwUpdater PasswordHashUpdater
	revoker   SessionRevoker
	logger    pkginterfaces.LoggerInterface
	auditFunc func(userID, action string)
}

// NewRotateKeyHandler creates a new RotateKeyHandler.
func NewRotateKeyHandler(keySvc KeyRotator) *RotateKeyHandler {
	return &RotateKeyHandler{keySvc: keySvc}
}

// SetPasswordUpdater sets the optional password hash updater.
func (h *RotateKeyHandler) SetPasswordUpdater(u PasswordHashUpdater) {
	h.pwUpdater = u
}

// SetSessionRevoker wires the session revoker used by ChangePassword to
// invalidate every outstanding JWT after a successful password change
// (G38). Optional: when nil, ChangePassword succeeds without revoking —
// matches the pre-G38 behavior. Production wiring supplies the auth
// service's RevokeAllUserSessions.
func (h *RotateKeyHandler) SetSessionRevoker(r SessionRevoker) {
	h.revoker = r
}

// SetLogger wires an optional logger used to surface non-fatal failures
// (e.g. revocation failing after a successful password change). When nil,
// such failures are silent — production wiring supplies the app logger.
func (h *RotateKeyHandler) SetLogger(l pkginterfaces.LoggerInterface) {
	h.logger = l
}

// SetAuditFunc sets an optional audit callback for key operations.
func (h *RotateKeyHandler) SetAuditFunc(f func(userID, action string)) {
	h.auditFunc = f
}

// RotateKey handles POST /api/v1/account/rotate-key.
// On success the response includes the new keyVersion AND a freshly-
// issued recoveryKey: the old recovery key wraps the now-discarded
// old DEK, so the user must save the new one. This is a one-time
// display — the API does not store it anywhere recoverable.
func (h *RotateKeyHandler) RotateKey(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req struct {
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password required for key rotation"})
		return
	}

	result, err := h.keySvc.RotateKeyWithPassword(c.Request.Context(), userID, []byte(req.Password), sessionID, 24*time.Hour)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidPassword) {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid password"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "key rotation failed"})
		return
	}

	if h.auditFunc != nil {
		h.auditFunc(userID, "rotate")
	}

	c.JSON(http.StatusOK, gin.H{
		"keyVersion":  result.NewKeyVersion,
		"recoveryKey": result.NewRecoveryKeyHex,
	})
}

// ChangePassword handles POST /api/v1/account/change-password
func (h *RotateKeyHandler) ChangePassword(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req struct {
		OldPassword string `json:"oldPassword" binding:"required"`
		NewPassword string `json:"newPassword" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "oldPassword and newPassword (min 8 chars) required"})
		return
	}

	if err := h.keySvc.ChangePassword(c.Request.Context(), userID, sessionID, []byte(req.OldPassword), []byte(req.NewPassword)); err != nil {
		if errors.Is(err, secrets.ErrInvalidPassword) {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid current password"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "password change failed"})
		return
	}

	// Also update the bcrypt hash in the user database
	if h.pwUpdater != nil {
		if err := h.pwUpdater.UpdatePasswordHash(c.Request.Context(), userID, []byte(req.NewPassword)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "password change failed"})
			return
		}
	}

	// G38: invalidate every outstanding JWT — including the caller's own
	// — so a token stolen before the change is no longer useful. Runs
	// only after both the DEK re-wrap and the bcrypt update commit, so a
	// stolen JWT cannot race the response. Best-effort: the password is
	// already changed and rollback is impossible, so a revocation failure
	// is logged and the change still reports success. Mirrors
	// password_reset.go:309-315 (OWASP ASVS V2.5.2).
	if h.revoker != nil {
		if err := h.revoker.RevokeAllUserSessions(c.Request.Context(), userID); err != nil && h.logger != nil {
			h.logger.Warn("ChangePassword: session revocation failed (non-fatal; password was changed)",
				"error", err.Error(), "user_id", userID)
		}
	}

	c.Status(http.StatusNoContent)
}

// RecoverAccount handles POST /api/v1/account/recover
func (h *RotateKeyHandler) RecoverAccount(c *gin.Context) {
	// This is a public-ish endpoint (user forgot password) but still needs some identity.
	// In practice, this would be called after email verification. For now, require userID in body.
	var req struct {
		UserID      string `json:"userId" binding:"required"`
		RecoveryKey string `json:"recoveryKey" binding:"required"`
		NewPassword string `json:"newPassword" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "userId, recoveryKey, and newPassword required"})
		return
	}

	newRecoveryKey, err := h.keySvc.ResetWithRecoveryKey(c.Request.Context(), req.UserID, req.RecoveryKey, []byte(req.NewPassword))
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid recovery key"})
		return
	}

	// Also update the bcrypt hash so the user can login with the new password
	if h.pwUpdater != nil {
		if err := h.pwUpdater.UpdatePasswordHash(c.Request.Context(), req.UserID, []byte(req.NewPassword)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "recovery failed"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"recoveryKey": newRecoveryKey})
}

// extractAuth gets userID and sessionID (jti) from the Gin context.
// Both values are type-asserted with the comma-ok form so a malformed
// context (e.g. middleware put a non-string under the key) produces an
// empty result rather than a goroutine panic that takes down the
// request. Empty userID is treated as unauthenticated by every caller.
func extractAuth(c *gin.Context) (userID, sessionID string) {
	if uid, exists := c.Get("userID"); exists {
		if s, ok := uid.(string); ok {
			userID = s
		}
	}
	if sid, exists := c.Get("sessionID"); exists {
		if s, ok := sid.(string); ok {
			sessionID = s
		}
	}
	return userID, sessionID
}

// extractMatchedSigningKey returns the JWT signing key that validated
// the caller's token, as set by AuthMiddleware (Epic 56). Returns nil
// for API-key auth, legacy-cache hits, or any handler reached without
// going through AuthMiddleware (tests).
//
// Pass the return value into KeyService.GetDEK so the rehydrate path
// can derive the per-session KEK from the same key the JWT validated
// under. nil is a valid input — GetDEK falls through to ErrDEKUnavailable
// which triggers soft-unlock at the UI.
func extractMatchedSigningKey(c *gin.Context) []byte {
	if v, ok := c.Get("jwt_signing_key"); ok {
		if b, ok := v.([]byte); ok {
			return b
		}
	}
	return nil
}

// handleSecretError maps domain errors to HTTP responses. US-46.4: the
// secrets-package sentinels are now *pkgerrors.StatusError values that
// carry their own HTTP status code and user-facing message.
//
// For security-sensitive errors (404/403/409/412), the StatusError.Message
// is the exact user-facing text — wrapping detail from fmt.Errorf must not
// leak internal paths or secret names to the client.
//
// For validation errors (400), the wrapped err.Error() is returned instead
// because it includes per-call detail the caller needs to fix the input
// (e.g. "ssh-key requires metadata with key_type field").
func handleSecretError(c *gin.Context, err error) {
	var se *pkgerrors.StatusError
	if errors.As(err, &se) {
		msg := se.Message
		if se.Status == http.StatusBadRequest {
			msg = err.Error()
		}
		c.JSON(se.StatusCode(), gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

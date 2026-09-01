// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"

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
	passwords        agentpush.PasswordProvider
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
// a resolver the reload-secrets endpoint and the SetBindings auto-notify
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

// SetPasswordProvider installs the workspace-password lookup the lazy
// fallback notifier needs — agentd enforces Basic auth on its user mux
// (#848), so a notifier without a provider can only ever fail
// with ErrNoPasswordProvider. Production wiring uses SetAgentPusher with
// a fully-constructed service; this setter keeps the setter-style
// construction path (tests, non-app wiring) functional.
func (h *SecretsHandler) SetPasswordProvider(p agentpush.PasswordProvider) {
	h.passwords = p
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
//
// US-70.3 (I12: revocation is absence): a plain delete by the owner IS
// a revoke-all-workspaces. ForceRevokeSecret removes every binding,
// bumps each affected workspace's stored revision (so the reconcile
// loop sees the divergence immediately), and the handler notifies every
// affected live pod. Notify failures are logged, never fatal — the pod
// converges on its next contact or on boot.
func (h *SecretsHandler) DeleteSecret(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	secretID := c.Param("id")
	affected, err := h.svc.ForceRevokeSecret(c.Request.Context(), userID, secretID)
	if err != nil {
		handleSecretError(c, err)
		return
	}

	for _, workspaceID := range affected {
		if _, nerr := h.getPusher().Notify(c.Request.Context(), userID, workspaceID); nerr != nil {
			h.warn("revoke fan-out notify failed; reconcile will converge the workspace",
				"workspaceID", workspaceID, "secretID", secretID, "error", nerr.Error())
		}
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

	h.notifyAgent(c, userID, workspaceID)

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
//
// US-70.3 flip: the endpoint no longer builds and pushes a batch body —
// it notifies the pod's resync endpoint and the pod re-pulls through
// the conditional bootstrap path (fresh SA token, apply-guard,
// terminal rev anchoring). The response reports what the POD did
// (status, applied revision, restart decision), not a server-built
// reload count.
//
// Failure classes map exactly like before:
//
//   - ErrNoPodIPResolver → 503 (wiring bug).
//   - ErrNoRunningPod → 409 (no reachable pod right now).
//   - everything else (transport, agentd 502) → 502.
func (h *SecretsHandler) ReloadSecrets(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workspaceID := c.Param("id")
	result, err := h.getPusher().Notify(c.Request.Context(), userID, workspaceID)
	if err != nil {
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

	c.JSON(http.StatusOK, result)
}

// notifyAgent runs the bind-time live delivery for a workspace: an
// empty notify so the pod re-pulls its batch through the conditional
// bootstrap path (US-70.3). Credentials bound during suspend are picked
// up at the next pod boot via the bootstrap endpoint; the notify only
// shrinks latency for a running pod.
//
// Errors never fail the bind request (I3): correctness is the reconcile
// loop's job. ErrNoRunningPod is expected during suspend/boot races and
// logged at info.
func (h *SecretsHandler) notifyAgent(c *gin.Context, userID, workspaceID string) {
	_, err := h.getPusher().Notify(c.Request.Context(), userID, workspaceID)
	if err == nil {
		return
	}
	if errors.Is(err, agentpush.ErrNoRunningPod) {
		h.info("resync notify skipped: no running pod",
			"workspaceID", workspaceID)
		return
	}
	h.warn("resync notify to agent failed",
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
// (podIPResolver, modelCache, logger, passwords). This lets the handler
// work with either the "inject an agentpush.Service" wiring OR the
// pre-existing "SetPodIPResolver + SetModelCache" wiring, so the
// migration to the shared notifier can happen without breaking any of
// the dozens of tests that use the older setter-style construction.
func (h *SecretsHandler) getPusher() *agentpush.Service {
	if h.pusher != nil {
		return h.pusher
	}
	opts := []agentpush.Option{}
	if h.podIPResolver != nil {
		opts = append(opts, agentpush.WithPodIPResolver(h.podIPResolver))
	}
	if h.passwords != nil {
		opts = append(opts, agentpush.WithPasswordProvider(h.passwords))
	}
	if h.modelCache != nil {
		opts = append(opts, agentpush.WithModelCache(h.modelCache))
	}
	if h.logger != nil {
		opts = append(opts, agentpush.WithLogger(h.logger))
	}
	h.pusher = agentpush.New(opts...)
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

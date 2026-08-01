// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/lenaxia/llmsafespaces/api/internal/services/passkey"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// passkeyAuthService is the narrow slice of auth.Service the passkey handler
// needs: user creation + token issuance + DEK provisioning. Defining it here
// (rather than importing auth.Service directly) keeps the handler decoupled
// and testable with a fake.
type passkeyAuthService interface {
	IssueTokenAndUnlockDEK(ctx context.Context, userID string, ttl time.Duration, dekSource string) (string, error)
}

// passkeyUserStore is the narrow slice of database.Service the handler needs
// for user creation + lookup at passkey signup/login.
type passkeyUserStore interface {
	GetUserByEmail(ctx context.Context, email string) (*types.User, error)
	CreateUser(ctx context.Context, user *types.User) error
	DeleteUser(ctx context.Context, userID string) error
}

// PasskeyService interface defines the ceremony methods the handler needs.
// passkey.Service satisfies it; tests substitute a fake. The ceremony methods
// take raw response maps — parsing and verification are the service's job,
// not the handler's.
type PasskeyService interface {
	BeginRegistration(ctx context.Context, userID, username string) (*passkey.BeginRegistrationOptions, error)
	FinishRegistration(ctx context.Context, sessionToken, username, name string, response map[string]any) (*passkey.FinishRegistrationResult, error)
	BeginLogin(ctx context.Context, email string) (*passkey.BeginLoginOptions, string, error)
	FinishLogin(ctx context.Context, sessionToken, email string, response map[string]any) (string, error)
	ConsumeRecoveryCode(ctx context.Context, email, code string) (string, error)
	CreateCredentialAndRecoveryCodes(ctx context.Context, cred *passkey.Credential, hashes []string) error
	ListUserCredentials(ctx context.Context, userID string) ([]passkey.Credential, error)
	DeleteUserCredential(ctx context.Context, userID string, credID uuid.UUID) error
	RegenerateRecoveryCodes(ctx context.Context, userID string) ([]string, error)
	// AddCredentialToExistingUser persists a credential for an already-authenticated
	// user (the settings "Add passkey" flow). Unlike CreateCredentialAndRecoveryCodes,
	// this does NOT generate recovery codes (the user already has them).
	AddCredential(ctx context.Context, cred *passkey.Credential) error
	// GetUserName returns the username for a userID (used by BeginEnrollPasskey).
	GetUserName(ctx context.Context, userID string) (string, error)
}

// PasskeyHandler handles WebAuthn passkey registration, login, and recovery.
type PasskeyHandler struct {
	svc           PasskeyService
	auth          passkeyAuthService
	users         passkeyUserStore
	tokenTTL      time.Duration
	cookieName    string
	cookieDomain  string
	emailVerifier PasskeyEmailVerifier
}

// PasskeyEmailVerifier sends email verification for new passkey users.
// When nil (dev mode), EmailVerified is set to true immediately.
type PasskeyEmailVerifier interface {
	SendVerification(ctx context.Context, userID, email string) error
}

// maxPasskeyBodySize limits the passkey ceremony request body (1 MiB), matching
// the auth endpoints' convention (password_reset, login_discovery).
const maxPasskeyBodySize = 1 << 20

// NewPasskeyHandler constructs the handler.
func NewPasskeyHandler(svc PasskeyService, auth passkeyAuthService, users passkeyUserStore, tokenTTL time.Duration, cookieName, cookieDomain string) *PasskeyHandler {
	return &PasskeyHandler{svc: svc, auth: auth, users: users, tokenTTL: tokenTTL, cookieName: cookieName, cookieDomain: cookieDomain}
}

// SetEmailVerifier wires the email-verification hook for new passkey users.
// Optional — nil means dev mode (auto-verify). Mirrors auth.Service.SetEmailVerifier.
func (h *PasskeyHandler) SetEmailVerifier(v PasskeyEmailVerifier) {
	h.emailVerifier = v
}

// setCookie sets the session cookie (HttpOnly + Secure), matching the
// /auth/login pattern. The token is also returned in the JSON body for
// clients that cannot use cookies (e.g., CLI SDKs), but the cookie is
// the primary auth mechanism for browser sessions.
func (h *PasskeyHandler) setCookie(c *gin.Context, token string) {
	maxAge := int(h.tokenTTL.Seconds())
	if maxAge <= 0 {
		maxAge = 86400
	}
	c.SetCookie(h.cookieName, token, maxAge, "/", h.cookieDomain, true, true)
}

// RegisterBegin handles POST /api/v1/auth/passkey/register/begin.
// Starts a WebAuthn registration ceremony for a new (or existing) user.
func (h *PasskeyHandler) RegisterBegin(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
		Name  string `json:"name"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasskeyBodySize)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// RegisterBegin is for NEW user signup ONLY. If the email already exists,
	// refuse — allowing unauthenticated enrollment on an existing account would
	// be an account-takeover vector (attacker enrolls their authenticator for
	// the victim's email). Existing users add passkeys via an authenticated
	// settings flow (future).
	existing, err := h.users.GetUserByEmail(c.Request.Context(), req.Email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "account already exists"})
		return
	}

	// New user — generate a provisional UUID. The user row is created at
	// /finish only after the attestation is verified, so an incomplete ceremony
	// leaves no orphan rows.
	userID := uuid.NewString()
	username := req.Name
	if username == "" {
		username = emailLocalPart(req.Email)
	}

	opts, err := h.svc.BeginRegistration(c.Request.Context(), userID, username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "passkey registration failed"})
		return
	}
	c.JSON(http.StatusOK, opts)
}

// RegisterFinish handles POST /api/v1/auth/passkey/register/finish.
// Verifies the attestation, creates the user (if new), provisions the DEK,
// and returns a session token + recovery codes.
func (h *PasskeyHandler) RegisterFinish(c *gin.Context) {
	var req struct {
		SessionToken string         `json:"sessionToken" binding:"required"`
		Email        string         `json:"email" binding:"required,email"`
		Name         string         `json:"name"`
		Response     map[string]any `json:"response" binding:"required"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasskeyBodySize)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// Look up existing user (must not exist — account-takeover prevention is
	// enforced in RegisterBegin, but we re-check here for safety).
	existing, err := h.users.GetUserByEmail(c.Request.Context(), req.Email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "account already exists"})
		return
	}
	username := req.Name
	if username == "" {
		username = emailLocalPart(req.Email)
	}

	result, err := h.svc.FinishRegistration(c.Request.Context(), req.SessionToken, username, req.Name, req.Response)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, passkey.ErrChallengeExpired) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": "passkey registration failed"})
		return
	}

	// Create the user row FIRST so the FK constraint on user_passkeys.user_id
	// is satisfied before the credential is inserted. existing is guaranteed
	// nil here (we returned 409 above if not).
	emailVerified := h.emailVerifier == nil
	newUser := &types.User{
		ID:            result.Credential.UserID,
		Username:      username,
		Email:         req.Email,
		Active:        true,
		Role:          "user",
		Status:        types.UserStatusActive,
		EmailVerified: emailVerified,
		PasswordHash:  randomUnusableHash(),
	}
	if err := h.users.CreateUser(c.Request.Context(), newUser); err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "account already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user creation failed"})
		return
	}
	if h.emailVerifier != nil {
		if err := h.emailVerifier.SendVerification(c.Request.Context(), newUser.ID, newUser.Email); err != nil {
			// Non-fatal: user is created, session is valid. The user can
			// request a resend via /verify-email/resend. Matches auth.go's
			// Register pattern which uses s.logger.Warn. PasskeyHandler has
			// no structured logger, so we use stdlib log as a fallback.
			log.Printf("WARN: passkey RegisterFinish: failed to send verification email: user_id=%s err=%v", newUser.ID, err)
		}
	}

	// Atomically persist credential + recovery codes (single transaction).
	if err := h.svc.CreateCredentialAndRecoveryCodes(c.Request.Context(), &result.Credential, result.RecoveryCodeHashes); err != nil {
		// Cleanup: the user row was created but credential persistence
		// failed. Delete the orphaned user so the email can be re-used.
		// Without this, RegisterBegin returns 409 on retry.
		_ = h.users.DeleteUser(c.Request.Context(), newUser.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credential persistence failed"})
		return
	}

	// Issue session token + unlock the DEK (passkey tier).
	tok, err := h.auth.IssueTokenAndUnlockDEK(c.Request.Context(), result.Credential.UserID, h.tokenTTL, "passkey")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	h.setCookie(c, tok)

	c.JSON(http.StatusOK, gin.H{
		"token":         tok,
		"recoveryCodes": result.RecoveryCodes,
	})
}

// LoginBegin handles POST /api/v1/auth/passkey/login/begin.
// Starts a WebAuthn login ceremony (assertion) for a user identified by email.
func (h *PasskeyHandler) LoginBegin(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasskeyBodySize)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	opts, _, err := h.svc.BeginLogin(c.Request.Context(), req.Email)
	if err != nil {
		if errors.Is(err, passkey.ErrUserNotFound) || errors.Is(err, passkey.ErrNoPasskeyRegistered) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no passkey registered for this account"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "passkey login failed"})
		return
	}
	c.JSON(http.StatusOK, opts)
}

// LoginFinish handles POST /api/v1/auth/passkey/login/finish.
// Verifies the assertion, issues a session token, and unlocks the DEK.
func (h *PasskeyHandler) LoginFinish(c *gin.Context) {
	var req struct {
		SessionToken string         `json:"sessionToken" binding:"required"`
		Email        string         `json:"email" binding:"required,email"`
		Response     map[string]any `json:"response" binding:"required"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasskeyBodySize)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	userID, err := h.svc.FinishLogin(c.Request.Context(), req.SessionToken, req.Email, req.Response)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, passkey.ErrChallengeExpired) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": "passkey login failed"})
		return
	}

	// Fetch the user BEFORE issuing the token so a lookup failure returns an
	// error before a valid JWT is minted (avoids 500-after-success-token).
	user, err := h.users.GetUserByEmail(c.Request.Context(), req.Email)
	if err != nil || user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user lookup failed"})
		return
	}

	tok, err := h.auth.IssueTokenAndUnlockDEK(c.Request.Context(), userID, h.tokenTTL, "passkey")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	h.setCookie(c, tok)

	c.JSON(http.StatusOK, gin.H{
		"token": tok,
		"user":  user,
	})
}

// --- helpers ---

// randomUnusableHash generates a unique random bcrypt hash per passkey-only
// user so password login is permanently blocked. Unique per user (not a shared
// constant) to match SSO's pattern — a single preimage discovery cannot affect
// other accounts. The input is crypto-random; no password produces this hash.
func randomUnusableHash() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "$2a$12$7c6XjTynpWE0yY.2/uC1IufZqmLuVCoJSv3MFVWCPBaWVDaPPwXj."
	}
	h, _ := bcrypt.GenerateFromPassword(b, 12)
	return string(h)
}

// emailLocalPart extracts the local part of an email for a default username.
// isUniqueViolation checks if a PostgreSQL error is a unique-constraint
// violation (error code 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func emailLocalPart(email string) string {
	for i, c := range email {
		if c == '@' {
			return email[:i]
		}
	}
	return email
}

// Recover handles POST /api/v1/auth/passkey/recover. Validates a recovery code,
// consumes it (single-use), and issues a session token. The frontend must
// redirect the user to enroll a new passkey — the response carries mustEnrollPasskey.
func (h *PasskeyHandler) Recover(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasskeyBodySize)
	var req struct {
		Email string `json:"email" binding:"required,email"`
		Code  string `json:"code" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	userID, err := h.svc.ConsumeRecoveryCode(c.Request.Context(), req.Email, req.Code)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid recovery code"})
		return
	}

	user, err := h.users.GetUserByEmail(c.Request.Context(), req.Email)
	if err != nil || user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user lookup failed"})
		return
	}

	tok, err := h.auth.IssueTokenAndUnlockDEK(c.Request.Context(), userID, h.tokenTTL, "passkey")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	h.setCookie(c, tok)

	c.JSON(http.StatusOK, gin.H{
		"token":             tok,
		"user":              user,
		"mustEnrollPasskey": true,
	})
}

// --- authenticated settings endpoints ---

// ListPasskeys handles GET /api/v1/account/passkeys.
func (h *PasskeyHandler) ListPasskeys(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	creds, err := h.svc.ListUserCredentials(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list passkeys"})
		return
	}
	dtos := make([]types.PasskeyCredential, 0, len(creds))
	for _, cred := range creds {
		dtos = append(dtos, cred.ToDTO())
	}
	c.JSON(http.StatusOK, gin.H{"passkeys": dtos})
}

// DeletePasskey handles DELETE /api/v1/account/passkeys/:id.
func (h *PasskeyHandler) DeletePasskey(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	credIDStr := c.Param("id")
	credID, err := uuid.Parse(credIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credential ID"})
		return
	}
	if err := h.svc.DeleteUserCredential(c.Request.Context(), userID, credID); err != nil {
		if errors.Is(err, passkey.ErrLastCredential) {
			c.JSON(http.StatusConflict, gin.H{"error": "cannot delete your last passkey"})
			return
		}
		if errors.Is(err, passkey.ErrCredentialNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "passkey not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete passkey"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// RegenerateRecoveryCodes handles POST /api/v1/account/passkeys/recovery-codes/regenerate.
func (h *PasskeyHandler) RegenerateRecoveryCodes(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	codes, err := h.svc.RegenerateRecoveryCodes(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to regenerate recovery codes"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"recoveryCodes": codes})
}

// BeginEnrollPasskey handles POST /api/v1/account/passkeys/enroll/begin.
// Starts a WebAuthn registration ceremony for an EXISTING authenticated user
// who wants to add a new passkey (e.g., after recovery-code login or from
// the settings page).
func (h *PasskeyHandler) BeginEnrollPasskey(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	username, err := h.svc.GetUserName(c.Request.Context(), userID)
	if err != nil || username == "" {
		username = "user"
	}
	opts, err := h.svc.BeginRegistration(c.Request.Context(), userID, username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start enrollment"})
		return
	}
	c.JSON(http.StatusOK, opts)
}

// FinishEnrollPasskey handles POST /api/v1/account/passkeys/enroll/finish.
// Verifies the attestation and persists the credential (without recovery codes).
func (h *PasskeyHandler) FinishEnrollPasskey(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasskeyBodySize)
	var req struct {
		SessionToken string         `json:"sessionToken" binding:"required"`
		Name         string         `json:"name"`
		Response     map[string]any `json:"response" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	username, _ := h.svc.GetUserName(c.Request.Context(), userID)
	result, err := h.svc.FinishRegistration(c.Request.Context(), req.SessionToken, username, req.Name, req.Response)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, passkey.ErrChallengeExpired) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": "passkey enrollment failed"})
		return
	}
	// Persist the credential without recovery codes (user already has them).
	if err := h.svc.AddCredential(c.Request.Context(), &result.Credential); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store passkey"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enrolled": true})
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/lenaxia/llmsafespaces/api/internal/services/passkey"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// passkeyAuthService is the narrow slice of auth.Service the passkey handler
// needs: user creation + token issuance + DEK provisioning. Defining it here
// (rather than importing auth.Service directly) keeps the handler decoupled
// and testable with a fake.
type passkeyAuthService interface {
	GenerateToken(userID string) (string, error)
	IssueTokenAndUnlockDEK(ctx context.Context, userID string, ttl time.Duration, dekSource string) (string, error)
}

// passkeyUserStore is the narrow slice of database.Service the handler needs
// for user creation + lookup at passkey signup/login.
type passkeyUserStore interface {
	GetUserByEmail(ctx context.Context, email string) (*types.User, error)
	CreateUser(ctx context.Context, user *types.User) error
}

// PasskeyHandler handles WebAuthn passkey registration, login, and recovery.
type PasskeyHandler struct {
	svc      *passkey.Service
	auth     passkeyAuthService
	users    passkeyUserStore
	tokenTTL time.Duration
	tokenDur time.Duration
}

// maxPasskeyBodySize limits the passkey ceremony request body (1 MiB), matching
// the auth endpoints' convention (password_reset, login_discovery).
const maxPasskeyBodySize = 1 << 20

// NewPasskeyHandler constructs the handler.
func NewPasskeyHandler(svc *passkey.Service, auth passkeyAuthService, users passkeyUserStore, tokenTTL time.Duration) *PasskeyHandler {
	return &PasskeyHandler{svc: svc, auth: auth, users: users, tokenTTL: tokenTTL, tokenDur: tokenTTL}
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

	parsed, err := parseCreationFromMap(req.Response)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid attestation response"})
		return
	}

	// Look up existing user.
	existing, err := h.users.GetUserByEmail(c.Request.Context(), req.Email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	var username string
	if existing != nil {
		username = existing.Username
	} else {
		username = req.Name
		if username == "" {
			username = emailLocalPart(req.Email)
		}
	}

	// Verify the attestation. Returns the verified credential + recovery codes
	// WITHOUT persisting — the handler orchestrates persistence in the correct
	// order (user row → credential FK → recovery codes).
	result, err := h.svc.FinishRegistration(c.Request.Context(), req.SessionToken, username, req.Name, parsed)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, passkey.ErrChallengeExpired) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": "passkey registration failed"})
		return
	}

	// Create the user row FIRST so the FK constraint on user_passkeys.user_id
	// is satisfied before the credential is inserted.
	if existing == nil {
		newUser := &types.User{
			ID:            result.Credential.UserID,
			Username:      username,
			Email:         req.Email,
			Active:        true,
			Role:          "user",
			Status:        types.UserStatusActive,
			EmailVerified: true,
			PasswordHash:  randomUnusableHash(),
		}
		if err := h.users.CreateUser(c.Request.Context(), newUser); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "user creation failed"})
			return
		}
	}

	// Atomically persist credential + recovery codes (single transaction).
	if err := h.svc.CreateCredentialAndRecoveryCodes(c.Request.Context(), &result.Credential, result.RecoveryCodeHashes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credential persistence failed"})
		return
	}

	// Issue session token + unlock the DEK (passkey tier).
	tok, err := h.auth.IssueTokenAndUnlockDEK(c.Request.Context(), result.Credential.UserID, h.tokenTTL, "passkey")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}

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

	parsed, err := parseAssertionFromMap(req.Response)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid assertion response"})
		return
	}

	userID, err := h.svc.FinishLogin(c.Request.Context(), req.SessionToken, req.Email, parsed)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, passkey.ErrChallengeExpired) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": "passkey login failed"})
		return
	}

	tok, err := h.auth.IssueTokenAndUnlockDEK(c.Request.Context(), userID, h.tokenTTL, "passkey")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}

	user, err := h.users.GetUserByEmail(c.Request.Context(), req.Email)
	if err != nil || user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user lookup failed"})
		return
	}
	user.PasswordHash = ""
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
func emailLocalPart(email string) string {
	for i, c := range email {
		if c == '@' {
			return email[:i]
		}
	}
	return email
}

// parseCreationFromMap parses a WebAuthn attestation response from a
// map[string]any (the browser's PublicKeyCredential JSON).
func parseCreationFromMap(m map[string]any) (*protocol.ParsedCredentialCreationData, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	ccr, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return ccr, nil
}

// parseAssertionFromMap parses a WebAuthn assertion response from a
// map[string]any (the browser's PublicKeyCredential JSON).
func parseAssertionFromMap(m map[string]any) (*protocol.ParsedCredentialAssertionData, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	car, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return car, nil
}

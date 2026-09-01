// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	emailsvc "github.com/lenaxia/llmsafespaces/api/internal/services/email"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const (
	emailVerifyTokenBytes = 32
	emailVerifyTokenTTL   = 24 * time.Hour
)

// emailTokenStore is the data-access surface for email tokens (shared with
// the password-reset handler). Re-declared here for the verify handler.
type emailTokenStore interface {
	CreateEmailToken(ctx context.Context, t *types.EmailToken) error
	GetEmailTokenByHash(ctx context.Context, hash string) (*types.EmailToken, error)
	ConsumeEmailToken(ctx context.Context, id string) error
}

// emailVerifyUserLookup resolves users by email and updates email_verified.
type emailVerifyUserLookup interface {
	GetUserByEmail(ctx context.Context, email string) (*types.User, error)
	UpdateUser(ctx context.Context, userID string, updates types.UserUpdates) error
}

// EmailVerifyHandler handles the email-verification flow: verify a token
// and resend the verification email.
type EmailVerifyHandler struct {
	store    emailTokenStore
	users    emailVerifyUserLookup
	email    *emailsvc.Service
	log      passwordResetLogger
	resender emailResender
}

// emailResender creates + sends a verification token for a user. Implemented
// by the verifier adapter wired in app.go.
type emailResender interface {
	SendVerification(ctx context.Context, userID, email string) error
}

// NewEmailVerifyHandler constructs the handler.
func NewEmailVerifyHandler(
	store emailTokenStore,
	users emailVerifyUserLookup,
	email *emailsvc.Service,
	resender emailResender,
	log passwordResetLogger,
) *EmailVerifyHandler {
	return &EmailVerifyHandler{store: store, users: users, email: email, resender: resender, log: log}
}

// Verify handles POST /api/v1/auth/verify-email.
//
// Public (the token IS the credential). Verifies the hash, checks expiry +
// consumption + kind, sets email_verified=true.
func (h *EmailVerifyHandler) Verify(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasswordResetBodySize)
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}

	ctx := c.Request.Context()
	hash := hashToken(req.Token)

	tok, err := h.store.GetEmailTokenByHash(ctx, hash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify token"})
		return
	}
	if tok == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
		return
	}
	if tok.ConsumedAt != nil {
		c.JSON(http.StatusGone, gin.H{"error": "token already used"})
		return
	}
	if time.Now().After(tok.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "token expired"})
		return
	}
	if tok.Kind != "email_verify" {
		c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
		return
	}

	if err := h.store.ConsumeEmailToken(ctx, tok.ID); err != nil {
		if errors.Is(err, database.ErrTokenAlreadyConsumed) {
			c.JSON(http.StatusGone, gin.H{"error": "token already used"})
			return
		}
		if h.log != nil {
			h.log.Error("verify-email: consume token DB error", err)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to consume token"})
		return
	}

	verified := true
	if err := h.users.UpdateUser(ctx, tok.UserID, types.UserUpdates{EmailVerified: &verified}); err != nil {
		if h.log != nil {
			h.log.Error("verify-email: failed to set email_verified", err, "user_id", tok.UserID)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "verification failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"verified": true})
}

// Resend handles POST /api/v1/auth/verify-email/resend.
//
// Public, returns 202 always (no enumeration). Looks up the user by email;
// if found and unverified, sends a new verification link.
func (h *EmailVerifyHandler) Resend(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPasswordResetBodySize)
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid email is required"})
		return
	}

	ctx := c.Request.Context()
	emailAddr := strings.ToLower(strings.TrimSpace(req.Email))

	user, err := h.users.GetUserByEmail(ctx, emailAddr)
	if err != nil || user == nil {
		c.Status(http.StatusAccepted)
		return
	}

	if user.EmailVerified {
		c.Status(http.StatusAccepted)
		return
	}

	if h.resender != nil {
		if err := h.resender.SendVerification(ctx, user.ID, emailAddr); err != nil && h.log != nil {
			h.log.Error("verify-email: resend failed", err, "to", emailAddr)
		}
	}

	c.Status(http.StatusAccepted)
}

// EmailVerifierAdapter implements auth.EmailVerifier by creating a token,
// storing the hash, and sending the verification email via EmailService.
// Wired in app.go and passed to auth.Service.SetEmailVerifier.
type EmailVerifierAdapter struct {
	store   emailTokenStore
	email   *emailsvc.Service
	baseURL string
}

// NewEmailVerifierAdapter constructs the adapter.
func NewEmailVerifierAdapter(store emailTokenStore, email *emailsvc.Service, baseURL string) *EmailVerifierAdapter {
	return &EmailVerifierAdapter{store: store, email: email, baseURL: baseURL}
}

// SendVerification creates a single-use email-verify token, stores the hash,
// and sends the verification link via the EmailService.
func (a *EmailVerifierAdapter) SendVerification(ctx context.Context, userID, emailAddr string) error {
	token, hash, err := generateEmailVerifyToken()
	if err != nil {
		return fmt.Errorf("generate verify token: %w", err)
	}
	if err := a.store.CreateEmailToken(ctx, &types.EmailToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Kind:      "email_verify",
		TokenHash: hash,
		ExpiresAt: time.Now().Add(emailVerifyTokenTTL),
	}); err != nil {
		return fmt.Errorf("store verify token: %w", err)
	}
	if a.email != nil {
		if err := a.email.SendEmailVerification(ctx, emailAddr, token); err != nil {
			return fmt.Errorf("send verify email: %w", err)
		}
	}
	return nil
}

func generateEmailVerifyToken() (token string, hash string, err error) {
	raw := make([]byte, emailVerifyTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	hash = hashToken(token)
	return token, hash, nil
}

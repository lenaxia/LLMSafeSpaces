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

	pkgerrors "github.com/lenaxia/llmsafespaces/pkg/errors"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// DEKUnlocker is the caller-shaped subset of KeyService used by the
// soft-unlock endpoint. *secrets.KeyService satisfies it.
//
// Returns a non-nil error only when the unlock could not be completed
// (wrong password, DB issue, no user keys). The durable jwt_sessions
// write inside UnlockDEKWithSigningKey is best-effort and surfaces via
// logs, not the return value — login-style behavior.
type DEKUnlocker interface {
	UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, activeSigningKey []byte) error
}

// UnlockDEKHandler handles POST /api/v1/auth/unlock-dek (Epic 56).
//
// The "soft" in soft-unlock means: no JWT invalidation, no full re-login.
// The DEK is re-derived from user_keys via the master RootKeyProvider
// (server-KEK tier — no password needed) and re-cached. On success the
// durable jwt_sessions row is rewritten under the user's MATCHED signing
// key — not the active key — so that a subsequent rehydrate after Valkey
// restart still works under the same JWT.
type UnlockDEKHandler struct {
	keys DEKUnlocker
}

// NewUnlockDEKHandler creates a handler with the given key-service.
func NewUnlockDEKHandler(keys DEKUnlocker) *UnlockDEKHandler {
	return &UnlockDEKHandler{keys: keys}
}

// Unlock handles POST /auth/unlock-dek.
func (h *UnlockDEKHandler) Unlock(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	// API-key callers cannot soft-unlock-to-backfill: there is no JWT to
	// wrap the durable row under (no matched signing key). Surface a
	// 400 with a clear hint pointing at the api_keys.WrappedDEK design
	// (api keys store WrappedDEK + KekSalt directly in the api_keys row
	// when decrypt_access=true). The frontend should never reach this
	// endpoint from an API-key auth context, but a misbehaving client
	// otherwise drops through to a generic 500 — better to be explicit.
	matchedSigningKey := extractMatchedSigningKey(c)
	if matchedSigningKey == nil || isAPIKeySessionID(sessionID) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "soft-unlock requires a JWT session",
			"hint":  "API-key sessions store the wrapped DEK on the api_keys row itself (decrypt_access=true); no soft-unlock is needed.",
		})
		return
	}

	// Re-derive the DEK from user_keys via the master RootKeyProvider
	// (password is nil — server-KEK tier, no user-held secret) and
	// re-cache + re-wrap durable row under the MATCHED key. The TTL is
	// the JWT's remaining lifetime.
	ttl := remainingTokenTTL(c)
	if ttl <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired; please log in again"})
		return
	}

	if err := h.keys.UnlockDEKWithSigningKey(c.Request.Context(), userID, nil, sessionID, ttl, matchedSigningKey); err != nil {
		var se *pkgerrors.StatusError
		if errors.As(err, &se) {
			c.JSON(se.Status, gin.H{"error": se.Message})
			return
		}
		if errors.Is(err, secrets.ErrServerKEKUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server key provider unavailable"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unlock failed"})
		return
	}

	c.Status(http.StatusNoContent)
}

// isAPIKeySessionID reports whether a sessionID belongs to an API-key
// authenticated request (set by AuthMiddleware as "apikey:<hash>").
func isAPIKeySessionID(sessionID string) bool {
	return strings.HasPrefix(sessionID, "apikey:")
}

// remainingTokenTTL returns the JWT's remaining lifetime, derived from
// the "jwt_exp_unix" gin context value set by AuthMiddleware. Falls
// back to a 1h ceiling when the exp is missing (e.g. an API-key
// session, an extremely malformed token that somehow validated, or a
// test that didn't stash exp on the context). Returns 0 only when the
// token is already past its exp — the handler treats 0 as "session
// expired" and aborts.
//
// The durable row inherits this ttl as expires_at, so the janitor
// prunes it on the same schedule as the JWT itself. Cap at 30 days
// (the longest legitimate JWT lifetime in the codebase: RememberMe).
func remainingTokenTTL(c *gin.Context) time.Duration {
	const maxTTL = 30 * 24 * time.Hour
	const fallbackTTL = time.Hour

	v, ok := c.Get("jwt_exp_unix")
	if !ok {
		return fallbackTTL
	}
	exp, ok := v.(int64)
	if !ok || exp <= 0 {
		return fallbackTTL
	}
	remaining := time.Until(time.Unix(exp, 0))
	if remaining <= 0 {
		// Token expired between AuthMiddleware accepting it (millisecond
		// race) and us reading exp. Surface 0 so the handler can return
		// 401 instead of writing a row that expires immediately.
		return 0
	}
	if remaining > maxTTL {
		return maxTTL
	}
	return remaining
}

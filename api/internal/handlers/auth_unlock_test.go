// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgerrors "github.com/lenaxia/llmsafespaces/pkg/errors"
)

// captureUnlocker is a DEKUnlocker spy that records the call args and
// returns a configurable error.
type captureUnlocker struct {
	err            error
	calls          int
	lastUserID     string
	lastSessionID  string
	lastPassword   []byte
	lastTTL        time.Duration
	lastSigningKey []byte
}

func (c *captureUnlocker) UnlockDEKWithSigningKey(_ context.Context, userID string, password []byte, sessionID string, ttl time.Duration, activeSigningKey []byte) error {
	c.calls++
	c.lastUserID = userID
	c.lastSessionID = sessionID
	c.lastPassword = append([]byte{}, password...)
	c.lastTTL = ttl
	c.lastSigningKey = append([]byte{}, activeSigningKey...)
	return c.err
}

func setupUnlockRouter(t *testing.T, unlocker DEKUnlocker, userID, sessionID string, matchedKey []byte) *gin.Engine {
	t.Helper()
	return setupUnlockRouterWithExp(t, unlocker, userID, sessionID, matchedKey, 0)
}

// setupUnlockRouterWithExp lets tests set the jwt_exp_unix gin context
// value to drive remainingTokenTTL.
func setupUnlockRouterWithExp(t *testing.T, unlocker DEKUnlocker, userID, sessionID string, matchedKey []byte, expUnix int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewUnlockDEKHandler(unlocker)
	r.POST("/auth/unlock-dek", func(c *gin.Context) {
		if userID != "" {
			c.Set("userID", userID)
		}
		if sessionID != "" {
			c.Set("sessionID", sessionID)
		}
		if matchedKey != nil {
			c.Set("jwt_signing_key", matchedKey)
		}
		if expUnix > 0 {
			c.Set("jwt_exp_unix", expUnix)
		}
	}, h.Unlock)
	return r
}

func doUnlockRequest(t *testing.T, r *gin.Engine, body any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/unlock-dek", rdr)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

func TestUnlockDEK_HappyPath(t *testing.T) {
	unlocker := &captureUnlocker{err: nil}
	matchedKey := []byte("matched-signing-key-32-bytes-pad")
	r := setupUnlockRouter(t, unlocker, "u-1", "11111111-2222-3333-4444-555555555555", matchedKey)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, unlocker.calls)
	assert.Equal(t, "u-1", unlocker.lastUserID)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", unlocker.lastSessionID)
	assert.Equal(t, matchedKey, unlocker.lastSigningKey, "soft-unlock MUST wrap under matched key, not active")
	assert.Greater(t, unlocker.lastTTL, time.Duration(0))
}

func TestUnlockDEK_Unauthenticated_Returns401(t *testing.T) {
	unlocker := &captureUnlocker{}
	// No userID set in context — simulates a request slipping past
	// AuthMiddleware in a misconfigured test or partial outage.
	r := setupUnlockRouter(t, unlocker, "", "", nil)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, unlocker.calls)
}

func TestUnlockDEK_NoMatchedSigningKey_Returns400(t *testing.T) {
	// Legacy cache hit, broken middleware, or test missing jwt_signing_key:
	// the soft-unlock has no key to wrap the durable row under. Reject
	// rather than producing a Redis-only unlock (no durable persistence).
	unlocker := &captureUnlocker{}
	r := setupUnlockRouter(t, unlocker, "u-3", "11111111-2222-3333-4444-555555555555", nil)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "requires a JWT session")
}

func TestUnlockDEK_APIKeySession_Returns400(t *testing.T) {
	// API-key sessionIDs are prefixed "apikey:hash". The api_keys table
	// already covers DEK durability for them — no soft-unlock needed.
	unlocker := &captureUnlocker{}
	r := setupUnlockRouter(t, unlocker, "u-4", "apikey:abcdef", []byte("sk"))

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "api_keys")
}

func TestUnlockDEK_InternalError_Returns500(t *testing.T) {
	unlocker := &captureUnlocker{err: errors.New("transient DB outage")}
	r := setupUnlockRouter(t, unlocker, "u-7", "11111111-2222-3333-4444-555555555555", []byte("sk"))

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	// Generic message — no leak of "transient DB outage" detail.
	assert.NotContains(t, rec.Body.String(), "transient")
}

func TestUnlockDEK_StatusError_MapsToStatusFromError(t *testing.T) {
	// The handler maps *StatusError → se.Status so service-layer HTTP
	// status codes propagate correctly (e.g. 503 for ErrServerKEKUnavailable).
	unlocker := &captureUnlocker{err: pkgerrors.NewStatusError(http.StatusServiceUnavailable, "key provider down")}
	r := setupUnlockRouter(t, unlocker, "u-8", "11111111-2222-3333-4444-555555555555", []byte("sk"))

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "key provider down")
}

func TestUnlockDEK_RegressionForRotatedJWT_WrapsUnderMatchedKey(t *testing.T) {
	// The #411 [HIGH] regression case: JWT signed under key A, B is
	// active. Soft-unlock MUST wrap with A (matched key), not B (active).
	// AuthMiddleware extracts A and stashes on context under
	// "jwt_signing_key". The handler passes that same A to the
	// unlocker. After a Valkey restart, the rehydrate path under
	// the same JWT (still validating against A) derives the same KEK
	// and recovers the DEK.
	keyA := []byte("previous-signing-key-A-32-bytes!")
	unlocker := &captureUnlocker{}
	r := setupUnlockRouter(t, unlocker, "u-rot", "11111111-2222-3333-4444-555555555555", keyA)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, keyA, unlocker.lastSigningKey, "regression: soft-unlock must wrap under matched key A, not the (test) active key")
}

func TestUnlockDEK_DurableRowTTLMatchesJWTRemaining(t *testing.T) {
	// soft-unlock at hour 1 of a 24h JWT should produce a durable row
	// with ~23h TTL — NOT a hardcoded 1h. Pin the contract: TTL must
	// equal time.Until(exp) within a small skew.
	unlocker := &captureUnlocker{}
	expIn4h := time.Now().Add(4 * time.Hour).Unix()
	r := setupUnlockRouterWithExp(t, unlocker, "u-ttl", "11111111-2222-3333-4444-555555555555", []byte("sk"), expIn4h)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	// Allow a 5s slack window for test scheduling jitter.
	assert.Greater(t, unlocker.lastTTL, 4*time.Hour-5*time.Second, "TTL should track JWT exp")
	assert.LessOrEqual(t, unlocker.lastTTL, 4*time.Hour, "TTL should not exceed JWT exp")
}

func TestUnlockDEK_NoJWTExp_FallsBackToOneHour(t *testing.T) {
	// API-key-like sessions or tests without exp on context: fall back to
	// 1h (the previous default). Documented in remainingTokenTTL.
	unlocker := &captureUnlocker{}
	r := setupUnlockRouterWithExp(t, unlocker, "u-noexp", "11111111-2222-3333-4444-555555555555", []byte("sk"), 0)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, time.Hour, unlocker.lastTTL, "no exp on context → 1h fallback")
}

func TestUnlockDEK_TokenAlreadyExpired_Returns401(t *testing.T) {
	// Race: token validated by AuthMiddleware nanoseconds before exp
	// passes, by the time the handler reads exp it's already past.
	// Return 401 rather than write a row that expires immediately.
	unlocker := &captureUnlocker{}
	expInPast := time.Now().Add(-time.Minute).Unix()
	r := setupUnlockRouterWithExp(t, unlocker, "u-exp", "11111111-2222-3333-4444-555555555555", []byte("sk"), expInPast)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, unlocker.calls)
}

func TestUnlockDEK_TTLCappedAt30Days(t *testing.T) {
	// Defensive: even a forged JWT with exp = NOW + 100 years should
	// not produce a 100-year-long durable row. 30d is the longest
	// legitimate JWT lifetime (RememberMe ceiling).
	unlocker := &captureUnlocker{}
	expWayOut := time.Now().Add(100 * 365 * 24 * time.Hour).Unix()
	r := setupUnlockRouterWithExp(t, unlocker, "u-huge", "11111111-2222-3333-4444-555555555555", []byte("sk"), expWayOut)

	rec := doUnlockRequest(t, r, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 30*24*time.Hour, unlocker.lastTTL)
}

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

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeKeyRotator is a minimal KeyRotator for the ChangePassword unit
// tests. It delegates ChangePassword to a real KeyService backed by
// in-memory stores so the success/failure semantics are authentic; the
// RotateKeyWithPassword and ResetWithRecoveryKey methods exist only to
// satisfy the interface and are never exercised by these tests.
type fakeKeyRotator struct {
	inner       *secrets.KeyService
	changeErr   error // forces ChangePassword to fail (non-nil)
	lastSession string
}

func (f *fakeKeyRotator) RotateKeyWithPassword(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) (secrets.RotationResult, error) {
	return f.inner.RotateKeyWithPassword(ctx, userID, password, sessionID, ttl)
}

func (f *fakeKeyRotator) ChangePassword(ctx context.Context, userID, sessionID string, oldPassword, newPassword []byte) error {
	f.lastSession = sessionID
	if f.changeErr != nil {
		return f.changeErr
	}
	return f.inner.ChangePassword(ctx, userID, sessionID, oldPassword, newPassword)
}

func (f *fakeKeyRotator) ResetWithRecoveryKey(ctx context.Context, userID string, recoveryKeyHex string, newPassword []byte) (string, error) {
	return f.inner.ResetWithRecoveryKey(ctx, userID, recoveryKeyHex, newPassword)
}

// fakeSessionRevoker records every RevokeAllUserSessions call.
type fakeSessionRevoker struct {
	calls  int
	lastID string
	err    error
}

func (r *fakeSessionRevoker) RevokeAllUserSessions(_ context.Context, userID string) error {
	r.calls++
	r.lastID = userID
	return r.err
}

// noopPwUpdater satisfies PasswordHashUpdater without a database.
type noopPwUpdater struct{}

func (noopPwUpdater) UpdatePasswordHash(_ context.Context, _ string, _ []byte) error { return nil }

// setupChangePasswordTest wires a gin router that injects the given
// userID + sessionID into the request context (mimicking AuthMiddleware)
// and routes /api/v1/account/change-password to a RotateKeyHandler built
// from the provided rotator and revoker. A nil revoker leaves the
// handler unwired (exercises the optional-setter path).
func setupChangePasswordTest(t *testing.T, userID, sessionID string, rotator *fakeKeyRotator, revoker *fakeSessionRevoker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewRotateKeyHandler(rotator)
	h.SetPasswordUpdater(noopPwUpdater{})
	if revoker != nil {
		// Assign to a local interface first so the call sees a non-nil
		// SessionRevoker — passing revoker (a *fakeSessionRevoker) when
		// nil would yield a non-nil interface wrapping a nil pointer,
		// tripping the h.revoker != nil guard in ChangePassword.
		var sr SessionRevoker = revoker
		h.SetSessionRevoker(sr)
	}

	g := r.Group("/api/v1/account")
	g.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("sessionID", sessionID)
		c.Next()
	})
	g.POST("/change-password", h.ChangePassword)
	return r
}

// withKey initializes a fakeKeyRotator with a real in-memory KeyService
// and pre-seeds it with the user's keys so ChangePassword can succeed.
func withKey(t *testing.T, password string) *fakeKeyRotator {
	t.Helper()
	keyStore := newTestKeyStore()
	dekCache := newTestDEKCache()
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	userID := "user-1"
	if _, err := keySvc.InitializeUserKeys(context.Background(), userID, []byte(password)); err != nil {
		t.Fatalf("seed InitializeUserKeys: %v", err)
	}
	return &fakeKeyRotator{inner: keySvc}
}

func doChangePassword(router *gin.Engine, oldPw, newPw string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{
		"oldPassword": oldPw,
		"newPassword": newPw,
	})
	req, _ := http.NewRequest("POST", "/api/v1/account/change-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// G38: a successful password change must revoke every outstanding JWT for
// the user — including the caller's own — so that a JWT stolen before the
// change is no longer useful. Mirrors the password-reset flow's OWASP-
// mandated revocation (password_reset.go:312 → RevokeAllUserSessions).
func TestChangePassword_RevokesAllSessionsOnSuccess(t *testing.T) {
	const userID = "user-1"
	const sessionID = "sess-current"
	rotator := withKey(t, "correct-old-pw")
	revoker := &fakeSessionRevoker{}
	router := setupChangePasswordTest(t, userID, sessionID, rotator, revoker)

	w := doChangePassword(router, "correct-old-pw", "new-pw-at-least-8")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	// The caller's sessionID must reach KeyService.ChangePassword so the
	// cached DEK for THIS session is evicted (existing behavior — the
	// eviction inside KeyService predates G38). Without this assertion a
	// future refactor that drops the sessionID arg would silently leave
	// the caller's cached DEK live.
	assert.Equal(t, sessionID, rotator.lastSession, "caller's sessionID must reach ChangePassword for DEK eviction")

	// G38: every outstanding JWT must be revoked, including the caller's.
	assert.Equal(t, 1, revoker.calls, "RevokeAllUserSessions must be called exactly once after a successful change")
	assert.Equal(t, userID, revoker.lastID, "RevokeAllUserSessions must target the caller's userID")
}

// G38: revocation failure must be non-fatal. The password has already
// been changed cryptographically; surfacing an error here would imply
// rollback, which the key layer cannot do. Mirrors password_reset.go's
// "log and continue" stance (password_reset.go:311-314).
func TestChangePassword_RevokerErrorIsNonFatal(t *testing.T) {
	const userID = "user-1"
	rotator := withKey(t, "correct-old-pw")
	revoker := &fakeSessionRevoker{err: errors.New("redis down")}
	router := setupChangePasswordTest(t, userID, "sess-1", rotator, revoker)

	w := doChangePassword(router, "correct-old-pw", "new-pw-at-least-8")
	require.Equal(t, http.StatusNoContent, w.Code, "revocation failure must not flip a successful change to 5xx")
	assert.Equal(t, 1, revoker.calls, "revoker was called despite the error")
}

// G38: wrong old password must NOT revoke sessions — otherwise a
// malicious party who can submit change-password requests could log the
// victim out everywhere without knowing the password (a DoS / active
// attack amplification of the auth-lockout DoS we already mitigate).
func TestChangePassword_WrongPasswordDoesNotRevoke(t *testing.T) {
	const userID = "user-1"
	rotator := withKey(t, "correct-old-pw")
	revoker := &fakeSessionRevoker{}
	router := setupChangePasswordTest(t, userID, "sess-1", rotator, revoker)

	w := doChangePassword(router, "wrong-old-pw", "new-pw-at-least-8")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 0, revoker.calls, "RevokeAllUserSessions must not run when ChangePassword fails")
}

// G38: when no revoker is wired (e.g. legacy unit setups, test harnesses
// that pre-date this fix), ChangePassword must still succeed — the new
// behavior is additive and optional. Mirrors how RotateKeyHandler already
// treats SetPasswordUpdater and SetAuditFunc as optional.
func TestChangePassword_NoRevokerWired_StillSucceeds(t *testing.T) {
	const userID = "user-1"
	rotator := withKey(t, "correct-old-pw")
	router := setupChangePasswordTest(t, userID, "sess-1", rotator, nil)

	w := doChangePassword(router, "correct-old-pw", "new-pw-at-least-8")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

// G38: missing auth context (userID empty) must short-circuit before
// touching the key service or the revoker — same guard the existing
// handler already applies via extractAuth.
func TestChangePassword_Unauthenticated_Returns401(t *testing.T) {
	rotator := withKey(t, "correct-old-pw")
	revoker := &fakeSessionRevoker{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewRotateKeyHandler(rotator)
	h.SetPasswordUpdater(noopPwUpdater{})
	h.SetSessionRevoker(revoker)
	r.POST("/api/v1/account/change-password", h.ChangePassword) // no auth middleware

	w := doChangePassword(r, "anything-here", "newpw-min8")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, 0, revoker.calls)
}

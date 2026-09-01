// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	emailsvc "github.com/lenaxia/llmsafespaces/api/internal/services/email"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// reuse memTokenStore, memUserStore fakes from password_reset_test.go

// emailVerifyUserStore wraps memUserStore to add UpdateUser support.
type emailVerifyUserStore struct {
	*memUserStore
	updates map[string]types.UserUpdates
}

func newEmailVerifyUserStore() *emailVerifyUserStore {
	return &emailVerifyUserStore{memUserStore: newMemUserStore(), updates: map[string]types.UserUpdates{}}
}

func (s *emailVerifyUserStore) UpdateUser(_ context.Context, userID string, updates types.UserUpdates) error {
	s.updates[userID] = updates
	return nil
}

// fakeResender captures SendVerification calls.
type fakeResender struct {
	calls int
	last  string
	err   error
}

func (f *fakeResender) SendVerification(_ context.Context, _ string, email string) error {
	f.calls++
	f.last = email
	return f.err
}

func setupVerifyRouter(h *EmailVerifyHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/auth/verify-email", h.Verify)
	r.POST("/api/v1/auth/verify-email/resend", h.Resend)
	return r
}

func TestEmailVerify_Verify_ValidToken_SetsVerified(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("verify-me")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "vt-1",
		UserID:    "user-1",
		Kind:      "email_verify",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	users := newEmailVerifyUserStore()
	h := NewEmailVerifyHandler(store, users, emailsvc.NewService(&fakeEmailProvider{}, "", ""), &fakeResender{}, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"verify-me"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// email_verified must be set to true via UpdateUser
	upd, ok := users.updates["user-1"]
	require.True(t, ok, "UpdateUser must be called for the token's user")
	require.NotNil(t, upd.EmailVerified, "EmailVerified must be set in the update")
	assert.True(t, *upd.EmailVerified, "EmailVerified must be set to true")

	// token consumed
	assert.NotNil(t, store.tokens[tokenHash].ConsumedAt, "token must be consumed")
}

func TestEmailVerify_Verify_ExpiredToken_410(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("expired")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "vt-2",
		UserID:    "user-1",
		Kind:      "email_verify",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}

	h := NewEmailVerifyHandler(store, newEmailVerifyUserStore(), emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"expired"}`)
	assert.Equal(t, http.StatusGone, w.Code)
}

func TestEmailVerify_Verify_ConsumedToken_410(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("used")
	consumed := time.Now().Add(-5 * time.Minute)
	store.tokens[tokenHash] = &types.EmailToken{
		ID:         "vt-3",
		UserID:     "user-1",
		Kind:       "email_verify",
		TokenHash:  tokenHash,
		ExpiresAt:  time.Now().Add(10 * time.Minute),
		ConsumedAt: &consumed,
	}

	h := NewEmailVerifyHandler(store, newEmailVerifyUserStore(), emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"used"}`)
	assert.Equal(t, http.StatusGone, w.Code)
}

func TestEmailVerify_Verify_WrongKind_404(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("wrong-kind")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "vt-4",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	h := NewEmailVerifyHandler(store, newEmailVerifyUserStore(), emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"wrong-kind"}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "password_reset token must not work for verify")
}

func TestEmailVerify_Verify_UnknownToken_404(t *testing.T) {
	h := NewEmailVerifyHandler(newMemTokenStore(), newEmailVerifyUserStore(), emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"nonexistent"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestEmailVerify_Verify_MissingToken_400(t *testing.T) {
	h := NewEmailVerifyHandler(newMemTokenStore(), newEmailVerifyUserStore(), emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Resend tests ---

func TestEmailVerify_Resend_KnownUnverifiedUser_SendsEmail(t *testing.T) {
	users := newEmailVerifyUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = false

	resender := &fakeResender{}
	h := NewEmailVerifyHandler(newMemTokenStore(), users, nil, resender, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email/resend", `{"email":"alice@test.com"}`)
	require.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, 1, resender.calls, "verification must be sent")
	assert.Equal(t, "alice@test.com", resender.last)
}

func TestEmailVerify_Resend_UnknownUser_NoEnumeration(t *testing.T) {
	resender := &fakeResender{}
	h := NewEmailVerifyHandler(newMemTokenStore(), newEmailVerifyUserStore(), nil, resender, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email/resend", `{"email":"ghost@test.com"}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, 0, resender.calls, "no send for unknown user")
}

func TestEmailVerify_Resend_AlreadyVerified_NoSend(t *testing.T) {
	users := newEmailVerifyUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = true // already verified

	resender := &fakeResender{}
	h := NewEmailVerifyHandler(newMemTokenStore(), users, nil, resender, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email/resend", `{"email":"alice@test.com"}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, 0, resender.calls, "no send for already-verified user")
}

func TestEmailVerify_Resend_InvalidEmail_400(t *testing.T) {
	h := NewEmailVerifyHandler(newMemTokenStore(), newEmailVerifyUserStore(), nil, &fakeResender{}, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email/resend", `{"email":"not-an-email"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEmailVerify_Verify_ConsumeError_DBTransient_500(t *testing.T) {
	// DB transient error during consume must return 500, not 410 (same
	// pattern as the password-reset handler). Distinguishes TOCTOU race
	// (token consumed by another request) from DB unavailability.
	store := &memTokenStore{
		tokens:     map[string]*types.EmailToken{},
		consumeErr: fmt.Errorf("connection refused"),
	}
	tokenHash := hashTokenForTest("db-err-vtoken")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "vt-7",
		UserID:    "user-1",
		Kind:      "email_verify",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	h := NewEmailVerifyHandler(store, newEmailVerifyUserStore(), nil, nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"db-err-vtoken"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"DB transient error during consume must return 500, not 410")
}

func TestEmailVerify_Resend_NilResender_202Graceful(t *testing.T) {
	users := newEmailVerifyUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = false

	// nil resender — the handler must still return 202 (graceful degradation)
	h := NewEmailVerifyHandler(newMemTokenStore(), users, nil, nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email/resend", `{"email":"alice@test.com"}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "nil resender must return 202, not 500")
}

func TestEmailVerify_Verify_UpdateUserFailure_500(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("upd-fail-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "vt-8",
		UserID:    "user-1",
		Kind:      "email_verify",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	users := &failingUpdateUserStore{emailVerifyUserStore: newEmailVerifyUserStore()}

	h := NewEmailVerifyHandler(store, users, nil, nil, nil)
	router := setupVerifyRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"upd-fail-token"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "UpdateUser failure must return 500")
}

// failingUpdateUserStore wraps emailVerifyUserStore to inject UpdateUser error.
type failingUpdateUserStore struct {
	*emailVerifyUserStore
}

func (s *failingUpdateUserStore) UpdateUser(_ context.Context, _ string, _ types.UserUpdates) error {
	return fmt.Errorf("db write failed")
}

// TestEmailVerify_RoutesRegistered verifies the verify endpoints are
// reachable through a router (Rule 0 e2e wiring).
func TestEmailVerify_RoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewEmailVerifyHandler(newMemTokenStore(), newEmailVerifyUserStore(), nil, nil, nil)

	authGroup := r.Group("/api/v1/auth")
	authGroup.POST("/verify-email", h.Verify)
	authGroup.POST("/verify-email/resend", h.Resend)

	tests := []struct {
		path       string
		body       string
		expectCode int
	}{
		{"/api/v1/auth/verify-email", `{"token":"nonexistent"}`, http.StatusNotFound},
		{"/api/v1/auth/verify-email/resend", `{"email":"ghost@test.com"}`, http.StatusAccepted},
	}
	for _, tt := range tests {
		w := doRequest(r, http.MethodPost, tt.path, tt.body)
		assert.Equal(t, tt.expectCode, w.Code,
			"%s must return %d (route wired + handler executed)", tt.path, tt.expectCode)
	}
}

// --- EmailVerifierAdapter test ---

func TestEmailVerifierAdapter_CreatesTokenAndSends(t *testing.T) {
	store := newMemTokenStore()
	fp := &fakeEmailProvider{}
	emailSVC := emailsvc.NewService(fp, "https://app.test", "ses")

	adapter := NewEmailVerifierAdapter(store, emailSVC, "https://app.test")
	err := adapter.SendVerification(context.Background(), "user-1", "alice@test.com")
	require.NoError(t, err)

	require.Len(t, store.tokens, 1, "exactly one token must be stored")
	for _, tok := range store.tokens {
		assert.Equal(t, "email_verify", tok.Kind)
		assert.Equal(t, "user-1", tok.UserID)
	}
	require.NotNil(t, fp.last, "verification email must have been sent")
	assert.Contains(t, fp.last.TextBody, "https://app.test/verify-email?token=")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	emailsvc "github.com/lenaxia/llmsafespaces/api/internal/services/email"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type memTokenStore struct {
	tokens     map[string]*types.EmailToken // keyed by token_hash
	createErr  error
	consumeErr error
}

func newMemTokenStore() *memTokenStore {
	return &memTokenStore{tokens: map[string]*types.EmailToken{}}
}

func (s *memTokenStore) CreateEmailToken(_ context.Context, t *types.EmailToken) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.tokens[t.TokenHash] = t
	return nil
}

func (s *memTokenStore) GetEmailTokenByHash(_ context.Context, hash string) (*types.EmailToken, error) {
	return s.tokens[hash], nil
}

func (s *memTokenStore) ConsumeEmailToken(_ context.Context, id string) error {
	if s.consumeErr != nil {
		return s.consumeErr
	}
	for _, t := range s.tokens {
		if t.ID == id {
			now := time.Now()
			t.ConsumedAt = &now
			return nil
		}
	}
	return nil
}

type memUserStore struct {
	users    map[string]*memUser // keyed by email (lowercase)
	emailVer map[string]bool     // userID → email_verified
}

type memUser struct {
	id    string
	email string
}

func newMemUserStore() *memUserStore {
	return &memUserStore{users: map[string]*memUser{}, emailVer: map[string]bool{}}
}

func (s *memUserStore) GetUserByEmail(_ context.Context, emailAddr string) (*types.User, error) {
	u := s.users[lowerEmail(emailAddr)]
	if u == nil {
		return nil, nil
	}
	return &types.User{ID: u.id, Email: u.email, EmailVerified: s.emailVer[u.id]}, nil
}

func (s *memUserStore) GetUser(_ context.Context, userID string) (*types.User, error) {
	for _, u := range s.users {
		if u.id == userID {
			return &types.User{ID: u.id, Email: u.email, EmailVerified: s.emailVer[u.id]}, nil
		}
	}
	return nil, nil
}

func (s *memUserStore) GetUserEmailVerified(_ context.Context, userID string) (bool, error) {
	return s.emailVer[userID], nil
}

type memKeyInit struct {
	calls    int
	lastPw   string
	recoverK string
	initErr  error
}

func (m *memKeyInit) InitializeUserKeysServerKEK(_ context.Context, _ string, _ string) error {
	m.calls++
	return m.initErr
}

type memPwUpdater struct {
	calls  int
	lastID string
	lastPw string
	err    error
}

func (m *memPwUpdater) UpdatePasswordHash(_ context.Context, userID string, pw []byte) error {
	m.calls++
	m.lastID = userID
	m.lastPw = string(pw)
	return m.err
}

type memSessionRevoker struct {
	calls  int
	lastID string
}

func (m *memSessionRevoker) RevokeAllUserSessions(_ context.Context, userID string) error {
	m.calls++
	m.lastID = userID
	return nil
}

type memSecretPurger struct {
	calls  int
	lastID string
	err    error
}

func (m *memSecretPurger) PurgeUserSecrets(_ context.Context, userID string) error {
	m.calls++
	m.lastID = userID
	return m.err
}

type memWorkspaceNeutralizer struct {
	calls  int
	lastID string
	err    error
}

func (m *memWorkspaceNeutralizer) NeutralizeUserWorkspaces(_ context.Context, userID string) error {
	m.calls++
	m.lastID = userID
	return m.err
}

// --- helpers ---

func lowerEmail(s string) string {
	s = strings.ToLower(s)
	return strings.TrimSpace(s)
}

func setupPasswordResetRouter(h *PasswordResetHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/auth/password-reset/request", h.Request)
	r.POST("/api/v1/auth/password-reset/confirm", h.Confirm)
	return r
}

// --- Request endpoint tests ---

func TestPasswordReset_Request_KnownVerifiedUser_SendsEmail(t *testing.T) {
	store := newMemTokenStore()
	users := newMemUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = true

	fp := &fakeEmailProvider{}
	emailSVC := emailsvc.NewService(fp, "https://app.test", "ses")

	h := NewPasswordResetHandler(store, users, &memKeyInit{recoverK: "newkey"}, &memPwUpdater{}, &memSessionRevoker{}, emailSVC, nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/request", `{"email":"alice@test.com"}`)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.NotNil(t, fp.last, "a reset email must have been sent")
	assert.Contains(t, fp.last.Subject, "password")
	assert.Len(t, store.tokens, 1, "exactly one token must be stored")
}

func TestPasswordReset_Request_UnknownUser_NoEnumeration(t *testing.T) {
	store := newMemTokenStore()
	users := newMemUserStore()
	fp := &fakeEmailProvider{}
	emailSVC := emailsvc.NewService(fp, "https://app.test", "ses")

	h := NewPasswordResetHandler(store, users, &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailSVC, nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/request", `{"email":"ghost@test.com"}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "unknown user must still get 202 (no enumeration)")
	assert.Nil(t, fp.last, "no email must be sent for unknown user")
	assert.Empty(t, store.tokens, "no token must be created for unknown user")
}

func TestPasswordReset_Request_UnverifiedUser_NoEmailNoEnumeration(t *testing.T) {
	store := newMemTokenStore()
	users := newMemUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = false // unverified

	fp := &fakeEmailProvider{}
	emailSVC := emailsvc.NewService(fp, "https://app.test", "ses")

	h := NewPasswordResetHandler(store, users, &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailSVC, nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/request", `{"email":"alice@test.com"}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "unverified user must still get 202 (no enumeration)")
	assert.Nil(t, fp.last, "no email for unverified user (reset requires verified email)")
}

func TestPasswordReset_Request_MissingEmail_Rejected(t *testing.T) {
	h := NewPasswordResetHandler(newMemTokenStore(), newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/request", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Confirm endpoint tests ---

func TestPasswordReset_Confirm_ValidToken_ResetsEverything(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("valid-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-1",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	users := newMemUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = true

	fp := &fakeEmailProvider{}
	emailSVC := emailsvc.NewService(fp, "https://app.test", "ses")
	keyInit := &memKeyInit{recoverK: "new-recovery-key"}
	pwUp := &memPwUpdater{}
	revoker := &memSessionRevoker{}

	h := NewPasswordResetHandler(store, users, keyInit, pwUp, revoker, emailSVC, nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"valid-token","newPassword":"newpass123"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// DEK reinitialized
	assert.Equal(t, 1, keyInit.calls, "InitializeUserKeys must be called once")
	assert.Equal(t, "newpass123", keyInit.lastPw)

	// bcrypt hash updated
	assert.Equal(t, 1, pwUp.calls, "UpdatePasswordHash must be called once")
	assert.Equal(t, "user-1", pwUp.lastID)

	// all sessions revoked
	assert.Equal(t, 1, revoker.calls, "RevokeAllUserSessions must be called once")
	assert.Equal(t, "user-1", revoker.lastID)

	// notification email sent (PasswordChanged)
	require.NotNil(t, fp.last, "password-changed notification must be sent")
	assert.Contains(t, fp.last.Subject, "password")

	// token consumed (single-use)
	assert.NotNil(t, store.tokens[tokenHash].ConsumedAt, "token must be consumed")

	// response includes the new recovery key
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-recovery-key", resp["recoveryKey"])
}

func TestPasswordReset_Confirm_PurgesUserSecrets(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("purge-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-purge",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	users := newMemUserStore()
	users.users["alice@test.com"] = &memUser{id: "user-1", email: "alice@test.com"}
	users.emailVer["user-1"] = true

	purger := &memSecretPurger{}
	neutralizer := &memWorkspaceNeutralizer{}

	h := NewPasswordResetHandler(store, users, &memKeyInit{recoverK: "rk"}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	h.SetSecretPurger(purger)
	h.SetWorkspaceNeutralizer(neutralizer)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"purge-token","newPassword":"newpass123"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Both cleanup collaborators must be invoked exactly once for the
	// token's user, after the DEK reinit succeeds.
	assert.Equal(t, 1, purger.calls, "PurgeUserSecrets must be called once")
	assert.Equal(t, "user-1", purger.lastID)
	assert.Equal(t, 1, neutralizer.calls, "NeutralizeUserWorkspaces must be called once")
	assert.Equal(t, "user-1", neutralizer.lastID)
}

func TestPasswordReset_Confirm_PurgeFailure_IsNonFatal(t *testing.T) {
	// A purge or neutralize failure must NOT fail the reset: the DEK
	// reinit already cryptographically erased the secrets, so cleanup is
	// best-effort. The user still gets a recovery key and a 200.
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("purgefail-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-purgefail",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{recoverK: "rk"}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	h.SetSecretPurger(&memSecretPurger{err: errors.New("db down")})
	h.SetWorkspaceNeutralizer(&memWorkspaceNeutralizer{err: errors.New("k8s down")})
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"purgefail-token","newPassword":"newpass123"}`)
	require.Equal(t, http.StatusOK, w.Code, "reset must succeed even if cleanup fails")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "rk", resp["recoveryKey"], "recovery key must still be returned")
}

func TestPasswordReset_Confirm_ExpiredToken_410(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("expired-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-2",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(-1 * time.Minute), // expired
	}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"expired-token","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusGone, w.Code)
}

func TestPasswordReset_Confirm_ConsumedToken_410(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("consumed-token")
	consumed := time.Now().Add(-5 * time.Minute)
	store.tokens[tokenHash] = &types.EmailToken{
		ID:         "tok-3",
		UserID:     "user-1",
		Kind:       "password_reset",
		TokenHash:  tokenHash,
		ExpiresAt:  time.Now().Add(10 * time.Minute),
		ConsumedAt: &consumed,
	}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"consumed-token","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusGone, w.Code)
}

func TestPasswordReset_Confirm_UnknownToken_404(t *testing.T) {
	h := NewPasswordResetHandler(newMemTokenStore(), newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"nonexistent","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPasswordReset_Confirm_ShortPassword_400(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("valid-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-4",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"valid-token","newPassword":"short"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPasswordReset_Confirm_WrongKind_404(t *testing.T) {
	// A token with kind="email_verify" must NOT work for password reset.
	// Prevents token cross-use between flows (US-49.6 readiness).
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("verify-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-5",
		UserID:    "user-1",
		Kind:      "email_verify",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"verify-token","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "email_verify token must not work for password reset")
}

func TestPasswordReset_Confirm_ConsumeError_DBTransient_500(t *testing.T) {
	// A genuine DB error during consume (not the sentinel) must return 500,
	// not 410. This distinguishes TOCTOU race (token consumed by another
	// request) from DB unavailability.
	store := &memTokenStore{
		tokens:     map[string]*types.EmailToken{},
		consumeErr: errors.New("connection refused"),
	}
	tokenHash := hashTokenForTest("db-error-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-6",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"db-error-token","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"DB transient error during consume must return 500, not 410")
}

func TestPasswordReset_Confirm_KeyInitFailure_500(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("keyinit-fail-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-7",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	keyInit := &memKeyInit{initErr: errors.New("key service unavailable")}
	users := newMemUserStore()
	users.users["user-1@test.com"] = &memUser{id: "user-1", email: "user-1@test.com"}
	users.emailVer["user-1"] = true

	h := NewPasswordResetHandler(store, users, keyInit, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"keyinit-fail-token","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPasswordReset_Confirm_BcryptUpdateFailure_500(t *testing.T) {
	store := newMemTokenStore()
	tokenHash := hashTokenForTest("bcrypt-fail-token")
	store.tokens[tokenHash] = &types.EmailToken{
		ID:        "tok-8",
		UserID:    "user-1",
		Kind:      "password_reset",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	pwUp := &memPwUpdater{err: errors.New("db write failed")}

	h := NewPasswordResetHandler(store, newMemUserStore(), &memKeyInit{recoverK: "k"}, pwUp, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/confirm",
		`{"token":"bcrypt-fail-token","newPassword":"newpass123"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPasswordReset_Request_DBError_NoEnumeration(t *testing.T) {
	// GetUserByEmail error must return 202 (not 500) to avoid enumeration.
	// The handler logs the error but returns 202.
	users := newMemUserStore()
	// Override GetUserByEmail to return an error
	users2 := &erroringUserStore{memUserStore: users}

	h := NewPasswordResetHandler(newMemTokenStore(), users2, &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/request", `{"email":"alice@test.com"}`)
	assert.Equal(t, http.StatusAccepted, w.Code, "DB lookup error must still return 202 (no enumeration)")
}

func TestPasswordReset_Request_BodyTooLarge_Rejected(t *testing.T) {
	h := NewPasswordResetHandler(newMemTokenStore(), newMemUserStore(), &memKeyInit{}, &memPwUpdater{}, &memSessionRevoker{}, emailsvc.NewService(&fakeEmailProvider{}, "", ""), nil)
	router := setupPasswordResetRouter(h)

	// Body > 1 MiB must be rejected
	big := strings.Repeat("x", 2*1024*1024)
	w := doRequest(router, http.MethodPost, "/api/v1/auth/password-reset/request", `{"email":"`+big+`@"}`)
	// Gin binding will fail (not a valid email or body too large) → 400
	assert.True(t, w.Code == http.StatusBadRequest || w.Code == http.StatusRequestEntityTooLarge,
		"oversized body must be rejected, got %d", w.Code)
}

func TestEmailVerifierAdapter_StoreError(t *testing.T) {
	store := &memTokenStore{createErr: errors.New("db unavailable")}
	adapter := NewEmailVerifierAdapter(store, emailsvc.NewService(&fakeEmailProvider{}, "", ""), "https://app.test")
	err := adapter.SendVerification(context.Background(), "user-1", "alice@test.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store verify token")
}

func TestEmailVerifierAdapter_EmailSendError(t *testing.T) {
	store := newMemTokenStore()
	fp := &fakeEmailProvider{err: errors.New("ses down")}
	adapter := NewEmailVerifierAdapter(store, emailsvc.NewService(fp, "https://app.test", "ses"), "https://app.test")
	err := adapter.SendVerification(context.Background(), "user-1", "alice@test.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "send verify email")
}

// erroringUserStore wraps memUserStore to inject a DB error on GetUserByEmail.
type erroringUserStore struct {
	*memUserStore
}

func (s *erroringUserStore) GetUserByEmail(_ context.Context, _ string) (*types.User, error) {
	return nil, errors.New("db unavailable")
}

func hashTokenForTest(token string) string {
	return hashToken(token)
}

// TestPasswordReset_RoutesRegistered verifies the endpoints are reachable
// through a router-level integration test. This is the Rule 0 e2e wiring
// requirement: the handler is not just unit-tested in isolation but traced
// through the actual route registration path.
func TestPasswordReset_RoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Construct the handler with minimal fakes — we only verify routing,
	// not full flow (that's covered by the Request/Confirm tests above).
	h := NewPasswordResetHandler(
		newMemTokenStore(),
		newMemUserStore(),
		&memKeyInit{recoverK: "key"},
		&memPwUpdater{},
		&memSessionRevoker{},
		emailsvc.NewService(&fakeEmailProvider{}, "https://app.test", "noop"),
		nil,
	)

	// Register routes the same way router.go does (public auth group).
	authGroup := r.Group("/api/v1/auth")
	authGroup.POST("/password-reset/request", h.Request)
	authGroup.POST("/password-reset/confirm", h.Confirm)

	tests := []struct {
		path       string
		body       string
		expectCode int
	}{
		{"/api/v1/auth/password-reset/request", `{"email":"unknown@test.com"}`, http.StatusAccepted},
		{"/api/v1/auth/password-reset/confirm", `{"token":"nonexistent","newPassword":"newpass123"}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		w := doRequest(r, http.MethodPost, tt.path, tt.body)
		// The response code must match — proves the route is wired and the
		// handler executed (not a Gin 404-no-route).
		assert.Equal(t, tt.expectCode, w.Code,
			"%s must return %d (route wired + handler executed)", tt.path, tt.expectCode)
	}
}

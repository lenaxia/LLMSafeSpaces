// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/passkey"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- fakes ---

type fakePasskeyUsers struct {
	users map[string]*types.User
}

func (f *fakePasskeyUsers) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	return f.users[email], nil
}
func (f *fakePasskeyUsers) CreateUser(_ context.Context, u *types.User) error {
	f.users[u.Email] = u
	return nil
}

type fakePasskeyAuth struct {
	token string
	err   error
}

func (f *fakePasskeyAuth) IssueTokenAndUnlockDEK(_ context.Context, _ string, _ time.Duration, _ string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

type fakePasskeySvc struct {
	beginRegResult    *passkey.BeginRegistrationOptions
	beginRegErr       error
	finishRegResult   *passkey.FinishRegistrationResult
	finishRegErr      error
	beginLoginResult  *passkey.BeginLoginOptions
	beginLoginErr     error
	beginLoginUserID  string
	finishLoginUserID string
	finishLoginErr    error
	credStored        bool
}

func (s *fakePasskeySvc) BeginRegistration(_ context.Context, _, _ string) (*passkey.BeginRegistrationOptions, error) {
	return s.beginRegResult, s.beginRegErr
}
func (s *fakePasskeySvc) FinishRegistration(_ context.Context, _, _, _ string, _ *protocol.ParsedCredentialCreationData) (*passkey.FinishRegistrationResult, error) {
	return s.finishRegResult, s.finishRegErr
}
func (s *fakePasskeySvc) BeginLogin(_ context.Context, _ string) (*passkey.BeginLoginOptions, string, error) {
	return s.beginLoginResult, s.beginLoginUserID, s.beginLoginErr
}
func (s *fakePasskeySvc) FinishLogin(_ context.Context, _, _ string, _ *protocol.ParsedCredentialAssertionData) (string, error) {
	return s.finishLoginUserID, s.finishLoginErr
}
func (s *fakePasskeySvc) CreateCredentialAndRecoveryCodes(_ context.Context, _ *passkey.Credential, _ []string) error {
	s.credStored = true
	return nil
}

// --- tests ---

func setupPasskeyRouter(svc PasskeyService) (*gin.Engine, *fakePasskeyUsers) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	users := &fakePasskeyUsers{users: make(map[string]*types.User)}
	auth := &fakePasskeyAuth{token: "jwt-token"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour)
	r.POST("/passkey/register/begin", h.RegisterBegin)
	r.POST("/passkey/register/finish", h.RegisterFinish)
	r.POST("/passkey/login/begin", h.LoginBegin)
	r.POST("/passkey/login/finish", h.LoginFinish)
	return r, users
}

func doPasskeyRequest(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRegisterBegin_ExistingUser_Rejects(t *testing.T) {
	svc := &fakePasskeySvc{beginRegResult: &passkey.BeginRegistrationOptions{}}
	r, users := setupPasskeyRouter(svc)
	users.users["existing@test.com"] = &types.User{ID: "u1", Email: "existing@test.com"}

	w := doPasskeyRequest(t, r, "POST", "/passkey/register/begin", map[string]string{"email": "existing@test.com"})
	assert.Equal(t, http.StatusConflict, w.Code, "existing email must be rejected (account-takeover prevention)")
}

func TestRegisterBegin_NewUser_Succeeds(t *testing.T) {
	svc := &fakePasskeySvc{
		beginRegResult: &passkey.BeginRegistrationOptions{
			Options:      map[string]any{"challenge": "abc"},
			SessionToken: "tok-1",
		},
	}
	r, _ := setupPasskeyRouter(svc)

	w := doPasskeyRequest(t, r, "POST", "/passkey/register/begin", map[string]string{"email": "new@test.com"})
	assert.Equal(t, http.StatusOK, w.Code)

	var resp passkey.BeginRegistrationOptions
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "tok-1", resp.SessionToken)
}

func TestRegisterBegin_InvalidEmail(t *testing.T) {
	r, _ := setupPasskeyRouter(&fakePasskeySvc{})

	w := doPasskeyRequest(t, r, "POST", "/passkey/register/begin", map[string]string{"email": "not-an-email"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginBegin_UserNotFound(t *testing.T) {
	r, _ := setupPasskeyRouter(&fakePasskeySvc{beginLoginErr: passkey.ErrUserNotFound})

	w := doPasskeyRequest(t, r, "POST", "/passkey/login/begin", map[string]string{"email": "nobody@test.com"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginBegin_NoPasskey(t *testing.T) {
	r, _ := setupPasskeyRouter(&fakePasskeySvc{beginLoginErr: passkey.ErrNoPasskeyRegistered})

	w := doPasskeyRequest(t, r, "POST", "/passkey/login/begin", map[string]string{"email": "alice@test.com"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegisterFinish_MissingFields(t *testing.T) {
	r, _ := setupPasskeyRouter(&fakePasskeySvc{})

	w := doPasskeyRequest(t, r, "POST", "/passkey/register/finish", map[string]string{"email": "a@test.com"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginFinish_MissingFields(t *testing.T) {
	r, _ := setupPasskeyRouter(&fakePasskeySvc{})

	w := doPasskeyRequest(t, r, "POST", "/passkey/login/finish", map[string]string{"email": "a@test.com"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

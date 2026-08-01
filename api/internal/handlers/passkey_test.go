// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/passkey"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- fakes ---

type fakePasskeyUsers struct {
	users         map[string]*types.User
	createUserErr error
}

func (f *fakePasskeyUsers) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	return f.users[email], nil
}
func (f *fakePasskeyUsers) GetUser(_ context.Context, userID string) (*types.User, error) {
	for _, u := range f.users {
		if u != nil && u.ID == userID {
			return u, nil
		}
	}
	return nil, nil
}
func (f *fakePasskeyUsers) CreateUser(_ context.Context, u *types.User) error {
	if f.createUserErr != nil {
		return f.createUserErr
	}
	f.users[u.Email] = u
	return nil
}
func (f *fakePasskeyUsers) DeleteUser(_ context.Context, userID string) error {
	for email, u := range f.users {
		if u.ID == userID {
			delete(f.users, email)
		}
	}
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
	recoveryUserID    string
	listResult        []passkey.Credential
	listErr           error
	deleteErr         error
	regenerateErr     error
	credPersistErr    error
	addCredErr        error
}

func (s *fakePasskeySvc) BeginRegistration(_ context.Context, _, _ string) (*passkey.BeginRegistrationOptions, error) {
	return s.beginRegResult, s.beginRegErr
}
func (s *fakePasskeySvc) FinishRegistration(_ context.Context, _, _, _ string, _ map[string]any) (*passkey.FinishRegistrationResult, error) {
	return s.finishRegResult, s.finishRegErr
}
func (s *fakePasskeySvc) BeginLogin(_ context.Context, _ string) (*passkey.BeginLoginOptions, string, error) {
	return s.beginLoginResult, s.beginLoginUserID, s.beginLoginErr
}
func (s *fakePasskeySvc) FinishLogin(_ context.Context, _, _ string, _ map[string]any) (string, error) {
	return s.finishLoginUserID, s.finishLoginErr
}
func (s *fakePasskeySvc) CreateCredentialAndRecoveryCodes(_ context.Context, _ *passkey.Credential, _ []string) error {
	if s.credPersistErr != nil {
		return s.credPersistErr
	}
	s.credStored = true
	return nil
}
func (s *fakePasskeySvc) ConsumeRecoveryCode(_ context.Context, email, _ string) (string, error) {
	if s.recoveryUserID != "" {
		return s.recoveryUserID, nil
	}
	return "", passkey.ErrRecoveryCodeNotFound
}
func (s *fakePasskeySvc) ListUserCredentials(_ context.Context, _ string) ([]passkey.Credential, error) {
	return s.listResult, s.listErr
}
func (s *fakePasskeySvc) DeleteUserCredential(_ context.Context, _ string, _ uuid.UUID) error {
	return s.deleteErr
}
func (s *fakePasskeySvc) RegenerateRecoveryCodes(_ context.Context, _ string) ([]string, error) {
	if s.regenerateErr != nil {
		return nil, s.regenerateErr
	}
	return []string{"NEW1", "NEW2"}, nil
}
func (s *fakePasskeySvc) AddCredential(_ context.Context, _ *passkey.Credential) error {
	return s.addCredErr
}
func (s *fakePasskeySvc) GetUserName(_ context.Context, _ string) (string, error) {
	return "testuser", nil
}

// --- tests ---

func setupPasskeyRouter(svc PasskeyService) (*gin.Engine, *fakePasskeyUsers) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	users := &fakePasskeyUsers{users: make(map[string]*types.User)}
	auth := &fakePasskeyAuth{token: "jwt-token"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
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

// validRegistrationResponseJSON returns a minimal valid WebAuthn attestation
// response JSON that parseCreationFromMap can parse without erroring. It is
// NOT a valid attestation — just structurally valid enough for parsing.
func validRegistrationResponseJSON() map[string]any {
	return map[string]any{
		"id":    "e30",
		"rawId": "e30",
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": "omNmbXRkbm9uZWdhdHRTdG10gGhhdXRoRGF0YVjUe30",
			"clientDataJSON":    "e30",
		},
	}
}

// validAssertionResponseJSON returns a minimal valid WebAuthn assertion
// response JSON that parseAssertionFromMap can parse without erroring.
func validAssertionResponseJSON() map[string]any {
	return map[string]any{
		"id":    "e30",
		"rawId": "e30",
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": "AQIDBA",
			"clientDataJSON":    "e30",
			"signature":         "AA",
		},
	}
}

// --- handler success-path tests ---

func TestRegisterFinish_NewUser_Succeeds(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential: passkey.Credential{
				UserID:       "new-user-id",
				CredentialID: []byte("cred-1"),
			},
			RecoveryCodes:      []string{"CODE1", "CODE2"},
			RecoveryCodeHashes: []string{"hash1", "hash2"},
		},
	}
	r, users := setupPasskeyRouter(svc)
	resp := doPasskeyRequest(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": "tok-1",
		"email":        "newfinish@test.com",
		"name":         "New User",
		"response":     validRegistrationResponseJSON(),
	})

	assert.Equal(t, http.StatusOK, resp.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	assert.NotEmpty(t, body["token"], "session token must be returned")
	assert.NotEmpty(t, body["recoveryCodes"], "recovery codes must be returned")

	created, _ := users.GetUserByEmail(context.Background(), "newfinish@test.com")
	require.NotNil(t, created, "user must be created on new signup")
	assert.Equal(t, "new-user-id", created.ID)
	assert.True(t, created.EmailVerified, "email must be auto-verified in dev mode (no email verifier wired)")
	assert.True(t, svc.credStored, "credential + recovery codes must be persisted")
}

func TestLoginFinish_Success(t *testing.T) {
	svc := &fakePasskeySvc{
		finishLoginUserID: "user-1",
	}
	r, users := setupPasskeyRouter(svc)
	users.users["alice@test.com"] = &types.User{
		ID:            "user-1",
		Username:      "alice",
		Email:         "alice@test.com",
		EmailVerified: true,
	}

	resp := doPasskeyRequest(t, r, "POST", "/passkey/login/finish", map[string]any{
		"sessionToken": "tok-1",
		"email":        "alice@test.com",
		"response":     validAssertionResponseJSON(),
	})

	assert.Equal(t, http.StatusOK, resp.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	assert.NotEmpty(t, body["token"], "session token must be returned")
	assert.NotNil(t, body["user"], "user object must be returned")
}

// --- recovery handler tests ---

func TestRecover_ValidCode_Succeeds(t *testing.T) {
	svc := &fakePasskeySvc{recoveryUserID: "user-1"}
	users := &fakePasskeyUsers{users: map[string]*types.User{
		"alice@test.com": {ID: "user-1", Email: "alice@test.com", Username: "alice"},
	}}
	auth := &fakePasskeyAuth{token: "jwt-tok"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/recover", h.Recover)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/recover", map[string]string{
		"email": "alice@test.com",
		"code":  "VALIDRECOVERYCODE12",
	})
	assert.Equal(t, http.StatusOK, resp.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	assert.NotEmpty(t, body["token"])
	assert.Equal(t, true, body["mustEnrollPasskey"])
}

func TestRecover_InvalidCode_Rejected(t *testing.T) {
	svc := &fakePasskeySvc{}
	users := &fakePasskeyUsers{users: map[string]*types.User{}}
	auth := &fakePasskeyAuth{token: "jwt-tok"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/recover", h.Recover)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/recover", map[string]string{
		"email": "alice@test.com",
		"code":  "WRONG-CODE",
	})
	assert.Equal(t, http.StatusUnauthorized, resp.Code)
}

func TestRecover_MissingFields(t *testing.T) {
	users := &fakePasskeyUsers{users: map[string]*types.User{}}
	h := NewPasskeyHandler(&fakePasskeySvc{}, &fakePasskeyAuth{}, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/recover", h.Recover)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/recover", map[string]string{"email": "a@test.com"})
	assert.Equal(t, http.StatusBadRequest, resp.Code)
}

func TestRecover_UserLookupFailure(t *testing.T) {
	svc := &fakePasskeySvc{recoveryUserID: "user-1"}
	// User NOT in the store — GetUserByEmail returns nil.
	users := &fakePasskeyUsers{users: map[string]*types.User{}}
	auth := &fakePasskeyAuth{token: "jwt-tok"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/recover", h.Recover)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/recover", map[string]string{
		"email": "ghost@test.com",
		"code":  "VALIDRECOVERYCODE12",
	})
	assert.Equal(t, http.StatusInternalServerError, resp.Code, "user-not-found after recovery-code consumption must 500")
}

func TestRecover_TokenIssuanceFailure(t *testing.T) {
	svc := &fakePasskeySvc{recoveryUserID: "user-1"}
	users := &fakePasskeyUsers{users: map[string]*types.User{
		"alice@test.com": {ID: "user-1", Email: "alice@test.com", Username: "alice"},
	}}
	auth := &fakePasskeyAuth{err: fmt.Errorf("KEK unavailable")}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/recover", h.Recover)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/recover", map[string]string{
		"email": "alice@test.com",
		"code":  "VALIDRECOVERYCODE12",
	})
	assert.Equal(t, http.StatusInternalServerError, resp.Code, "token issuance failure must 500")
}

func TestRegisterFinish_ChallengeExpired(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegErr: passkey.ErrChallengeExpired,
	}
	r, _ := setupPasskeyRouter(svc)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": "expired-tok",
		"email":        "new@test.com",
		"response":     validRegistrationResponseJSON(),
	})
	assert.Equal(t, http.StatusBadRequest, resp.Code)
}

func TestLoginFinish_ChallengeExpired(t *testing.T) {
	svc := &fakePasskeySvc{
		finishLoginErr: passkey.ErrChallengeExpired,
	}
	r, users := setupPasskeyRouter(svc)
	users.users["alice@test.com"] = &types.User{ID: "u1", Email: "alice@test.com", Username: "alice"}

	resp := doPasskeyRequest(t, r, "POST", "/passkey/login/finish", map[string]any{
		"sessionToken": "expired-tok",
		"email":        "alice@test.com",
		"response":     validAssertionResponseJSON(),
	})
	assert.Equal(t, http.StatusBadRequest, resp.Code)
}

func TestRegisterFinish_PersistenceFailure(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential:         passkey.Credential{UserID: "new-id"},
			RecoveryCodes:      []string{"c1"},
			RecoveryCodeHashes: []string{"h1"},
		},
	}
	r, users := setupPasskeyRouter(svc)
	users.createUserErr = fmt.Errorf("DB down")

	resp := doPasskeyRequest(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": "tok-1",
		"email":        "persist-fail@test.com",
		"response":     validRegistrationResponseJSON(),
	})
	assert.Equal(t, http.StatusInternalServerError, resp.Code)
}

func TestLoginFinish_TokenIssuanceFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakePasskeySvc{finishLoginUserID: "user-1"}
	users := &fakePasskeyUsers{users: map[string]*types.User{
		"alice@test.com": {ID: "user-1", Email: "alice@test.com", Username: "alice"},
	}}
	auth := &fakePasskeyAuth{err: fmt.Errorf("KEK unavailable")}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/login/finish", h.LoginFinish)

	resp := doPasskeyRequest(t, r, "POST", "/passkey/login/finish", map[string]any{
		"sessionToken": "tok-1",
		"email":        "alice@test.com",
		"response":     validAssertionResponseJSON(),
	})
	assert.Equal(t, http.StatusInternalServerError, resp.Code)
}

// --- settings endpoint tests ---

func setupAuthenticatedRouter(svc PasskeyService, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	users := &fakePasskeyUsers{users: map[string]*types.User{}}
	auth := &fakePasskeyAuth{token: "jwt"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	// Inject userID into context (simulates AuthMiddleware).
	r.Use(func(c *gin.Context) { c.Set("userID", userID); c.Next() })
	r.GET("/account/passkeys", h.ListPasskeys)
	r.DELETE("/account/passkeys/:id", h.DeletePasskey)
	r.POST("/account/passkeys/recovery-codes/regenerate", h.RegenerateRecoveryCodes)
	r.POST("/account/passkeys/enroll/begin", h.BeginEnrollPasskey)
	r.POST("/account/passkeys/enroll/finish", h.FinishEnrollPasskey)
	return r
}

func TestListPasskeys_ReturnsCredentials(t *testing.T) {
	svc := &fakePasskeySvc{}
	svc.listResult = []passkey.Credential{
		{ID: uuid.New(), UserID: "u1", Name: "YubiKey", CreatedAt: time.Now()},
	}
	r := setupAuthenticatedRouter(svc, "u1")

	w := doPasskeyRequest(t, r, "GET", "/account/passkeys", nil)
	assert.Equal(t, 200, w.Code)

	var resp struct{ Passkeys []map[string]any }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Passkeys, 1)
}

func TestListPasskeys_RequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &fakePasskeySvc{}
	users := &fakePasskeyUsers{users: map[string]*types.User{}}
	h := NewPasskeyHandler(svc, &fakePasskeyAuth{}, users, time.Hour, "lsp_session", "")
	r := gin.New()
	// No userID in context.
	r.GET("/account/passkeys", h.ListPasskeys)

	w := doPasskeyRequest(t, r, "GET", "/account/passkeys", nil)
	assert.Equal(t, 401, w.Code)
}

func TestDeletePasskey_Succeeds(t *testing.T) {
	svc := &fakePasskeySvc{}
	credID := uuid.New()
	svc.listResult = []passkey.Credential{{ID: credID, UserID: "u1"}}
	r := setupAuthenticatedRouter(svc, "u1")

	w := doPasskeyRequest(t, r, "DELETE", "/account/passkeys/"+credID.String(), nil)
	assert.Equal(t, 200, w.Code)
}

func TestDeletePasskey_LastCredentialRefused(t *testing.T) {
	svc := &fakePasskeySvc{deleteErr: passkey.ErrLastCredential}
	r := setupAuthenticatedRouter(svc, "u1")
	credID := uuid.New()

	w := doPasskeyRequest(t, r, "DELETE", "/account/passkeys/"+credID.String(), nil)
	assert.Equal(t, 409, w.Code)
}

func TestDeletePasskey_NotFound(t *testing.T) {
	svc := &fakePasskeySvc{deleteErr: passkey.ErrCredentialNotFound}
	r := setupAuthenticatedRouter(svc, "u1")
	credID := uuid.New()

	w := doPasskeyRequest(t, r, "DELETE", "/account/passkeys/"+credID.String(), nil)
	assert.Equal(t, 404, w.Code)
}

func TestRegenerateRecoveryCodes_ReturnsNewCodes(t *testing.T) {
	svc := &fakePasskeySvc{}
	r := setupAuthenticatedRouter(svc, "u1")

	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/recovery-codes/regenerate", nil)
	assert.Equal(t, 200, w.Code)

	var resp struct{ RecoveryCodes []string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.RecoveryCodes, 2) // fakePasskeySvc returns 2 codes
}

func TestListPasskeys_StoreError_500(t *testing.T) {
	svc := &fakePasskeySvc{listErr: fmt.Errorf("DB down")}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "GET", "/account/passkeys", nil)
	assert.Equal(t, 500, w.Code)
}

func TestRegenerateRecoveryCodes_StoreError_500(t *testing.T) {
	svc := &fakePasskeySvc{regenerateErr: fmt.Errorf("DB down")}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/recovery-codes/regenerate", nil)
	assert.Equal(t, 500, w.Code)
}

func TestDeletePasskey_InvalidUUID_400(t *testing.T) {
	r := setupAuthenticatedRouter(&fakePasskeySvc{}, "u1")
	w := doPasskeyRequest(t, r, "DELETE", "/account/passkeys/not-a-uuid", nil)
	assert.Equal(t, 400, w.Code)
}

func TestDeletePasskey_RequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewPasskeyHandler(&fakePasskeySvc{}, &fakePasskeyAuth{}, &fakePasskeyUsers{users: map[string]*types.User{}}, time.Hour, "lsp_session", "")
	r := gin.New()
	r.DELETE("/account/passkeys/:id", h.DeletePasskey)
	w := doPasskeyRequest(t, r, "DELETE", "/account/passkeys/"+uuid.New().String(), nil)
	assert.Equal(t, 401, w.Code)
}

func TestRegenerateRecoveryCodes_RequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewPasskeyHandler(&fakePasskeySvc{}, &fakePasskeyAuth{}, &fakePasskeyUsers{users: map[string]*types.User{}}, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/account/passkeys/recovery-codes/regenerate", h.RegenerateRecoveryCodes)
	r.POST("/account/passkeys/enroll/begin", h.BeginEnrollPasskey)
	r.POST("/account/passkeys/enroll/finish", h.FinishEnrollPasskey)
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/recovery-codes/regenerate", nil)
	assert.Equal(t, 401, w.Code)
}

// --- enroll endpoint tests ---

func TestBeginEnrollPasskey_Succeeds(t *testing.T) {
	svc := &fakePasskeySvc{
		beginRegResult: &passkey.BeginRegistrationOptions{
			Options:      map[string]any{"challenge": "xyz"},
			SessionToken: "enroll-tok",
		},
	}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/begin", nil)
	assert.Equal(t, 200, w.Code)
}

func TestBeginEnrollPasskey_RequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewPasskeyHandler(&fakePasskeySvc{}, &fakePasskeyAuth{}, &fakePasskeyUsers{users: map[string]*types.User{}}, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/account/passkeys/enroll/begin", h.BeginEnrollPasskey)
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/begin", nil)
	assert.Equal(t, 401, w.Code)
}

func TestFinishEnrollPasskey_Succeeds(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential: passkey.Credential{UserID: "u1"},
		},
	}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/finish", map[string]any{
		"sessionToken": "tok",
		"response":     map[string]any{"id": "x"},
	})
	assert.Equal(t, 200, w.Code)
}

func TestFinishEnrollPasskey_CredentialOwnershipMismatch_403(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential: passkey.Credential{UserID: "different-user"},
		},
	}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/finish", map[string]any{
		"sessionToken": "tok",
		"response":     map[string]any{"id": "x"},
	})
	assert.Equal(t, 403, w.Code)
}

func TestFinishEnrollPasskey_RequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewPasskeyHandler(&fakePasskeySvc{}, &fakePasskeyAuth{}, &fakePasskeyUsers{users: map[string]*types.User{}}, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/account/passkeys/enroll/finish", h.FinishEnrollPasskey)
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/finish", map[string]any{
		"sessionToken": "tok",
		"response":     map[string]any{"id": "x"},
	})
	assert.Equal(t, 401, w.Code)
}

func TestFinishEnrollPasskey_MissingFields_400(t *testing.T) {
	r := setupAuthenticatedRouter(&fakePasskeySvc{}, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/finish", map[string]any{})
	assert.Equal(t, 400, w.Code)
}

// --- regression tests for bug fixes ---

func TestRegisterFinish_OrphanCleanup_OnCredentialFailure(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential:         passkey.Credential{UserID: "orphan-user"},
			RecoveryCodeHashes: []string{"h1"},
		},
		credPersistErr: fmt.Errorf("DB down"),
	}
	r, users := setupPasskeyRouter(svc)
	resp := doPasskeyRequest(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": "tok",
		"email":        "orphan@test.com",
		"response":     validRegistrationResponseJSON(),
	})
	assert.Equal(t, 500, resp.Code)
	// User must have been cleaned up (deleted).
	created, _ := users.GetUserByEmail(context.Background(), "orphan@test.com")
	assert.Nil(t, created, "orphaned user must be deleted after credential persistence failure")
}

func TestRegisterFinish_UniqueViolation_Returns409(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential:         passkey.Credential{UserID: "dup-user"},
			RecoveryCodeHashes: []string{"h1"},
		},
	}
	r, users := setupPasskeyRouter(svc)
	// Simulate unique-constraint violation on CreateUser.
	users.createUserErr = &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	resp := doPasskeyRequest(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": "tok",
		"email":        "dup@test.com",
		"response":     validRegistrationResponseJSON(),
	})
	assert.Equal(t, 409, resp.Code)
}

func TestFinishEnrollPasskey_AddCredentialFailure_500(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegResult: &passkey.FinishRegistrationResult{
			Credential: passkey.Credential{UserID: "u1"},
		},
		addCredErr: fmt.Errorf("DB down"),
	}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/finish", map[string]any{
		"sessionToken": "tok",
		"response":     map[string]any{"id": "x"},
	})
	assert.Equal(t, 500, w.Code)
}

func TestFinishEnrollPasskey_ChallengeExpired_400(t *testing.T) {
	svc := &fakePasskeySvc{
		finishRegErr: passkey.ErrChallengeExpired,
	}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/finish", map[string]any{
		"sessionToken": "expired",
		"response":     map[string]any{"id": "x"},
	})
	assert.Equal(t, 400, w.Code)
}

func TestBeginEnrollPasskey_BeginRegistrationFails_500(t *testing.T) {
	svc := &fakePasskeySvc{beginRegErr: fmt.Errorf("webauthn init failed")}
	r := setupAuthenticatedRouter(svc, "u1")
	w := doPasskeyRequest(t, r, "POST", "/account/passkeys/enroll/begin", nil)
	assert.Equal(t, 500, w.Code)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- Security e2e tests: body size limits ---

func TestRegister_BodyTooLarge_Rejected(t *testing.T) {
	router, _ := newAuthFixture(t)

	largePayload := strings.Repeat("x", 2*1024*1024) // 2 MiB
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register",
		strings.NewReader(`{"username":"u","email":"e@e.com","password":"`+largePayload+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// MaxBytesReader causes either 400 or EOF during bind
	assert.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusRequestEntityTooLarge,
		"oversized body should be rejected, got %d", rec.Code)
}

func TestLogin_BodyTooLarge_Rejected(t *testing.T) {
	router, _ := newAuthFixture(t)

	largePayload := strings.Repeat("x", 2*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"e@e.com","password":"`+largePayload+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusRequestEntityTooLarge,
		"oversized body should be rejected, got %d", rec.Code)
}

func TestCreateAPIKey_BodyTooLarge_Rejected(t *testing.T) {
	router, _ := newAuthenticatedFixture(t, "user-1")

	largePayload := strings.Repeat("x", 2*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/api-keys",
		strings.NewReader(`{"name":"`+largePayload+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.True(t, rec.Code == http.StatusBadRequest || rec.Code == http.StatusRequestEntityTooLarge,
		"oversized body should be rejected, got %d", rec.Code)
}

// --- Security: error messages must not leak internals ---

func TestRegister_DuplicateEmail_GenericError(t *testing.T) {
	router, svc := newAuthFixture(t)

	svc.auth.On("Register", mock.Anything, mock.Anything).
		Return(nil, errors.New("registration failed"))

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "taken@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	errMsg, _ := body["error"].(string)
	assert.Equal(t, "registration failed", errMsg)
	assert.NotContains(t, errMsg, "email", "error must not reveal email is taken")
	assert.NotContains(t, errMsg, "duplicate", "error must not reveal duplication")
	assert.NotContains(t, errMsg, "exists", "error must not reveal existence")
}

// TestRegister_DuplicateEmail_ConflictError_GenericOnWire pins the WIRE
// response when the auth service returns the real duplicate-email error
// (audit #1649 gap C): the conflict status is kept, but the rendered message
// must stay generic. Pre-fix, the service returned
// NewConflictError("user","email",...) whose Error() renders
// "conflict: user email already exists (registration failed)" — an
// account-existence oracle on a public endpoint. The sibling test above
// mocks a plain error, so it never saw the leak.
func TestRegister_DuplicateEmail_ConflictError_GenericOnWire(t *testing.T) {
	router, svc := newAuthFixture(t)

	svc.auth.On("Register", mock.Anything, mock.Anything).
		Return(nil, &apierrors.APIError{
			Type:    apierrors.ErrorTypeConflict,
			Code:    "conflict",
			Message: "registration failed",
		})

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "taken@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusConflict, rec.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	errMsg, _ := body["error"].(string)
	assert.Equal(t, "conflict: registration failed", errMsg)
	assert.NotContains(t, errMsg, "exists", "error must not reveal existence")
	assert.NotContains(t, errMsg, "already", "error must not reveal duplication")
	assert.NotContains(t, errMsg, "email", "error must not reveal email is taken")
}

func TestLogin_WrongPassword_GenericError(t *testing.T) {
	router, svc := newAuthFixture(t)

	svc.auth.On("Login", mock.Anything, mock.Anything).
		Return(nil, errors.New("invalid email or password"))

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/login", types.LoginRequest{
		Email:    "nobody@example.com",
		Password: "wrong",
	})

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	errMsg, _ := body["error"].(string)
	assert.Equal(t, "invalid email or password", errMsg)
	assert.NotContains(t, errMsg, "not found", "error must be identical for wrong email vs wrong password")
	assert.NotContains(t, errMsg, "disabled", "error must not reveal account state")
}

func TestLogin_InactiveUser_GenericError(t *testing.T) {
	router, svc := newAuthFixture(t)

	svc.auth.On("Login", mock.Anything, mock.Anything).
		Return(nil, errors.New("invalid email or password"))

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/login", types.LoginRequest{
		Email:    "disabled@example.com",
		Password: "correctpassword",
	})

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	errMsg, _ := body["error"].(string)
	assert.Equal(t, "invalid email or password", errMsg,
		"inactive user must get same error as wrong password to prevent enumeration")
}

// --- Security: binding errors must not leak struct internals ---

func TestRegister_InvalidJSON_SanitizedError(t *testing.T) {
	router, _ := newAuthFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register",
		strings.NewReader(`{invalid json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	errMsg, _ := body["error"].(string)
	assert.NotContains(t, errMsg, "json", "must not expose JSON parser internals")
	assert.NotContains(t, errMsg, "unmarshal", "must not expose Go type details")
}

// --- Security: password never returned in any response ---

func TestRegister_PasswordNotInResponse(t *testing.T) {
	router, svc := newAuthFixture(t)

	svc.auth.On("Register", mock.Anything, mock.Anything).Return(&types.AuthResponse{
		Token: "jwt-token",
		User: types.User{
			ID:       "u1",
			Username: "testuser",
			Email:    "test@example.com",
			Active:   true,
			Role:     "user",
		},
	}, nil)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "test@example.com",
		Password: "supersecret123",
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.NotContains(t, rec.Body.String(), "password", "response must not contain 'password'")
	assert.NotContains(t, rec.Body.String(), "hash", "response must not contain 'hash'")
	assert.NotContains(t, rec.Body.String(), "secret", "response must not contain user's password")
}

func TestLogin_PasswordNotInResponse(t *testing.T) {
	router, svc := newAuthFixture(t)

	svc.auth.On("Login", mock.Anything, mock.Anything).Return(&types.AuthResponse{
		Token: "jwt-token",
		User: types.User{
			ID:       "u1",
			Username: "testuser",
			Email:    "test@example.com",
		},
	}, nil)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/login", types.LoginRequest{
		Email:    "test@example.com",
		Password: "supersecret123",
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "password")
	assert.NotContains(t, rec.Body.String(), "hash")
}

// --- Security: API key list must not expose secret values ---

func TestListAPIKeys_SecretsStripped(t *testing.T) {
	router, svc := newAuthenticatedFixture(t, "user-1")

	svc.auth.On("ListAPIKeys", mock.Anything, "user-1").Return([]*types.APIKey{
		{ID: "k1", Name: "key-one", Prefix: "lsp_", Active: true},
	}, nil)

	rec := doRequest(t, router, http.MethodGet, "/api/v1/auth/api-keys", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var keys []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &keys))
	assert.Len(t, keys, 1)
	assert.Equal(t, "k1", keys[0]["id"])
	assert.Equal(t, "key-one", keys[0]["name"])

	_, hasKey := keys[0]["key"]
	assert.False(t, hasKey, "listed keys must not expose the secret 'key' field")
}

// --- Security: API key creation returns secret only once ---

func TestCreateAPIKey_SecretOnlyOnCreation(t *testing.T) {
	router, svc := newAuthenticatedFixture(t, "user-1")

	svc.auth.On("CreateAPIKey", mock.Anything, "user-1", mock.Anything, mock.Anything, mock.Anything).Return(&types.APIKey{
		ID:     "k1",
		Name:   "my-key",
		Key:    "lsp_supersecret123",
		Prefix: "lsp_",
		Active: true,
	}, nil)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/api-keys",
		types.CreateAPIKeyRequest{Name: "my-key"})

	assert.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "lsp_supersecret123", body["key"], "key must be returned on creation")
}

// --- Security: unauthenticated access to protected endpoints ---

func TestAPIKeyEndpoints_RequireAuth(t *testing.T) {
	router, _ := newAuthFixture(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/auth/api-keys"},
		{http.MethodGet, "/api/v1/auth/api-keys"},
		{http.MethodDelete, "/api/v1/auth/api-keys/some-id"},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"%s %s should require auth", ep.method, ep.path)
	}
}

// --- Security: no HTTP methods other than intended ---

func TestRegister_RejectsGet(t *testing.T) {
	router, _ := newAuthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/register", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "GET /register should 404 (not 200)")
}

func TestLogin_RejectsGet(t *testing.T) {
	router, _ := newAuthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "GET /login should 404")
}

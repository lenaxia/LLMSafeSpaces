// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestE2E_CeremonyThroughHTTP is the full end-to-end ceremony test through real
// HTTP requests. It wires a real passkey.Service (real go-webauthn verification)
// backed by miniredis, wraps it in a gin router, and exercises the complete
// register → login flow over HTTP using the test authenticator.
//
// This proves the entire request path works: HTTP parsing → challenge
// generation/storage → go-webauthn attestation verification → credential
// persistence → challenge consumption → assertion verification → sign-count
// tracking. No fakes on the ceremony path — the crypto is real.
func TestE2E_CeremonyThroughHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Real Redis (miniredis) for the challenge store.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	// Real passkey service with real go-webauthn.
	store := &memStore{}
	sessionStore := NewCacheSessionStore(redisClient)
	users := &fakeUserLookup{users: make(map[string]*types.User)}
	svc, err := New(ServiceConfig{
		RPID:      "localhost",
		RPName:    "Test",
		RPOrigins: []string{"https://localhost"},
		Store:     store,
		Users:     users,
		Sessions:  sessionStore,
	})
	require.NoError(t, err)

	// Build a gin router that wraps the service, mirroring the real handler.
	r := gin.New()
	r.POST("/passkey/register/begin", func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid request"})
			return
		}
		if _, exists := users.users[req.Email]; exists {
			c.JSON(409, gin.H{"error": "exists"})
			return
		}
		userID := fmt.Sprintf("e2e-user-%d", time.Now().UnixNano())
		username := req.Name
		if username == "" {
			username = strings.Split(req.Email, "@")[0]
		}
		opts, err := svc.BeginRegistration(c.Request.Context(), userID, username)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		opts.Options["_provisionalUserID"] = userID
		c.JSON(200, opts)
	})
	r.POST("/passkey/register/finish", func(c *gin.Context) {
		var req struct {
			SessionToken string         `json:"sessionToken"`
			Email        string         `json:"email"`
			Name         string         `json:"name"`
			Response     map[string]any `json:"response"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid request"})
			return
		}
		username := req.Name
		if username == "" {
			username = strings.Split(req.Email, "@")[0]
		}
		result, err := svc.FinishRegistration(c.Request.Context(), req.SessionToken, username, req.Name, req.Response)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// Create user + persist credential + recovery codes.
		users.users[req.Email] = &types.User{ID: result.Credential.UserID, Username: username, Email: req.Email}
		_ = svc.CreateCredentialAndRecoveryCodes(c.Request.Context(), &result.Credential, result.RecoveryCodeHashes)
		c.JSON(200, gin.H{
			"recoveryCodes": result.RecoveryCodes,
			"userID":        result.Credential.UserID,
		})
	})
	r.POST("/passkey/login/begin", func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid request"})
			return
		}
		opts, _, err := svc.BeginLogin(c.Request.Context(), req.Email)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, opts)
	})
	r.POST("/passkey/login/finish", func(c *gin.Context) {
		var req struct {
			SessionToken string         `json:"sessionToken"`
			Email        string         `json:"email"`
			Response     map[string]any `json:"response"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid request"})
			return
		}
		userID, err := svc.FinishLogin(c.Request.Context(), req.SessionToken, req.Email, req.Response)
		if err != nil {
			c.JSON(401, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"userID": userID})
	})

	auth := newTestAuthenticator()

	// === PHASE 1: Registration over HTTP ===

	// Step 1: begin registration.
	regBegin := doPost(t, r, "POST", "/passkey/register/begin",
		map[string]string{"email": "e2e@test.com", "name": "E2E User"})
	require.Equal(t, 200, regBegin.Code)

	var regBeginResp struct {
		Options      map[string]any `json:"options"`
		SessionToken string         `json:"sessionToken"`
	}
	require.NoError(t, json.Unmarshal(regBegin.Body.Bytes(), &regBeginResp))
	require.NotEmpty(t, regBeginResp.SessionToken)

	// Extract the challenge from the flat WebAuthn options.
	challenge, ok := regBeginResp.Options["challenge"].(string)
	require.True(t, ok, "options must contain a challenge string")
	require.NotEmpty(t, challenge)

	// Step 2: generate attestation with the test authenticator.
	attResp, err := auth.generateRegistrationResponse(challenge)
	require.NoError(t, err)

	// Step 3: finish registration.
	regFinish := doPost(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": regBeginResp.SessionToken,
		"email":        "e2e@test.com",
		"name":         "E2E User",
		"response":     attResp,
	})
	require.Equal(t, 200, regFinish.Code, "registration must succeed: %s", regFinish.Body.String())

	var regFinishResp struct {
		RecoveryCodes []string `json:"recoveryCodes"`
		UserID        string   `json:"userID"`
	}
	require.NoError(t, json.Unmarshal(regFinish.Body.Bytes(), &regFinishResp))
	require.Len(t, regFinishResp.RecoveryCodes, RecoveryCodeCount)
	require.NotEmpty(t, regFinishResp.UserID)

	// Verify the credential was persisted.
	creds, _ := store.ListCredentials(context.Background(), regFinishResp.UserID)
	require.Len(t, creds, 1, "one credential must be stored")

	// === PHASE 2: Login over HTTP ===

	// Step 4: begin login.
	loginBegin := doPost(t, r, "POST", "/passkey/login/begin",
		map[string]string{"email": "e2e@test.com"})
	require.Equal(t, 200, loginBegin.Code)

	var loginBeginResp struct {
		Options      map[string]any `json:"options"`
		SessionToken string         `json:"sessionToken"`
	}
	require.NoError(t, json.Unmarshal(loginBegin.Body.Bytes(), &loginBeginResp))
	require.NotEmpty(t, loginBeginResp.SessionToken)

	// Extract the login challenge.
	loginChallenge, ok := loginBeginResp.Options["challenge"].(string)
	require.True(t, ok)

	require.True(t, ok, "login options must contain a challenge string")
	require.NotEmpty(t, loginChallenge)

	// Step 5: generate assertion with the test authenticator (same key).
	assertResp, err := auth.generateAssertionResponse(loginChallenge)
	require.NoError(t, err)

	// Step 6: finish login.
	loginFinish := doPost(t, r, "POST", "/passkey/login/finish", map[string]any{
		"sessionToken": loginBeginResp.SessionToken,
		"email":        "e2e@test.com",
		"response":     assertResp,
	})
	require.Equal(t, 200, loginFinish.Code, "login must succeed: %s", loginFinish.Body.String())

	var loginFinishResp struct {
		UserID string `json:"userID"`
	}
	require.NoError(t, json.Unmarshal(loginFinish.Body.Bytes(), &loginFinishResp))
	assert.Equal(t, regFinishResp.UserID, loginFinishResp.UserID, "login must return the same user ID")

	// Sign count must be updated.
	creds2, _ := store.ListCredentials(context.Background(), loginFinishResp.UserID)
	require.Len(t, creds2, 1)
	assert.GreaterOrEqual(t, creds2[0].SignCount, uint32(1), "sign count must be updated after login")
}

// TestE2E_Replay_RejectedOverHTTP verifies that a consumed challenge cannot be
// replayed over HTTP — the single-use guarantee is enforced end-to-end.
func TestE2E_Replay_RejectedOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	store := &memStore{}
	sessionStore := NewCacheSessionStore(redisClient)
	users := &fakeUserLookup{users: make(map[string]*types.User)}
	svc, err := New(ServiceConfig{
		RPID:      "localhost",
		RPName:    "Test",
		RPOrigins: []string{"https://localhost"},
		Store:     store,
		Users:     users,
		Sessions:  sessionStore,
	})
	require.NoError(t, err)

	r := gin.New()
	r.POST("/passkey/register/begin", func(c *gin.Context) {
		opts, _ := svc.BeginRegistration(c.Request.Context(), "replay-user", "replay")
		c.JSON(200, opts)
	})
	r.POST("/passkey/register/finish", func(c *gin.Context) {
		var req struct {
			SessionToken string         `json:"sessionToken"`
			Response     map[string]any `json:"response"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid request"})
			return
		}
		_, err := svc.FinishRegistration(c.Request.Context(), req.SessionToken, "replay", "Replay", req.Response)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// Begin registration.
	regBegin := doPost(t, r, "POST", "/passkey/register/begin", map[string]string{})
	var beginResp struct {
		SessionToken string `json:"sessionToken"`
	}
	require.NoError(t, json.Unmarshal(regBegin.Body.Bytes(), &beginResp))

	// First finish — should fail (invalid attestation) but consume the challenge.
	firstFinish := doPost(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": beginResp.SessionToken,
		"response":     map[string]any{},
	})
	require.NotEqual(t, 200, firstFinish.Code, "empty attestation must fail")

	// Replay — same token, must get ErrChallengeExpired.
	replay := doPost(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": beginResp.SessionToken,
		"response":     map[string]any{},
	})
	require.NotEqual(t, 200, replay.Code)
	assert.Contains(t, replay.Body.String(), "expired", "replayed challenge must be rejected")
}

func doPost(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, strings.NewReader(string(jsonBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestE2E_InvalidAttestation_RejectedOverHTTP verifies that a malformed
// attestation response is rejected over HTTP, and the challenge is consumed
// (single-use) so it cannot be retried.
func TestE2E_InvalidAttestation_RejectedOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	store := &memStore{}
	users := &fakeUserLookup{users: make(map[string]*types.User)}
	svc, err := New(ServiceConfig{
		RPID:      "localhost",
		RPName:    "Test",
		RPOrigins: []string{"https://localhost"},
		Store:     store,
		Users:     users,
		Sessions:  NewCacheSessionStore(redisClient),
	})
	require.NoError(t, err)

	r := gin.New()
	r.POST("/passkey/register/begin", func(c *gin.Context) {
		var req struct{ Email string }
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid"})
			return
		}
		opts, _ := svc.BeginRegistration(c.Request.Context(), "invalid-user", "test")
		c.JSON(200, opts)
	})
	r.POST("/passkey/register/finish", func(c *gin.Context) {
		var req struct {
			SessionToken string         `json:"sessionToken"`
			Response     map[string]any `json:"response"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid"})
			return
		}
		_, err := svc.FinishRegistration(c.Request.Context(), req.SessionToken, "test", "Test", req.Response)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// Begin registration.
	regBegin := doPost(t, r, "POST", "/passkey/register/begin", map[string]string{"email": "invalid@test.com"})
	require.Equal(t, 200, regBegin.Code)
	var beginResp struct {
		SessionToken string `json:"sessionToken"`
	}
	require.NoError(t, json.Unmarshal(regBegin.Body.Bytes(), &beginResp))

	// Send an invalid attestation (empty response map — will fail at verification).
	badFinish := doPost(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": beginResp.SessionToken,
		"response":     map[string]any{"id": "bad", "rawId": "bad", "type": "public-key"},
	})
	require.NotEqual(t, 200, badFinish.Code, "invalid attestation must be rejected")

	// The challenge must be consumed — retry must fail with "expired".
	retry := doPost(t, r, "POST", "/passkey/register/finish", map[string]any{
		"sessionToken": beginResp.SessionToken,
		"response":     map[string]any{"id": "bad", "rawId": "bad", "type": "public-key"},
	})
	require.NotEqual(t, 200, retry.Code)
	assert.Contains(t, retry.Body.String(), "expired", "consumed challenge must not be reusable")
}

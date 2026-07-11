// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestE2E_RealAuth_WorkspaceEnv tests PUT/GET/DELETE /workspaces/:id/env
func TestE2E_RealAuth_WorkspaceEnv(t *testing.T) {
	router, token, _ := setupRealAuthRouter(t)
	base := startServer(t, router)
	c := &http.Client{Timeout: 30 * time.Second}

	// Set env vars
	resp := doPut(t, c, base+"/api/v1/workspaces/ws-env-test/env",
		`{"vars":{"DATABASE_URL":"postgres://x","API_KEY":"secret123"}}`, token)
	if resp.StatusCode != 204 {
		t.Fatalf("SetEnv: expected 204, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// Get env vars (names only, never values)
	resp = doGet(t, c, base+"/api/v1/workspaces/ws-env-test/env", token)
	if resp.StatusCode != 200 {
		t.Fatalf("GetEnv: expected 200, got %d", resp.StatusCode)
	}
	var envResp struct{ Vars []string }
	json.NewDecoder(resp.Body).Decode(&envResp)
	resp.Body.Close()
	if len(envResp.Vars) != 2 {
		t.Errorf("Expected 2 env vars, got %d", len(envResp.Vars))
	}

	// Delete one env var
	req, _ := http.NewRequest("DELETE", base+"/api/v1/workspaces/ws-env-test/env/DATABASE_URL", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = c.Do(req)
	if resp.StatusCode != 204 {
		t.Fatalf("DeleteEnv: expected 204, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// Verify only 1 remains
	resp = doGet(t, c, base+"/api/v1/workspaces/ws-env-test/env", token)
	json.NewDecoder(resp.Body).Decode(&envResp)
	resp.Body.Close()
	if len(envResp.Vars) != 1 {
		t.Errorf("Expected 1 env var after delete, got %d", len(envResp.Vars))
	}

	t.Log("E2E WorkspaceEnv: PUT/GET/DELETE — PASSED")
}

// TestE2E_RealAuth_ChangePassword tests POST /account/change-password
func TestE2E_RealAuth_ChangePassword(t *testing.T) {
	router, token, svc := setupRealAuthRouter(t)
	base := startServer(t, router)
	c := &http.Client{Timeout: 30 * time.Second}

	// Change password
	resp := doPost(t, c, base+"/api/v1/account/change-password",
		`{"oldPassword":"secure-password-123","newPassword":"new-secure-password-456"}`, token)
	if resp.StatusCode != 204 {
		t.Fatalf("ChangePassword: expected 204, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// Login with old password should fail
	resp = doPost(t, c, base+"/api/v1/auth/login",
		`{"email":"test@example.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 401 {
		t.Errorf("Old password login: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Login with new password should succeed
	resp = doPost(t, c, base+"/api/v1/auth/login",
		`{"email":"test@example.com","password":"new-secure-password-456"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("New password login: expected 200, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	var loginResp struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&loginResp)
	resp.Body.Close()
	newToken := loginResp.Token

	// G38 regression: the pre-change JWT (the `token` variable from
	// setup) MUST stop working immediately after a successful password
	// change. Before the fix, the cached DEK + still-valid signature let
	// the stolen token keep reading secrets (and any authenticated
	// endpoint) until natural expiry. This is the OWASP ASVS V2.5.2
	// invariant: changing a password invalidates all existing sessions.
	// Uses GET /secrets because /auth/me is not wired in the test router;
	// any authenticated endpoint serves the same purpose.
	resp = doGet(t, c, base+"/api/v1/secrets", token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("G38 regression: pre-change JWT must be rejected after change-password; got status %d (expected 401)", resp.StatusCode)
	}
	resp.Body.Close()

	// New JWT must still work — proves the rejection above is specific
	// to the pre-change token, not a global auth outage.
	resp = doGet(t, c, base+"/api/v1/secrets", newToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("new JWT must work after change-password; got status %d (expected 200)", resp.StatusCode)
	}
	resp.Body.Close()

	// Secrets should still work with new token
	resp = doPost(t, c, base+"/api/v1/secrets",
		`{"name":"after-pw-change","type":"api-key","value":"sk-test","metadata":{"kind":"x","slug":"x"}}`, newToken)
	if resp.StatusCode != 201 {
		t.Fatalf("Create after pw change: expected 201, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	_ = svc // suppress unused
	t.Log("E2E ChangePassword: change → old fails → new works → old JWT rejected → new JWT works → secrets work — PASSED")
}

// TestE2E_RealAuth_ChangePassword_WrongOld tests wrong old password
func TestE2E_RealAuth_ChangePassword_WrongOld(t *testing.T) {
	router, token, _ := setupRealAuthRouter(t)
	base := startServer(t, router)
	c := &http.Client{Timeout: 30 * time.Second}

	resp := doPost(t, c, base+"/api/v1/account/change-password",
		`{"oldPassword":"wrong-password","newPassword":"doesnt-matter"}`, token)
	if resp.StatusCode != 403 {
		t.Fatalf("Wrong old password: expected 403, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	t.Log("E2E ChangePassword wrong old: 403 — PASSED")
}

// TestE2E_RealAuth_Recover tests POST /account/recover
func TestE2E_RealAuth_Recover(t *testing.T) {
	router, _, svc := setupRealAuthRouter(t)
	base := startServer(t, router)
	c := &http.Client{Timeout: 30 * time.Second}

	// Get the recovery key (stored during registration in the key store)
	// We need to access it from the test setup — it's returned by InitializeUserKeys
	// but not exposed via API. For this test, we'll use the userID from setup.
	userID := svc.testUserID
	recoveryKey := svc.testRecoveryKey

	if recoveryKey == "" {
		t.Skip("Recovery key not captured during setup")
	}

	// Recover with recovery key
	body := fmt.Sprintf(`{"userId":"%s","recoveryKey":"%s","newPassword":"recovered-password-789"}`, userID, recoveryKey)
	resp := doPost(t, c, base+"/api/v1/account/recover", body, "")
	if resp.StatusCode != 200 {
		t.Fatalf("Recover: expected 200, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	var recoverResp struct {
		RecoveryKey string `json:"recoveryKey"`
	}
	json.NewDecoder(resp.Body).Decode(&recoverResp)
	resp.Body.Close()
	if recoverResp.RecoveryKey == "" {
		t.Error("Should return new recovery key")
	}

	// Login with recovered password
	resp = doPost(t, c, base+"/api/v1/auth/login",
		`{"email":"test@example.com","password":"recovered-password-789"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("Login after recovery: expected 200, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// Old recovery key should no longer work
	resp = doPost(t, c, base+"/api/v1/account/recover", body, "")
	if resp.StatusCode != 403 {
		t.Errorf("Old recovery key: expected 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	t.Log("E2E Recover: reset → login → old key invalid — PASSED")
}

// TestE2E_RealAuth_RotateKey_ThenSecrets tests rotation doesn't break existing secrets
func TestE2E_RealAuth_RotateKey_ThenSecrets(t *testing.T) {
	router, token, _ := setupRealAuthRouter(t)
	base := startServer(t, router)
	c := &http.Client{Timeout: 30 * time.Second}

	// Create a secret before rotation
	resp := doPost(t, c, base+"/api/v1/secrets",
		`{"name":"pre-rotate","type":"api-key","value":"sk-before","metadata":{"kind":"x","slug":"x"}}`, token)
	if resp.StatusCode != 201 {
		t.Fatalf("Create pre-rotate: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Rotate
	resp = doPost(t, c, base+"/api/v1/account/rotate-key",
		`{"password":"secure-password-123"}`, token)
	if resp.StatusCode != 200 {
		t.Fatalf("Rotate: %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// Create a secret after rotation (uses new DEK)
	resp = doPost(t, c, base+"/api/v1/secrets",
		`{"name":"post-rotate","type":"api-key","value":"sk-after","metadata":{"kind":"y","slug":"y"}}`, token)
	if resp.StatusCode != 201 {
		t.Fatalf("Create post-rotate: expected 201, got %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	// List should show both
	resp = doGet(t, c, base+"/api/v1/secrets", token)
	var listResp struct{ Secrets []struct{ Name string } }
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if len(listResp.Secrets) != 2 {
		t.Errorf("Expected 2 secrets, got %d", len(listResp.Secrets))
	}

	t.Log("E2E RotateKey then secrets: create before + after rotation — PASSED")
}

// === Shared setup ===

type testContext struct {
	testUserID      string
	testRecoveryKey string
}

func setupRealAuthRouter(t *testing.T) (*gin.Engine, string, *testContext) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &fullMockDB{users: make(map[string]*types.User)}
	// Stateful cache (not mockCache{}) so cache-dependent code paths —
	// JWT revocation, session tracking — actually persist between
	// requests within a single test. The stateless mockCache made it
	// impossible to assert post-change-password JWT rejection.
	cache := newStatefulMockCache()

	authSvc, _ := New(cfg, log, db, cache)

	keyStore := &memKeyStore{records: make(map[string]*secrets.UserKeyRecord)}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := &memSecretStore{secrets: make(map[string]*secrets.UserSecret), bindings: make(map[string][]string)}
	secretSvc := secrets.NewSecretService(keySvc, secretStore)
	secretsHandler := handlers.NewSecretsHandler(secretSvc)
	// US-29.4: WorkspaceEnvHandler is the new owner of env endpoints.
	envHandler := handlers.NewWorkspaceEnvHandler(secretSvc)
	rotateHandler := handlers.NewRotateKeyHandler(keySvc)
	rotateHandler.SetPasswordUpdater(&bcryptUpdater{db: db})
	// G38: wire the auth service's RevokeAllUserSessions so the e2e
	// setup mirrors production wiring and the post-change-password old-
	// JWT-rejection regression can be exercised end-to-end.
	rotateHandler.SetSessionRevoker(authSvc)

	tc := &testContext{}
	authSvc.SetKeyService(&capturingKeyService{inner: keySvc, tc: tc})

	router := gin.New()

	// Public routes
	router.POST("/api/v1/auth/register", func(c *gin.Context) {
		var req types.RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		resp, err := authSvc.Register(c.Request.Context(), req)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// Capture recovery key for test
		tc.testUserID = resp.User.ID
		// Note: recovery key is returned during InitializeUserKeys in
		// Register; tests that need it should subscribe to that path
		// directly. See keyStore.GetUserKey above for the row-existence
		// check that previously lived here.
		c.JSON(201, resp)
	})
	router.POST("/api/v1/auth/login", func(c *gin.Context) {
		var req types.LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		resp, err := authSvc.Login(c.Request.Context(), req)
		if err != nil {
			c.JSON(401, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, resp)
	})
	router.POST("/api/v1/account/recover", rotateHandler.RecoverAccount)

	// Authenticated routes
	authed := router.Group("/api/v1")
	authed.Use(authSvc.AuthMiddleware())
	authed.POST("/secrets", secretsHandler.CreateSecret)
	authed.GET("/secrets", secretsHandler.ListSecrets)
	authed.DELETE("/secrets/:id", secretsHandler.DeleteSecret)
	authed.PUT("/workspaces/:id/env", envHandler.SetWorkspaceEnv)
	authed.GET("/workspaces/:id/env", envHandler.GetWorkspaceEnv)
	authed.DELETE("/workspaces/:id/env/:name", envHandler.DeleteWorkspaceEnv)
	authed.POST("/account/rotate-key", rotateHandler.RotateKey)
	authed.POST("/account/change-password", rotateHandler.ChangePassword)
	authed.POST("/api-keys", func(c *gin.Context) {
		var req types.CreateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		sid, _ := c.Get("sessionID")
		sidStr, _ := sid.(string)
		apiKey, err := authSvc.CreateAPIKey(c.Request.Context(), authSvc.GetUserID(c), req, sidStr, nil)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, apiKey)
	})

	// Register + login to get token
	// We do this programmatically to avoid HTTP overhead in setup
	regResp, err := authSvc.Register(context.Background(), types.RegisterRequest{
		Username: "testuser", Email: "test@example.com", Password: "secure-password-123",
	})
	if err != nil {
		t.Fatalf("Setup register: %v", err)
	}
	tc.testUserID = regResp.User.ID

	loginResp, err := authSvc.Login(context.Background(), types.LoginRequest{
		Email: "test@example.com", Password: "secure-password-123",
	})
	if err != nil {
		t.Fatalf("Setup login: %v", err)
	}

	return router, loginResp.Token, tc
}

func startServer(t *testing.T, router *gin.Engine) string {
	t.Helper()
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv.URL
}

func doPut(t *testing.T, c *http.Client, url, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	return buf.String()
}

// bcryptUpdater implements PasswordHashUpdater for tests.
type bcryptUpdater struct {
	db *fullMockDB
}

func (u *bcryptUpdater) UpdatePasswordHash(_ context.Context, userID string, newPassword []byte) error {
	user := u.db.users[userID]
	if user == nil {
		return fmt.Errorf("user not found")
	}
	hash, err := bcrypt.GenerateFromPassword(newPassword, 4) // low cost for tests
	if err != nil {
		return err
	}
	user.PasswordHash = string(hash)
	return nil
}

// capturingKeyService wraps a real KeyService and captures the recovery key.
type capturingKeyService struct {
	inner *secrets.KeyService
	tc    *testContext
}

func (c *capturingKeyService) InitializeUserKeys(ctx context.Context, userID string, password []byte) (string, error) {
	recoveryKey, err := c.inner.InitializeUserKeys(ctx, userID, password)
	if err == nil {
		c.tc.testRecoveryKey = recoveryKey
	}
	return recoveryKey, err
}

func (c *capturingKeyService) UnlockDEK(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) error {
	return c.inner.UnlockDEK(ctx, userID, password, sessionID, ttl)
}

func (c *capturingKeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, _ []byte) error {
	return c.UnlockDEK(ctx, userID, password, sessionID, ttl)
}

func (c *capturingKeyService) DeleteDurableSessionsForUser(_ context.Context, _ string) error {
	return nil
}

func (c *capturingKeyService) HasKeys(ctx context.Context, userID string) (bool, error) {
	return c.inner.HasKeys(ctx, userID)
}

func (c *capturingKeyService) GetDEK(ctx context.Context, sessionID string, matchedSigningKey []byte) ([]byte, error) {
	return c.inner.GetDEK(ctx, sessionID, nil)
}

func (c *capturingKeyService) CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error {
	return c.inner.CacheDEK(ctx, sessionID, dek, ttl)
}

func TestE2E_APIKey_CreateWithDecryptAccess_SecretsOperationSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &apiKeyAwareDB{
		users:   make(map[string]*types.User),
		apiKeys: make(map[string]*types.APIKey),
	}
	cache := &mockCache{}

	authSvc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New auth: %v", err)
	}

	keyStore := &memKeyStore{records: make(map[string]*secrets.UserKeyRecord)}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	secretStore := &memSecretStore{secrets: make(map[string]*secrets.UserSecret), bindings: make(map[string][]string)}
	secretSvc := secrets.NewSecretService(keySvc, secretStore)
	secretsHandler := handlers.NewSecretsHandler(secretSvc)

	masterKey := make([]byte, 32)
	rand.Read(masterKey)
	authSvc.SetMasterKey(masterKey)
	authSvc.SetKeyService(keySvc)

	router := gin.New()

	router.POST("/api/v1/auth/register", func(c *gin.Context) {
		var req types.RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		resp, err := authSvc.Register(c.Request.Context(), req)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, resp)
	})
	router.POST("/api/v1/auth/login", func(c *gin.Context) {
		var req types.LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		resp, err := authSvc.Login(c.Request.Context(), req)
		if err != nil {
			c.JSON(401, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, resp)
	})

	authed := router.Group("/api/v1")
	authed.Use(authSvc.AuthMiddleware())
	authed.POST("/api-keys", func(c *gin.Context) {
		var req types.CreateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		sid, _ := c.Get("sessionID")
		sidStr, _ := sid.(string)
		apiKey, err := authSvc.CreateAPIKey(c.Request.Context(), authSvc.GetUserID(c), req, sidStr, nil)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, apiKey)
	})
	authed.POST("/secrets", secretsHandler.CreateSecret)
	authed.GET("/secrets", secretsHandler.ListSecrets)

	srv := httptest.NewServer(router)
	defer srv.Close()

	base := srv.URL
	client := &http.Client{Timeout: 30 * time.Second}

	resp := doPost(t, client, base+"/api/v1/auth/register",
		`{"username":"e2euser","email":"e2e@test.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 201 {
		t.Fatalf("Register: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doPost(t, client, base+"/api/v1/auth/login",
		`{"email":"e2e@test.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("Login: %d", resp.StatusCode)
	}
	var loginResp struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&loginResp)
	resp.Body.Close()
	jwt := loginResp.Token

	resp = doPost(t, client, base+"/api/v1/api-keys",
		`{"name":"dek-key","decryptAccess":true}`, jwt)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Create API key with decrypt_access: %d: %s", resp.StatusCode, body)
	}
	var apiKeyResp struct {
		ID            string `json:"id"`
		Key           string `json:"key"`
		DecryptAccess bool   `json:"decryptAccess"`
	}
	json.NewDecoder(resp.Body).Decode(&apiKeyResp)
	resp.Body.Close()

	if !apiKeyResp.DecryptAccess {
		t.Fatal("Expected decryptAccess=true")
	}
	if apiKeyResp.Key == "" {
		t.Fatal("Expected raw key in response")
	}

	resp = doPost(t, client, base+"/api/v1/secrets",
		`{"name":"e2e-secret","type":"api-key","value":"sk-test-123","metadata":{"kind":"test","slug":"test"}}`, apiKeyResp.Key)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Create secret with API key: %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = doGet(t, client, base+"/api/v1/secrets", apiKeyResp.Key)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("List secrets with API key: %d: %s", resp.StatusCode, body)
	}
	var listResp struct{ Secrets []struct{ Name string } }
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if len(listResp.Secrets) != 1 || listResp.Secrets[0].Name != "e2e-secret" {
		t.Fatalf("Expected 1 secret 'e2e-secret', got %v", listResp.Secrets)
	}

	t.Log("E2E API Key DEK: register → login → create key with decrypt_access → create secret → list — PASSED")
}

type apiKeyAwareDB struct {
	users   map[string]*types.User
	apiKeys map[string]*types.APIKey
}

func (m *apiKeyAwareDB) GetUser(_ context.Context, id string) (*types.User, error) {
	u := m.users[id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (m *apiKeyAwareDB) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	for _, u := range m.users {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *apiKeyAwareDB) CreateUser(_ context.Context, u *types.User) error {
	cp := *u
	m.users[u.ID] = &cp
	return nil
}
func (m *apiKeyAwareDB) CountUsers(_ context.Context) (int, error) { return len(m.users), nil }
func (m *apiKeyAwareDB) UpdateUser(_ context.Context, userID string, updates types.UserUpdates) error {
	u, ok := m.users[userID]
	if !ok {
		return nil
	}
	if updates.EmailVerified != nil {
		u.EmailVerified = *updates.EmailVerified
	}
	return nil
}
func (m *apiKeyAwareDB) DeleteUser(context.Context, string) error { return nil }
func (m *apiKeyAwareDB) SetUserStatus(context.Context, string, types.UserStatus) error {
	return nil
}
func (m *apiKeyAwareDB) GetUserByAPIKey(_ context.Context, key string) (*types.User, error) {
	for _, k := range m.apiKeys {
		if k.Key == key && k.Active {
			return m.users[k.UserID], nil
		}
	}
	return nil, nil
}
func (m *apiKeyAwareDB) CreateAPIKey(_ context.Context, k *types.APIKey) error {
	cp := *k
	m.apiKeys[k.ID] = &cp
	return nil
}
func (m *apiKeyAwareDB) ListAPIKeys(context.Context, string) ([]*types.APIKey, error) {
	return nil, nil
}
func (m *apiKeyAwareDB) GetAPIKey(context.Context, string, string) (*types.APIKey, error) {
	return nil, nil
}
func (m *apiKeyAwareDB) DeleteAPIKey(context.Context, string, string) error { return nil }
func (m *apiKeyAwareDB) GetAPIKeyRecordByHash(_ context.Context, keyHash string) (*types.APIKey, error) {
	for _, k := range m.apiKeys {
		if k.Key == keyHash && k.Active {
			return k, nil
		}
	}
	return nil, nil
}
func (m *apiKeyAwareDB) UpdateAPIKeyDEK(_ context.Context, keyID string, wrappedDEK, kekSalt []byte, synced bool) error {
	for _, k := range m.apiKeys {
		if k.ID == keyID {
			k.WrappedDEK = wrappedDEK
			k.KekSalt = kekSalt
			k.DekSynced = synced
			break
		}
	}
	return nil
}
func (m *apiKeyAwareDB) ListAPIKeysWithDecrypt(_ context.Context, userID string) ([]*types.APIKey, error) {
	var keys []*types.APIKey
	for _, k := range m.apiKeys {
		if k.UserID == userID && k.DecryptAccess && k.Active {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
func (m *apiKeyAwareDB) GetWorkspace(context.Context, string) (*types.WorkspaceMetadata, error) {
	return nil, nil
}
func (m *apiKeyAwareDB) CreateWorkspace(context.Context, *types.WorkspaceMetadata) error {
	return nil
}
func (m *apiKeyAwareDB) UpdateWorkspace(context.Context, string, types.WorkspaceUpdates) error {
	return nil
}
func (m *apiKeyAwareDB) DeleteWorkspace(context.Context, string) error { return nil }
func (m *apiKeyAwareDB) ListWorkspaces(context.Context, string, int, int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	return nil, nil, nil
}
func (m *apiKeyAwareDB) CountWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *apiKeyAwareDB) CountActiveWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *apiKeyAwareDB) SyncWorkspaceVersionInfo(context.Context, string, string, string) {}
func (m *apiKeyAwareDB) MarkWorkspaceDeleted(context.Context, string)                     {}
func (m *apiKeyAwareDB) CheckPermission(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (m *apiKeyAwareDB) CheckResourceOwnership(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (m *apiKeyAwareDB) ListSessionIndex(context.Context, string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (m *apiKeyAwareDB) DeleteSessionIndex(context.Context, string) error { return nil }
func (m *apiKeyAwareDB) DeleteSessionTree(context.Context, string, string) error {
	return nil
}
func (m *apiKeyAwareDB) UpsertSessionMessage(context.Context, string, string, time.Time) error {
	return nil
}
func (m *apiKeyAwareDB) UpsertSessionTitle(context.Context, string, string, string) error {
	return nil
}
func (m *apiKeyAwareDB) UpsertSessionParent(context.Context, string, string, string) error {
	return nil
}
func (m *apiKeyAwareDB) UpsertSessionContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (m *apiKeyAwareDB) UpdateSessionLastSeen(_ context.Context, _, _ string) error { return nil }
func (m *apiKeyAwareDB) Ping(context.Context) error                                 { return nil }
func (m *apiKeyAwareDB) Start() error                                               { return nil }
func (m *apiKeyAwareDB) Stop() error                                                { return nil }
func (m *apiKeyAwareDB) ListAllWorkspaceOwners(context.Context) (map[string]string, error) {
	return nil, nil
}

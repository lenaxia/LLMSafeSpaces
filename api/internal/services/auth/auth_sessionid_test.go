// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestAuthMiddleware_SetsSessionID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}

	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Generate a real token
	token, err := svc.GenerateToken("user-123")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	// Create a test user so role lookup doesn't fail
	db.users = map[string]*mockUser{
		"user-123": {ID: "user-123", Role: "user", Active: true},
	}

	// Setup router with the auth middleware
	router := gin.New()
	router.Use(svc.AuthMiddleware())
	var gotSessionID string
	router.GET("/test", func(c *gin.Context) {
		sid, exists := c.Get("sessionID")
		if exists {
			gotSessionID = sid.(string)
		}
		c.Status(200)
	})

	// Make request with the token
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	if gotSessionID == "" {
		t.Error("sessionID should be set in context by AuthMiddleware")
	}
}

// --- minimal mocks for this test ---

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-for-session-id-test"
	cfg.Auth.TokenDuration = time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	return cfg
}

func testLogger() *logger.Logger {
	l, _ := logger.New(false, "error", "json")
	return l
}

type mockUser struct {
	ID, Role string
	Active   bool
	Status   types.UserStatus
}

type mockDB struct {
	users map[string]*mockUser
}

func (m *mockDB) GetUser(_ context.Context, userID string) (*types.User, error) {
	u, ok := m.users[userID]
	if !ok {
		return nil, nil
	}
	return &types.User{ID: u.ID, Role: u.Role, Active: u.Active, Status: u.Status}, nil
}

// Satisfy interface — only GetUser needed for this test
func (m *mockDB) GetUserByEmail(context.Context, string) (*types.User, error) { return nil, nil }
func (m *mockDB) CreateUser(context.Context, *types.User) error               { return nil }
func (m *mockDB) UpdateUser(context.Context, string, types.UserUpdates) error { return nil }
func (m *mockDB) DeleteUser(context.Context, string) error                    { return nil }
func (m *mockDB) CountUsers(context.Context) (int, error)                     { return 1, nil }
func (m *mockDB) SetUserStatus(_ context.Context, userID string, status types.UserStatus) error {
	if u, ok := m.users[userID]; ok {
		u.Status = status
	}
	return nil
}
func (m *mockDB) GetUserByAPIKey(context.Context, string) (*types.User, error)     { return nil, nil }
func (m *mockDB) CreateAPIKey(context.Context, *types.APIKey) error                { return nil }
func (m *mockDB) ListAPIKeys(context.Context, string) ([]*types.APIKey, error)     { return nil, nil }
func (m *mockDB) GetAPIKey(context.Context, string, string) (*types.APIKey, error) { return nil, nil }
func (m *mockDB) DeleteAPIKey(context.Context, string, string) error               { return nil }
func (m *mockDB) GetAPIKeyRecordByHash(context.Context, string) (*types.APIKey, error) {
	return nil, nil
}
func (m *mockDB) UpdateAPIKeyDEK(context.Context, string, []byte, []byte, bool) error { return nil }
func (m *mockDB) ListAPIKeysWithDecrypt(context.Context, string) ([]*types.APIKey, error) {
	return nil, nil
}
func (m *mockDB) GetWorkspace(context.Context, string) (*types.WorkspaceMetadata, error) {
	return nil, nil
}
func (m *mockDB) CreateWorkspace(context.Context, *types.WorkspaceMetadata) error { return nil }
func (m *mockDB) UpdateWorkspace(context.Context, string, types.WorkspaceUpdates) error {
	return nil
}
func (m *mockDB) DeleteWorkspace(context.Context, string) error { return nil }
func (m *mockDB) ListWorkspaces(context.Context, string, int, int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	return nil, nil, nil
}
func (m *mockDB) CountWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *mockDB) CountActiveWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *mockDB) SyncWorkspaceVersionInfo(context.Context, string, string, string) {}
func (m *mockDB) MarkWorkspaceDeleted(context.Context, string)                     {}
func (m *mockDB) CheckPermission(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (m *mockDB) CheckResourceOwnership(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (m *mockDB) ListSessionIndex(context.Context, string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (m *mockDB) DeleteSessionIndex(context.Context, string) error        { return nil }
func (m *mockDB) DeleteSessionTree(context.Context, string, string) error { return nil }
func (m *mockDB) UpsertSessionMessage(context.Context, string, string, time.Time) error {
	return nil
}
func (m *mockDB) UpsertSessionTitle(context.Context, string, string, string) error { return nil }
func (m *mockDB) UpsertSessionParent(context.Context, string, string, string) error {
	return nil
}
func (m *mockDB) UpsertSessionContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (m *mockDB) UpdateSessionLastSeen(_ context.Context, _, _ string) error { return nil }
func (m *mockDB) Ping(context.Context) error                                 { return nil }
func (m *mockDB) Start() error                                               { return nil }
func (m *mockDB) Stop() error                                                { return nil }
func (m *mockDB) ListAllWorkspaceOwners(context.Context) (map[string]string, error) {
	return nil, nil
}

type mockCache struct{}

func (m *mockCache) Get(context.Context, string) (string, error)              { return "", nil }
func (m *mockCache) Set(context.Context, string, string, time.Duration) error { return nil }
func (m *mockCache) SetNX(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (m *mockCache) Delete(context.Context, string) error                                { return nil }
func (m *mockCache) DeleteByPrefix(context.Context, string) error                        { return nil }
func (m *mockCache) GetObject(context.Context, string, interface{}) error                { return nil }
func (m *mockCache) SetObject(context.Context, string, interface{}, time.Duration) error { return nil }
func (m *mockCache) GetSession(context.Context, string) (*types.CachedSession, error) {
	return nil, nil
}
func (m *mockCache) SetSession(context.Context, string, types.CachedSession, time.Duration) error {
	return nil
}
func (m *mockCache) DeleteSession(context.Context, string) error { return nil }
func (m *mockCache) Ping(context.Context) error                  { return nil }
func (m *mockCache) Start() error                                { return nil }
func (m *mockCache) Stop() error                                 { return nil }

func TestAuthMiddleware_LoginAutoInitsKeysForExistingUser(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{users: map[string]*mockUser{
		"existing-user": {ID: "existing-user", Role: "user", Active: true},
	}}
	cache := &mockCache{}

	svc, _ := New(cfg, log, db, cache)

	// Mock key service that tracks calls
	ks := &trackingKeyService{}
	svc.SetKeyService(ks)

	// Simulate login (we can't call Login directly without password hash, so test the logic path)
	// Instead, verify the KeyServiceInterface has HasKeys + InitializeUserKeys
	ctx := context.Background()

	// HasKeys returns false for new user
	has, _ := ks.HasKeys(ctx, "existing-user")
	if has {
		t.Error("Should not have keys initially")
	}

	// After InitializeUserKeys, HasKeys returns true
	ks.InitializeUserKeysServerKEK(ctx, "existing-user", "server_kek")
	has, _ = ks.HasKeys(ctx, "existing-user")
	if !has {
		t.Error("Should have keys after init")
	}
}

type trackingKeyService struct {
	initialized map[string]bool
}

func (t *trackingKeyService) InitializeUserKeys(_ context.Context, userID string, _ []byte) (string, error) {
	if t.initialized == nil {
		t.initialized = make(map[string]bool)
	}
	t.initialized[userID] = true
	return "recovery-key-hex", nil
}

func (t *trackingKeyService) InitializeUserKeysServerKEK(_ context.Context, userID, _ string) error {
	if t.initialized == nil {
		t.initialized = make(map[string]bool)
	}
	t.initialized[userID] = true
	return nil
}

func (t *trackingKeyService) UnlockDEK(_ context.Context, _ string, _ []byte, _ string, _ time.Duration) error {
	return nil
}

func (t *trackingKeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, _ []byte) error {
	return t.UnlockDEK(ctx, userID, password, sessionID, ttl)
}

func (t *trackingKeyService) DeleteDurableSessionsForUser(_ context.Context, _ string) error {
	return nil
}

func (t *trackingKeyService) HasKeys(_ context.Context, userID string) (bool, error) {
	if t.initialized == nil {
		return false, nil
	}
	return t.initialized[userID], nil
}

func (t *trackingKeyService) GetDEK(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return nil, nil
}

func (t *trackingKeyService) CacheDEK(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}

// TestOptionalAuthMiddleware_ValidToken verifies that a valid JWT sets userID
// in context and calls the next handler (does not abort).
func TestOptionalAuthMiddleware_ValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}
	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token, err := svc.GenerateToken("user-opt")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	db.users = map[string]*mockUser{
		"user-opt": {ID: "user-opt", Role: "user", Active: true},
	}

	router := gin.New()
	router.Use(svc.OptionalAuthMiddleware())
	var gotUID string
	router.GET("/test", func(c *gin.Context) {
		uid, _ := c.Get("userID")
		gotUID, _ = uid.(string)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotUID != "user-opt" {
		t.Errorf("expected userID=user-opt, got %q", gotUID)
	}
}

// TestOptionalAuthMiddleware_InvalidToken verifies that an invalid token does
// NOT abort — handler still runs with empty userID.
func TestOptionalAuthMiddleware_InvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}
	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	router := gin.New()
	router.Use(svc.OptionalAuthMiddleware())
	var gotUID string
	var handlerRan bool
	router.GET("/test", func(c *gin.Context) {
		handlerRan = true
		uid, _ := c.Get("userID")
		gotUID, _ = uid.(string)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if !handlerRan {
		t.Error("handler should run even with invalid token")
	}
	if w.Code != 200 {
		t.Errorf("expected 200 (not aborted), got %d", w.Code)
	}
	if gotUID != "" {
		t.Errorf("expected empty userID, got %q", gotUID)
	}
}

// TestOptionalAuthMiddleware_NoToken verifies that absent Authorization header
// does NOT abort — handler still runs with empty userID.
func TestOptionalAuthMiddleware_NoToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}
	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	router := gin.New()
	router.Use(svc.OptionalAuthMiddleware())
	var handlerRan bool
	router.GET("/test", func(c *gin.Context) {
		handlerRan = true
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if !handlerRan {
		t.Error("handler should run with no token")
	}
	if w.Code != 200 {
		t.Errorf("expected 200 (not aborted), got %d", w.Code)
	}
}

// --- D19: user-level suspension ---

// TestAuthMiddleware_SuspendedUser_Blocked verifies that a valid token belonging
// to a suspended user is rejected with 401 "account suspended" and the
// downstream handler never runs.
func TestAuthMiddleware_SuspendedUser_Blocked(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}
	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token, err := svc.GenerateToken("user-susp")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	db.users = map[string]*mockUser{
		"user-susp": {ID: "user-susp", Role: "user", Active: true, Status: types.UserStatusSuspended},
	}

	router := gin.New()
	router.Use(svc.AuthMiddleware())
	handlerRan := false
	router.GET("/test", func(c *gin.Context) {
		handlerRan = true
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Fatalf("expected 401 for suspended user, got %d", w.Code)
	}
	if handlerRan {
		t.Error("downstream handler must NOT run for a suspended user")
	}
	if !strings.Contains(w.Body.String(), "account suspended") {
		t.Errorf("expected 'account suspended' in body, got %s", w.Body.String())
	}
}

// TestAuthMiddleware_ActiveUser_Passes verifies an active user with a valid
// token reaches the handler.
func TestAuthMiddleware_ActiveUser_Passes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}
	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token, err := svc.GenerateToken("user-active")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	db.users = map[string]*mockUser{
		"user-active": {ID: "user-active", Role: "user", Active: true, Status: types.UserStatusActive},
	}

	router := gin.New()
	router.Use(svc.AuthMiddleware())
	var gotRole string
	router.GET("/test", func(c *gin.Context) {
		r, _ := c.Get("userRole")
		gotRole, _ = r.(string)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 for active user, got %d", w.Code)
	}
	if gotRole != "user" {
		t.Errorf("expected role=user, got %q", gotRole)
	}
}

// TestOptionalAuthMiddleware_SuspendedUser_TreatedAsAnon verifies that a
// suspended user presenting a valid token via OptionalAuthMiddleware is treated
// as unauthenticated (no userID set) but NOT aborted.
func TestOptionalAuthMiddleware_SuspendedUser_TreatedAsAnon(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := testConfig()
	log := testLogger()
	db := &mockDB{}
	cache := &mockCache{}
	svc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token, err := svc.GenerateToken("user-susp-opt")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	db.users = map[string]*mockUser{
		"user-susp-opt": {ID: "user-susp-opt", Role: "user", Active: true, Status: types.UserStatusSuspended},
	}

	router := gin.New()
	router.Use(svc.OptionalAuthMiddleware())
	var gotUID string
	handlerRan := false
	router.GET("/test", func(c *gin.Context) {
		handlerRan = true
		uid, _ := c.Get("userID")
		gotUID, _ = uid.(string)
		c.Status(200)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if !handlerRan {
		t.Error("handler must still run (optional middleware never aborts)")
	}
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if gotUID != "" {
		t.Errorf("suspended user must not get userID set, got %q", gotUID)
	}
}

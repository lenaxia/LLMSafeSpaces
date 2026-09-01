// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestE2E_RealAuth_SecretCRUD proves the full flow works with the REAL auth
// middleware: register → login → create secret → list → delete.
// This is the test that would have caught BUG 2 (sessionID not set).
func TestE2E_RealAuth_SecretCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Setup real auth service
	cfg := testConfig()
	log := testLogger()
	db := &fullMockDB{users: make(map[string]*types.User)}
	cache := &mockCache{}

	authSvc, err := New(cfg, log, db, cache)
	if err != nil {
		t.Fatalf("New auth: %v", err)
	}

	// Setup real secrets service
	keyStore := &memKeyStore{records: make(map[string]*secrets.UserKeyRecord)}
	dekCache := &memDEKCache{store: make(map[string][]byte)}
	keySvc := secrets.NewKeyService(keyStore, dekCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	keySvc.SetAPIKeyStore(nil, testProv)
	secretStore := &memSecretStore{secrets: make(map[string]*secrets.UserSecret), bindings: make(map[string][]string)}
	secretSvc := secrets.NewSecretService(keySvc, secretStore)
	secretsHandler := handlers.NewSecretsHandler(secretSvc)

	// Wire key service into auth
	authSvc.SetKeyService(keySvc)

	// Build router with REAL auth middleware
	router := gin.New()
	authed := router.Group("/api/v1")
	authed.Use(authSvc.AuthMiddleware())
	authed.POST("/secrets", secretsHandler.CreateSecret)
	authed.GET("/secrets", secretsHandler.ListSecrets)
	authed.DELETE("/secrets/:id", secretsHandler.DeleteSecret)

	// Register endpoint (public)
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

	// Login endpoint (public)
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

	// Start real server
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: router}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	base := "http://" + ln.Addr().String()
	// Generous timeout: under -race the back-to-back register+login pair runs
	// two cost-12 bcrypts which can exceed a small budget on CI runners.
	client := &http.Client{Timeout: 30 * time.Second}

	// === Register ===
	resp := doPost(t, client, base+"/api/v1/auth/register",
		`{"username":"testuser","email":"test@example.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 201 {
		t.Fatalf("Register: %d", resp.StatusCode)
	}
	var regResp struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&regResp)
	resp.Body.Close()

	// === Login (to get fresh token with DEK unlocked) ===
	resp = doPost(t, client, base+"/api/v1/auth/login",
		`{"email":"test@example.com","password":"secure-password-123"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("Login: %d", resp.StatusCode)
	}
	var loginResp struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&loginResp)
	resp.Body.Close()
	token := loginResp.Token

	// === Create secret (THIS is what failed with BUG 2) ===
	resp = doPost(t, client, base+"/api/v1/secrets",
		`{"name":"my-key","type":"api-key","value":"sk-secret-123","metadata":{"kind":"openai","slug":"openai"}}`, token)
	if resp.StatusCode != 201 {
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		t.Fatalf("Create secret: expected 201, got %d: %s", resp.StatusCode, string(body[:n]))
	}
	var created struct{ ID, Name string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == "" {
		t.Fatal("Created secret has no ID")
	}

	// === List secrets ===
	resp = doGet(t, client, base+"/api/v1/secrets", token)
	if resp.StatusCode != 200 {
		t.Fatalf("List: %d", resp.StatusCode)
	}
	var listResp struct{ Secrets []struct{ ID string } }
	_ = json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if len(listResp.Secrets) != 1 {
		t.Errorf("Expected 1 secret, got %d", len(listResp.Secrets))
	}

	// === Delete ===
	req, _ := http.NewRequest("DELETE", base+"/api/v1/secrets/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = client.Do(req)
	if resp.StatusCode != 204 {
		t.Fatalf("Delete: %d", resp.StatusCode)
	}
	resp.Body.Close()

	t.Log("E2E with REAL auth middleware: register → login → create → list → delete — PASSED")
}

func doPost(t *testing.T, c *http.Client, url, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func doGet(t *testing.T, c *http.Client, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// --- Mocks that satisfy the full interfaces ---

type fullMockDB struct {
	users map[string]*types.User
}

func (m *fullMockDB) GetUser(_ context.Context, id string) (*types.User, error) {
	u := m.users[id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (m *fullMockDB) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	for _, u := range m.users {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *fullMockDB) CreateUser(_ context.Context, u *types.User) error {
	cp := *u
	m.users[u.ID] = &cp
	return nil
}
func (m *fullMockDB) CountUsers(_ context.Context) (int, error) { return len(m.users), nil }
func (m *fullMockDB) UpdateUser(_ context.Context, userID string, updates types.UserUpdates) error {
	u, ok := m.users[userID]
	if !ok {
		return nil
	}
	if updates.EmailVerified != nil {
		u.EmailVerified = *updates.EmailVerified
	}
	return nil
}
func (m *fullMockDB) DeleteUser(context.Context, string) error                      { return nil }
func (m *fullMockDB) SetUserStatus(context.Context, string, types.UserStatus) error { return nil }
func (m *fullMockDB) GetUserByAPIKey(context.Context, string) (*types.User, error)  { return nil, nil }
func (m *fullMockDB) CreateAPIKey(context.Context, *types.APIKey) error             { return nil }
func (m *fullMockDB) ListAPIKeys(context.Context, string) ([]*types.APIKey, error)  { return nil, nil }
func (m *fullMockDB) GetAPIKey(context.Context, string, string) (*types.APIKey, error) {
	return nil, nil
}
func (m *fullMockDB) DeleteAPIKey(context.Context, string, string) error { return nil }
func (m *fullMockDB) GetAPIKeyRecordByHash(context.Context, string) (*types.APIKey, error) {
	return nil, nil
}
func (m *fullMockDB) UpdateAPIKeyDEK(context.Context, string, []byte, []byte, bool) error {
	return nil
}
func (m *fullMockDB) ListAPIKeysWithDecrypt(context.Context, string) ([]*types.APIKey, error) {
	return nil, nil
}
func (m *fullMockDB) GetWorkspace(context.Context, string) (*types.WorkspaceMetadata, error) {
	return nil, nil
}
func (m *fullMockDB) CreateWorkspace(context.Context, *types.WorkspaceMetadata) error { return nil }
func (m *fullMockDB) UpdateWorkspace(context.Context, string, types.WorkspaceUpdates) error {
	return nil
}
func (m *fullMockDB) DeleteWorkspace(context.Context, string) error { return nil }
func (m *fullMockDB) ListWorkspaces(context.Context, string, int, int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	return nil, nil, nil
}
func (m *fullMockDB) CountWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *fullMockDB) CountActiveWorkspacesByUserAndOrg(context.Context, string, string) (int, error) {
	return 0, nil
}
func (m *fullMockDB) SyncWorkspaceVersionInfo(context.Context, string, string, string) {}
func (m *fullMockDB) MarkWorkspaceDeleted(context.Context, string)                     {}
func (m *fullMockDB) CheckPermission(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (m *fullMockDB) CheckResourceOwnership(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (m *fullMockDB) ListSessionIndex(context.Context, string) ([]types.SessionListItem, error) {
	return nil, nil
}

func (m *fullMockDB) InsertSessionAlert(context.Context, string, string, string, int) error {
	return nil
}
func (m *fullMockDB) ListSessionAlerts(context.Context, string, int) ([]types.SessionAlert, error) {
	return nil, nil
}
func (m *fullMockDB) DeleteSessionIndex(context.Context, string) error        { return nil }
func (m *fullMockDB) DeleteSessionTree(context.Context, string, string) error { return nil }
func (m *fullMockDB) UpsertSessionMessage(context.Context, string, string, time.Time) error {
	return nil
}
func (m *fullMockDB) UpsertSessionTitle(context.Context, string, string, string) error { return nil }
func (m *fullMockDB) UpsertSessionParent(context.Context, string, string, string) error {
	return nil
}
func (m *fullMockDB) UpsertSessionContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (m *fullMockDB) UpdateSessionLastSeen(_ context.Context, _, _ string) error { return nil }
func (m *fullMockDB) Ping(context.Context) error                                 { return nil }
func (m *fullMockDB) Start() error                                               { return nil }
func (m *fullMockDB) Stop() error                                                { return nil }
func (m *fullMockDB) ListAllWorkspaceOwners(context.Context) (map[string]string, error) {
	return nil, nil
}

// --- In-memory secrets mocks ---

type memKeyStore struct {
	records map[string]*secrets.UserKeyRecord
}

func (m *memKeyStore) GetUserKey(_ context.Context, id string) (*secrets.UserKeyRecord, error) {
	r := m.records[id]
	if r == nil {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}
func (m *memKeyStore) CreateUserKey(_ context.Context, r *secrets.UserKeyRecord) error {
	m.records[r.UserID] = r
	return nil
}
func (m *memKeyStore) UpdateWrappedDEK(_ context.Context, id string, dek, salt []byte, v int) error {
	r := m.records[id]
	if r != nil {
		r.WrappedDEK = dek
		r.Salt = salt
		r.KeyVersion = v
	}
	return nil
}

type memDEKCache struct {
	store map[string][]byte
}

func (m *memDEKCache) CacheDEK(_ context.Context, id string, dek []byte, _ time.Duration) error {
	cp := make([]byte, len(dek))
	copy(cp, dek)
	m.store[id] = cp
	return nil
}
func (m *memDEKCache) GetDEK(_ context.Context, id string) ([]byte, error) { return m.store[id], nil }
func (m *memDEKCache) EvictDEK(_ context.Context, id string) error         { delete(m.store, id); return nil }

type memSecretStore struct {
	secrets  map[string]*secrets.UserSecret
	bindings map[string][]string
	audit    []*secrets.AuditEntry
	counter  int
}

func (m *memSecretStore) CreateSecret(_ context.Context, s *secrets.UserSecret) error {
	for _, ex := range m.secrets {
		if ex.UserID == s.UserID && ex.Name == s.Name {
			return fmt.Errorf("%w: %s", secrets.ErrDuplicateSecret, s.Name)
		}
	}
	m.counter++
	s.ID = fmt.Sprintf("sec-%d", m.counter)
	cp := *s
	m.secrets[s.ID] = &cp
	return nil
}
func (m *memSecretStore) GetSecret(_ context.Context, uid, id string) (*secrets.UserSecret, error) {
	s := m.secrets[id]
	if s == nil || s.UserID != uid {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}
func (m *memSecretStore) GetSecretByName(_ context.Context, uid, name string) (*secrets.UserSecret, error) {
	for _, s := range m.secrets {
		if s.UserID == uid && s.Name == name {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *memSecretStore) ListSecrets(_ context.Context, uid string) ([]*secrets.UserSecret, error) {
	var r []*secrets.UserSecret
	for _, s := range m.secrets {
		if s.UserID == uid {
			cp := *s
			r = append(r, &cp)
		}
	}
	return r, nil
}
func (m *memSecretStore) ListGlobalDefaultSecrets(_ context.Context, uid string) ([]*secrets.UserSecret, error) {
	var r []*secrets.UserSecret
	for _, s := range m.secrets {
		if s.UserID == uid && s.GlobalDefault {
			cp := *s
			r = append(r, &cp)
		}
	}
	return r, nil
}
func (m *memSecretStore) UpdateSecret(_ context.Context, s *secrets.UserSecret) error {
	m.secrets[s.ID] = s
	return nil
}
func (m *memSecretStore) ReEncryptUserSecrets(ctx context.Context, userID string, newKeyVersion int, transform func([]byte) ([]byte, error), commit func(context.Context) error) error {
	updates := make(map[string][]byte)
	for id, s := range m.secrets {
		if s.UserID != userID {
			continue
		}
		newCT, err := transform(s.Ciphertext)
		if err != nil {
			return err
		}
		updates[id] = newCT
	}
	if commit != nil {
		if err := commit(ctx); err != nil {
			return err
		}
	}
	for id, newCT := range updates {
		m.secrets[id].Ciphertext = newCT
		m.secrets[id].KeyVersion = newKeyVersion
	}
	return nil
}
func (m *memSecretStore) DeleteSecret(_ context.Context, uid, id string) error {
	s := m.secrets[id]
	if s == nil || s.UserID != uid {
		return fmt.Errorf("%w: %s", secrets.ErrSecretNotFound, id)
	}
	delete(m.secrets, id)
	return nil
}
func (m *memSecretStore) SetBindings(_ context.Context, ws string, ids []string) error {
	m.bindings[ws] = ids
	return nil
}
func (m *memSecretStore) AddBindings(_ context.Context, ws string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	existing := m.bindings[ws]
	seen := make(map[string]struct{}, len(existing)+len(ids))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, id)
	}
	m.bindings[ws] = existing
	return nil
}
func (m *memSecretStore) GetBindings(_ context.Context, ws string) ([]*secrets.UserSecret, error) {
	sids := m.bindings[ws]
	var result []*secrets.UserSecret
	for _, sid := range sids {
		if s, ok := m.secrets[sid]; ok {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}
func (m *memSecretStore) GetBindingsForSecret(context.Context, string) ([]string, error) {
	return nil, nil
}
func (m *memSecretStore) LogAudit(_ context.Context, e *secrets.AuditEntry) error {
	m.audit = append(m.audit, e)
	return nil
}
func (m *memSecretStore) QueryAudit(_ context.Context, uid string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	return nil, nil
}

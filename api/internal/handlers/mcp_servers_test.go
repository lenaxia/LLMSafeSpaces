// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/stretchr/testify/assert"
)

// stubMCPStore is a test double for mcpServerStore.
type stubMCPStore struct {
	servers  []*secrets.MCPServerRow
	created  *secrets.MCPServerRow
	deleted  string
	count    int
	countErr error
	bindErr  error
}

func (s *stubMCPStore) CreateMCPServer(_ context.Context, row *secrets.MCPServerRow) error {
	s.created = row
	return nil
}
func (s *stubMCPStore) ListMCPServers(_ context.Context, _, _ string) ([]*secrets.MCPServerRow, error) {
	return s.servers, nil
}
func (s *stubMCPStore) GetMCPServer(_ context.Context, ownerType, ownerID, serverID string) (*secrets.MCPServerRow, error) {
	for _, r := range s.servers {
		if r.ID == serverID && r.OwnerType == ownerType && r.OwnerID == ownerID {
			return r, nil
		}
	}
	return nil, nil
}
func (s *stubMCPStore) UpdateMCPServer(_ context.Context, _, _, _ string, row *secrets.MCPServerRow) error {
	return nil
}
func (s *stubMCPStore) DeleteMCPServer(_ context.Context, ownerType, ownerID, serverID string) error {
	for _, r := range s.servers {
		if r.ID == serverID && r.OwnerType == ownerType && r.OwnerID == ownerID {
			s.deleted = serverID
			return nil
		}
	}
	return pgx.ErrNoRows
}
func (s *stubMCPStore) CountMCPServersByOwner(_ context.Context, _, _ string) (int, error) {
	return s.count, s.countErr
}
func (s *stubMCPStore) CountWorkspaceMCPServers(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (s *stubMCPStore) GetWorkspaceOrgIDForMCP(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (s *stubMCPStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]secrets.MCPServerBindingRow, error) {
	return nil, nil
}
func (s *stubMCPStore) BindMCPServerToWorkspace(_ context.Context, _, _ string) error {
	return s.bindErr
}
func (s *stubMCPStore) UnbindMCPServerFromWorkspace(_ context.Context, _, _ string) error { return nil }
func (s *stubMCPStore) CreateMCPServerAutoApply(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (s *stubMCPStore) DeleteMCPServerAutoApply(_ context.Context, _, _ string, _ *string) error {
	return nil
}
func (s *stubMCPStore) ListMCPServerAutoApply(_ context.Context, _ string) ([]secrets.MCPAutoApplyRule, error) {
	return nil, nil
}
func (s *stubMCPStore) BackfillMCPServerAutoApply(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (s *stubMCPStore) SeedWorkspaceMCPServers(_ context.Context, _, _ string, _ *string) error {
	return nil
}

// stubMcpOrgChecker for user-scope gate tests.
type stubMcpOrgChecker struct {
	orgID    string
	policies []*types.OrgPolicy
	userPlan string
}

func (m *stubMcpOrgChecker) GetUserOrgID(_ context.Context, _ string) (string, error) {
	return m.orgID, nil
}
func (m *stubMcpOrgChecker) GetOrgPolicies(_ context.Context, _ string) ([]*types.OrgPolicy, error) {
	return m.policies, nil
}
func (m *stubMcpOrgChecker) GetUserPlan(_ context.Context, _ string) (string, error) {
	if m.userPlan == "" {
		return "free", nil
	}
	return m.userPlan, nil
}

func init() { gin.SetMode(gin.TestMode) }

func TestValidateMCPServerCreate(t *testing.T) {
	cases := []struct {
		name    string
		req     *types.CreateMCPServerRequest
		wantErr string
	}{
		{"valid http", &types.CreateMCPServerRequest{Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp"}, ""},
		{"valid stdio", &types.CreateMCPServerRequest{Name: "github", Transport: "stdio", Command: "npx"}, ""},
		{"invalid name", &types.CreateMCPServerRequest{Name: "has space", Transport: "http", URL: "https://x.com"}, "invalid name"},
		{"invalid transport", &types.CreateMCPServerRequest{Name: "x", Transport: "ws"}, "invalid transport"},
		{"missing url for http", &types.CreateMCPServerRequest{Name: "x", Transport: "http"}, "url is required"},
		{"missing command for stdio", &types.CreateMCPServerRequest{Name: "x", Transport: "stdio"}, "command is required"},
		{"ssrf loopback", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://127.0.0.1/mcp"}, "blocked"},
		{"ssrf metadata", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://169.254.169.254/mcp"}, "blocked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateMCPServerCreate(c.req)
			if c.wantErr == "" {
				assert.NoError(t, err)
			} else {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("want error containing %q, got %v", c.wantErr, err)
				}
			}
		})
	}
}

func TestAdminList_Empty(t *testing.T) {
	store := &stubMCPStore{servers: nil}
	h := NewAdminMCPServersHandler(store, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	h.AdminList(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct{ Servers []json.RawMessage }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Empty(t, resp.Servers)
}

func TestUserCreate_OrgMemberBlocked(t *testing.T) {
	store := &stubMCPStore{}
	oc := &stubMcpOrgChecker{orgID: "org-123"} // user IS in an org
	h := NewUserMCPServersHandler(store, oc, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Set("userID", "user-1")
	h.UserCreate(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserCreate_OrgMemberAllowedWhenPolicyTrue(t *testing.T) {
	store := &stubMCPStore{}
	allowed := true
	policyJSON, _ := json.Marshal(allowed)
	oc := &stubMcpOrgChecker{
		orgID:    "org-123",
		policies: []*types.OrgPolicy{{Key: types.PolicyAllowUserMcpServers, Value: policyJSON}},
		userPlan: "free",
	}
	h := NewUserMCPServersHandler(store, oc, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UserCreate(c)

	// Should get 403 (no session DEK) — but a DIFFERENT 403 than the
	// org-policy one. The org policy check passed; we now reach the DEK
	// gate, which requires a password-authenticated session.
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "password-authenticated session")
}

func TestUserCreate_QuotaExceeded(t *testing.T) {
	store := &stubMCPStore{count: 5}
	oc := &stubMcpOrgChecker{orgID: "", userPlan: "free"} // solo user
	h := NewUserMCPServersHandler(store, oc, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UserCreate(c)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestUserCreate_SoloUnlimitedPlan(t *testing.T) {
	store := &stubMCPStore{count: 100}
	oc := &stubMcpOrgChecker{orgID: "", userPlan: "enterprise"}
	h := NewUserMCPServersHandler(store, oc, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UserCreate(c)

	// Enterprise passes quota; reaches DEK gate (no session → 403)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "password-authenticated session")
}

// --- Bind/Unbind ownership tests ---

func TestBind_RejectsForeignServer(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-org-1", OwnerType: "org", OwnerID: "org-999", Name: "foreign", Transport: "http", Enabled: true},
		},
	}
	h := NewUserMCPServersHandler(store, &stubMcpOrgChecker{}, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Params = gin.Params{{Key: "id", Value: "srv-org-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"workspaceId":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Bind(c)

	// verifyServerOwnership checks (ownerType="user", ownerID="user-1") →
	// server "srv-org-1" belongs to org-999, not user-1 → 404.
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestBind_AllowsOwnedServer(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-user-1", OwnerType: "user", OwnerID: "user-1", Name: "mine", Transport: "http", Enabled: true},
		},
	}
	h := NewUserMCPServersHandler(store, &stubMcpOrgChecker{}, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Params = gin.Params{{Key: "id", Value: "srv-user-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"workspaceId":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Bind(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "bound")
}

func TestUnbind_RejectsForeignServer(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-org-1", OwnerType: "org", OwnerID: "org-999", Name: "foreign", Transport: "http", Enabled: true},
		},
	}
	h := NewUserMCPServersHandler(store, &stubMcpOrgChecker{}, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Params = gin.Params{
		{Key: "id", Value: "srv-org-1"},
		{Key: "workspaceId", Value: "ws-1"},
	}
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	h.Unbind(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Admin create happy-path ---

func TestAdminCreate_HappyPath(t *testing.T) {
	store := &stubMCPStore{}
	// Use a test encryptor that just base64-encodes (not real crypto).
	enc := &stubEncryptor{}
	h := NewAdminMCPServersHandler(store, enc)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "admin-1")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp","headers":{"Authorization":"Bearer tok"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.AdminCreate(c)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.NotNil(t, store.created)
	assert.Equal(t, "wiki", store.created.Name)
	assert.True(t, store.created.Enabled)
}

// --- Admin delete ---

func TestAdminDelete_NotFound(t *testing.T) {
	store := &stubMCPStore{}
	h := NewAdminMCPServersHandler(store, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "nonexistent"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	h.AdminDelete(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminDelete_Success(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-1", OwnerType: "admin", OwnerID: "_platform", Name: "wiki", Transport: "http", Enabled: true},
		},
	}
	h := NewAdminMCPServersHandler(store, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "srv-1"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	h.AdminDelete(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "srv-1", store.deleted)
}

// --- Kill-switch ---

func TestOrgCreate_KillSwitchDisabled(t *testing.T) {
	store := &stubMCPStore{}
	enc := &stubEncryptor{}
	h := NewOrgMCPServersHandler(store, enc, &stubMcpOrgChecker{})
	h.SetSettings(&stubSettings{allowOrgAdmin: false})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "org-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://x.com"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.OrgCreate(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "disabled")
}

func TestOrgCreate_KillSwitchEnabled(t *testing.T) {
	store := &stubMCPStore{}
	enc := &stubEncryptor{}
	h := NewOrgMCPServersHandler(store, enc, &stubMcpOrgChecker{})
	h.SetSettings(&stubSettings{allowOrgAdmin: true})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "org-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://x.com"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.OrgCreate(c)

	assert.Equal(t, http.StatusCreated, w.Code)
}

// --- Test helpers ---

type stubEncryptor struct{}

func (s *stubEncryptor) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	return append([]byte("enc:"), plaintext...), nil
}
func (s *stubEncryptor) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) > 4 && string(ciphertext[:4]) == "enc:" {
		return ciphertext[4:], nil
	}
	return ciphertext, nil
}

type stubSettings struct{ allowOrgAdmin bool }

func (s *stubSettings) GetBool(_ context.Context, _ string) (bool, error) {
	return s.allowOrgAdmin, nil
}

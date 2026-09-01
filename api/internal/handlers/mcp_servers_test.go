// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// stubMCPStore is a test double for mcpServerStore.
type stubMCPStore struct {
	servers  []*secrets.MCPServerRow
	created  *secrets.MCPServerRow
	deleted  string
	count    int
	countErr error
	bindErr  error
	// wsOrgID controls the value returned by GetWorkspaceOrgIDForMCP.
	// Empty (default) means "personal workspace" → resolveWorkspaceQuota
	// early-returns. Set non-empty to exercise the org-policy quota path.
	wsOrgID string
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
func (s *stubMCPStore) UpdateMCPServer(_ context.Context, ownerType, ownerID, _ string, row *secrets.MCPServerRow) error {
	for i, r := range s.servers {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			if row.Ciphertext != nil {
				s.servers[i].Ciphertext = row.Ciphertext
			}
			s.servers[i].Name = row.Name
			s.servers[i].Enabled = row.Enabled
			return nil
		}
	}
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
	return s.wsOrgID, nil
}
func (s *stubMCPStore) GetWorkspaceUserIDForMCP(_ context.Context, _ string) (string, error) {
	return "user-1", nil
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
		{"ssrf loopback", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://127.0.0.1/mcp"}, "private"},
		{"ssrf metadata", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://169.254.169.254/mcp"}, "private"},
		{"ssrf rfc1918", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://10.0.0.1/mcp"}, "private"},
		{"ssrf rfc1918-2", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://192.168.1.1/mcp"}, "private"},
		{"ssrf rfc1918-3", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://172.16.0.1/mcp"}, "private"},
		{"ssrf unspecified", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://0.0.0.0/mcp"}, "private"},
		{"ssrf localhost", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "http://localhost/mcp"}, "internal"},
		{"env injection LD_PRELOAD", &types.CreateMCPServerRequest{Name: "x", Transport: "stdio", Command: "sh", Env: map[string]string{"LD_PRELOAD": "/tmp/evil.so"}}, "blocked"},
		{"env injection empty", &types.CreateMCPServerRequest{Name: "x", Transport: "stdio", Command: "sh", Env: map[string]string{"": "val"}}, "empty"},
		{"header crlf injection", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "https://x.com", Headers: map[string]string{"X-Inject\r\nEvil": "val"}}, "CR/LF"},
		{"valid env+headers", &types.CreateMCPServerRequest{Name: "x", Transport: "http", URL: "https://x.com", Env: map[string]string{"GITHUB_TOKEN": "tok"}, Headers: map[string]string{"Authorization": "Bearer tok"}}, ""},
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

func TestOrgUpdate_KillSwitchDisabled(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-1", OwnerType: "org", OwnerID: "org-1", Name: "wiki", Transport: "http", Enabled: true},
		},
	}
	enc := &stubEncryptor{}
	h := NewOrgMCPServersHandler(store, enc, &stubMcpOrgChecker{})
	h.SetSettings(&stubSettings{allowOrgAdmin: false})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "org-1"}, {Key: "serverId", Value: "srv-1"}}
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"enabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.OrgUpdate(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "disabled")
}

func TestOrgDelete_KillSwitchDisabled(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-1", OwnerType: "org", OwnerID: "org-1", Name: "wiki", Transport: "http", Enabled: true},
		},
	}
	h := NewOrgMCPServersHandler(store, &stubEncryptor{}, &stubMcpOrgChecker{})
	h.SetSettings(&stubSettings{allowOrgAdmin: false})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "org-1"}, {Key: "serverId", Value: "srv-1"}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	h.OrgDelete(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "disabled")
}

func TestOrgUpdate_KillSwitchFailClosed(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-1", OwnerType: "org", OwnerID: "org-1", Name: "wiki", Transport: "http", Enabled: true},
		},
	}
	h := NewOrgMCPServersHandler(store, &stubEncryptor{}, &stubMcpOrgChecker{})
	h.SetSettings(&stubSettings{readErr: true})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "org-1"}, {Key: "serverId", Value: "srv-1"}}
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"enabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.OrgUpdate(c)

	// Fail-closed: read error → 403 (not 200/500)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestValidateMCPServerUpdate_SSRFReject(t *testing.T) {
	ssrfURL := "http://169.254.169.254/latest/meta-data/"
	req := &types.UpdateMCPServerRequest{URL: &ssrfURL}
	err := validateMCPServerUpdate(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "private")
}

func TestValidateMCPServerUpdate_EnvInjection(t *testing.T) {
	env := map[string]string{"LD_PRELOAD": "/tmp/evil.so"}
	req := &types.UpdateMCPServerRequest{Env: &env}
	err := validateMCPServerUpdate(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "blocked")
}

// --- Update handler test (the decryptExisting path) ---

func TestAdminUpdate_PartialHeadersPreservesEnv(t *testing.T) {
	// Pre-existing server with both env and headers.
	existingPayload, _ := types.DecodeMCPServerSecretPayload([]byte(`{"env":{"TOKEN":"secret123"},"headers":{"X-Old":"old-val"}}`))
	plaintext, _ := existingPayload.Encode()
	enc := &stubEncryptor{}
	ciphertext, _ := enc.Encrypt(context.Background(), plaintext)

	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{
				ID: "srv-1", OwnerType: "admin", OwnerID: "_platform",
				Name: "wiki", Transport: "http", URL: "https://wiki.com",
				Ciphertext: ciphertext, Enabled: true,
			},
		},
	}
	h := NewAdminMCPServersHandler(store, enc)

	// PUT only headers — env must be preserved.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "srv-1"}}
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"headers":{"Authorization":"Bearer new-tok"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.AdminUpdate(c)

	assert.Equal(t, http.StatusOK, w.Code, "update should succeed: %s", w.Body.String())

	// Verify the stored ciphertext now has the NEW headers but PRESERVED env.
	updated := store.servers[0]
	decrypted, err := enc.Decrypt(context.Background(), updated.Ciphertext)
	assert.NoError(t, err)
	payload, err := types.DecodeMCPServerSecretPayload(decrypted)
	assert.NoError(t, err)
	assert.Equal(t, "secret123", payload.Env["TOKEN"], "existing env must be preserved")
	assert.Equal(t, "Bearer new-tok", payload.Headers["Authorization"], "new headers must be applied")
	assert.Empty(t, payload.Headers["X-Old"], "old headers should be replaced")
}

func TestAdminUpdate_EnableToggle(t *testing.T) {
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-1", OwnerType: "admin", OwnerID: "_platform", Name: "wiki", Transport: "http", Enabled: true},
		},
	}
	h := NewAdminMCPServersHandler(store, &stubEncryptor{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "srv-1"}}
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"enabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.AdminUpdate(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"enabled":false`)
}

func TestAdminUpdate_NotFound(t *testing.T) {
	store := &stubMCPStore{}
	h := NewAdminMCPServersHandler(store, &stubEncryptor{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "nonexistent"}}
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"enabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.AdminUpdate(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestOrgCreate_KillSwitchNilSettings_FailClosed(t *testing.T) {
	store := &stubMCPStore{}
	enc := &stubEncryptor{}
	h := NewOrgMCPServersHandler(store, enc, &stubMcpOrgChecker{})
	// Deliberately do NOT call SetSettings — simulates misconfigured deployment.
	// orgAdminAllowed should fail-closed (return false), blocking the create.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "org-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://x.com"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.OrgCreate(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "disabled")
}

// --- Regression: nil orgChecker must not panic (production 500 fix) ---

func TestUserCreate_NilOrgChecker_DoesNotPanic(t *testing.T) {
	// Reproduces the production bug: handler constructed with nil orgChecker
	// (init ordering in app.go). Pre-fix this panicked (nil deref → 500).
	// Post-fix it returns 503 with a clear error.
	store := &stubMCPStore{}
	// Deliberately pass nil for orgChecker, keys, keyStore.
	h := NewUserMCPServersHandler(store, nil, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Set("userPlan", "free")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	// Must not panic — should return 503.
	assert.NotPanics(t, func() {
		h.UserCreate(c)
	})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestUserCreate_DeferredOrgChecker_Works(t *testing.T) {
	// Verifies the deferred-wiring mechanism: construct with nil, then
	// SetOrgChecker before serving, and the handler works correctly.
	store := &stubMCPStore{}
	h := NewUserMCPServersHandler(store, nil, nil, nil)

	// Deferred wiring (as app.go does after pgOrgStore is created).
	oc := &stubMcpOrgChecker{orgID: "org-123"} // user is in an org
	h.SetOrgChecker(oc)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Set("userPlan", "free")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"wiki","transport":"http","url":"https://wiki.example.com/mcp"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UserCreate(c)

	// Org member with no policy → 403 (not 503/500/panic).
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "disabled")
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

type stubSettings struct {
	allowOrgAdmin bool
	readErr       bool
}

func (s *stubSettings) GetBool(_ context.Context, _ string) (bool, error) {
	if s.readErr {
		return false, fmt.Errorf("simulated read error")
	}
	return s.allowOrgAdmin, nil
}

// stubMcpAuditLogger records audit calls so tests can assert events reach
// the logger after the deferred SetAudit wiring.
type stubMcpAuditLogger struct {
	auditCalls int
	orgCalls   int
	lastAction string
	lastDomain string
	lastTarget string
}

func (s *stubMcpAuditLogger) LogAuditEvent(_ context.Context, domain, _, action, targetID string, _ *string, _ map[string]any) error {
	s.auditCalls++
	s.lastDomain = domain
	s.lastAction = action
	s.lastTarget = targetID
	return nil
}
func (s *stubMcpAuditLogger) LogOrgEvent(_ context.Context, _, _, action, targetID string, _ map[string]any) error {
	s.orgCalls++
	s.lastAction = action
	s.lastTarget = targetID
	return nil
}

// --- Regression: admin handler audit events must reach the logger after
// deferred SetAudit wiring (PR #622 fix — adminMcpHandler.SetAudit was
// nil-wired, silently dropping all platform-admin MCP audit events). ---

func TestAdminCreate_DeferredAudit_LogsEvent(t *testing.T) {
	// Construct the admin handler (no audit at construction time — mirrors
	// app.go which creates pgOrgStore AFTER the handler).
	store := &stubMCPStore{}
	enc := &stubEncryptor{}
	h := NewAdminMCPServersHandler(store, enc)

	// Deferred wiring: SetAudit called after pgOrgStore is created.
	audit := &stubMcpAuditLogger{}
	h.SetAudit(audit)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"platform-tools","transport":"http","url":"https://mcp.example.com/sse"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.AdminCreate(c)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, 1, audit.auditCalls, "admin create must emit exactly one audit event after deferred SetAudit")
	assert.Equal(t, "mcp_server.create", audit.lastAction)
	assert.Equal(t, "admin", audit.lastDomain)
}

// --- Regression: resolveWorkspaceQuota must not panic when orgChecker is
// nil (same nil-orgChecker bug class as UserCreate — PR #622 review
// deferred this guard; it's now added). The path is reachable via Bind on
// an org-owned workspace when orgChecker is nil (init-ordering window).
// Fail-safe: return the default quota instead of dereferencing nil. ---

func TestBind_NilOrgChecker_OrgOwnedWorkspace_DoesNotPanic(t *testing.T) {
	// Store returns a non-empty orgID → resolveWorkspaceQuota does NOT
	// early-return; it reaches the orgChecker.GetOrgPolicies call.
	store := &stubMCPStore{
		servers: []*secrets.MCPServerRow{
			{ID: "srv-1", OwnerType: types.MCPServerOwnerUser, OwnerID: "user-1"},
		},
		wsOrgID: "org-999", // org-owned workspace → exercises the quota path
	}
	// Deliberately nil orgChecker (the init-ordering bug condition).
	h := NewUserMCPServersHandler(store, nil, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	// serverID is read from c.Param("serverId") (or "id"). Without this the
	// ownership check 404s before reaching resolveWorkspaceQuota — the test
	// would pass identically with or without the nil-guard.
	c.Params = gin.Params{{Key: "serverId", Value: "srv-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"serverId":"srv-1","workspaceId":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	// Pre-fix: h.orgChecker.GetOrgPolicies on nil → panic → 500.
	// Post-fix: nil-guard returns default quota → bind proceeds.
	assert.NotPanics(t, func() {
		h.Bind(c)
	})
	// Bind should succeed (quota not exceeded — stub count is 0, default
	// quota is > 0), not crash.
	assert.NotEqual(t, http.StatusInternalServerError, w.Code)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type mockOrgStore struct {
	mu                    sync.Mutex
	orgs                  map[string]*types.Organization
	members               map[string][]*types.OrgMember
	billingAccounts       map[string]string
	listOrgsForUserResult []*types.OrgResponse
	listOrgsForUserErr    error
	createErr             error
	slugExists            bool
	usersByEmail          map[string]string
	userByEmailErr        error
	updateStatusErr       error
	userOrgID             map[string]string
	userOrgIDErr          error
	markVerifiedCalls     []string
	markVerifiedErr       error
	auditEvents           []mockAuditEvent
	auditErr              error
	getMemberErr          error
}

type mockAuditEvent struct {
	OrgID, ActorID, Action, TargetID string
	Metadata                         map[string]any
}

func newMockOrgStore() *mockOrgStore {
	return &mockOrgStore{
		orgs:            make(map[string]*types.Organization),
		members:         make(map[string][]*types.OrgMember),
		billingAccounts: make(map[string]string),
		usersByEmail:    make(map[string]string),
		userOrgID:       make(map[string]string),
	}
}

func (m *mockOrgStore) CreateOrgWithAdmin(_ context.Context, org *types.Organization, adminUserID string) (*types.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return nil, m.createErr
	}
	cp := *org
	if cp.Status == "" {
		cp.Status = types.OrgStatusPendingActivation
	}
	if cp.PlanID == "" {
		cp.PlanID = types.PlanFree
	}
	if cp.SubscriptionStatus == "" {
		cp.SubscriptionStatus = types.SubscriptionInactive
	}
	m.orgs[org.ID] = &cp
	m.members[org.ID] = []*types.OrgMember{
		{OrgID: org.ID, UserID: adminUserID, Role: types.OrgRoleAdmin},
	}
	return &cp, nil
}

func (m *mockOrgStore) GetOrg(_ context.Context, orgID string) (*types.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if org, ok := m.orgs[orgID]; ok {
		cp := *org
		return &cp, nil
	}
	return nil, nil
}

func (m *mockOrgStore) GetOrgBySlug(_ context.Context, slug string) (*types.Organization, error) {
	if m.slugExists {
		return &types.Organization{ID: "existing", Slug: slug}, nil
	}
	return nil, nil
}

func (m *mockOrgStore) ListOrgsForUser(_ context.Context, _ string) ([]*types.OrgResponse, error) {
	return m.listOrgsForUserResult, m.listOrgsForUserErr
}

func (m *mockOrgStore) UpdateOrg(_ context.Context, orgID string, req types.UpdateOrgRequest) (*types.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	org, ok := m.orgs[orgID]
	if !ok {
		return nil, nil
	}
	if req.Name != "" {
		org.Name = req.Name
	}
	if req.Slug != "" {
		org.Slug = req.Slug
	}
	cp := *org
	return &cp, nil
}

func (m *mockOrgStore) SoftDeleteOrg(_ context.Context, orgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.orgs, orgID)
	return nil
}

func (m *mockOrgStore) IsOrgMember(_ context.Context, orgID, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mem := range m.members[orgID] {
		if mem.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockOrgStore) IsOrgAdmin(_ context.Context, orgID, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mem := range m.members[orgID] {
		if mem.UserID == userID && mem.Role == types.OrgRoleAdmin {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockOrgStore) GetOrgMember(_ context.Context, orgID, userID string) (*types.OrgMember, error) {
	if m.getMemberErr != nil {
		return nil, m.getMemberErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mem := range m.members[orgID] {
		if mem.UserID == userID {
			cp := *mem
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockOrgStore) ListOrgMembers(_ context.Context, orgID string) ([]*types.OrgMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.members[orgID], nil
}

func (m *mockOrgStore) AddOrgMember(_ context.Context, orgID, userID string, role types.OrgRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.members[orgID] = append(m.members[orgID], &types.OrgMember{
		OrgID: orgID, UserID: userID, Role: role,
	})
	return nil
}

func (m *mockOrgStore) RemoveOrgMember(_ context.Context, orgID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	members := m.members[orgID]
	for i, mem := range members {
		if mem.UserID == userID {
			m.members[orgID] = append(members[:i], members[i+1:]...)
			break
		}
	}
	return nil
}

func (m *mockOrgStore) RemoveOrgAdminIfNotLast(_ context.Context, orgID, targetUserID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	adminCount := 0
	for _, mem := range m.members[orgID] {
		if mem.Role == types.OrgRoleAdmin {
			adminCount++
		}
	}
	if adminCount <= 1 {
		return false, nil
	}
	members := m.members[orgID]
	for i, mem := range members {
		if mem.UserID == targetUserID {
			m.members[orgID] = append(members[:i], members[i+1:]...)
			break
		}
	}
	return true, nil
}

func (m *mockOrgStore) DemoteOrgAdminIfNotLast(_ context.Context, orgID, targetUserID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	adminCount := 0
	for _, mem := range m.members[orgID] {
		if mem.Role == types.OrgRoleAdmin {
			adminCount++
		}
	}
	if adminCount <= 1 {
		return false, nil
	}
	for _, mem := range m.members[orgID] {
		if mem.UserID == targetUserID {
			mem.Role = types.OrgRoleMember
			break
		}
	}
	return true, nil
}

func (m *mockOrgStore) UpdateOrgMemberRole(_ context.Context, orgID, userID string, role types.OrgRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mem := range m.members[orgID] {
		if mem.UserID == userID {
			mem.Role = role
			return nil
		}
	}
	return nil
}

func (m *mockOrgStore) ListOrgWorkspaces(_ context.Context, _ string, _, _ int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	return []*types.WorkspaceMetadata{}, &types.PaginationMetadata{Total: 0}, nil
}

func (m *mockOrgStore) GetUserIDByEmail(_ context.Context, email string) (string, error) {
	if m.userByEmailErr != nil {
		return "", m.userByEmailErr
	}
	return m.usersByEmail[email], nil
}

func (m *mockOrgStore) GetUserOrgID(_ context.Context, userID string) (string, error) {
	if m.userOrgIDErr != nil {
		return "", m.userOrgIDErr
	}
	return m.userOrgID[userID], nil
}

func (m *mockOrgStore) GetStripeCustomerID(_ context.Context, orgID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.billingAccounts[orgID], nil
}

func (m *mockOrgStore) UpdateOrgStatus(_ context.Context, orgID string, status *types.OrgStatus, sub *types.OrgSubscriptionStatus, plan *types.OrgPlan) error {
	if m.updateStatusErr != nil {
		return m.updateStatusErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if org, ok := m.orgs[orgID]; ok {
		if status != nil {
			org.Status = *status
		}
		if sub != nil {
			org.SubscriptionStatus = *sub
		}
		if plan != nil {
			org.PlanID = *plan
		}
	}
	return nil
}

func (m *mockOrgStore) MarkUserEmailVerified(_ context.Context, userID string) error {
	if m.markVerifiedErr != nil {
		return m.markVerifiedErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markVerifiedCalls = append(m.markVerifiedCalls, userID)
	for _, members := range m.members {
		for _, mem := range members {
			if mem.UserID == userID {
				mem.EmailVerified = true
			}
		}
	}
	return nil
}

func (m *mockOrgStore) LogOrgEvent(_ context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error {
	if m.auditErr != nil {
		return m.auditErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditEvents = append(m.auditEvents, mockAuditEvent{
		OrgID: orgID, ActorID: actorID, Action: action, TargetID: targetID, Metadata: metadata,
	})
	return nil
}

type mockOrgAuthService struct{ userID string }

func (m *mockOrgAuthService) GetUserID(_ *gin.Context) string { return m.userID }

func setupOrgTestRouter(t *testing.T, store *mockOrgStore) (*gin.Engine, *OrgsHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	handler := NewOrgsHandler(store, &mockOrgAuthService{userID: "admin-1"})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "admin-1")
		c.Set("userRole", "admin")
		c.Next()
	})

	orgGroup := router.Group("/api/v1/orgs")
	orgGroup.POST("", handler.Create)
	orgGroup.GET("", handler.List)
	orgGroup.GET("/:id", handler.Get)
	orgGroup.PUT("/:id", handler.Update)
	orgGroup.DELETE("/:id", handler.Delete)
	orgGroup.GET("/:id/workspaces", handler.ListWorkspaces)
	orgGroup.GET("/:id/members", handler.ListMembers)
	orgGroup.POST("/:id/members", handler.AddMember)
	orgGroup.DELETE("/:id/members/:userID", handler.RemoveMember)
	orgGroup.PUT("/:id/members/:userID", handler.ChangeMemberRole)
	orgGroup.POST("/:id/members/:userID/verify", handler.VerifyMember)

	return router, handler
}

func doRequest(router *gin.Engine, method, path string, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// --- Tests ---

func TestOrgsHandler_RemoveMember_LastAdmin(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "admin-2", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "DELETE", "/api/v1/orgs/org-1/members/admin-2", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_RemoveMember_LastAdminBlocked(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "other", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "DELETE", "/api/v1/orgs/org-1/members/other", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("with 2 admins, removing one should succeed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_RemoveMember_SelfRemovalBlocked(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "admin-2", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "DELETE", "/api/v1/orgs/org-1/members/admin-1", "")
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for self-removal, got %d", w.Code)
	}
}

func TestOrgsHandler_RemoveMember_NotFound(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "DELETE", "/api/v1/orgs/org-1/members/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestOrgsHandler_ChangeMemberRole_PromoteToAdmin(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "PUT", "/api/v1/orgs/org-1/members/member-1", `{"role":"admin"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	member, _ := store.GetOrgMember(context.Background(), "org-1", "member-1")
	if member == nil {
		t.Fatal("member not found")
	}
	if member.Role != types.OrgRoleAdmin {
		t.Errorf("expected role=admin, got %q", member.Role)
	}
}

func TestOrgsHandler_ChangeMemberRole_DemoteLastAdminBlocked(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "PUT", "/api/v1/orgs/org-1/members/member-1", `{"role":"member"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for same role, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_ChangeMemberRole_DemoteSelfBlocked(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "admin-2", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "PUT", "/api/v1/orgs/org-1/members/admin-1", `{"role":"member"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for self-demotion, got %d", w.Code)
	}
}

func TestOrgsHandler_Delete_SucceedsWithWorkspaces(t *testing.T) {
	// S12: with always-org-attributed workspaces (D4), deletion succeeds even
	// when the org has active workspaces — they become frozen, not converted to
	// personal. The old OrgHasActiveWorkspaces guard is removed.
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "DELETE", "/api/v1/orgs/org-1", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 (deletion succeeds), got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_List_Success(t *testing.T) {
	store := newMockOrgStore()
	store.listOrgsForUserResult = []*types.OrgResponse{
		{Organization: types.Organization{ID: "org-1", Name: "Test"}, UserRole: types.OrgRoleAdmin, MemberCount: 3},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "GET", "/api/v1/orgs", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var orgs []*types.OrgResponse
	json.Unmarshal(w.Body.Bytes(), &orgs)
	if len(orgs) != 1 || orgs[0].Name != "Test" {
		t.Errorf("unexpected response: %s", w.Body.String())
	}
}

func TestOrgsHandler_AddMember_Success(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members", `{"userId":"new-user","role":"member"}`)
	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_AddMember_AdminRole(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members", `{"userId":"new-admin","role":"admin"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	member, _ := store.GetOrgMember(context.Background(), "org-1", "new-admin")
	if member == nil {
		t.Fatal("new admin member not found")
	}
	if member.Role != types.OrgRoleAdmin {
		t.Errorf("expected role=admin, got %q", member.Role)
	}
}

func TestOrgsHandler_AddMember_AlreadyInAnotherOrg_Conflict(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	store.userOrgID["taken-user"] = "org-2"
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members", `{"userId":"taken-user","role":"member"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for user already in another org, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "another organization") {
		t.Errorf("expected 'another organization' message, got: %s", w.Body.String())
	}
}

func TestOrgsHandler_AddMember_GetUserOrgIDError_500(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	store.userOrgIDErr = errors.New("db down")
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members", `{"userId":"new-user","role":"member"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on GetUserOrgID error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_ListWorkspaces_LimitCapped(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "GET", "/api/v1/orgs/org-1/workspaces?limit=999999", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// --- VerifyMember (admin force-verify, bypassing email validation) ---

func TestOrgsHandler_VerifyMember_Success(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin, Email: "admin@example.com"},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember, Email: "member@example.com", EmailVerified: false},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members/member-1/verify", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// MarkUserEmailVerified must have been called with the target user ID.
	if len(store.markVerifiedCalls) != 1 || store.markVerifiedCalls[0] != "member-1" {
		t.Errorf("expected one MarkUserEmailVerified('member-1') call, got %v", store.markVerifiedCalls)
	}

	// The audit log must contain a member.verify event scoped to the org,
	// actor=admin-1, target=member-1.
	if len(store.auditEvents) != 1 {
		t.Fatalf("expected one audit event, got %d", len(store.auditEvents))
	}
	ev := store.auditEvents[0]
	if ev.OrgID != "org-1" || ev.ActorID != "admin-1" || ev.Action != "member.verify" || ev.TargetID != "member-1" {
		t.Errorf("audit event mismatch: %+v", ev)
	}
	if ev.Metadata["email"] != "member@example.com" {
		t.Errorf("audit metadata.email = %v, want member@example.com", ev.Metadata["email"])
	}

	// The mock mirrors verification onto the membership row; an admin
	// re-listing members should now see emailVerified=true.
	member, _ := store.GetOrgMember(context.Background(), "org-1", "member-1")
	if member == nil || !member.EmailVerified {
		t.Error("member row must reflect emailVerified=true after verify")
	}
}

func TestOrgsHandler_VerifyMember_AlreadyVerified_Idempotent(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember, EmailVerified: true},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members/member-1/verify", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for idempotent verify, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgsHandler_VerifyMember_NotFound(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members/ghost/verify", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-member, got %d: %s", w.Code, w.Body.String())
	}
	if len(store.markVerifiedCalls) != 0 {
		t.Errorf("MarkUserEmailVerified must not be called for non-member, got %v", store.markVerifiedCalls)
	}
}

func TestOrgsHandler_VerifyMember_GetMemberError_500(t *testing.T) {
	store := newMockOrgStore()
	store.getMemberErr = errors.New("db connectivity lost")
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members/member-1/verify", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on GetOrgMember error, got %d: %s", w.Code, w.Body.String())
	}
	if len(store.markVerifiedCalls) != 0 {
		t.Errorf("MarkUserEmailVerified must not be called when membership check fails, got %v", store.markVerifiedCalls)
	}
}

func TestOrgsHandler_VerifyMember_MarkVerifiedError_500(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember},
	}
	store.markVerifiedErr = errors.New("db down")
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members/member-1/verify", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on MarkUserEmailVerified error, got %d: %s", w.Code, w.Body.String())
	}
	if len(store.auditEvents) != 0 {
		t.Errorf("no audit event should be emitted when verification fails, got %d", len(store.auditEvents))
	}
}

// TestOrgsHandler_VerifyMember_AuditFailureNonFatal proves the audit-log
// write does not undo a successful verification. A real-world audit-log table
// outage must not block the admin's intent.
func TestOrgsHandler_VerifyMember_AuditFailureNonFatal(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember},
	}
	store.auditErr = errors.New("audit table full")
	router, handler := setupOrgTestRouter(t, store)

	// Wire a capture logger to assert the warning surfaces.
	captured := &warnCaptureLogger{}
	handler.SetLogger(captured)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members/member-1/verify", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when audit fails, got %d: %s", w.Code, w.Body.String())
	}
	if len(store.markVerifiedCalls) != 1 {
		t.Errorf("verification must have been persisted, got %v", store.markVerifiedCalls)
	}
	if !captured.warned {
		t.Error("logger.Warn must be called when audit emission fails")
	}
}

type warnCaptureLogger struct {
	warned bool
}

func (c *warnCaptureLogger) Warn(_ string, _ ...any) { c.warned = true }

// --- Create tests ---------------------------------------------------------

// TestOrgsHandler_Create_HyphenatedSlug verifies that the slug validator
// accepts URL-safe hyphenated slugs. The frontend's slugify() produces
// hyphens from multi-word names (e.g. "My Org" -> "my-org") and the previous
// `binding:"alphanum"` tag rejected those, producing an opaque 400 on every
// real-world org creation.
func TestOrgsHandler_Create_HyphenatedSlug(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	router, _ := setupOrgTestRouter(t, store)

	body := `{"name":"My Org","slug":"my-org","ownerEmail":"owner@example.com","planId":"enterprise"}`
	w := doRequest(router, "POST", "/api/v1/orgs", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.CreateOrgResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Slug != "my-org" {
		t.Errorf("expected slug 'my-org', got %q", resp.Slug)
	}
}

// TestOrgsHandler_Create_SlugValidation enumerates accepted and rejected slug
// shapes. The canonical slug format is lowercase letters, digits, and single
// hyphens between segments (no leading/trailing or consecutive hyphens).
func TestOrgsHandler_Create_SlugValidation(t *testing.T) {
	cases := []struct {
		slug      string
		wantValid bool
	}{
		{"myorg", true},
		{"my-org", true},
		{"my-org-1", true},
		{"abc123", true},
		{"123", true},
		{"a1", true},
		{"My-Org", true},                 // uppercase accepted; lowercased server-side
		{"my_org", false},                // underscore rejected
		{"my org", false},                // space rejected
		{"-myorg", false},                // leading hyphen rejected
		{"myorg-", false},                // trailing hyphen rejected
		{"my--org", false},               // consecutive hyphens rejected
		{"a", false},                     // below min length
		{"", false},                      // empty
		{strings.Repeat("a", 51), false}, // above max length
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.slug, func(t *testing.T) {
			store := newMockOrgStore()
			store.usersByEmail["owner@example.com"] = "owner-1"
			router, _ := setupOrgTestRouter(t, store)

			b, _ := json.Marshal(map[string]any{
				"name":       "Test Org",
				"slug":       tc.slug,
				"ownerEmail": "owner@example.com",
				"planId":     "enterprise",
			})
			w := doRequest(router, "POST", "/api/v1/orgs", string(b))

			if tc.wantValid {
				if w.Code != http.StatusCreated {
					t.Fatalf("slug %q: expected 201, got %d: %s", tc.slug, w.Code, w.Body.String())
				}
			} else {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("slug %q: expected 400, got %d: %s", tc.slug, w.Code, w.Body.String())
				}
			}
		})
	}
}

// TestOrgsHandler_Create_ValidationDetails verifies the handler returns
// per-field validation details in the response body so the client can show
// the user which field is wrong, instead of the opaque "invalid request body".
func TestOrgsHandler_Create_ValidationDetails(t *testing.T) {
	store := newMockOrgStore()
	router, _ := setupOrgTestRouter(t, store)

	// Bad slug: contains uppercase + underscore + leading hyphen
	body := `{"name":"My Org","slug":"-Bad_Slug","ownerEmail":"owner@example.com","planId":"enterprise"}`
	w := doRequest(router, "POST", "/api/v1/orgs", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details object in response, got: %s", w.Body.String())
	}
	if _, ok := details["slug"]; !ok {
		t.Errorf("expected details.slug field-level error, got: %s", w.Body.String())
	}
}

// TestOrgsHandler_Create_ValidationDetails_BadEmail verifies that a bad email
// surfaces as an ownerEmail-keyed details entry.
func TestOrgsHandler_Create_ValidationDetails_BadEmail(t *testing.T) {
	store := newMockOrgStore()
	router, _ := setupOrgTestRouter(t, store)

	body := `{"name":"My Org","slug":"my-org","ownerEmail":"not-an-email","planId":"enterprise"}`
	w := doRequest(router, "POST", "/api/v1/orgs", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details object in response, got: %s", w.Body.String())
	}
	if _, ok := details["ownerEmail"]; !ok {
		t.Errorf("expected details.ownerEmail field-level error, got: %s", w.Body.String())
	}
}

// TestOrgsHandler_Create_MalformedJSON returns 400 with a generic body-level
// error (not a per-field map) because the request never bound to the struct.
func TestOrgsHandler_Create_MalformedJSON(t *testing.T) {
	store := newMockOrgStore()
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs", `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestOrgsHandler_Create_NonPlatformAdminForbidden builds a router that does
// NOT set userRole=admin and verifies the 403 path still works (the new
// validation path must not have moved the platform-admin guard earlier or
// later in a way that breaks this).
func TestOrgsHandler_Create_NonPlatformAdminForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockOrgStore()
	handler := NewOrgsHandler(store, &mockOrgAuthService{userID: "user-1"})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		// userRole intentionally not set -> not a platform admin
		c.Next()
	})
	router.POST("/api/v1/orgs", handler.Create)

	body := `{"name":"My Org","slug":"my-org","ownerEmail":"owner@example.com","planId":"enterprise"}`
	w := doRequest(router, "POST", "/api/v1/orgs", body)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Update tests ---------------------------------------------------------
//
// These mirror the Create slug tests on the Update handler path. Update has
// historically had no handler-level tests (the existing suite only covers
// it indirectly via the route registration). The Update path was actively
// modified by this PR (the alphanum -> slug binding tag swap) so per Rule 5
// we must validate that path explicitly.

// TestOrgsHandler_Update_HyphenatedSlug verifies the Update handler accepts
// hyphenated slugs — the same regression Create suffered from.
func TestOrgsHandler_Update_HyphenatedSlug(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "old-slug"}
	router, _ := setupOrgTestRouter(t, store)

	body := `{"slug":"my-new-org"}`
	w := doRequest(router, "PUT", "/api/v1/orgs/org-1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgs["org-1"].Slug != "my-new-org" {
		t.Errorf("expected stored slug 'my-new-org', got %q", store.orgs["org-1"].Slug)
	}
}

// TestOrgsHandler_Update_BadSlugReturnsDetails verifies the Update handler
// rejects invalid slugs with per-field details, mirroring the Create path.
func TestOrgsHandler_Update_BadSlugReturnsDetails(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "old-slug"}
	router, _ := setupOrgTestRouter(t, store)

	body := `{"slug":"bad_slug"}`
	w := doRequest(router, "PUT", "/api/v1/orgs/org-1", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details object, got: %s", w.Body.String())
	}
	if _, ok := details["slug"]; !ok {
		t.Errorf("expected details.slug, got: %s", w.Body.String())
	}
	// Ensure the bad input did not reach the store.
	if store.orgs["org-1"].Slug != "old-slug" {
		t.Errorf("invalid slug must not have been persisted; got %q", store.orgs["org-1"].Slug)
	}
}

// TestOrgsHandler_Update_SlugLowercased mirrors
// TestCreateOrg_Admin_SlugLowercased on the Update path. The handler must
// lowercase mixed-case slugs before persistence so case-insensitive
// uniqueness (enforced by idx_orgs_slug_lower_active in migration 000030)
// and display consistency hold across both Create and Update.
func TestOrgsHandler_Update_SlugLowercased(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "acme"}
	router, _ := setupOrgTestRouter(t, store)

	body := `{"slug":"My-New-Org"}`
	w := doRequest(router, "PUT", "/api/v1/orgs/org-1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgs["org-1"].Slug != "my-new-org" {
		t.Errorf("slug should be lowercased to 'my-new-org', got %q", store.orgs["org-1"].Slug)
	}
}

// --- AddMember + ChangeMemberRole validation-detail tests ----------------
//
// The PR wired bindingErrorResponse() into AddMember and ChangeMemberRole.
// The helper is unit-tested in binding_errors_test.go and the integration
// pattern is verified for Create/Update — these tests close the remaining
// gap (flagged in PR #427 review iteration 2) by exercising the helper
// through these two handlers' real request types.

// TestOrgsHandler_AddMember_MissingUserIdReturnsDetails verifies that
// AddMember returns per-field validation details when a required field is
// omitted, rather than the legacy opaque "invalid request body" message.
func TestOrgsHandler_AddMember_MissingUserIdReturnsDetails(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	// userId omitted; role present and valid
	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members", `{"role":"member"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details object, got: %s", w.Body.String())
	}
	if _, ok := details["userId"]; !ok {
		t.Errorf("expected details.userId, got: %s", w.Body.String())
	}
}

// TestOrgsHandler_AddMember_MissingRoleReturnsDetails covers the other
// required field on AddOrgMemberRequest.
func TestOrgsHandler_AddMember_MissingRoleReturnsDetails(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "POST", "/api/v1/orgs/org-1/members", `{"userId":"new-user"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details object, got: %s", w.Body.String())
	}
	if _, ok := details["role"]; !ok {
		t.Errorf("expected details.role, got: %s", w.Body.String())
	}
}

// TestOrgsHandler_ChangeMemberRole_MissingRoleReturnsDetails verifies the
// PUT /orgs/:id/members/:userID handler returns per-field details for an
// empty body. The only required field is role.
func TestOrgsHandler_ChangeMemberRole_MissingRoleReturnsDetails(t *testing.T) {
	store := newMockOrgStore()
	store.orgs["org-1"] = &types.Organization{ID: "org-1"}
	store.members["org-1"] = []*types.OrgMember{
		{OrgID: "org-1", UserID: "admin-1", Role: types.OrgRoleAdmin},
		{OrgID: "org-1", UserID: "member-1", Role: types.OrgRoleMember},
	}
	router, _ := setupOrgTestRouter(t, store)

	w := doRequest(router, "PUT", "/api/v1/orgs/org-1/members/member-1", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	details, ok := resp["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details object, got: %s", w.Body.String())
	}
	if _, ok := details["role"]; !ok {
		t.Errorf("expected details.role, got: %s", w.Body.String())
	}
}

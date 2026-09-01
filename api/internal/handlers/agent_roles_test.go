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
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/services/role"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// mockRoleStore implements roleStore for agent-role handler tests.
type mockRoleStore struct{ mock.Mock }

func (m *mockRoleStore) GetAgentRole(ctx context.Context, id string) (*types.AgentRole, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AgentRole), args.Error(1)
}
func (m *mockRoleStore) ListAgentRoles(ctx context.Context, scope, orgID string) ([]*types.AgentRole, error) {
	args := m.Called(ctx, scope, orgID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.AgentRole), args.Error(1)
}
func (m *mockRoleStore) CreateAgentRole(ctx context.Context, r *types.AgentRole, c []byte) (*types.AgentRole, error) {
	args := m.Called(ctx, r, c)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AgentRole), args.Error(1)
}
func (m *mockRoleStore) UpdateAgentRole(ctx context.Context, id string, r *types.AgentRole, c []byte) (*types.AgentRole, error) {
	args := m.Called(ctx, id, r, c)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AgentRole), args.Error(1)
}
func (m *mockRoleStore) DeleteAgentRole(ctx context.Context, id string) error {
	return m.Called(ctx, id).Error(0)
}
func (m *mockRoleStore) GetRoleDependents(ctx context.Context, id string) ([]*types.AgentRole, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.AgentRole), args.Error(1)
}
func (m *mockRoleStore) HasRoleWorkspaceUsage(ctx context.Context, id string) (bool, error) {
	args := m.Called(ctx, id)
	return args.Bool(0), args.Error(1)
}
func (m *mockRoleStore) SetOrgDefaultRole(ctx context.Context, orgID, id string) error {
	return m.Called(ctx, orgID, id).Error(0)
}
func (m *mockRoleStore) GetWorkspaceAgentRole(ctx context.Context, wsID string) (*types.AgentRole, error) {
	args := m.Called(ctx, wsID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AgentRole), args.Error(1)
}
func (m *mockRoleStore) SetWorkspaceAgentRole(ctx context.Context, wsID, roleID, userID string) error {
	return m.Called(ctx, wsID, roleID, userID).Error(0)
}
func (m *mockRoleStore) ClearWorkspaceAgentRole(ctx context.Context, wsID, userID string) error {
	return m.Called(ctx, wsID, userID).Error(0)
}
func (m *mockRoleStore) GetWorkspaceOrgID(ctx context.Context, wsID string) (string, error) {
	args := m.Called(ctx, wsID)
	return args.String(0), args.Error(1)
}
func (m *mockRoleStore) GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error) {
	args := m.Called(ctx, orgID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.OrgPolicy), args.Error(1)
}
func (m *mockRoleStore) LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, meta map[string]any) error {
	return m.Called(ctx, orgID, actorID, action, targetID, meta).Error(0)
}
func (m *mockRoleStore) LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, meta map[string]any) error {
	return m.Called(ctx, domain, actorID, action, targetID, orgID, meta).Error(0)
}

type stubOrgAuth struct{ userID string }

func (s stubOrgAuth) GetUserID(_ *gin.Context) string { return s.userID }

var _ orgAuthService = stubOrgAuth{}

func newRoleHandlerTest(store *mockRoleStore, userID string) *AgentRoleHandler {
	return NewAgentRoleHandler(store, role.New(store), stubOrgAuth{userID: userID}, nil)
}

// doRoleRequest registers handler on a parametrized route and serves a request.
func doRoleRequest(t *testing.T, method, route, url string, handler gin.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Handle(method, route, handler)
	var req *http.Request
	if body != nil {
		raw, _ := json.Marshal(body)
		req = httptest.NewRequest(method, url, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, url, nil)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// TestSetWorkspaceRole_OrgLocked_Returns403: when the org has
// allow_user_prompt=false, role selection is refused with 403.
func TestSetWorkspaceRole_OrgLocked_Returns403(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetAgentRole", mock.Anything, "role-1").Return(&types.AgentRole{Scope: "platform", ID: "role-1"}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("false")},
	}, nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/agent-role", "/ws/ws-1/agent-role",
		h.SetWorkspaceRole, map[string]string{"roleId": "role-1"})

	assert.Equal(t, http.StatusForbidden, rr.Code)
	store.AssertNotCalled(t, "SetWorkspaceAgentRole")
}

// TestSetWorkspaceRole_CrossOrgRole_Returns400: an org-scoped role belonging to
// a different org cannot be selected.
func TestSetWorkspaceRole_CrossOrgRole_Returns400(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")
	otherOrg := "org-other"

	store.On("GetAgentRole", mock.Anything, "role-9").Return(&types.AgentRole{
		Scope: "org", ID: "role-9", OrgID: &otherOrg,
	}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}, nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/agent-role", "/ws/ws-1/agent-role",
		h.SetWorkspaceRole, map[string]string{"roleId": "role-9"})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	store.AssertNotCalled(t, "SetWorkspaceAgentRole")
}

// TestSetWorkspaceRole_Allowed_Succeeds: happy path sets the role (200).
func TestSetWorkspaceRole_Allowed_Succeeds(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetAgentRole", mock.Anything, "role-1").Return(&types.AgentRole{Scope: "platform", ID: "role-1"}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}, nil)
	store.On("SetWorkspaceAgentRole", mock.Anything, "ws-1", "role-1", "user-1").Return(nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/agent-role", "/ws/ws-1/agent-role",
		h.SetWorkspaceRole, map[string]string{"roleId": "role-1"})

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertCalled(t, "SetWorkspaceAgentRole", mock.Anything, "ws-1", "role-1", "user-1")
}

// TestClearWorkspaceRole_OrgLocked_Returns403: clearing is also gated by the
// allow_user_prompt policy.
func TestClearWorkspaceRole_OrgLocked_Returns403(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("false")},
	}, nil)

	rr := doRoleRequest(t, http.MethodDelete, "/ws/:id/agent-role", "/ws/ws-1/agent-role",
		h.ClearWorkspaceRole, nil)

	assert.Equal(t, http.StatusForbidden, rr.Code)
	store.AssertNotCalled(t, "ClearWorkspaceAgentRole")
}

// TestClearWorkspaceRole_Allowed_Succeeds: happy path clears the role (200).
func TestClearWorkspaceRole_Allowed_Succeeds(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}, nil)
	store.On("ClearWorkspaceAgentRole", mock.Anything, "ws-1", "user-1").Return(nil)

	rr := doRoleRequest(t, http.MethodDelete, "/ws/:id/agent-role", "/ws/ws-1/agent-role",
		h.ClearWorkspaceRole, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
}

// TestDeletePlatform_DependentRoles_Returns409: dependent roles → 409 Conflict.
func TestDeletePlatform_DependentRoles_Returns409(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetRoleDependents", mock.Anything, "role-1").Return([]*types.AgentRole{
		{ID: "role-2", Name: "Child"},
	}, nil)
	store.On("HasRoleWorkspaceUsage", mock.Anything, "role-1").Return(false, nil)

	rr := doRoleRequest(t, http.MethodDelete, "/admin/agent-roles/:id", "/admin/agent-roles/role-1",
		h.DeletePlatform, nil)

	assert.Equal(t, http.StatusConflict, rr.Code)
	store.AssertNotCalled(t, "DeleteAgentRole")
}

// TestDeletePlatform_DBError_Returns500: a non-conflict error from CheckDelete
// must surface as 500, not 409 (regression for the dead type-switch bug).
func TestDeletePlatform_DBError_Returns500(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetRoleDependents", mock.Anything, "role-1").Return(nil, errors.New("db down"))
	store.On("HasRoleWorkspaceUsage", mock.Anything, mock.Anything).Return(false, nil)

	rr := doRoleRequest(t, http.MethodDelete, "/admin/agent-roles/:id", "/admin/agent-roles/role-1",
		h.DeletePlatform, nil)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	store.AssertNotCalled(t, "DeleteAgentRole")
}

// --- C1: cross-tenant authorization bypass regression tests ---

// TestGetOrg_OtherOrgRole_Returns404: an org admin must not read a role
// belonging to a different org via the org route.
func TestGetOrg_OtherOrgRole_Returns404(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")
	otherOrg := "org-other"
	store.On("GetAgentRole", mock.Anything, "role-9").Return(&types.AgentRole{
		Scope: "org", ID: "role-9", OrgID: &otherOrg,
	}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/o/:id/agent-roles/:roleId", "/o/orgA/agent-roles/role-9",
		h.GetOrg, nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestGetOrg_PlatformRole_Returns404: an org admin must not read a platform
// role via the org route.
func TestGetOrg_PlatformRole_Returns404(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")
	store.On("GetAgentRole", mock.Anything, "role-p").Return(&types.AgentRole{
		Scope: "platform", ID: "role-p",
	}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/o/:id/agent-roles/:roleId", "/o/orgA/agent-roles/role-p",
		h.GetOrg, nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestDeleteOrg_OtherOrgRole_Returns404: deletion is scoped to the route org.
func TestDeleteOrg_OtherOrgRole_Returns404(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")
	otherOrg := "org-other"
	store.On("GetAgentRole", mock.Anything, "role-9").Return(&types.AgentRole{
		Scope: "org", ID: "role-9", OrgID: &otherOrg,
	}, nil)

	rr := doRoleRequest(t, http.MethodDelete, "/o/:id/agent-roles/:roleId", "/o/orgA/agent-roles/role-9",
		h.DeleteOrg, nil)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	store.AssertNotCalled(t, "DeleteAgentRole")
}

// --- C2: allow_user_prompt default-locked semantics ---

// TestSetWorkspaceRole_AllowUserPromptUnset_Returns403: when the org has NOT set
// the toggle, writes must be rejected (default-locked), matching the resolution
// path — otherwise the accepted value is silently discarded at delivery.
func TestSetWorkspaceRole_AllowUserPromptUnset_Returns403(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("GetAgentRole", mock.Anything, "role-1").Return(&types.AgentRole{Scope: "platform", ID: "role-1"}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{}, nil) // unset → locked

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/agent-role", "/ws/ws-1/agent-role",
		h.SetWorkspaceRole, map[string]string{"roleId": "role-1"})

	assert.Equal(t, http.StatusForbidden, rr.Code)
	store.AssertNotCalled(t, "SetWorkspaceAgentRole")
}

// --- C4: default-role create must not violate the partial unique index ---

// TestCreateOrg_IsDefault_True_InsertsFalseThenSwaps: creating a default role
// must INSERT with is_default=false (SetOrgDefaultRole does the atomic swap) so
// a second is_default=true row never exists to violate the partial unique index.
func TestCreateOrg_IsDefault_True_InsertsFalseThenSwaps(t *testing.T) {
	store := new(mockRoleStore)
	h := newRoleHandlerTest(store, "user-1")

	store.On("CreateAgentRole", mock.Anything,
		mock.MatchedBy(func(r *types.AgentRole) bool { return !r.IsDefault }), mock.Anything).
		Return(&types.AgentRole{ID: "role-new", Scope: "org"}, nil)
	store.On("SetOrgDefaultRole", mock.Anything, "org-1", "role-new").Return(nil)
	store.On("LogOrgEvent", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	body := map[string]any{"name": "Reviewer", "slug": "reviewer", "isDefault": true}
	rr := doRoleRequest(t, http.MethodPost, "/o/:id/agent-roles", "/o/org-1/agent-roles",
		h.CreateOrg, body)

	assert.Equal(t, http.StatusCreated, rr.Code)
	store.AssertCalled(t, "SetOrgDefaultRole", mock.Anything, "org-1", "role-new")
}

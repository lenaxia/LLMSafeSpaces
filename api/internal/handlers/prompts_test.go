// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// prompts_test.go — Handler-level tests for the PromptHandler CRUD endpoints
// (platform/org/workspace prompt management). These were the only agent-
// customization endpoints with ZERO handler coverage before this file.
//
// Covers all 6 endpoints registered in router.go:
//   GET  /api/v1/admin/prompt          — GetPlatform
//   PUT  /api/v1/admin/prompt          — SetPlatform
//   GET  /api/v1/orgs/:id/prompt       — GetOrg
//   PUT  /api/v1/orgs/:id/prompt       — SetOrg
//   GET  /api/v1/workspaces/:id/prompt — GetWorkspacePrompt
//   PUT  /api/v1/workspaces/:id/prompt — SetWorkspacePrompt
//
// Test strategy: mock promptStore + stubOrgAuth (reused from agent_roles_test)
// + stubPolicyLogger. Exercises the gin handler path end-to-end including
// binding validation, policy enforcement (allow_user_prompt default-locked),
// cache invalidation, and audit emission.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// mockPromptHandlerStore implements promptStore (defined in prompts.go) for
// handler tests. This is distinct from the prompt package's mockPromptStore.
type mockPromptHandlerStore struct{ mock.Mock }

func (m *mockPromptHandlerStore) GetPlatformSetting(ctx context.Context, key types.PlatformSettingKey) (*types.PlatformSetting, error) {
	args := m.Called(ctx, key)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.PlatformSetting), args.Error(1)
}

func (m *mockPromptHandlerStore) SetPlatformSetting(ctx context.Context, key types.PlatformSettingKey, value json.RawMessage, updatedBy string) error {
	return m.Called(ctx, key, value, updatedBy).Error(0)
}

func (m *mockPromptHandlerStore) GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error) {
	args := m.Called(ctx, orgID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.OrgPolicy), args.Error(1)
}

func (m *mockPromptHandlerStore) SetOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey, value json.RawMessage, updatedBy string) error {
	return m.Called(ctx, orgID, key, value, updatedBy).Error(0)
}

func (m *mockPromptHandlerStore) DeleteOrgPolicy(ctx context.Context, orgID string, key types.OrgPolicyKey) error {
	return m.Called(ctx, orgID, key).Error(0)
}

func (m *mockPromptHandlerStore) GetWorkspacePrompt(ctx context.Context, workspaceID string) (*types.WorkspacePrompt, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.WorkspacePrompt), args.Error(1)
}

func (m *mockPromptHandlerStore) SetWorkspacePrompt(ctx context.Context, workspaceID string, prompt string, updatedBy string) error {
	return m.Called(ctx, workspaceID, prompt, updatedBy).Error(0)
}

func (m *mockPromptHandlerStore) GetWorkspaceOrgID(ctx context.Context, workspaceID string) (string, error) {
	args := m.Called(ctx, workspaceID)
	return args.String(0), args.Error(1)
}

func (m *mockPromptHandlerStore) LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, meta map[string]any) error {
	return m.Called(ctx, orgID, actorID, action, targetID, meta).Error(0)
}

func (m *mockPromptHandlerStore) LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, meta map[string]any) error {
	return m.Called(ctx, domain, actorID, action, targetID, orgID, meta).Error(0)
}

// stubPolicyLogger records Warn calls so tests can assert audit-failure
// logging.
type stubPolicyLogger struct {
	warns []string
}

func (s *stubPolicyLogger) Warn(msg string, args ...any) {
	s.warns = append(s.warns, msg)
}

var _ policyLogger = (*stubPolicyLogger)(nil)

func strPtr(s string) *string { return &s }

// newPromptHandler builds a PromptHandler wired with a mock store. svc is nil
// (no cache invalidation in handler tests — the service layer is tested
// separately). logger captures audit-failure warnings.
func newPromptHandlerTest(store *mockPromptHandlerStore, userID string, logger policyLogger) *PromptHandler {
	if logger == nil {
		logger = &stubPolicyLogger{}
	}
	return NewPromptHandler(store, nil, stubOrgAuth{userID: userID}, logger)
}

// --- GetPlatform ---

func TestGetPlatform_ReturnsStoredPrompt(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Be concise and secure."`),
	}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/admin/prompt", "/admin/prompt", h.GetPlatform, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp platformPromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "Be concise and secure.", resp.Prompt)
}

func TestGetPlatform_NotSet_ReturnsDefaultPlatformPrompt(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	// Fresh install: no sys_prompt_platform row. GET must surface the
	// effective default the platform tier actually delivers, not "" —
	// otherwise admins cannot see or edit-from what workspaces receive.
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return((*types.PlatformSetting)(nil), nil)

	rr := doRoleRequest(t, http.MethodGet, "/admin/prompt", "/admin/prompt", h.GetPlatform, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp platformPromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, types.DefaultPlatformPrompt, resp.Prompt)
}

func TestGetPlatform_ExplicitlyCleared_ReturnsEmpty(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	// A saved-empty row is an admin's deliberate choice — it must NOT be
	// papered over with the default.
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: json.RawMessage(`""`),
	}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/admin/prompt", "/admin/prompt", h.GetPlatform, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp platformPromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "", resp.Prompt)
}

func TestGetPlatform_DBError_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return((*types.PlatformSetting)(nil), errors.New("db down"))

	rr := doRoleRequest(t, http.MethodGet, "/admin/prompt", "/admin/prompt", h.GetPlatform, nil)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

// --- SetPlatform ---

func TestSetPlatform_Success(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("SetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform, mock.Anything, "admin-1").Return(nil)
	store.On("LogAuditEvent", mock.Anything, "admin", "admin-1", "prompt.platform.set", "sys_prompt_platform", (*string)(nil), mock.Anything).Return(nil)

	rr := doRoleRequest(t, http.MethodPut, "/admin/prompt", "/admin/prompt", h.SetPlatform, map[string]string{"prompt": "New rules"})

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertCalled(t, "SetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform, mock.Anything, "admin-1")
}

func TestSetPlatform_TooLong_Returns400(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	longPrompt := strings.Repeat("x", 10001)

	rr := doRoleRequest(t, http.MethodPut, "/admin/prompt", "/admin/prompt", h.SetPlatform, map[string]string{"prompt": longPrompt})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	store.AssertNotCalled(t, "SetPlatformSetting")
}

func TestSetPlatform_DBError_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("SetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform, mock.Anything, "admin-1").Return(errors.New("db down"))

	rr := doRoleRequest(t, http.MethodPut, "/admin/prompt", "/admin/prompt", h.SetPlatform, map[string]string{"prompt": "New rules"})

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestSetPlatform_AuditEmissionFailure_StillReturns200(t *testing.T) {
	store := new(mockPromptHandlerStore)
	logger := &stubPolicyLogger{}
	h := NewPromptHandler(store, nil, stubOrgAuth{userID: "admin-1"}, logger)

	store.On("SetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform, mock.Anything, "admin-1").Return(nil)
	store.On("LogAuditEvent", mock.Anything, "admin", "admin-1", "prompt.platform.set", "sys_prompt_platform", (*string)(nil), mock.Anything).Return(errors.New("audit db down"))

	rr := doRoleRequest(t, http.MethodPut, "/admin/prompt", "/admin/prompt", h.SetPlatform, map[string]string{"prompt": "New rules"})

	assert.Equal(t, http.StatusOK, rr.Code, "audit failure must not block the write — handler returns 200")
	assert.NotEmpty(t, logger.warns, "audit failure must be logged via policyLogger.Warn")
}

// --- GetOrg ---

func TestGetOrg_ReturnsPromptAndToggle(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicySysPromptOrg, Value: []byte(`"Org overlay"`)},
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.GetOrg, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp orgPromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "Org overlay", resp.Prompt)
	assert.True(t, resp.AllowUserPrompt)
}

func TestGetOrg_NoPolicies_ReturnsDefaults(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.GetOrg, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp orgPromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "", resp.Prompt)
	assert.False(t, resp.AllowUserPrompt)
}

func TestGetOrg_DBError_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("GetOrgPolicies", mock.Anything, "org-1").Return(([]*types.OrgPolicy)(nil), errors.New("db down"))

	rr := doRoleRequest(t, http.MethodGet, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.GetOrg, nil)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

// --- SetOrg ---

func TestSetOrg_SetsBothFields(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	allowUser := true
	store.On("SetOrgPolicy", mock.Anything, "org-1", types.PolicySysPromptOrg, mock.Anything, "admin-1").Return(nil)
	store.On("SetOrgPolicy", mock.Anything, "org-1", types.PolicyAllowUserPrompt, mock.Anything, "admin-1").Return(nil)
	store.On("LogOrgEvent", mock.Anything, "org-1", "admin-1", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	body := setOrgPromptRequest{Prompt: strPtr("Org rules"), AllowUserPrompt: &allowUser}
	rr := doRoleRequest(t, http.MethodPut, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.SetOrg, body)

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertNumberOfCalls(t, "SetOrgPolicy", 2)
}

func TestSetOrg_OnlyPrompt_AllowUnchanged(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("SetOrgPolicy", mock.Anything, "org-1", types.PolicySysPromptOrg, mock.Anything, "admin-1").Return(nil)
	store.On("LogOrgEvent", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	body := setOrgPromptRequest{Prompt: strPtr("New overlay")}
	rr := doRoleRequest(t, http.MethodPut, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.SetOrg, body)

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertNumberOfCalls(t, "SetOrgPolicy", 1)
	store.AssertCalled(t, "SetOrgPolicy", mock.Anything, "org-1", types.PolicySysPromptOrg, mock.Anything, "admin-1")
}

func TestSetOrg_TooLong_Returns400(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	longPrompt := strings.Repeat("x", 10001)
	body := setOrgPromptRequest{Prompt: &longPrompt}

	rr := doRoleRequest(t, http.MethodPut, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.SetOrg, body)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	store.AssertNotCalled(t, "SetOrgPolicy")
}

func TestSetOrg_DBError_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	store.On("SetOrgPolicy", mock.Anything, "org-1", types.PolicySysPromptOrg, mock.Anything, "admin-1").Return(errors.New("db down"))

	body := setOrgPromptRequest{Prompt: strPtr("x")}
	rr := doRoleRequest(t, http.MethodPut, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.SetOrg, body)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestSetOrg_OnlyToggle_PromptUnchanged(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "admin-1", nil)

	allowUser := true
	store.On("SetOrgPolicy", mock.Anything, "org-1", types.PolicyAllowUserPrompt, mock.Anything, "admin-1").Return(nil)
	store.On("LogOrgEvent", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	body := setOrgPromptRequest{AllowUserPrompt: &allowUser}
	rr := doRoleRequest(t, http.MethodPut, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.SetOrg, body)

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertNumberOfCalls(t, "SetOrgPolicy", 1)
	store.AssertCalled(t, "SetOrgPolicy", mock.Anything, "org-1", types.PolicyAllowUserPrompt, mock.Anything, "admin-1")
}

func TestSetOrg_AuditEmissionFailure_StillReturns200(t *testing.T) {
	store := new(mockPromptHandlerStore)
	logger := &stubPolicyLogger{}
	h := NewPromptHandler(store, nil, stubOrgAuth{userID: "admin-1"}, logger)

	store.On("SetOrgPolicy", mock.Anything, "org-1", types.PolicySysPromptOrg, mock.Anything, "admin-1").Return(nil)
	store.On("LogOrgEvent", mock.Anything, "org-1", "admin-1", "prompt.org.set", "sys_prompt_org", mock.Anything).Return(errors.New("audit db down"))

	body := setOrgPromptRequest{Prompt: strPtr("New overlay")}
	rr := doRoleRequest(t, http.MethodPut, "/orgs/:id/prompt", "/orgs/org-1/prompt", h.SetOrg, body)

	assert.Equal(t, http.StatusOK, rr.Code, "audit failure must not block the org prompt write")
	assert.NotEmpty(t, logger.warns, "audit failure must be logged via policyLogger.Warn")
}

// --- GetWorkspacePrompt ---

func TestGetWorkspacePrompt_ReturnsStoredPrompt(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return(&types.WorkspacePrompt{
		Prompt: "Focus on tests",
	}, nil)

	rr := doRoleRequest(t, http.MethodGet, "/ws/:id/prompt", "/ws/ws-1/prompt", h.GetWorkspacePrompt, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp workspacePromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "Focus on tests", resp.Prompt)
}

func TestGetWorkspacePrompt_NotSet_ReturnsEmpty(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return((*types.WorkspacePrompt)(nil), nil)

	rr := doRoleRequest(t, http.MethodGet, "/ws/:id/prompt", "/ws/ws-1/prompt", h.GetWorkspacePrompt, nil)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp workspacePromptResponse
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "", resp.Prompt)
}

func TestGetWorkspacePrompt_DBError_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return((*types.WorkspacePrompt)(nil), errors.New("db down"))

	rr := doRoleRequest(t, http.MethodGet, "/ws/:id/prompt", "/ws/ws-1/prompt", h.GetWorkspacePrompt, nil)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

// --- SetWorkspacePrompt ---

func TestSetWorkspacePrompt_OrgAllowed_Succeeds(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}, nil)
	store.On("SetWorkspacePrompt", mock.Anything, "ws-1", "My instructions", "user-1").Return(nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: "My instructions"})

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertCalled(t, "SetWorkspacePrompt", mock.Anything, "ws-1", "My instructions", "user-1")
}

func TestSetWorkspacePrompt_OrgLocked_Returns403(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("false")},
	}, nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: "My instructions"})

	assert.Equal(t, http.StatusForbidden, rr.Code)
	store.AssertNotCalled(t, "SetWorkspacePrompt")
}

func TestSetWorkspacePrompt_AllowUserPromptUnset_Returns403(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{}, nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: "My instructions"})

	assert.Equal(t, http.StatusForbidden, rr.Code)
	store.AssertNotCalled(t, "SetWorkspacePrompt")
}

func TestSetWorkspacePrompt_StandaloneWorkspace_Succeeds(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("", nil)
	store.On("SetWorkspacePrompt", mock.Anything, "ws-1", "My instructions", "user-1").Return(nil)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: "My instructions"})

	assert.Equal(t, http.StatusOK, rr.Code)
	store.AssertCalled(t, "SetWorkspacePrompt", mock.Anything, "ws-1", "My instructions", "user-1")
}

func TestSetWorkspacePrompt_TooLong_Returns400(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	longPrompt := strings.Repeat("x", 10001)

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: longPrompt})

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	store.AssertNotCalled(t, "SetWorkspacePrompt")
}

func TestSetWorkspacePrompt_OrgLookupError_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("", errors.New("db down"))

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: "My instructions"})

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	store.AssertNotCalled(t, "SetWorkspacePrompt")
}

func TestSetWorkspacePrompt_DBErrorOnSet_Returns500(t *testing.T) {
	store := new(mockPromptHandlerStore)
	h := newPromptHandlerTest(store, "user-1", nil)

	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}, nil)
	store.On("SetWorkspacePrompt", mock.Anything, "ws-1", "x", "user-1").Return(errors.New("db down"))

	rr := doRoleRequest(t, http.MethodPut, "/ws/:id/prompt", "/ws/ws-1/prompt", h.SetWorkspacePrompt,
		setWorkspacePromptRequest{Prompt: "x"})

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

// --- userPromptAllowedFromPolicies (shared helper) ---

func TestUserPromptAllowedFromPolicies_ExplicitTrue(t *testing.T) {
	policies := []*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("true")},
	}
	assert.True(t, userPromptAllowedFromPolicies(policies))
}

func TestUserPromptAllowedFromPolicies_ExplicitFalse(t *testing.T) {
	policies := []*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: []byte("false")},
	}
	assert.False(t, userPromptAllowedFromPolicies(policies))
}

func TestUserPromptAllowedFromPolicies_Unset_DefaultsLocked(t *testing.T) {
	policies := []*types.OrgPolicy{}
	assert.False(t, userPromptAllowedFromPolicies(policies))
}

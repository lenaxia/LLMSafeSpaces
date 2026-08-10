// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// MockAPIClient implements APIClient for testing.
type MockAPIClient struct {
	mock.Mock
}

func (m *MockAPIClient) CreateWorkspace(ctx context.Context, req CreateWorkspaceReq) (*WorkspaceResp, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*WorkspaceResp), args.Error(1)
}

func (m *MockAPIClient) ActivateWorkspace(ctx context.Context, workspaceID string) (*ActivateResp, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ActivateResp), args.Error(1)
}

func (m *MockAPIClient) SuspendWorkspace(ctx context.Context, workspaceID string) error {
	return m.Called(ctx, workspaceID).Error(0)
}

func (m *MockAPIClient) RefreshWorkspace(ctx context.Context, workspaceID string) (*RefreshWorkspaceResp, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*RefreshWorkspaceResp), args.Error(1)
}

func (m *MockAPIClient) CreateSession(ctx context.Context, workspaceID string) (*SessionResp, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*SessionResp), args.Error(1)
}

func (m *MockAPIClient) GetHistory(ctx context.Context, workspaceID, sessionID string) ([]Message, error) {
	args := m.Called(ctx, workspaceID, sessionID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]Message), args.Error(1)
}

func (m *MockAPIClient) SendMessage(ctx context.Context, workspaceID, sessionID, message string, timeout time.Duration) (string, error) {
	args := m.Called(ctx, workspaceID, sessionID, message, timeout)
	return args.String(0), args.Error(1)
}

func (m *MockAPIClient) CreateCredential(ctx context.Context, req CreateCredentialReq) (*CredentialResp, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*CredentialResp), args.Error(1)
}

func (m *MockAPIClient) ListCredentials(ctx context.Context) ([]CredentialResp, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]CredentialResp), args.Error(1)
}

func (m *MockAPIClient) DeleteCredential(ctx context.Context, credentialID string) error {
	return m.Called(ctx, credentialID).Error(0)
}

func (m *MockAPIClient) BindCredential(ctx context.Context, workspaceID string, credentialIDs []string) error {
	return m.Called(ctx, workspaceID, credentialIDs).Error(0)
}

func (m *MockAPIClient) ListModels(ctx context.Context, workspaceID string) (json.RawMessage, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(json.RawMessage), args.Error(1)
}

func (m *MockAPIClient) SetModel(ctx context.Context, workspaceID, model string) error {
	return m.Called(ctx, workspaceID, model).Error(0)
}

func (m *MockAPIClient) QuestionReply(ctx context.Context, workspaceID, requestID string, answers [][]string) error {
	return m.Called(ctx, workspaceID, requestID, answers).Error(0)
}

func (m *MockAPIClient) QuestionReject(ctx context.Context, workspaceID, requestID string) error {
	return m.Called(ctx, workspaceID, requestID).Error(0)
}

func (m *MockAPIClient) PermissionReply(ctx context.Context, workspaceID, requestID, reply, message string) error {
	return m.Called(ctx, workspaceID, requestID, reply, message).Error(0)
}

func (m *MockAPIClient) CancelWorkflowRun(ctx context.Context, runID string) error {
	return m.Called(ctx, runID).Error(0)
}

func (m *MockAPIClient) ListWorkflows(ctx context.Context) (json.RawMessage, error) {
	args := m.Called(ctx)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) GetWorkflow(ctx context.Context, workflowID string) (json.RawMessage, error) {
	args := m.Called(ctx, workflowID)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) CreateWorkflow(ctx context.Context, name, specYAML, status string) (json.RawMessage, error) {
	args := m.Called(ctx, name, specYAML, status)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) UpdateWorkflow(ctx context.Context, workflowID, name, status, specYAML string) (json.RawMessage, error) {
	args := m.Called(ctx, workflowID, name, status, specYAML)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) RunWorkflow(ctx context.Context, workflowID, input, workspaceID string) (json.RawMessage, error) {
	args := m.Called(ctx, workflowID, input, workspaceID)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) GetWorkflowRunStatus(ctx context.Context, runID string) (json.RawMessage, error) {
	args := m.Called(ctx, runID)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) ListTriggers(ctx context.Context) (json.RawMessage, error) {
	args := m.Called(ctx)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) CreateTrigger(ctx context.Context, name, sourceType, sourceConfig, workspaceID, workflowID, prompt, memoryMode, captureMode, preserveSession string) (json.RawMessage, error) {
	args := m.Called(ctx, name, sourceType, sourceConfig, workspaceID, workflowID, prompt, memoryMode, captureMode, preserveSession)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) UpdateTrigger(ctx context.Context, triggerID, enabled string) (json.RawMessage, error) {
	args := m.Called(ctx, triggerID, enabled)
	return json.RawMessage(args.String(0)), args.Error(1)
}

func (m *MockAPIClient) DeleteTrigger(ctx context.Context, triggerID string) error {
	return m.Called(ctx, triggerID).Error(0)
}

func newTestHandlers() (*handlers, *MockAPIClient) {
	mockClient := &MockAPIClient{}
	h := &handlers{client: mockClient, timeout: 300 * time.Second}
	return h, mockClient
}

func makeReq(name string, args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return req
}

// ===== workspace_create =====

func TestWorkspaceCreate_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateWorkspace", ctx, CreateWorkspaceReq{
		Runtime: "python:3.10",
		Name:    "my-project",
	}).Return(&WorkspaceResp{ID: "ws-123", Name: "my-project", Runtime: "python:3.10", Phase: "Active"}, nil)

	result, err := h.workspaceCreate(ctx, makeReq("workspace_create", map[string]any{
		"runtime": "python:3.10",
		"name":    "my-project",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "ws-123")
	mockClient.AssertExpectations(t)
}

func TestWorkspaceCreate_MissingRuntime(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.workspaceCreate(context.Background(), makeReq("workspace_create", map[string]any{}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "runtime")
}

func TestWorkspaceCreate_APIError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateWorkspace", ctx, CreateWorkspaceReq{Runtime: "python:3.10"}).
		Return((*WorkspaceResp)(nil), assert.AnError)

	result, err := h.workspaceCreate(ctx, makeReq("workspace_create", map[string]any{"runtime": "python:3.10"}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	mockClient.AssertExpectations(t)
}

// ===== workspace_activate =====

func TestWorkspaceActivate_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("ActivateWorkspace", ctx, "ws-123").
		Return(&ActivateResp{Resumed: "ws-123"}, nil)

	result, err := h.workspaceActivate(ctx, makeReq("workspace_activate", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "ws-123")
	mockClient.AssertExpectations(t)
}

func TestWorkspaceActivate_MissingID(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.workspaceActivate(context.Background(), makeReq("workspace_activate", map[string]any{}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "workspace_id")
}

// ===== workspace_stop =====

func TestWorkspaceStop_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("SuspendWorkspace", ctx, "ws-123").Return(nil)

	result, err := h.workspaceStop(ctx, makeReq("workspace_stop", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "ws-123")
	mockClient.AssertExpectations(t)
}

func TestWorkspaceStop_MissingID(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.workspaceStop(context.Background(), makeReq("workspace_stop", map[string]any{}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// ===== workspace_refresh_compute =====

func TestWorkspaceRefreshCompute_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("RefreshWorkspace", ctx, "ws-123").
		Return(&RefreshWorkspaceResp{RestartGeneration: 5}, nil)

	result, err := h.workspaceRefreshCompute(ctx, makeReq("workspace_refresh_compute", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	text := result.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "ws-123")
	assert.Contains(t, text, "5")
	mockClient.AssertExpectations(t)
}

func TestWorkspaceRefreshCompute_MissingID(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.workspaceRefreshCompute(context.Background(), makeReq("workspace_refresh_compute", map[string]any{}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestWorkspaceRefreshCompute_APIError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("RefreshWorkspace", ctx, "ws-123").
		Return((*RefreshWorkspaceResp)(nil), fmt.Errorf("conflict: workspace terminating"))

	result, err := h.workspaceRefreshCompute(ctx, makeReq("workspace_refresh_compute", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "failed to refresh workspace compute")
	mockClient.AssertExpectations(t)
}

// ===== session_create =====

func TestSessionCreate_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateSession", ctx, "ws-123").Return(&SessionResp{ID: "sess-456"}, nil)

	result, err := h.sessionCreate(ctx, makeReq("session_create", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "sess-456")
	mockClient.AssertExpectations(t)
}

func TestSessionCreate_MissingWorkspaceID(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.sessionCreate(context.Background(), makeReq("session_create", map[string]any{}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// ===== session_message =====

func TestSessionMessage_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("SendMessage", ctx, "ws-123", "sess-456", "hello", 300*time.Second).
		Return("Hello! How can I help?", nil)

	result, err := h.sessionMessage(ctx, makeReq("session_message", map[string]any{
		"workspace_id": "ws-123",
		"session_id":   "sess-456",
		"message":      "hello",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, "Hello! How can I help?", result.Content[0].(mcp.TextContent).Text)
	mockClient.AssertExpectations(t)
}

func TestSessionMessage_MissingMessage(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.sessionMessage(context.Background(), makeReq("session_message", map[string]any{
		"workspace_id": "ws-123",
		"session_id":   "sess-456",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "message")
}

func TestSessionMessage_TooLarge(t *testing.T) {
	h, _ := newTestHandlers()

	bigMsg := strings.Repeat("x", maxMessageSize+1)
	result, err := h.sessionMessage(context.Background(), makeReq("session_message", map[string]any{
		"workspace_id": "ws-123",
		"session_id":   "sess-456",
		"message":      bigMsg,
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "too large")
}

func TestSessionMessage_APIError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("SendMessage", ctx, "ws-123", "sess-456", "hello", 300*time.Second).
		Return("", assert.AnError)

	result, err := h.sessionMessage(ctx, makeReq("session_message", map[string]any{
		"workspace_id": "ws-123",
		"session_id":   "sess-456",
		"message":      "hello",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	mockClient.AssertExpectations(t)
}

// ===== session_history =====

func TestSessionHistory_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("GetHistory", ctx, "ws-123", "sess-456").Return([]Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "Hi there!"},
	}, nil)

	result, err := h.sessionHistory(ctx, makeReq("session_history", map[string]any{
		"workspace_id": "ws-123",
		"session_id":   "sess-456",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	text := result.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "hello")
	assert.Contains(t, text, "Hi there!")
	mockClient.AssertExpectations(t)
}

func TestSessionHistory_MissingSessionID(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.sessionHistory(context.Background(), makeReq("session_history", map[string]any{
		"workspace_id": "ws-123",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// ===== credential_create =====

func TestCredentialCreate_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateCredential", ctx, CreateCredentialReq{
		Name:   "My Anthropic",
		Kind:   "anthropic",
		Slug:   "my-anthropic",
		APIKey: "sk-test",
	}).Return(&CredentialResp{ID: "cred-9", Name: "My Anthropic", Type: "llm-provider"}, nil)

	result, err := h.credentialCreate(ctx, makeReq("credential_create", map[string]any{
		"kind":    "anthropic",
		"slug":    "my-anthropic",
		"api_key": "sk-test",
		"name":    "My Anthropic",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "cred-9")
	mockClient.AssertExpectations(t)
}

func TestCredentialCreate_PassesOptionalFields(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateCredential", ctx, CreateCredentialReq{
		Name:        "",
		Kind:        "openai_compatible",
		Slug:        "litellm-prod",
		APIKey:      "sk-test",
		BaseURL:     "https://litellm.example.test/v1",
		Default:     "openai/gpt-oss",
		WorkspaceID: "ws-123",
	}).Return(&CredentialResp{ID: "cred-9", Type: "llm-provider"}, nil)

	_, err := h.credentialCreate(ctx, makeReq("credential_create", map[string]any{
		"kind":          "openai_compatible",
		"slug":          "litellm-prod",
		"api_key":       "sk-test",
		"base_url":      "https://litellm.example.test/v1",
		"default_model": "openai/gpt-oss",
		"workspace_id":  "ws-123",
	}))

	require.NoError(t, err)
	mockClient.AssertExpectations(t)
}

func TestCredentialCreate_MissingKind(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.credentialCreate(context.Background(), makeReq("credential_create", map[string]any{
		"slug":    "my-anthropic",
		"api_key": "sk-test",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "kind")
}

func TestCredentialCreate_MissingSlug(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.credentialCreate(context.Background(), makeReq("credential_create", map[string]any{
		"kind":    "anthropic",
		"api_key": "sk-test",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "slug")
}

func TestCredentialCreate_MissingAPIKey(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.credentialCreate(context.Background(), makeReq("credential_create", map[string]any{
		"kind": "anthropic",
		"slug": "my-anthropic",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "api_key")
}

func TestCredentialCreate_APIError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateCredential", ctx, CreateCredentialReq{
		Kind: "anthropic", Slug: "my-anthropic", APIKey: "sk-test",
	}).Return((*CredentialResp)(nil), fmt.Errorf("400 invalid metadata: kind is required"))

	result, err := h.credentialCreate(ctx, makeReq("credential_create", map[string]any{
		"kind": "anthropic", "slug": "my-anthropic", "api_key": "sk-test",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "failed to create credential")
	mockClient.AssertExpectations(t)
}

// TestValidCredentialKinds_MatchesSecretsValidKinds pins the local
// validCredentialKinds slice (used by the credential_create tool schema) to
// the canonical pkg/secrets.ValidKinds. The MCP server binary deliberately
// does not import pkg/secrets, so the enum is mirrored locally; this
// test-only import is the drift gate that keeps the two in lockstep.
func TestValidCredentialKinds_MatchesSecretsValidKinds(t *testing.T) {
	assert.Equal(t, secrets.ValidKinds, validCredentialKinds,
		"validCredentialKinds must mirror secrets.ValidKinds exactly; update both together")
}

// ===== NewServer integration =====

func TestNewServer_RegistersAllTools(t *testing.T) {
	mockClient := &MockAPIClient{}
	srv := NewServer(mockClient, 300*time.Second)
	require.NotNil(t, srv)
}

// ===== session_question_reply =====

func TestSessionQuestionReply_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("QuestionReply", ctx, "ws-123", "que_abc123", [][]string{{"yes"}}).Return(nil)

	result, err := h.sessionQuestionReply(ctx, makeReq("session_question_reply", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc123",
		"answers":      `[["yes"]]`,
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "answered")
	mockClient.AssertExpectations(t)
}

func TestSessionQuestionReply_InvalidAnswersJSON(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.sessionQuestionReply(context.Background(), makeReq("session_question_reply", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc123",
		"answers":      "not-json",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "JSON array")
}

func TestSessionQuestionReply_MissingArgs(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.sessionQuestionReply(context.Background(), makeReq("session_question_reply", map[string]any{
		"workspace_id": "ws-123",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// ===== session_question_reject =====

func TestSessionQuestionReject_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("QuestionReject", ctx, "ws-123", "que_abc123").Return(nil)

	result, err := h.sessionQuestionReject(ctx, makeReq("session_question_reject", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc123",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "rejected")
	mockClient.AssertExpectations(t)
}

// ===== session_permission_reply =====

func TestSessionPermissionReply_HappyPath(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("PermissionReply", ctx, "ws-123", "per_abc_123", "always", "").Return(nil)

	result, err := h.sessionPermissionReply(ctx, makeReq("session_permission_reply", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "per_abc_123",
		"reply":        "always",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "always")
	mockClient.AssertExpectations(t)
}

func TestSessionPermissionReply_InvalidReply(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.sessionPermissionReply(context.Background(), makeReq("session_permission_reply", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "per_abc_123",
		"reply":        "maybe",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "'once', 'always', or 'reject'")
}

// ===== run_resolve (US-65.7) =====

func TestRunResolve_QuestionReply(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("QuestionReply", ctx, "ws-123", "que_abc", [][]string{{"yes"}}).Return(nil)

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc",
		"reply":        `[["yes"]]`,
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "answered")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_QuestionReject(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("QuestionReject", ctx, "ws-123", "que_abc").Return(nil)

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc",
		"reply":        "reject",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "rejected")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_PermissionAllow(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("PermissionReply", ctx, "ws-123", "per_abc", "always", "").Return(nil)

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "per_abc",
		"reply":        "always",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "always")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_PermissionReject(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("PermissionReply", ctx, "ws-123", "per_abc", "reject", "").Return(nil)

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "per_abc",
		"reply":        "reject",
	}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "reject")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_InvalidRequestIDPrefix(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.runResolve(context.Background(), makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "unknown_abc",
		"reply":        "whatever",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "must start with 'que_'")
}

func TestRunResolve_MissingArgs(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.runResolve(context.Background(), makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestRunResolve_QuestionInvalidJSON(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.runResolve(context.Background(), makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc",
		"reply":        "not-json",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "JSON array")
}

func TestRunResolve_QuestionReplyError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("QuestionReply", ctx, "ws-123", "que_abc", [][]string{{"yes"}}).
		Return(fmt.Errorf("agent pod unreachable"))

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc",
		"reply":        `[["yes"]]`,
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "failed to reply to question")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_QuestionRejectError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("QuestionReject", ctx, "ws-123", "que_abc").
		Return(fmt.Errorf("network error"))

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "que_abc",
		"reply":        "reject",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "failed to reject question")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_PermissionReplyError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("PermissionReply", ctx, "ws-123", "per_abc", "once", "").
		Return(fmt.Errorf("timeout"))

	result, err := h.runResolve(ctx, makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "per_abc",
		"reply":        "once",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "failed to reply to permission")
	mockClient.AssertExpectations(t)
}

func TestRunResolve_PermissionInvalidReply(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.runResolve(context.Background(), makeReq("run_resolve", map[string]any{
		"workspace_id": "ws-123",
		"request_id":   "per_abc",
		"reply":        "maybe",
	}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "'once', 'always', or 'reject'")
}

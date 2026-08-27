// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// MCP client ↔ production-router integration gate (#1037).
//
// Drives pkg/mcp.HTTPClient (and the MCP tool layer built on it) against
// the REAL route registration from NewRouter — the unit tests in pkg/mcp
// mock the HTTP layer at whatever path the client requests, so path/
// method/body drift between the client and the router is invisible to
// them. That drift is how #1033–#1036 shipped: session_create 404s,
// session_message subscribes to a removed SSE route, trigger_update
// sends a string where the API binds *bool, workflow_update always sends
// every field. This file is the gate that would have caught all four.
//
// Scope: the surfaces pkg/mcp actually consumes — workspace lifecycle
// (WorkspaceService), sessions/new + proxy (ProxyHandler + adapter),
// question/permission replies, and the Epic 64 workflow/trigger CRUD.
// Routes whose handlers need service fakes that do not exist yet
// (secrets, models, credentials) are wired in the OpenAPI contract
// fixture only and remain route-presence-checked there; full-handler
// wiring for every route is tracked separately in #1043.
//
// Fixture pattern: production NewRouter + mocked services (testify)
// + in-memory workflow/trigger stores that capture the bound request
// structs, so assertions prove real json binding against pkg/types —
// not just route resolution.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	llmv1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	mcppkg "github.com/lenaxia/llmsafespaces/pkg/mcp"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

const (
	mcpTestUserID   = "user-1"
	mcpTestWSID     = "ws-1"
	mcpTestSession  = "ses_new_1"
	mcpTestPassword = "test-pw"
)

// --- In-memory fakes for the Epic 64 handlers' store interfaces ---

// fakeWorkflowStore implements the handlers' workflowStore interface
// (unexported; satisfied structurally). Only the methods exercised by
// the MCP surface carry real behavior; the rest return zero values.
type fakeWorkflowStore struct {
	mu        sync.Mutex
	workflows map[string]*wf.WorkflowRow
	runs      map[string]*wf.WorkflowRunRow
	nextRun   int
	updates   []*wf.WorkflowUpdate
}

func newFakeWorkflowStore() *fakeWorkflowStore {
	return &fakeWorkflowStore{workflows: map[string]*wf.WorkflowRow{}, runs: map[string]*wf.WorkflowRunRow{}}
}

func (s *fakeWorkflowStore) CreateWorkflow(_ context.Context, row *wf.WorkflowRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.workflows[row.ID]; exists {
		return fmt.Errorf("unique violation")
	}
	if row.ID == "" {
		row.ID = fmt.Sprintf("wfl_%d", len(s.workflows)+1)
	}
	s.workflows[row.ID] = row
	return nil
}

func (s *fakeWorkflowStore) ListWorkflows(_ context.Context, ownerType, ownerID string) ([]*wf.WorkflowRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*wf.WorkflowRow
	for _, r := range s.workflows {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeWorkflowStore) GetWorkflow(_ context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (s *fakeWorkflowStore) UpdateWorkflow(_ context.Context, ownerType, ownerID, workflowID string, upd *wf.WorkflowUpdate) (*wf.WorkflowRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	if upd != nil && upd.Name != nil {
		r.Name = *upd.Name
	}
	if upd != nil && upd.Status != nil {
		r.Status = *upd.Status
	}
	if upd != nil && upd.SpecYAML != nil {
		r.SpecYAML = *upd.SpecYAML
	}
	s.updates = append(s.updates, upd)
	return r, nil
}

func (s *fakeWorkflowStore) DeleteWorkflow(_ context.Context, ownerType, ownerID, workflowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workflows, workflowID)
	return nil
}

func (s *fakeWorkflowStore) CountWorkflowsByOwner(ctx context.Context, ownerType, ownerID string) (int, error) {
	rows, err := s.ListWorkflows(ctx, ownerType, ownerID)
	return len(rows), err
}

func (s *fakeWorkflowStore) CreateWorkflowRun(_ context.Context, row *wf.WorkflowRunRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun++
	if row.ID == "" {
		row.ID = fmt.Sprintf("run_%d", s.nextRun)
	}
	s.runs[row.ID] = row
	return nil
}

func (s *fakeWorkflowStore) GetWorkflowRun(_ context.Context, runID string) (*wf.WorkflowRunRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (s *fakeWorkflowStore) UpdateWorkflowRunStatus(_ context.Context, runID, status string, _ *string, _ json.RawMessage, _ json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return wf.ErrNotFound
	}
	r.Status = status
	return nil
}

func (s *fakeWorkflowStore) ListWorkflowRuns(_ context.Context, workflowID string, _, _ int) ([]*wf.WorkflowRunRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*wf.WorkflowRunRow
	for _, r := range s.runs {
		if r.WorkflowID == workflowID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeWorkflowStore) ListNodeRuns(_ context.Context, _ string) ([]*wf.WorkflowNodeRunRow, error) {
	return nil, nil
}

func (s *fakeWorkflowStore) ListWorkflowRunsByWorkspace(_ context.Context, _ string) ([]*wf.WorkflowRunRow, error) {
	return nil, nil
}

func (s *fakeWorkflowStore) ListSessionOrigins(_ context.Context, _ string) ([]*wf.SessionOriginRow, error) {
	return nil, nil
}

// fakeTriggerStore implements the handlers' triggerStore interface.
type fakeTriggerStore struct {
	mu       sync.Mutex
	triggers map[string]*wf.TriggerRow
	updates  []*wf.TriggerUpdate
}

func newFakeTriggerStore() *fakeTriggerStore {
	return &fakeTriggerStore{triggers: map[string]*wf.TriggerRow{}}
}

func (s *fakeTriggerStore) CreateTrigger(_ context.Context, row *wf.TriggerRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.ID == "" {
		row.ID = fmt.Sprintf("trg_%d", len(s.triggers)+1)
	}
	s.triggers[row.ID] = row
	return nil
}

func (s *fakeTriggerStore) ListTriggers(_ context.Context, ownerType, ownerID string) ([]*wf.TriggerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*wf.TriggerRow
	for _, r := range s.triggers {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeTriggerStore) GetTrigger(_ context.Context, ownerType, ownerID, triggerID string) (*wf.TriggerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (s *fakeTriggerStore) UpdateTrigger(_ context.Context, ownerType, ownerID, triggerID string, upd *wf.TriggerUpdate) (*wf.TriggerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	if upd != nil && upd.Enabled != nil {
		r.Enabled = *upd.Enabled
	}
	if upd != nil && upd.Name != nil {
		r.Name = *upd.Name
	}
	s.updates = append(s.updates, upd)
	return r, nil
}

func (s *fakeTriggerStore) DeleteTrigger(_ context.Context, _, _, triggerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.triggers, triggerID)
	return nil
}

func (s *fakeTriggerStore) CountTriggersByOwner(ctx context.Context, ownerType, ownerID string) (int, error) {
	rows, err := s.ListTriggers(ctx, ownerType, ownerID)
	return len(rows), err
}

func (s *fakeTriggerStore) CreateWebhook(_ context.Context, _ *wf.WebhookRow) error { return nil }
func (s *fakeTriggerStore) GetWebhookByTriggerID(_ context.Context, _ string) (*wf.WebhookRow, error) {
	return nil, wf.ErrNotFound
}
func (s *fakeTriggerStore) UpdateWebhookSecret(_ context.Context, _ string, _ []byte, _ int) error {
	return nil
}
func (s *fakeTriggerStore) ListTriggerFires(_ context.Context, _ string, _, _ int) ([]*wf.TriggerFireRow, error) {
	return nil, nil
}

// fakeQuotaChecker satisfies the handlers' workflowQuotaChecker with an
// effectively-unbounded limit.
type fakeQuotaChecker struct{}

func (fakeQuotaChecker) GetInt(_ context.Context, _ string) (int, error) { return 1_000_000, nil }

// noopTriggerEncryptor satisfies triggerEncryptor.
type noopTriggerEncryptor struct{}

func (noopTriggerEncryptor) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

// --- Fixture ---

type mcpFixture struct {
	apiSrv    *httptest.Server
	client    *mcppkg.HTTPClient
	broker    *eventbroker.UserEventBroker
	wfStore   *fakeWorkflowStore
	trgStore  *fakeTriggerStore
	agentSrv  *httptest.Server // stub opencode pod
	promptGot chan map[string]any
}

func (f *mcpFixture) lastWorkflowUpdate() *wf.WorkflowUpdate {
	f.wfStore.mu.Lock()
	defer f.wfStore.mu.Unlock()
	if len(f.wfStore.updates) == 0 {
		return nil
	}
	return f.wfStore.updates[len(f.wfStore.updates)-1]
}

func (f *mcpFixture) lastTriggerUpdate() *wf.TriggerUpdate {
	f.trgStore.mu.Lock()
	defer f.trgStore.mu.Unlock()
	if len(f.trgStore.updates) == 0 {
		return nil
	}
	return f.trgStore.updates[len(f.trgStore.updates)-1]
}

func mcpPasswordSecret(workspaceID, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("workspace-pw-%s", workspaceID),
			Namespace: "default",
		},
		Data: map[string][]byte{"password": []byte(password)},
	}
}

func newMCPRouterFixture(t *testing.T) *mcpFixture {
	t.Helper()

	// Stub opencode pod: answers the adapter's V1 send with a completed
	// assistant message, history with an array (the adapter streams a
	// JSON array), and records the prompt body it received.
	promptGot := make(chan map[string]any, 8)
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			select {
			case promptGot <- body:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1","time":{"created":1786400000000}},"parts":[{"type":"text","text":"ok"}]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/message"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"info":{"role":"assistant","id":"msg_1","time":{"created":1786400000000}},"parts":[{"type":"text","text":"ok"}]}]`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(agentSrv.Close)
	backendHost, backendPortStr, ok := strings.Cut(strings.TrimPrefix(agentSrv.URL, "http://"), ":")
	require.True(t, ok, "backend URL must contain a port: %s", agentSrv.URL)
	backendPort, err := strconv.Atoi(backendPortStr)
	require.NoError(t, err)

	// Kubernetes mock: workspace CRD is Active with the stub pod's IP;
	// the Clientset fake carries the pod password secret.
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	wsCRD := &llmv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: mcpTestWSID, Namespace: "default"},
		Status:     llmv1.WorkspaceStatus{Phase: llmv1.WorkspacePhaseActive, PodIP: backendHost},
	}
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, mcpTestWSID, metav1.GetOptions{}).Return(wsCRD, nil).Maybe()

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)
	_, err = fakeClientset.CoreV1().Secrets("default").Create(context.Background(), mcpPasswordSecret(mcpTestWSID, mcpTestPassword), metav1.CreateOptions{})
	require.NoError(t, err)

	// Workspace service mock: ownership passes, sessions/new returns the
	// production EnsureSessionResponse shape (sessionId, camelCase).
	ws := &imocks.MockWorkspaceService{}
	ws.On("ResolveWorkspace", mock.Anything, mcpTestWSID).Return(&types.WorkspaceMetadata{
		ID: mcpTestWSID, UserID: mcpTestUserID, Name: "test", Runtime: "python:3.10",
	}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mcpTestUserID, mock.Anything).Return(nil).Maybe()
	ws.On("EnsureSession", mock.Anything, mcpTestUserID, mcpTestWSID).Return(&types.EnsureSessionResponse{
		WorkspaceID: mcpTestWSID, WorkspacePhase: "Active", SessionID: mcpTestSession, Resumed: true,
	}, nil).Maybe()
	ws.On("CreateWorkspace", mock.Anything, mcpTestUserID, mock.Anything).Return(&types.Workspace{
		ID: "ws-created", Name: "created", Runtime: "python:3.10", Phase: "Pending",
	}, nil).Maybe()
	ws.On("ActivateWorkspace", mock.Anything, mcpTestUserID, mcpTestWSID).Return(&types.ActivateWorkspaceResponse{
		Resumed: mcpTestWSID,
	}, nil).Maybe()
	ws.On("SuspendWorkspace", mock.Anything, mcpTestUserID, mcpTestWSID).Return(nil).Maybe()
	ws.On("RefreshWorkspaceCompute", mock.Anything, mcpTestUserID, mcpTestWSID).Return(&types.RefreshWorkspaceResult{
		RestartGeneration: 2,
	}, nil).Maybe()

	// Auth middleware: authenticated as mcpTestUserID on every request.
	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(ginHandlerSettingUserID(mcpTestUserID))
	auth.On("GetUserID", mock.Anything).Return(mcpTestUserID)

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &contractMockServices{auth: auth, met: met, ws: ws}

	// Proxy handler with the opencode adapter pointed at the stub pod.
	log := mcpTestLogger(t)
	proxy, err := handlers.NewProxyHandler(k8sMock, log, "default", nil, &opencode.Dialect{})
	require.NoError(t, err)
	adapter := opencode.NewAdapter(
		proxy.AdapterPasswordResolver(),
		proxy.AdapterPodIPResolver(),
		nil,
		opencode.WithAdapterHTTPClient(agentSrv.Client()),
		opencode.WithAdapterPort(backendPort),
	)
	proxy.SetAdapter(adapter)
	broker := eventbroker.NewUserEventBroker()
	proxy.SetUserBrokerForTest(broker)

	// Epic 64 handlers over in-memory stores.
	wfStore := newFakeWorkflowStore()
	trgStore := newFakeTriggerStore()
	cfg := RouterConfig{
		UserWorkflowsHandler: handlers.NewUserWorkflowsHandler(wfStore, fakeQuotaChecker{}),
		UserTriggersHandler:  handlers.NewUserTriggersHandler(trgStore, fakeQuotaChecker{}, noopTriggerEncryptor{}),
	}

	ginSetTestMode()
	router := NewRouter(svc, log, proxy, cfg)
	apiSrv := httptest.NewServer(router)
	t.Cleanup(apiSrv.Close)

	return &mcpFixture{
		apiSrv:    apiSrv,
		broker:    broker,
		wfStore:   wfStore,
		trgStore:  trgStore,
		agentSrv:  agentSrv,
		promptGot: promptGot,
		client: &mcppkg.HTTPClient{
			BaseURL:    apiSrv.URL,
			HTTPClient: apiSrv.Client(),
		},
	}
}

// callMCPTool drives one tool through the in-process MCP server built on
// the REAL HTTPClient — tool schema → handler → client → router →
// handler → store, the entire chain.
func callMCPTool(t *testing.T, apiClient mcppkg.APIClient, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	srv := mcppkg.NewServer(apiClient, 10*time.Second)
	cli, err := mcpclient.NewInProcessClient(srv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, cli.Start(ctx))

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "mcp-router-test", Version: "1.0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)

	result, err := cli.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	require.NoError(t, err)
	return result
}

func toolText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.False(t, result.IsError, "tool errored: %s", toolResultText(result))
	return toolResultText(result)
}

func toolResultText(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	if tc, ok := result.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// --- RED tests: reproduce #1033–#1036 against the production router ---

// TestMCPClientSessionCreate pins #1033: CreateSession must hit
// POST /workspaces/:id/sessions/new and decode the sessionId field of
// types.EnsureSessionResponse. The pre-fix client posted /sessions
// (404) and decoded `id` (empty even on the right route).
func TestMCPClientSessionCreate_ResolvesProductionRoute(t *testing.T) {
	f := newMCPRouterFixture(t)

	resp, err := f.client.CreateSession(context.Background(), mcpTestWSID)
	require.NoError(t, err, "CreateSession must resolve on the production router")
	require.NotNil(t, resp)
	assert.Equal(t, mcpTestSession, resp.ID,
		"CreateSession must decode the sessionId field of EnsureSessionResponse")
}

// TestMCPClientSessionMessage_QuestionEventViaSSE pins #1034 (and the
// prompt-body half of the same flow): session_message must deliver a
// parts-based prompt body the /prompt handler accepts, subscribe to the
// live /session-events route, and surface a structured question result
// when the broker emits agent.question for the session. The pre-fix
// client sent {"message": ...} (rejected: "text must not be empty")
// and subscribed to the removed /events route.
func TestMCPClientSessionMessage_QuestionEventViaSSE(t *testing.T) {
	f := newMCPRouterFixture(t)

	done := make(chan string, 1)
	go func() {
		out, err := f.client.SendMessage(context.Background(), mcpTestWSID, mcpTestSession, "please continue", 10*time.Second)
		if err != nil {
			done <- "ERROR: " + err.Error()
			return
		}
		done <- out
	}()

	// Wait for the SSE subscription to land on the broker, then emit an
	// agent.question event for the session — the structured-result path.
	require.Eventually(t, func() bool {
		return f.broker.WorkspaceSubscriberCount(mcpTestWSID) > 0
	}, 5*time.Second, 10*time.Millisecond, "client must subscribe to the workspace SSE stream")

	f.broker.PublishToWorkspace(mcpTestWSID, mcpSSEEvent("agent.question", map[string]any{
		"session_id": mcpTestSession,
		"id":         "que_abc123",
		"options":    []string{"yes", "no"},
	}, mcpTestSession, ""))

	select {
	case out := <-done:
		require.NotContains(t, out, "ERROR", "SendMessage must succeed end-to-end")
		var parsed struct {
			Type    string          `json:"type"`
			Request json.RawMessage `json:"request"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &parsed), "question result must be the structured JSON: %q", out)
		assert.Equal(t, "question", parsed.Type, "agent.question must produce a structured question result, got: %q", out)
		assert.Contains(t, string(parsed.Request), "que_abc123")
	case <-time.After(15 * time.Second):
		t.Fatal("SendMessage did not return after question event")
	}

	// The prompt leg must have delivered a parts-based text body the
	// handler accepted (pre-fix: {"message": ...} → 400 empty text).
	select {
	case body := <-f.promptGot:
		require.NotEmpty(t, body, "prompt body must reach the agent pod")
		parts, ok := body["parts"].([]any)
		require.True(t, ok, "prompt body must be parts-shaped, got: %v", body)
		require.Len(t, parts, 1)
		part, _ := parts[0].(map[string]any)
		assert.Equal(t, "text", part["type"])
		assert.Equal(t, "please continue", part["text"])
	default:
		t.Fatal("prompt never reached the agent pod — the /prompt leg of session_message is broken")
	}
}

// TestMCPClientSessionMessage_IdleTermination pins the SSE completion
// contract of session_message: session.status=idle for the target
// session terminates the wait and the final assistant text is returned
// via the history fallback.
func TestMCPClientSessionMessage_IdleTermination(t *testing.T) {
	f := newMCPRouterFixture(t)

	done := make(chan string, 1)
	go func() {
		out, err := f.client.SendMessage(context.Background(), mcpTestWSID, mcpTestSession, "hi", 10*time.Second)
		if err != nil {
			done <- "ERROR: " + err.Error()
			return
		}
		done <- out
	}()

	require.Eventually(t, func() bool {
		return f.broker.WorkspaceSubscriberCount(mcpTestWSID) > 0
	}, 5*time.Second, 10*time.Millisecond)

	f.broker.PublishToWorkspace(mcpTestWSID, mcpSSEEvent("session.status", nil, mcpTestSession, "idle"))

	select {
	case out := <-done:
		require.NotContains(t, out, "ERROR")
		assert.Equal(t, "ok", out, "idle-terminated send returns the final assistant text via history fallback")
	case <-time.After(15 * time.Second):
		t.Fatal("SendMessage did not return after idle event")
	}
}

// TestMCPClientSessionMessage_LiveContentFromSessionEvents pins #1053:
// streamed part.delta content arrives inside session.event envelopes and
// must be captured live — the old read of a top-level "content" field
// (which the broker envelope never carries) meant live accumulation was
// dead code and every response fell through to the history round-trip.
func TestMCPClientSessionMessage_LiveContentFromSessionEvents(t *testing.T) {
	f := newMCPRouterFixture(t)

	done := make(chan string, 1)
	go func() {
		out, err := f.client.SendMessage(context.Background(), mcpTestWSID, mcpTestSession, "hi", 10*time.Second)
		if err != nil {
			done <- "ERROR: " + err.Error()
			return
		}
		done <- out
	}()

	require.Eventually(t, func() bool {
		return f.broker.WorkspaceSubscriberCount(mcpTestWSID) > 0
	}, 5*time.Second, 10*time.Millisecond)

	// Stream two deltas as real contract events inside session.event
	// envelopes — exactly what publishClientEvents puts on the wire.
	for _, chunk := range []string{"Hello ", "live world!"} {
		f.broker.PublishToWorkspace(mcpTestWSID, apitypes.WorkspaceSSEEvent{
			Type:      "session.event",
			SessionID: mcpTestSession,
			Data: map[string]any{
				"type":      "part.delta",
				"sessionId": mcpTestSession,
				"delta":     chunk,
			},
		})
	}
	f.broker.PublishToWorkspace(mcpTestWSID, mcpSSEEvent("session.status", nil, mcpTestSession, "idle"))

	select {
	case out := <-done:
		require.NotContains(t, out, "ERROR")
		assert.Equal(t, "Hello live world!", out,
			"streamed part.delta content must be captured live from session.event data (#1053)")
	case <-time.After(15 * time.Second):
		t.Fatal("SendMessage did not return after idle event")
	}
}

// TestMCPRouterTriggerUpdate_EnabledBoolBinds pins #1035: the
// trigger_update tool must declare enabled as boolean and the client
// must send {"enabled": <bool>} — the API binds
// types.UpdateTriggerRequest.Enabled *bool. The pre-fix tool declared a
// string and the client always sent one, so every call 400'd.
func TestMCPRouterTriggerUpdate_EnabledBoolBinds(t *testing.T) {
	f := newMCPRouterFixture(t)
	seedTrigger(t, f, "trg_1")

	result := callMCPTool(t, f.client, "trigger_update", map[string]any{
		"trigger_id": "trg_1",
		"enabled":    true,
	})
	out := toolText(t, result)
	assert.Contains(t, out, "trg_1", "trigger_update must succeed against the production router: %s", out)

	upd := f.lastTriggerUpdate()
	require.NotNil(t, upd, "UpdateTrigger must reach the store")
	require.NotNil(t, upd.Enabled, "enabled must bind as *bool (nil = keep existing)")
	assert.True(t, *upd.Enabled)
}

// TestMCPRouterTriggerUpdate_EnabledOmittedKeepsExisting pins the
// partial-update semantics: omitted enabled sends no key at all (nil =
// keep existing), not {"enabled": ""}.
func TestMCPRouterTriggerUpdate_EnabledOmittedKeepsExisting(t *testing.T) {
	f := newMCPRouterFixture(t)
	seedTrigger(t, f, "trg_2")

	result := callMCPTool(t, f.client, "trigger_update", map[string]any{
		"trigger_id": "trg_2",
	})
	out := toolText(t, result)
	assert.Contains(t, out, "trg_2")

	upd := f.lastTriggerUpdate()
	require.NotNil(t, upd)
	assert.Nil(t, upd.Enabled, "omitted enabled must not be sent (nil = keep existing)")
}

// TestMCPRouterWorkflowUpdate_PartialUpdate pins #1036: workflow_update
// with only status must send ONLY {"status": "active"}. The pre-fix
// client always sent name/status/specYaml with empty strings for
// omitted args, so every partial update 400'd (invalid workflow name).
func TestMCPRouterWorkflowUpdate_PartialUpdate(t *testing.T) {
	f := newMCPRouterFixture(t)
	seedWorkflow(t, f, "wfl_1")

	result := callMCPTool(t, f.client, "workflow_update", map[string]any{
		"workflow_id": "wfl_1",
		"status":      "active",
	})
	out := toolText(t, result)
	assert.Contains(t, out, "wfl_1", "partial workflow_update must succeed: %s", out)

	upd := f.lastWorkflowUpdate()
	require.NotNil(t, upd, "UpdateWorkflow must reach the store")
	assert.Nil(t, upd.Name, "omitted name must stay nil (empty string fails validation)")
	assert.Nil(t, upd.SpecYAML, "omitted spec_yaml must stay nil")
	require.NotNil(t, upd.Status)
	assert.Equal(t, "active", *upd.Status)
}

// TestMCPRouterWorkflowUpdate_AllFields pins the all-fields path of the
// same tool.
func TestMCPRouterWorkflowUpdate_AllFields(t *testing.T) {
	f := newMCPRouterFixture(t)
	seedWorkflow(t, f, "wfl_2")

	result := callMCPTool(t, f.client, "workflow_update", map[string]any{
		"workflow_id": "wfl_2",
		"name":        "renamed-flow",
		"status":      "archived",
	})
	out := toolText(t, result)
	assert.Contains(t, out, "wfl_2", out)

	upd := f.lastWorkflowUpdate()
	require.NotNil(t, upd)
	require.NotNil(t, upd.Name)
	assert.Equal(t, "renamed-flow", *upd.Name)
	require.NotNil(t, upd.Status)
	assert.Equal(t, "archived", *upd.Status)
}

// --- GREEN pins: route/method/body matrix for the remaining MCP surface ---

// TestMCPClientWorkspaceLifecycle drives create → activate → refresh →
// suspend through the production router with the mocked
// WorkspaceService.
func TestMCPClientWorkspaceLifecycle(t *testing.T) {
	f := newMCPRouterFixture(t)
	ctx := context.Background()

	created, err := f.client.CreateWorkspace(ctx, mcppkg.CreateWorkspaceReq{Runtime: "python:3.10", Name: "test"})
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, "ws-created", created.ID)
	assert.Equal(t, "python:3.10", created.Runtime)

	act, err := f.client.ActivateWorkspace(ctx, mcpTestWSID)
	require.NoError(t, err)
	assert.Equal(t, mcpTestWSID, act.Resumed)

	refresh, err := f.client.RefreshWorkspace(ctx, mcpTestWSID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), refresh.RestartGeneration)

	require.NoError(t, f.client.SuspendWorkspace(ctx, mcpTestWSID))
}

// TestMCPClientWorkflowAndTriggerCRUD sweeps the remaining Epic 64
// client methods: each must resolve on the production router with the
// right method and a body that binds.
func TestMCPClientWorkflowAndTriggerCRUD(t *testing.T) {
	f := newMCPRouterFixture(t)
	ctx := context.Background()

	created := callMCPTool(t, f.client, "workflow_create", map[string]any{
		"name":      "sweep-flow",
		"spec_yaml": `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"def handler(i): return {}"}}],"edges":[]}`,
	})
	out := toolText(t, created)
	var wfResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &wfResp), out)
	require.NotEmpty(t, wfResp.ID)
	assert.Equal(t, "draft", wfResp.Status)

	listed, err := f.client.ListWorkflows(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(listed), "sweep-flow")

	got, err := f.client.GetWorkflow(ctx, wfResp.ID)
	require.NoError(t, err)
	assert.Contains(t, string(got), "sweep-flow")

	run, err := f.client.RunWorkflow(ctx, wfResp.ID, `{"x":1}`, mcpTestWSID)
	require.NoError(t, err)
	var runResp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(run), &runResp), string(run))
	require.NotEmpty(t, runResp.ID)

	status, err := f.client.GetWorkflowRunStatus(ctx, runResp.ID)
	require.NoError(t, err)
	assert.Contains(t, string(status), "queued")

	require.NoError(t, f.client.CancelWorkflowRun(ctx, runResp.ID))

	// Triggers
	trgCreated := callMCPTool(t, f.client, "trigger_create", map[string]any{
		"name":          "sweep-trigger",
		"source_type":   "cron",
		"source_config": `{"expr":"0 9 * * *","tz":"UTC"}`,
		"workspace_id":  mcpTestWSID,
		"prompt":        "standup",
	})
	trgOut := toolText(t, trgCreated)
	assert.Contains(t, trgOut, "sweep-trigger")

	triggers, err := f.client.ListTriggers(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(triggers), "sweep-trigger")

	// Extract the trigger id from the list for delete.
	var trgList struct {
		Triggers []struct {
			ID string `json:"id"`
		} `json:"triggers"`
	}
	require.NoError(t, json.Unmarshal([]byte(triggers), &trgList))
	require.NotEmpty(t, trgList.Triggers)
	require.NoError(t, f.client.DeleteTrigger(ctx, trgList.Triggers[0].ID))
}

// TestMCPClientQuestionAndPermissionReply pins the Epic 16 input-reply
// routes: the right method + path on the production router, through the
// auth + ownership middleware. The replies are opaque pass-throughs to
// the pod (no API-side body binding), and the legacy proxy leg dials the
// fixed agent port — unreachable from this fixture — so the assertion
// level is route resolution: anything but 404/405/401 proves the route
// exists, the method matches, and the ID validation passed.
func TestMCPClientQuestionAndPermissionReply(t *testing.T) {
	f := newMCPRouterFixture(t)
	ctx := context.Background()

	assertRouteResolved(t, func() error {
		return f.client.QuestionReply(ctx, mcpTestWSID, "que_abc123", [][]string{{"yes"}})
	})
	assertRouteResolved(t, func() error {
		return f.client.QuestionReject(ctx, mcpTestWSID, "que_abc123")
	})
	assertRouteResolved(t, func() error {
		return f.client.PermissionReply(ctx, mcpTestWSID, "per_xyz1", "once", "")
	})
}

func assertRouteResolved(t *testing.T, call func() error) {
	t.Helper()
	err := call()
	if err == nil {
		return
	}
	msg := err.Error()
	assert.NotContains(t, msg, "API error 404", "route must exist on the production router")
	assert.NotContains(t, msg, "API error 405", "method must match the production router")
	assert.NotContains(t, msg, "API error 400", "request ID validation must accept the client's IDs")
	assert.NotContains(t, msg, "API error 401", "auth middleware must accept the fixture identity")
}

// TestMCPClientHistoryResolves pins the history route the client uses
// (GET /sessions/:sid/message) — the same route the SendMessage
// fallback depends on.
func TestMCPClientHistoryResolves(t *testing.T) {
	f := newMCPRouterFixture(t)

	_, err := f.client.GetHistory(context.Background(), mcpTestWSID, mcpTestSession)
	require.NoError(t, err, "GetHistory must resolve on the production router")
}

// --- helpers ---

func seedWorkflow(t *testing.T, f *mcpFixture, id string) {
	t.Helper()
	err := f.wfStore.CreateWorkflow(context.Background(), &wf.WorkflowRow{
		ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: mcpTestUserID,
		Name: "seed-flow", Slug: "seed-flow", Status: types.WorkflowStatusDraft,
	})
	require.NoError(t, err)
}

func seedTrigger(t *testing.T, f *mcpFixture, id string) {
	t.Helper()
	err := f.trgStore.CreateTrigger(context.Background(), &wf.TriggerRow{
		ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: mcpTestUserID,
		Name: "seed-trigger", Enabled: false, SourceType: "cron",
	})
	require.NoError(t, err)
}

// mcpSSEEvent builds a broker WorkspaceSSEEvent with optional
// session/status fields.
func mcpSSEEvent(eventType string, data map[string]any, sessionID, status string) apitypes.WorkspaceSSEEvent {
	return apitypes.WorkspaceSSEEvent{
		Type:      eventType,
		Data:      data,
		SessionID: sessionID,
		Status:    status,
	}
}

func ginHandlerSettingUserID(userID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("userID", userID)
		c.Next()
	}
}

func mcpTestLogger(t *testing.T) *apilogger.Logger {
	t.Helper()
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)
	return log
}

func ginSetTestMode() {
	gin.SetMode(gin.TestMode)
}

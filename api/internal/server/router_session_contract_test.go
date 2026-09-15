// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// Session-surface contract conformance (#1304 review r1 f1 — the issue's
// mandatory live-router gate): drives raw HTTP through the production
// NewRouter with the REAL ProxyHandler wired to a fake agent.Adapter
// that returns canned values built from the pkg/session CONTRACT types,
// then validates every captured response body against the exact
// response-row schema in sdks/openapi.yaml (jsonschema v6).
//
// This is the test mocked-path drift is invisible to: the 202 receipt
// body, the 204 no-content rows, and the Session shape were all shipped
// by handlers while the spec said otherwise — route parity
// (TestOpenAPIRouterContract) is presence-only by design and caught
// none of it. Here, if the handler emits a status the spec has no row
// for, or a body the row's schema rejects, the case fails.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	llmv1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const (
	contractWSID  = "ws-contract"
	contractSID   = "ses_contract1"
	contractUID   = "user-contract"
	specResURL    = "mem://llmsafespaces-openapi.json"
	contractPodIP = "10.42.0.99"
)

// sessionContractAdapter fakes ONLY the session-surface methods; the
// embedded nil interface panics on any unexpected call, so a handler
// reaching for an unwired method fails the test loudly.
type sessionContractAdapter struct {
	agent.Adapter
}

func contractSession() *session.Session {
	started := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	return &session.Session{
		ID:           contractSID,
		WorkspaceID:  contractWSID,
		ParentID:     "ses_parent",
		Title:        "Conformance",
		AgentID:      "plan",
		Model:        &session.ModelRef{ID: "claude-sonnet-4.5", Provider: "anthropic"},
		Status:       session.StatusBusy,
		Cost:         &session.Cost{InputTokens: 120, OutputTokens: 80, TotalTokens: 200, CostUSD: 0.002},
		ContextUsage: &session.ContextUsage{Used: 45000, Window: 200000},
		Time:         &session.TimeRange{StartedAt: started},
		Summary:      "wire the seam",
	}
}

func contractMessages() []session.Message {
	created := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	return []session.Message{
		{ID: "msg_u1", SessionID: contractSID, Type: session.MessageUser, CreatedAt: &created, Text: "hello"},
		{ID: "msg_a1", SessionID: contractSID, Type: session.MessageAssistant, CreatedAt: &created, Parts: []session.Part{
			{Type: session.PartText, ID: "p1", Text: "working"},
			{Type: session.PartTool, ID: "p2", Tool: &session.ToolPart{
				CallID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`),
				State: session.ToolState{Status: session.ToolStatusCompleted},
			}},
			{Type: session.PartFileChange, ID: "p3", FileChange: &session.FileDiff{
				Path: "a.txt", Status: session.ChangeAdded, Patch: "--- /dev/null\n+++ b/a.txt\n",
			}},
		}},
	}
}

func (a *sessionContractAdapter) GetSession(_ context.Context, _, _, _ string) (*session.Session, error) {
	return contractSession(), nil
}

func (a *sessionContractAdapter) ListSessions(_ context.Context, _, _ string) ([]session.Session, error) {
	return []session.Session{*contractSession()}, nil
}

func (a *sessionContractAdapter) Send(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
	msgs := contractMessages()
	return &msgs[1], nil
}

func (a *sessionContractAdapter) GetHistory(_ context.Context, _, _, _ string) ([]session.Message, error) {
	return contractMessages(), nil
}

func (a *sessionContractAdapter) GetHistoryPage(_ context.Context, _, _, _ string, _ int) ([]session.Message, error) {
	return contractMessages(), nil
}

func (a *sessionContractAdapter) Abort(_ context.Context, _, _, _ string) error { return nil }

func (a *sessionContractAdapter) DeleteSession(_ context.Context, _, _, _ string) error { return nil }

func (a *sessionContractAdapter) ListPending(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
	return []session.InputRequest{
		{
			ID: "que_contract1", SessionID: contractSID, Kind: session.InputQuestion,
			Question: "Which language?", Header: "Pick one",
			Options:  []session.InputOption{{Label: "Go"}, {Label: "Python", Description: "batteries"}},
			Multiple: true, Custom: true,
		},
		{
			ID: "per_contract1", SessionID: contractSID, Kind: session.InputPermission,
			Permission: "bash", Patterns: []string{"/workspace/src/main.go"},
			Always: []string{"/workspace/*"}, Metadata: map[string]json.RawMessage{"command": json.RawMessage(`"go build"`)},
		},
	}, nil
}

// specCompiler loads sdks/openapi.yaml once per test into a jsonschema
// compiler. Schemas are compiled per case from their spec pointers, so
// a missing response row fails loudly instead of vacuously passing.
func specCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	data := specBytes(t)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(data, &doc))
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource(specResURL, doc))
	return c
}

func specBytes(t *testing.T) []byte {
	t.Helper()
	repoRoot, err := findRepoRoot()
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(repoRoot, "sdks", "openapi.yaml"))
	require.NoError(t, err)
	return data
}

// specRowSchema compiles the application/json schema of one response
// row: pathKey is the raw OpenAPI path (e.g. /workspaces/{id}/...), op
// is get/post/delete, status the documented code.
func specRowSchema(t *testing.T, c *jsonschema.Compiler, pathKey, op, status string) *jsonschema.Schema {
	t.Helper()
	esc := strings.ReplaceAll(pathKey, "/", "~1")
	ref := fmt.Sprintf("%s#/paths/%s/%s/responses/%s/content/application~1json/schema", specResURL, esc, op, status)
	s, err := c.Compile(ref)
	require.NoError(t, err, "spec must document a JSON body for %s %s -> %s", strings.ToUpper(op), pathKey, status)
	return s
}

func newSessionContractEnv(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	wsCRD := &llmv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: contractWSID, Namespace: "default"},
		Status:     llmv1.WorkspaceStatus{Phase: llmv1.WorkspacePhaseActive, PodIP: contractPodIP},
	}
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, contractWSID, metav1.GetOptions{}).Return(wsCRD, nil).Maybe()

	log := mcpTestLogger(t)
	proxy, err := handlers.NewProxyHandler(k8sMock, log, "default", nil, &sessionContractAdapter{})
	require.NoError(t, err)
	mr := miniredis.RunT(t)
	proxy.SetOutboxForTest(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(ginHandlerSettingUserID(contractUID))
	auth.On("GetUserID", mock.Anything).Return(contractUID)

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	ws := &imocks.MockWorkspaceService{}
	ws.On("ResolveWorkspace", mock.Anything, contractWSID).Return(&types.WorkspaceMetadata{
		ID: contractWSID, UserID: contractUID, Name: "contract", Runtime: "base",
	}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, contractUID, mock.Anything).Return(nil).Maybe()

	svc := &contractMockServices{auth: auth, met: met, ws: ws}
	// Explicit (mostly zero) config, the orgs/mcp wire-test pattern:
	// the variadic REPLACES DefaultRouterConfig, whose production
	// RequireHTTPS would 301 these plain-http test requests.
	router := NewRouter(svc, log, proxy, RouterConfig{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func contractDo(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequest(method, srv.URL+path, nil)
	} else {
		req, err = http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// validateRow checks the captured wire body against the spec's response
// row for that status.
func validateRow(t *testing.T, c *jsonschema.Compiler, pathKey, op, status string, gotStatus int, body []byte) {
	t.Helper()
	require.Equal(t, status, fmt.Sprintf("%d", gotStatus), "handler status for %s %s", strings.ToUpper(op), pathKey)
	var decoded any
	if len(body) > 0 {
		require.NoError(t, json.Unmarshal(body, &decoded), "body must be valid JSON: %s", body)
	}
	require.NoError(t, specRowSchema(t, c, pathKey, op, status).Validate(decoded),
		"handler body must satisfy the spec row for %s %s -> %s: %s", strings.ToUpper(op), pathKey, status, body)
}

func TestSessionContractConformance(t *testing.T) {
	srv := newSessionContractEnv(t)
	c := specCompiler(t)

	msgPath := "/workspaces/{id}/sessions/{sessionId}/message"
	sessPath := "/workspaces/{id}/sessions/{sessionId}"

	t.Run("sendMessage", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodPost,
			"/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID+"/message",
			`{"content":"hello","parts":[{"type":"text","text":"hello"}]}`)
		validateRow(t, c, msgPath, "post", "200", code, body)
	})

	t.Run("getHistory", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodGet, "/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID+"/message?limit=2", "")
		validateRow(t, c, msgPath, "get", "200", code, body)
	})

	t.Run("getSession", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodGet, "/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID, "")
		validateRow(t, c, sessPath, "get", "200", code, body)
	})

	t.Run("listQuestions", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodGet, "/api/v1/workspaces/"+contractWSID+"/question", "")
		validateRow(t, c, "/workspaces/{id}/question", "get", "200", code, body)
	})

	t.Run("listPermissions", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodGet, "/api/v1/workspaces/"+contractWSID+"/permission", "")
		validateRow(t, c, "/workspaces/{id}/permission", "get", "200", code, body)
	})

	t.Run("sendPromptAsync accepted then duplicate", func(t *testing.T) {
		promptPath := "/workspaces/{id}/sessions/{sessionId}/prompt"
		body := `{"clientMessageID":"cm-contract-1","parts":[{"type":"text","text":"hello"}]}`
		code, raw := contractDo(t, srv, http.MethodPost, "/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID+"/prompt", body)
		validateRow(t, c, promptPath, "post", "202", code, raw)

		code, raw = contractDo(t, srv, http.MethodPost, "/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID+"/prompt", body)
		validateRow(t, c, promptPath, "post", "200", code, raw)
		var receipt map[string]any
		require.NoError(t, json.Unmarshal(raw, &receipt))
		require.Equal(t, "duplicate", receipt["status"], "duplicate retry echoes the ORIGINAL entry: %s", raw)
	})

	t.Run("abortSession is bodyless 204", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodPost, "/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID+"/abort", "")
		require.Equal(t, 204, code)
		require.Len(t, body, 0, "204 must carry no body")
		requireRowDocumented(t, c, "/workspaces/{id}/sessions/{sessionId}/abort", "post", "204")
	})

	t.Run("deleteSession is bodyless 204", func(t *testing.T) {
		code, body := contractDo(t, srv, http.MethodDelete, "/api/v1/workspaces/"+contractWSID+"/sessions/"+contractSID, "")
		require.Equal(t, 204, code)
		require.Len(t, body, 0, "204 must carry no body")
		requireRowDocumented(t, c, sessPath, "delete", "204")
	})
}

// requireRowDocumented asserts the spec has a row for the status (204
// rows carry no schema — presence is the contract).
func requireRowDocumented(t *testing.T, c *jsonschema.Compiler, pathKey, op, status string) {
	t.Helper()
	esc := strings.ReplaceAll(pathKey, "/", "~1")
	ref := fmt.Sprintf("%s#/paths/%s/%s/responses/%s", specResURL, esc, op, status)
	_, err := c.Compile(ref)
	require.NoError(t, err, "spec must document the %s row for %s %s", status, strings.ToUpper(op), pathKey)
}

// TestSessionContractConformance_HarnessDiscriminates is the harness's
// own red-check: a body the contract Session schema rejects (unknown
// status enum, missing id) must FAIL validation — proving green rows
// are pins, not vacuous passes.
func TestSessionContractConformance_HarnessDiscriminates(t *testing.T) {
	c := specCompiler(t)
	esc := strings.ReplaceAll("/workspaces/{id}/sessions/{sessionId}", "/", "~1")
	s, err := c.Compile(specResURL + "#/paths/" + esc + "/get/responses/200/content/application~1json/schema")
	require.NoError(t, err)

	require.Error(t, s.Validate(map[string]any{"status": "not-in-enum"}), "unknown enum value must be rejected")
	require.Error(t, s.Validate(map[string]any{"status": "busy"}), "missing required id must be rejected")
	require.NoError(t, s.Validate(map[string]any{"id": "ses_1", "status": "busy"}), "minimal contract session must pass")
}

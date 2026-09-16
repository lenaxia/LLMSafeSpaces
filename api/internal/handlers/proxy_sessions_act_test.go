// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// epic-71 / s1-sessions (#1372): the sessions-cluster write routes go
// through agentd Act in the authority regime (S1: the API makes zero
// mutating harness calls); the adapter path survives only under flag-off.
// REST routes and response shapes stay IDENTICAL — the parity rows pin
// both regimes to the same bodies for the same harness fixture bytes.

// sessionsActStubPod records Act payloads and serves a configurable
// response (success JSON or a connect error body).
type sessionsActStubPod struct {
	server *httptest.Server
	got    chan map[string]any
	fail   string // connect code to serve ("" = success)
	// failMessage overrides the connect error message ("" = "stubbed failure").
	failMessage string
	// result is marshaled as the Act response body on success.
	result any
}

func newSessionsActStubPod(t *testing.T, fail string, result any) *sessionsActStubPod {
	t.Helper()
	stub := &sessionsActStubPod{got: make(chan map[string]any, 8), fail: fail, result: result}
	mux := http.NewServeMux()
	mux.HandleFunc("/llmsafespaces.abi.v1.HarnessABIService/Act", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1<<20)
		n, _ := r.Body.Read(body)
		var m map[string]any
		_ = json.Unmarshal(body[:n], &m)
		stub.got <- m
		w.Header().Set("Content-Type", "application/json")
		if stub.fail != "" {
			msg := stub.failMessage
			if msg == "" {
				msg = "stubbed failure"
			}
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": stub.fail, "message": msg})
			return
		}
		_ = json.NewEncoder(w).Encode(stub.result)
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *sessionsActStubPod) endpoint() (host string, port int) {
	u := strings.TrimPrefix(s.server.URL, "http://")
	idx := lastIndexColon(u)
	port = mustPort(s.server.URL)
	return u[:idx], port
}

type sessionsActEnv struct {
	router  *gin.Engine
	handler *ProxyHandler
	stub    *sessionsActStubPod
	adapter *mockAdapter
	mr      *miniredis.Miniredis
}

type sessionsActOpts struct {
	terminus bool
	// fail arms the stub pod with a connect error code ("" = success).
	fail string
	// failMessage overrides the connect error message.
	failMessage string
	// result is the stub pod's Act response body.
	result any
	// adapter overrides the handler's adapter after construction.
	adapter *mockAdapter
}

func newSessionsActEnv(t *testing.T, opts sessionsActOpts) *sessionsActEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	stub := newSessionsActStubPod(t, opts.fail, opts.result)
	stub.failMessage = opts.failMessage
	podHost, podPort := stub.endpoint()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	adapter := &mockAdapter{}
	if opts.adapter != nil {
		adapter = opts.adapter
	}
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, adapter)
	require.NoError(t, err)
	handler.agentdPortOverride = podPort
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-s1", "user-1")

	ws := makeWorkspaceCRDWithStatus("ws-s1", podHost, string(v1.WorkspacePhaseActive), "ws-s1")
	wsMock.On("Get", mock.Anything, "ws-s1", mock.Anything).Return(ws, nil).Maybe()

	pwSecret := makePasswordSecret("ws-s1", "pw")
	_, err = fakeClientset.CoreV1().Secrets("default").Create(context.Background(), pwSecret, metav1.CreateOptions{})
	require.NoError(t, err)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	_ = redis.NewClient(&redis.Options{Addr: mr.Addr()})

	if opts.terminus {
		handler.SetAgentdTerminus(true)
	}

	router := gin.New()
	g := router.Group("/api/v1/workspaces/:id")
	g.POST("/sessions", handler.CreateSession)
	g.POST("/sessions/:sessionId/message", handler.SendMessage)
	g.POST("/sessions/:sessionId/prompt", handler.SendPromptAsync)
	g.POST("/sessions/:sessionId/abort", handler.AbortSession)
	g.DELETE("/sessions/:sessionId", handler.DeleteSession)
	return &sessionsActEnv{router: router, handler: handler, stub: stub, adapter: adapter, mr: mr}
}

func (e *sessionsActEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, newStrReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *sessionsActEnv) captured(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-e.stub.got:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no Act payload captured")
		return nil
	}
}

func (e *sessionsActEnv) noAct(t *testing.T) {
	t.Helper()
	select {
	case m := <-e.stub.got:
		t.Fatalf("unexpected Act payload in flag-off regime: %v", m)
	default:
	}
}

// sendFixture loads the captured 1.18.10 flat-tool assistant message and
// translates it through the SAME exported seam both write paths use.
func sendFixture(t *testing.T) session.Message {
	t.Helper()
	raw := fixtureBytes(t)
	msg, _, err := opencode.ParseMessageWire(raw)
	require.NoError(t, err)
	return msg
}

func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	arr := fixtureArray(t)
	return []byte(arr[1])
}

func fixtureArray(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../pkg/agent/opencode/testdata/history_1_18_10_flat_tool.json")
	require.NoError(t, err)
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &arr))
	require.GreaterOrEqual(t, len(arr), 2)
	out := make([]string, len(arr))
	for i, r := range arr {
		out[i] = string(r)
	}
	return out
}

// --- CreateSession ------------------------------------------------------

func TestSessionsAct_CreateSession_HappyPath(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result: map[string]any{
			"sessionId": "",
			"createSession": map[string]any{
				"session": map[string]any{
					"id":     "ses_new1",
					"title":  "Untitled",
					"status": "SESSION_STATUS_IDLE",
				},
			},
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions", "")
	require.Equal(t, http.StatusOK, w.Code)

	payload := env.captured(t)
	assert.Equal(t, "", payload["sessionId"])
	cs, ok := payload["createSession"].(map[string]any)
	require.True(t, ok, "payload carries the createSession arm: %v", payload)
	assert.Equal(t, map[string]any{}, cs, "no title → empty action body")

	var got session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "ses_new1", got.ID)
	assert.Equal(t, "Untitled", got.Title)
	assert.Equal(t, session.StatusIdle, got.Status)
	assert.Equal(t, "ws-s1", got.WorkspaceID, "the handler stamps the workspace id (agentd cannot know it)")
}

func TestSessionsAct_CreateSession_Transport502(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{terminus: true, fail: "unavailable"})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions", "")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.JSONEq(t, `{"error":"failed to create session"}`, w.Body.String(),
		"the authority-regime failure keeps the route's pinned 502 body (wire shapes do not move)")
}

func TestSessionsAct_CreateSession_FlagOffUsesAdapter(t *testing.T) {
	adapter := &mockAdapter{createSessionFn: func(context.Context, string, string, string) (*session.Session, error) {
		return &session.Session{ID: "ses_adapter", WorkspaceID: "ws-s1"}, nil
	}}
	env := newSessionsActEnv(t, sessionsActOpts{terminus: false, adapter: adapter})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions", "")
	require.Equal(t, http.StatusOK, w.Code)
	env.noAct(t)
	assert.Contains(t, w.Body.String(), "ses_adapter")
}

// --- SendMessage --------------------------------------------------------

func TestSessionsAct_SendMessage_ActPayloadAndResponse(t *testing.T) {
	msg := sendFixture(t)
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result:   sendResultFor(t, msg),
	})

	body := `{"parts":[{"type":"text","text":"hello"}],"model":{"modelID":"glm-5.3","providerID":"thekaocloud"}}`
	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	payload := env.captured(t)
	assert.Equal(t, "ses_1", payload["sessionId"])
	send, ok := payload["send"].(map[string]any)
	require.True(t, ok, "payload carries the send arm: %v", payload)
	assert.Equal(t, "hello", send["text"])
	model, ok := send["model"].(map[string]any)
	require.True(t, ok, "the per-prompt selector rides the action: %v", send)
	assert.Equal(t, "glm-5.3", model["id"])
	assert.Equal(t, "thekaocloud", model["provider"])

	var got session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, msg.ID, got.ID)
	assert.Equal(t, msg.Type, got.Type)
	require.Len(t, got.Parts, len(msg.Parts))
	assert.Equal(t, msg.Parts[0].Text, got.Parts[0].Text)
}

// TestSessionsAct_SendMessage_RegimeParity: the SAME harness fixture bytes
// produce deep-equal REST responses in both regimes — the response contract
// does not move, only the write path.
func TestSessionsAct_SendMessage_RegimeParity(t *testing.T) {
	msg := sendFixture(t)

	flagOff := newSessionsActEnv(t, sessionsActOpts{
		terminus: false,
		adapter: &mockAdapter{sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return &msg, nil
		}},
	})
	wFlagOff := flagOff.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusOK, wFlagOff.Code, wFlagOff.Body.String())

	terminus := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result:   sendResultFor(t, msg),
	})
	wTerminus := terminus.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusOK, wTerminus.Code, wTerminus.Body.String())

	assert.JSONEq(t, wFlagOff.Body.String(), wTerminus.Body.String(),
		"both regimes emit the identical contract message JSON for the same fixture")
}

func TestSessionsAct_SendMessage_Transport502(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{terminus: true, fail: "unavailable"})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.JSONEq(t, `{"error":"failed to send message"}`, w.Body.String(),
		"the #817/#944 error body is the pinned contract — unchanged in the authority regime")
}

func TestSessionsAct_SendMessage_FlagOffUsesAdapter(t *testing.T) {
	adapter := &mockAdapter{sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
		return &session.Message{ID: "msg_adapter", Type: session.MessageAssistant}, nil
	}}
	env := newSessionsActEnv(t, sessionsActOpts{terminus: false, adapter: adapter})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusOK, w.Code)
	env.noAct(t)
	assert.Contains(t, w.Body.String(), "msg_adapter")
}

// --- AbortSession -------------------------------------------------------

func TestSessionsAct_AbortSession_ActPayloadAnd204(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result: map[string]any{
			"sessionId": "ses_1",
			"interrupt": map[string]any{},
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/abort", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Body.String())

	payload := env.captured(t)
	assert.Equal(t, "ses_1", payload["sessionId"])
	assert.Equal(t, map[string]any{}, payload["interrupt"], "abort IS the interrupt verb (D1)")
}

func TestSessionsAct_AbortSession_Transport502(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{terminus: true, fail: "unavailable"})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/abort", "")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.JSONEq(t, `{"error":"failed to abort session"}`, w.Body.String())
}

func TestSessionsAct_AbortSession_FlagOffUsesAdapter(t *testing.T) {
	var called bool
	adapter := &mockAdapter{abortFn: func(context.Context, string, string, string) error {
		called = true
		return nil
	}}
	env := newSessionsActEnv(t, sessionsActOpts{terminus: false, adapter: adapter})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/abort", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	env.noAct(t)
	assert.True(t, called)
}

// --- DeleteSession ------------------------------------------------------

func TestSessionsAct_DeleteSession_ActPayloadSideEffectsAnd204(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result: map[string]any{
			"sessionId":     "ses_1",
			"deleteSession": map[string]any{},
		},
	})

	wsSub, err := env.handler.userBroker.SubscribeWorkspace("ws-s1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-s1", wsSub)

	w := env.do(t, http.MethodDelete, "/api/v1/workspaces/ws-s1/sessions/ses_1", "")
	require.Equal(t, http.StatusNoContent, w.Code)

	payload := env.captured(t)
	assert.Equal(t, "ses_1", payload["sessionId"])
	assert.Equal(t, map[string]any{}, payload["deleteSession"])

	// Post-delete side effects run after a successful Act exactly as
	// after the adapter delete: tombstone + SSE status event.
	assert.True(t, env.handler.isSessionDeleted("ws-s1", "ses_1"),
		"the tombstone must survive the write-path migration (late-event suppression)")
	evt := recvWithTimeout(t, wsSub, "session.status")
	assert.Equal(t, "deleted", evt.Status, "the deleted status event clears the frontend sidebar")
}

func TestSessionsAct_DeleteSession_Transport502(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{terminus: true, fail: "unavailable"})

	w := env.do(t, http.MethodDelete, "/api/v1/workspaces/ws-s1/sessions/ses_1", "")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.JSONEq(t, `{"error":"failed to delete session"}`, w.Body.String())

	assert.False(t, env.handler.isSessionDeleted("ws-s1", "ses_1"),
		"side effects must NOT run when the Act delete failed")
}

func TestSessionsAct_DeleteSession_FlagOffUsesAdapter(t *testing.T) {
	var called bool
	adapter := &mockAdapter{deleteSessionFn: func(context.Context, string, string, string) error {
		called = true
		return nil
	}}
	env := newSessionsActEnv(t, sessionsActOpts{terminus: false, adapter: adapter})

	w := env.do(t, http.MethodDelete, "/api/v1/workspaces/ws-s1/sessions/ses_1", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	env.noAct(t)
	assert.True(t, called)
}

// --- RenameSessionInAgent -----------------------------------------------

func TestSessionsAct_RenameSession_ActPayloadAndFlagOff(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result: map[string]any{
			"sessionId":     "ses_1",
			"renameSession": map[string]any{},
		},
	})

	err := env.handler.RenameSessionInAgent(context.Background(), "ws-s1", "ses_1", "New Title")
	require.NoError(t, err)

	payload := env.captured(t)
	assert.Equal(t, "ses_1", payload["sessionId"])
	rename, ok := payload["renameSession"].(map[string]any)
	require.True(t, ok, "payload carries the renameSession arm: %v", payload)
	assert.Equal(t, "New Title", rename["title"])

	// Flag-off: the typed adapter method, no Act.
	var adapterCalled bool
	flagOff := newSessionsActEnv(t, sessionsActOpts{
		terminus: false,
		adapter: &mockAdapter{renameSessionFn: func(context.Context, string, string, string, string) error {
			adapterCalled = true
			return nil
		}},
	})
	err = flagOff.handler.RenameSessionInAgent(context.Background(), "ws-s1", "ses_1", "T")
	require.NoError(t, err)
	flagOff.noAct(t)
	assert.True(t, adapterCalled)
}

func TestSessionsAct_RenameSession_ActErrorReturns(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{terminus: true, fail: "unavailable"})

	err := env.handler.RenameSessionInAgent(context.Background(), "ws-s1", "ses_1", "T")
	require.Error(t, err, "the caller (router's title route) logs the error — it must surface")
}

// --- syncSend (the outbox-less fallback shares the Act path) ------------

func TestSessionsAct_SyncSendFallback_RoutesThroughAct(t *testing.T) {
	msg := sendFixture(t)
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result:   sendResultFor(t, msg),
	})

	// SendPromptAsync with no outbox (dev/test) falls back to syncSend —
	// S1 has no carve-outs: the fallback writes through Act too.
	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/prompt",
		`{"parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	payload := env.captured(t)
	assert.Equal(t, "ses_1", payload["sessionId"])
	send, ok := payload["send"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "hello", send["text"])
}

// --- helpers -------------------------------------------------------------

// sendResultFor builds the stub pod's Act response for a send: the
// contract message rendered in ABI protojson (camelCase) — the wire shape
// agentd's actor emits.
func sendResultFor(t *testing.T, msg session.Message) map[string]any {
	t.Helper()
	parts := make([]any, 0, len(msg.Parts))
	for _, p := range msg.Parts {
		part := map[string]any{"id": p.ID}
		switch p.Type {
		case session.PartText:
			part["type"] = "PART_TYPE_TEXT"
			part["text"] = p.Text
		case session.PartReasoning:
			part["type"] = "PART_TYPE_REASONING"
			part["reasoning"] = p.Reasoning
		case session.PartTool:
			part["type"] = "PART_TYPE_TOOL"
			tool := map[string]any{"callId": p.Tool.CallID, "name": p.Tool.Name}
			if len(p.Tool.Input) > 0 {
				tool["input"] = base64.StdEncoding.EncodeToString(p.Tool.Input)
			}
			if len(p.Tool.Output) > 0 {
				tool["output"] = base64.StdEncoding.EncodeToString(p.Tool.Output)
			}
			state := map[string]any{"status": toolStatusABIName(p.Tool.State.Status)}
			if p.Tool.State.StartedAt != nil {
				state["startedAt"] = p.Tool.State.StartedAt.UTC().Format(time.RFC3339Nano)
			}
			if p.Tool.State.CompletedAt != nil {
				state["completedAt"] = p.Tool.State.CompletedAt.UTC().Format(time.RFC3339Nano)
			}
			tool["state"] = state
			part["tool"] = tool
		}
		parts = append(parts, part)
	}
	out := map[string]any{
		"sessionId": msg.SessionID,
		"send": map[string]any{
			"message": map[string]any{
				"id":        msg.ID,
				"sessionId": msg.SessionID,
				"type":      messageTypeName(msg.Type),
				"parts":     parts,
			},
		},
	}
	if msg.CreatedAt != nil {
		out["send"].(map[string]any)["message"].(map[string]any)["createdAt"] = msg.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func toolStatusABIName(s session.ToolStatus) string {
	switch s {
	case session.ToolStatusPending:
		return "TOOL_STATUS_PENDING"
	case session.ToolStatusRunning:
		return "TOOL_STATUS_RUNNING"
	case session.ToolStatusCompleted:
		return "TOOL_STATUS_COMPLETED"
	case session.ToolStatusError:
		return "TOOL_STATUS_ERROR"
	}
	return "TOOL_STATUS_UNSPECIFIED"
}

func messageTypeName(t session.MessageType) string {
	switch t {
	case session.MessageUser:
		return "MESSAGE_TYPE_USER"
	case session.MessageAssistant:
		return "MESSAGE_TYPE_ASSISTANT"
	case session.MessageShell:
		return "MESSAGE_TYPE_SHELL"
	case session.MessageSystem:
		return "MESSAGE_TYPE_SYSTEM"
	case session.MessageAgentSwitch:
		return "MESSAGE_TYPE_AGENT_SWITCH"
	case session.MessageModelSwitch:
		return "MESSAGE_TYPE_MODEL_SWITCH"
	case session.MessageCompaction:
		return "MESSAGE_TYPE_COMPACTION"
	}
	return "MESSAGE_TYPE_UNSPECIFIED"
}

// --- r1 review findings: the send transport's parity with the adapter ---

// TestSessionsAct_SendMessage_LargeResult (r1 f2): a tool-output-bearing
// assistant message can exceed the input-verbs' 1 MiB Act cap (bytes
// fields base64-inflate ~33%) — the send path must carry the adapter's
// 64 MiB bound or large turns 502 despite pod-side success.
func TestSessionsAct_SendMessage_LargeResult(t *testing.T) {
	msg := sendFixture(t)
	msg.Parts[0].Text = strings.Repeat("x", (1<<20)+4096) // > 1 MiB in one text part
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result:   sendResultFor(t, msg),
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.NotEmpty(t, got.Parts)
	assert.Len(t, got.Parts[0].Text, (1<<20)+4096, "the large result body survives the Act round trip")
}

// TestSessionActHTTPClient_NoHardTimeout (r1 f1): a synchronous send is a
// full LLM turn — the sessions Act transport must not carry the outbox
// deliverer's 3m30s hard client timeout (the same pin the adapter path
// holds, TestHTTPClient_NoHardTimeout; the request context is the correct
// boundary).
func TestSessionActHTTPClient_NoHardTimeout(t *testing.T) {
	assert.Equal(t, time.Duration(0), sessionActHTTPClient.Timeout,
		"sessions Act transport must not have a hard timeout — context deadline is the correct boundary")
}

// TestSessionsAct_SessionIdIsAuthoritative (r1 minor): the caller's
// sessionID wins over any stray key an action map carries.
func TestSessionsAct_SessionIdIsAuthoritative(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		result: map[string]any{
			"sessionId":     "ses_authoritative",
			"deleteSession": map[string]any{},
		},
	})

	_, err := env.handler.actSessionAction(context.Background(), "ws-s1", "ses_authoritative",
		map[string]any{"deleteSession": map[string]any{}, "sessionId": "ses_stray"})
	require.NoError(t, err)

	payload := env.captured(t)
	assert.Equal(t, "ses_authoritative", payload["sessionId"],
		"the injected session id must be authoritative — a stray action-map key must never override it")
}

// TestSessionsAct_SendMessage_TextOnlyWedge422 (#1307 × #1372): the
// authority regime keeps the actionable wedge error — agentd's actor
// embeds the harness 400 body in the typed connect error, and the handler
// classifies the marker to the same structured 422 the adapter path
// serves. Red pre-fix: every Act failure mapped to the generic 502.
func TestSessionsAct_SendMessage_TextOnlyWedge422(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		fail:     "invalid_argument",
		failMessage: "POST /session/ses_1/message: status 400: " +
			`{"name":"ProviderServerError","data":{"message":"Provider request failed with HTTP 400: litellm.BadRequestError: ZaiException - messages.content.type is invalid, allowed values: ['text']"}}`,
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"continue"}]}`)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "text_only_model_image_history")
	assert.Contains(t, w.Body.String(), "vision")
	assert.NotContains(t, w.Body.String(), "litellm.BadRequestError", "raw provider body must not be the user surface")
}

// TestSessionsAct_SendMessage_WedgeRequiresInvalidArgumentCode (r2 f3):
// main's gate classifies only harness-400 bodies; the Act-path probe must
// match that strictness — a non-invalid_argument connect error whose
// embedded body happens to carry the marker stays the generic 502 (parity
// with flag-off for identical harness bytes).
func TestSessionsAct_SendMessage_WedgeRequiresInvalidArgumentCode(t *testing.T) {
	env := newSessionsActEnv(t, sessionsActOpts{
		terminus: true,
		fail:     "internal",
		failMessage: "POST /session/ses_1/message: status 500: " +
			`{"data":{"message":"... messages.content.type is invalid ..."}}`,
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-s1/sessions/ses_1/message",
		`{"parts":[{"type":"text","text":"continue"}]}`)
	require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "failed to send message",
		"a non-400-class failure never renders the wedge surface")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// epic-71 / 4a (#1302): the input write routes go through agentd Act
// with AnswerInputAction (S1: the API makes zero mutating harness calls
// in the authority regime); the adapter path survives only under the
// flag-off regime.

type inputActEnv struct {
	router  *gin.Engine
	handler *ProxyHandler
	inbox   *inbox.Service
}

// answerActStubPod records answerQuestion actions; success envelope
// mirrors the real Act wire (bare result message).
type answerActStubPod struct {
	server *httptest.Server
	got    chan map[string]any
	fail   string // connect code to serve ("" = success)
}

func newAnswerActStubPod(t *testing.T, fail string) *answerActStubPod {
	t.Helper()
	stub := &answerActStubPod{got: make(chan map[string]any, 4), fail: fail}
	mux := http.NewServeMux()
	mux.HandleFunc("/llmsafespaces.abi.v1.HarnessABIService/Act", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllLimited(r)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		stub.got <- m
		w.Header().Set("Content-Type", "application/json")
		if stub.fail != "" {
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": stub.fail, "message": "stubbed failure"})
			return
		}
		ans, _ := m["answerQuestion"].(map[string]any)
		id := ""
		if ans != nil {
			id, _ = ans["inputId"].(string)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": m["sessionId"],
			"answerQuestion": map[string]any{
				"inputId": id,
			},
		})
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func readAllLimited(r *http.Request) ([]byte, error) {
	buf := make([]byte, 1<<20)
	n, _ := r.Body.Read(buf)
	return buf[:n], nil
}

type inputActOpts struct {
	terminus  bool
	podURL    string
	listFn    func(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error)
	configure func(h *ProxyHandler)
}

func newInputActEnv(t *testing.T, opts inputActOpts) *inputActEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-act", "user-1")

	wsName := "ws-act"
	podHost := ""
	if opts.podURL != "" {
		podHost = strings.TrimPrefix(opts.podURL, "http://")
		if idx := lastIndexColon(podHost); idx >= 0 {
			port := mustPort(opts.podURL)
			handler.agentdPortOverride = port
			podHost = podHost[:idx]
		}
	}
	ws := makeWorkspaceCRDWithStatus(wsName, podHost, string(v1.WorkspacePhaseActive), wsName)
	wsMock.On("Get", mock.Anything, wsName, mock.Anything).Return(ws, nil).Maybe()

	pwSecret := makePasswordSecret(wsName, "pw")
	_, err = fakeClientset.CoreV1().Secrets("default").Create(context.Background(), pwSecret, metav1.CreateOptions{})
	require.NoError(t, err)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	in := inbox.New(client)
	handler.SetInboxStoreForTest(in)

	handler.state().SetWorkspaceConfig(context.Background(), "ws-act", wsstate.Config{})
	handler.adapter = &mockAdapter{listPendingFn: opts.listFn}

	if opts.terminus {
		handler.SetAgentdTerminus(true)
	}
	if opts.configure != nil {
		opts.configure(handler)
	}

	router := gin.New()
	g := router.Group("/api/v1/workspaces/:id")
	g.POST("/question/:requestID/reply", handler.QuestionReply)
	g.POST("/question/:requestID/reject", handler.QuestionReject)
	g.POST("/permission/:requestID/reply", handler.PermissionReply)
	g.POST("/sessions/:sessionId/actions", handler.SessionAction)
	return &inputActEnv{router: router, handler: handler, inbox: in}
}

func lastIndexColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func mustPort(url string) int {
	idx := lastIndexColon(url)
	port := 0
	for _, c := range url[idx+1:] {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return port
}

func (e *inputActEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr = newStrReader(body)
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func newStrReader(s string) *fakeBodyReader { return &fakeBodyReader{s: []byte(s)} }

type fakeBodyReader struct {
	s []byte
	i int
}

func (r *fakeBodyReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

func liveQuestion(id, ses string) []session.InputRequest {
	return []session.InputRequest{{
		ID: id, SessionID: ses, Kind: session.InputQuestion,
		Question: "Go?", Options: []session.InputOption{{Label: "Go", Description: ""}},
	}}
}

func TestInputAct_QuestionReplyForwardsAnswerAction(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_abc123", "ses_live"), nil
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_abc123/reply", `{"answers":[["Go"]]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	select {
	case got := <-stub.got:
		assert.Equal(t, "ses_live", got["sessionId"], "the ask's session addresses Act")
		ans, ok := got["answerQuestion"].(map[string]any)
		require.True(t, ok, "payload: %v", got)
		assert.Equal(t, "que_abc123", ans["inputId"])
		assert.Equal(t, []any{"Go"}, ans["optionIds"], "answers ride option_ids (the harness wire is rebuilt byte-compatibly agentd-side)")
		assert.Empty(t, ans["reply"])
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
	assert.JSONEq(t, `{"status":"answered"}`, w.Body.String())
}

func TestInputAct_PermissionReplyCarriesReplyAndMessage(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "per_abc123", SessionID: "ses_live", Kind: session.InputPermission,
				Permission: "bash", Patterns: []string{"ls"},
			}}, nil
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_abc123/reply", `{"reply":"reject","message":"too risky"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	select {
	case got := <-stub.got:
		ans, ok := got["answerQuestion"].(map[string]any)
		require.True(t, ok, "payload: %v", got)
		assert.Equal(t, "per_abc123", ans["inputId"])
		assert.Equal(t, "reject", ans["reply"])
		assert.Equal(t, "too risky", ans["message"], "deny feedback preserved (4a D2)")
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
}

func TestInputAct_QuestionRejectIsTheDismissExit(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_abc123", "ses_live"), nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "que_abc123", SessionID: "ses_live", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Go?", RecordedAt: time.Now().UTC(),
	}))

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_abc123/reject", `{}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	select {
	case got := <-stub.got:
		ans, ok := got["answerQuestion"].(map[string]any)
		require.True(t, ok, "payload: %v", got)
		assert.Equal(t, "que_abc123", ans["inputId"])
		assert.Equal(t, "reject", ans["reply"], "question reject rides the reply vocabulary (4a D1)")
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
	// Disposition: a live-rejected ask is the dismiss exit (#1313 S11).
	left, err := env.inbox.List(context.Background(), "ws-act", "ses_live")
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestInputAct_DeadAskWithoutRecord404s(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil // nothing live, no inbox record
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_gone/reply", `{"answers":[["Go"]]}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "dead ask + no record: nothing to address Act at")
	select {
	case <-stub.got:
		t.Fatal("Act must not be called for an unresolvable ask")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestInputAct_ConnectErrorSurfaces(t *testing.T) {
	stub := newAnswerActStubPod(t, "not_found")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_abc123", "ses_live"), nil
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_abc123/reply", `{"answers":[["Go"]]}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "connect codes map (not_found → 404), never a silent no-op")
}

func TestInputAct_AdapterPathSurvivesFlagOff(t *testing.T) {
	answered := false
	env := newInputActEnv(t, inputActOpts{
		terminus: false,
		// PodIP only satisfies the workspace-readiness guard; the
		// adapter path never dials agentd.
		podURL: "http://127.0.0.1:1",
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_abc123", "ses_live"), nil
		},
		configure: func(h *ProxyHandler) {
			h.adapter = &mockAdapter{
				listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
					return liveQuestion("que_abc123", "ses_live"), nil
				},
				answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error {
					answered = true
					return nil
				},
			}
		},
	})

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_abc123/reply", `{"answers":[["Go"]]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, answered, "flag-off regime keeps the batch-3 adapter path (D4 single regime per flag)")
}

func TestInputAct_SessionActionDispositionHook(t *testing.T) {
	// 3a's deferred item: answering through the GENERIC actions route
	// terminalizes the inbox record (the MCP/SDK path).
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "que_via_actions", SessionID: "ses_mcp", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Via SDK?", RecordedAt: time.Now().UTC(),
	}))

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/sessions/ses_mcp/actions",
		`{"answerQuestion":{"inputId":"que_via_actions","optionIds":["Yes"]}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	select {
	case got := <-stub.got:
		ans, _ := got["answerQuestion"].(map[string]any)
		assert.Equal(t, "que_via_actions", ans["inputId"])
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
	left, err := env.inbox.List(context.Background(), "ws-act", "ses_mcp")
	require.NoError(t, err)
	assert.Empty(t, left, "the SessionAction answer terminalizes the inbox record (3a deferral, landed)")
}

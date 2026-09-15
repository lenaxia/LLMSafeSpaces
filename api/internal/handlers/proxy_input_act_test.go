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
	mr      *miniredis.Miniredis
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

	// #1362: the adapter is a required ctor param — pass the opts-shaped
	// mock up front (configure may replace it later, same as before).
	ctorAdapter := &mockAdapter{}
	if opts.listFn != nil {
		ctorAdapter.listPendingFn = opts.listFn
	}
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, ctorAdapter)
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
	g.DELETE("/sessions/:sessionId/inbox/:requestID", handler.DismissInboxRecord)
	return &inputActEnv{router: router, handler: handler, inbox: in, mr: mr}
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
	// The reply 200 is bodyless (the published contract —
	// sdks/openapi.yaml replyQuestion "200": no content schema): the
	// status code IS the accept signal; a body here would misclassify
	// live answers as late answers in the body-parsing SDKs (r1 f1).
	assert.Empty(t, w.Body.String(), "reply 200 must carry no body")
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
	assert.Empty(t, w.Body.String(), "reply 200 must carry no body (the published contract)")
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
	assert.Empty(t, w.Body.String(), "reject 200 must carry no body (the published contract — same row as the reply pins)")

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
	assert.Empty(t, w.Body.String(), "flag-off reply 200 must carry no body (same pin family as the terminus rows)")
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

// --- r1 remediation rows ---

func TestInputAct_LiveMissInboxHitActLands(t *testing.T) {
	// r1 missing-case 1: the ask is NOT in the live set (dead) but its
	// inbox record identifies the session — Act still lands (reject on
	// a dead ask = resolve-by-absence agentd-side). Only reachable on
	// QuestionReject among the routes (tryLateAnswer intercepts replies).
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil // live miss
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "que_dead1", SessionID: "ses_frominbox", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Dead?", RecordedAt: time.Now().UTC(),
	}))

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_dead1/reject", `{}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	select {
	case got := <-stub.got:
		assert.Equal(t, "ses_frominbox", got["sessionId"], "the inbox record resolves the session on a live miss")
		ans, _ := got["answerQuestion"].(map[string]any)
		assert.Equal(t, "que_dead1", ans["inputId"])
		assert.Equal(t, "reject", ans["reply"])
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
}

func TestInputAct_PermissionReplyRejectDismissesRecord(t *testing.T) {
	// r1 missing-case 2: PermissionReply terminus reject → dismissed
	// disposition + the resolved event (r1 f2 — the client's only clear
	// for a dead ask).
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "per_live9", SessionID: "ses_p", Kind: session.InputPermission,
				Permission: "bash", Patterns: []string{"ls"},
			}}, nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "per_live9", SessionID: "ses_p", Kind: inbox.KindPermission, Status: inbox.StatusPending,
		Permission: "bash", RecordedAt: time.Now().UTC(),
	}))
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_live9/reply", `{"reply":"reject"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	left, err := env.inbox.List(context.Background(), "ws-act", "ses_p")
	require.NoError(t, err)
	assert.Empty(t, left, "reject dismisses the record")
	evt := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_live9", evt.RequestID, "the disposition publishes the clear event (r1 f2)")
}

func TestInputAct_SessionActionRejectDismissesAndPublishes(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "per_act9", SessionID: "ses_mcp", Kind: inbox.KindPermission, Status: inbox.StatusPending,
		Permission: "bash", RecordedAt: time.Now().UTC(),
	}))
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/sessions/ses_mcp/actions",
		`{"answerQuestion":{"inputId":"per_act9","reply":"reject"}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	left, err := env.inbox.List(context.Background(), "ws-act", "ses_mcp")
	require.NoError(t, err)
	assert.Empty(t, left, "SessionAction reject dismisses (r1 missing-case 2)")
	evt := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_act9", evt.RequestID)
}

func TestInputAct_QuestionReplyPublishesResolvedEvent(t *testing.T) {
	// r1 f2: the REST answer path publishes the clear event — for a LIVE
	// ask the harness event may follow (idempotent duplicate); for the
	// record-carrying path this is the designed clear.
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_evt7", "ses_e"), nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "que_evt7", SessionID: "ses_e", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Go?", RecordedAt: time.Now().UTC(),
	}))
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_evt7/reply", `{"answers":[["Go"]]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	evt := recvWithTimeout(t, userSub, "agent.question.resolved")
	assert.Equal(t, "que_evt7", evt.RequestID)
}

func TestInputAct_UnknownLiveSetIsNonAuthoritative(t *testing.T) {
	// r1 robustness: a ListPending failure with no identifying record is
	// 503 (non-authoritative), never an authoritative 404 (#1302 doctrine).
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	})
	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_unresolvable/reply", `{"answers":[["Go"]]}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "unknown pending set → 503 + Retry-After, not 404")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

func TestInputAct_QuestionRejectConnectErrorAndFlagOff(t *testing.T) {
	failStub := newAnswerActStubPod(t, "not_found")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: failStub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_ce1", "ses_ce"), nil
		},
	})
	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_ce1/reject", `{}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "connect codes map on reject too (r1 minor)")

	okStub := newAnswerActStubPod(t, "")
	rejected := false
	env2 := newInputActEnv(t, inputActOpts{
		terminus: false, podURL: okStub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_ce2", "ses_ce2"), nil
		},
		configure: func(h *ProxyHandler) {
			h.adapter = &mockAdapter{
				listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
					return liveQuestion("que_ce2", "ses_ce2"), nil
				},
				rejectInputFn: func(_ context.Context, _, _, _ string) error {
					rejected = true
					return nil
				},
			}
		},
	})
	w2 := env2.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_ce2/reject", `{}`)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.Empty(t, w2.Body.String(), "flag-off reject 200 must carry no body (same pin family as the terminus rows)")
	assert.True(t, rejected, "flag-off keeps the adapter reject (r1 minor)")
}

func TestInputAct_PermissionReplyFlagOff(t *testing.T) {
	replied := false
	env := newInputActEnv(t, inputActOpts{
		terminus: false, podURL: "http://127.0.0.1:1",
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
		configure: func(h *ProxyHandler) {
			h.adapter = &mockAdapter{
				listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
					return []session.InputRequest{{
						ID: "per_fo", SessionID: "ses_fo", Kind: session.InputPermission, Permission: "bash",
					}}, nil
				},
				replyPermissionFn: func(_ context.Context, _, _, _, reply, msg string) error {
					replied = true
					assert.Equal(t, "once", reply)
					assert.Empty(t, msg)
					return nil
				},
			}
		},
	})
	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_fo/reply", `{"reply":"once"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, w.Body.String(), "flag-off permission reply 200 must carry no body (same pin family as the terminus rows)")
	assert.True(t, replied, "flag-off keeps the adapter permission reply (r1 minor)")
}

func TestInputAct_DismissLiveAskGoesThroughAct(t *testing.T) {
	// r1 S1: DismissInboxRecord's live reject rides Act in the authority
	// regime — the API makes zero mutating harness calls.
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_dm1", "ses_dm"), nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "que_dm1", SessionID: "ses_dm", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Live?", RecordedAt: time.Now().UTC(),
	}))

	w := env.do(t, http.MethodDelete, "/api/v1/workspaces/ws-act/sessions/ses_dm/inbox/que_dm1", "")

	require.Equal(t, http.StatusNoContent, w.Code)

	select {
	case got := <-stub.got:
		ans, ok := got["answerQuestion"].(map[string]any)
		require.True(t, ok, "payload: %v", got)
		assert.Equal(t, "que_dm1", ans["inputId"])
		assert.Equal(t, "reject", ans["reply"], "dismiss of a live ask = the reject vocabulary through Act (r1 S1)")
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called for the live dismiss")
	}
}

func TestInputAct_UnknownLiveSet503OnAllThreeRoutes(t *testing.T) {
	// r3 carried: the tri-state 503 was pinned only on QuestionReply;
	// the identical copy-paste on the other two routes gets its own pin.
	stub := newAnswerActStubPod(t, "")
	mk := func(route, body string) int {
		env := newInputActEnv(t, inputActOpts{
			terminus: true, podURL: stub.server.URL,
			listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
				return nil, assert.AnError
			},
		})
		w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act"+route, body)
		return w.Code
	}
	assert.Equal(t, http.StatusServiceUnavailable, mk("/question/que_u1/reject", `{}`), "reject: unknown set → 503")
	assert.Equal(t, http.StatusServiceUnavailable, mk("/permission/per_u1/reply", `{"reply":"once"}`), "permission: unknown set → 503")
}

func TestInputAct_QuestionReplyFlattensAllAnswerGroups(t *testing.T) {
	// r3 carried: multi-group answers must not be silently dropped.
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_multi1", "ses_m"), nil
		},
	})
	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_multi1/reply", `{"answers":[["Go","and","custom"],["second"]]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	select {
	case got := <-stub.got:
		ans, ok := got["answerQuestion"].(map[string]any)
		require.True(t, ok, "payload: %v", got)
		assert.Equal(t, []any{"Go", "and", "custom", "second"}, ans["optionIds"], "ALL answer groups ride the action (no silent drop)")
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
}

// --- auto-approve rides the same Act path (S1, #1302 r1-f2) ---------

// The headless auto-approve must not make direct mutating harness calls:
// in the authority regime it goes through agentd Act with
// AnswerInputAction reply="always" — exactly the PermissionReply path.
func TestAutoApprovePermission_ActRegime(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "per_auto1", SessionID: "ses_live", Kind: session.InputPermission,
				Permission: "bash", Patterns: []string{"ls"},
			}}, nil
		},
	})

	env.handler.autoApprovePermission("ws-act", "per_auto1")

	select {
	case got := <-stub.got:
		ans, ok := got["answerQuestion"].(map[string]any)
		require.True(t, ok, "payload: %v", got)
		assert.Equal(t, "per_auto1", ans["inputId"])
		assert.Equal(t, "always", ans["reply"], "auto-approve rides the permission vocabulary through Act")
		assert.Equal(t, "ses_live", got["sessionId"], "the ask's live session addresses Act")
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called — auto-approve bypassed the Act path")
	}
}

// Unknown pending set (unreadable live set, no identifying record):
// auto-approve skips non-authoritatively — no Act call, no panic.
func TestAutoApprovePermission_ActUnknownPendingSetSkips(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	})

	require.NotPanics(t, func() { env.handler.autoApprovePermission("ws-act", "per_gone") })
	select {
	case got := <-stub.got:
		t.Fatalf("Act must not fire on an unknown pending set, got %v", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// Act transport failure: warn-and-return — never a panic.
func TestAutoApprovePermission_ActErrorNoPanic(t *testing.T) {
	stub := newAnswerActStubPod(t, "not_found")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "per_dead1", SessionID: "ses_live", Kind: session.InputPermission,
				Permission: "bash",
			}}, nil
		},
	})

	require.NotPanics(t, func() { env.handler.autoApprovePermission("ws-act", "per_dead1") })
	// The stub pod answered; the drain keeps the channel empty for the next reader.
	select {
	case <-stub.got:
	case <-time.After(2 * time.Second):
		t.Fatal("Act was never called")
	}
}

// Readable pending set but the ask is absent (no inbox record either):
// the skip is non-authoritative — no Act call, no panic.
func TestAutoApprovePermission_ActAbsentAskSkips(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{}, nil
		},
	})

	require.NotPanics(t, func() { env.handler.autoApprovePermission("ws-act", "per_absent") })
	select {
	case got := <-stub.got:
		t.Fatalf("Act must not fire for an absent ask, got %v", got)
	case <-time.After(300 * time.Millisecond):
	}
}

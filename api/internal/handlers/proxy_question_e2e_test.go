// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// US-16.13's question round-trip, ported to the US-69.11 seams: the
// pending-input lifecycle arrives through the usage bridge (the retired
// tracker's dialect translation is gone), while the reply/reject POST
// proxying is unchanged. The full request path stays wired together:
//
//	pod raises an ABI INPUT_REQUEST
//	  → usageBridge.InputRequested → broker → user subscriber ("agent.question")
//	POST /question/<id>/reply
//	  → proxy forwards to pod with Basic Auth + correct path
//	pod resolves the input
//	  → usageBridge.InputResolved → user subscriber ("agent.question.resolved")

func newQuestionFlowEnv(t *testing.T, podHandler http.HandlerFunc) *testEnv {
	t.Helper()
	env := newTestEnvWithBackend(t, podHandler)
	env.handler.dialect = &agentoc.Dialect{}
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	t.Cleanup(stubUsageStream())
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-pw")
	return env
}

func TestE2E_QuestionFlow_FullRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu               sync.Mutex
		replyPathHit     string
		replyBody        string
		replyContentType string
	)

	podBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok, "Basic Auth must reach the pod")
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-pw", pass)

		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reply") {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			replyPathHit = r.URL.Path
			replyBody = string(body)
			replyContentType = r.Header.Get("Content-Type")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		// Default: echo method+path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"method": r.Method,
			"path":   r.URL.Path,
		})
	})

	env := newQuestionFlowEnv(t, podBackend)

	// Question routes are not in the default proxy group; add them on the same router.
	env.router.GET("/api/v1/workspaces/:id/question", env.handler.ListQuestions)
	env.router.POST("/api/v1/workspaces/:id/question/:requestID/reply", env.handler.QuestionReply)
	env.router.POST("/api/v1/workspaces/:id/question/:requestID/reject", env.handler.QuestionReject)

	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	// 1. The pod raises a pending question (ABI INPUT_REQUEST).
	(&usageBridge{h: env.handler}).InputRequested("ws-1", &abiv1.InputRequest{
		Id: "que_e2e", SessionId: "ses_e2e", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
		Question: "Pick?", Header: "H",
		Options: []*abiv1.InputOption{{Label: "A", Description: "a"}},
	})

	// 2. The user stream receives agent.question.
	askEvt := recvWithTimeout(t, userSub, "agent.question")
	assert.Equal(t, "que_e2e", askEvt.RequestID)
	assert.Equal(t, "ses_e2e", askEvt.SessionID)
	assert.Equal(t, "ws-1", askEvt.WorkspaceID)

	// 3. User POSTs /question/que_e2e/reply with their answer.
	replyPayload := `{"answers":[["A"]]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/question/que_e2e/reply",
		strings.NewReader(replyPayload))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "reply must succeed against the pod")

	// 4. Verify the pod received the reply at the correct dialect-specific path.
	mu.Lock()
	gotPath := replyPathHit
	gotBody := replyBody
	gotCT := replyContentType
	mu.Unlock()
	assert.NotEmpty(t, gotPath, "pod must receive the reply POST")
	assert.Contains(t, gotPath, "que_e2e", "path must include the question ID")
	assert.Contains(t, gotPath, "/reply", "path must be the reply endpoint")
	assert.Equal(t, replyPayload, gotBody, "reply body must reach the pod verbatim")
	assert.Equal(t, "application/json", gotCT)

	// 5. The pod resolves the input; the user stream gets agent.question.resolved.
	(&usageBridge{h: env.handler}).InputResolved("ws-1", "ses_e2e", "que_e2e")

	resolvedEvt := recvWithTimeout(t, userSub, "agent.question.resolved")
	resData, ok := resolvedEvt.Data.(map[string]string)
	require.True(t, ok, "resolved event data must be a map")
	assert.Equal(t, "que_e2e", resData["request_id"])
	assert.Equal(t, "ses_e2e", resData["session_id"])
}

// TestE2E_QuestionFlow_RejectClearsQuestion mirrors the round-trip for
// the reject branch: the reject POST reaches the pod at the
// dialect-specific reject endpoint and the input resolves.
func TestE2E_QuestionFlow_RejectClearsQuestion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu           sync.Mutex
		rejectPath   string
		rejectMethod string
	)
	podBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			t.Errorf("pod backend requires Basic Auth")
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reject") {
			mu.Lock()
			rejectPath = r.URL.Path
			rejectMethod = r.Method
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "path": r.URL.Path})
	})

	env := newQuestionFlowEnv(t, podBackend)
	env.router.POST("/api/v1/workspaces/:id/question/:requestID/reject", env.handler.QuestionReject)

	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	(&usageBridge{h: env.handler}).InputRequested("ws-1", &abiv1.InputRequest{
		Id: "que_rej", SessionId: "ses_rej", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
		Question: "Q?",
	})

	askEvt := recvWithTimeout(t, userSub, "agent.question")
	assert.Equal(t, "agent.question", askEvt.Type)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/question/que_rej/reject", nil)
	env.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	mu.Lock()
	gotPath := rejectPath
	gotMethod := rejectMethod
	mu.Unlock()
	assert.NotEmpty(t, gotPath, "pod must receive the reject POST")
	assert.Contains(t, gotPath, "que_rej")
	assert.Contains(t, gotPath, "/reject")
	assert.Equal(t, http.MethodPost, gotMethod)

	(&usageBridge{h: env.handler}).InputResolved("ws-1", "ses_rej", "que_rej")
	resolvedEvt := recvWithTimeout(t, userSub, "agent.question.resolved")
	resData, ok := resolvedEvt.Data.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "que_rej", resData["request_id"])
}

// TestE2E_QuestionFlow_BadRequestIDReturns400 verifies the input validation
// gate: the proxy must reject malformed IDs before they reach the pod.
func TestE2E_QuestionFlow_BadRequestIDReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)
	env.handler.dialect = &agentoc.Dialect{}
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-pw")

	env.router.POST("/api/v1/workspaces/:id/question/:requestID/reply", env.handler.QuestionReply)

	cases := []string{"not-an-id", "per_abc", "que_", "QUE_UPPER"}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/workspaces/ws-1/question/"+id+"/reply", nil)
			env.router.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"malformed ID %q must be rejected at the proxy before reaching the pod", id)
		})
	}
}

// TestE2E_QuestionFlow_SuspendedWorkspaceReturns503 verifies that a
// suspended workspace never reaches the pod — the proxy must 503 before
// forwarding.
func TestE2E_QuestionFlow_SuspendedWorkspaceReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)
	env.handler.dialect = &agentoc.Dialect{}
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseSuspended), "ws-1"), nil).Maybe()

	env.router.POST("/api/v1/workspaces/:id/question/:requestID/reply", env.handler.QuestionReply)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/question/que_abc/reply", nil)
	env.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

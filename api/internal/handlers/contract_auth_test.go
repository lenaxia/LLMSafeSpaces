// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"io"
	"net"
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
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// authRecordingBackend is a test HTTP backend that records whether each
// incoming request carried valid Basic auth. Used by the contract test
// (US-29.7) to prove every opencode-proxied route sends credentials.
type authRecordingBackend struct {
	mu       sync.Mutex
	requests []authRecord
}

type authRecord struct {
	path    string
	method  string
	hasAuth bool
	user    string
	pass    string
}

func newAuthRecordingBackend() *authRecordingBackend {
	return &authRecordingBackend{}
}

func (b *authRecordingBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		b.mu.Lock()
		b.requests = append(b.requests, authRecord{
			path:    r.URL.Path,
			method:  r.Method,
			hasAuth: ok,
			user:    user,
			pass:    pass,
		})
		b.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sessions") && r.Method == "GET":
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/sessions") && r.Method == "POST":
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"id":"ses_test","title":"test","status":"idle"}`)
		default:
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{}`)
		}
	}
}

func (b *authRecordingBackend) allRequestsHadAuth() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		return false
	}
	for _, r := range b.requests {
		if !r.hasAuth || r.user != "opencode" || r.pass == "" {
			return false
		}
	}
	return true
}

func (b *authRecordingBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

func (b *authRecordingBackend) records() []authRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]authRecord, len(b.requests))
	copy(cp, b.requests)
	return cp
}

// recordsSince returns records added after the given index (exclusive).
// Used by per-route subtests to assert auth on only the records generated
// by that route's request.
func (b *authRecordingBackend) recordsSince(idx int) []authRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	if idx >= len(b.requests) {
		return nil
	}
	cp := make([]authRecord, len(b.requests)-idx)
	copy(cp, b.requests[idx:])
	return cp
}

// TestContract_ProxyRoutesSendBasicAuth (US-29.7) verifies that every
// route proxied through ProxyHandler to opencode (port 4096) includes a
// valid `Authorization: Basic` header. If a new route is added to the
// proxy group that calls opencode without auth, this test fails.
//
// This is a CONTRACT test, not a behavioral test: it only checks auth
// header presence, not response semantics. Each route receives a minimal
// request designed to reach the opencode backend.
func TestContract_ProxyRoutesSendBasicAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	backend := newAuthRecordingBackend()
	srv := httptest.NewServer(backend.handler())
	t.Cleanup(srv.Close)

	transport := &redirectTransport{server: srv}
	httpClient := &http.Client{Transport: transport}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	log := &testLogger{}
	handler, err := NewProxyHandler(k8sMock, log, "default", httpClient, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()

	// Set up workspace + password
	wsName := "ws-contract"
	ws := makeWorkspaceCRDWithStatus(wsName, "10.0.0.1", string(v1.WorkspacePhaseActive), wsName)
	wsMock.On("Get", mock.Anything, wsName, metav1.GetOptions{}).Return(ws, nil).Maybe()

	pwSecret := makePasswordSecret(wsName, "contract-pw")
	_, err = fakeClientset.CoreV1().Secrets("default").Create(context.Background(), pwSecret, metav1.CreateOptions{})
	require.NoError(t, err)

	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	{
		// This list mirrors registerProxyRoutes in api/internal/server/router.go:921.
		// If a new proxy route is added there, add it here too. A future
		// improvement would move this test to the server package and call
		// registerProxyRoutes directly to eliminate drift.
		proxy.POST("/sessions/:sessionId/message", handler.SendMessage)
		proxy.POST("/sessions/:sessionId/prompt", handler.SendPromptAsync)
		proxy.POST("/sessions/:sessionId/queue", handler.EnqueueMessage)
		proxy.GET("/sessions/:sessionId/queue", handler.ListQueue)
		proxy.DELETE("/sessions/:sessionId/queue/:messageId", handler.DeleteQueueMessage)
		proxy.GET("/sessions/:sessionId/message", handler.GetHistory)
		proxy.GET("/sessions/:sessionId", handler.GetSession)
		proxy.POST("/sessions/:sessionId/abort", handler.AbortSession)
		proxy.DELETE("/sessions/:sessionId", handler.DeleteSession)
		proxy.GET("/question", handler.ListQuestions)
		proxy.POST("/question/:requestID/reply", handler.QuestionReply)
		proxy.POST("/question/:requestID/reject", handler.QuestionReject)
		proxy.GET("/permission", handler.ListPermissions)
		proxy.POST("/permission/:requestID/reply", handler.PermissionReply)
	}

	routes := []struct {
		method         string
		path           string
		body           string
		desc           string
		reachesBackend bool // false = route short-circuits before proxying in this test setup
	}{
		// These 4 routes reach the backend with the minimal test fixture:
		{"POST", "/api/v1/workspaces/ws-contract/sessions/ses_x/message", `{"content":"hi"}`, "SendMessage", true},
		{"GET", "/api/v1/workspaces/ws-contract/sessions/ses_x/message", "", "GetHistory", true},
		{"GET", "/api/v1/workspaces/ws-contract/sessions/ses_x", "", "GetSession", true},
		{"DELETE", "/api/v1/workspaces/ws-contract/sessions/ses_x", "", "DeleteSession", true},
		// These routes short-circuit before proxying (no session/queue/question/permission
		// exists in the test fixture, so the handler returns early without hitting opencode).
		// The aggregate assertion below still catches them if a future change makes them proxy.
		{"POST", "/api/v1/workspaces/ws-contract/sessions/ses_x/queue", `{"content":"hi"}`, "EnqueueMessage", false},
		{"GET", "/api/v1/workspaces/ws-contract/sessions/ses_x/queue", "", "ListQueue", false},
		{"GET", "/api/v1/workspaces/ws-contract/question", "", "ListQuestions", false},
		{"POST", "/api/v1/workspaces/ws-contract/question/req1/reply", `{"reply":"yes"}`, "QuestionReply", false},
		{"POST", "/api/v1/workspaces/ws-contract/question/req1/reject", "", "QuestionReject", false},
		{"GET", "/api/v1/workspaces/ws-contract/permission", "", "ListPermissions", false},
		{"POST", "/api/v1/workspaces/ws-contract/permission/req1/reply", `{"reply":"allow"}`, "PermissionReply", false},
	}

	for _, rt := range routes {
		rt := rt
		t.Run(rt.desc, func(t *testing.T) {
			before := backend.count()
			var body io.Reader
			if rt.body != "" {
				body = strings.NewReader(rt.body)
			}
			req := httptest.NewRequest(rt.method, rt.path, body)
			if rt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			newRecords := backend.recordsSince(before)
			if rt.reachesBackend {
				require.NotEmpty(t, newRecords,
					"%s: expected to reach the opencode backend but no backend request was recorded", rt.desc)
				for _, rec := range newRecords {
					assert.True(t, rec.hasAuth && rec.user == "opencode" && rec.pass != "",
						"%s: proxied request to %s %s missing Basic auth: %+v",
						rt.desc, rec.method, rec.path, rec)
				}
			}
		})
	}

	// Cross-route sanity: every backend request across ALL subtests carried auth.
	require.Greater(t, backend.count(), 0, "no routes reached the backend — test setup is broken")
	assert.True(t, backend.allRequestsHadAuth(),
		"one or more opencode-proxied requests were missing Basic auth; records: %+v", backend.records())
}

// TestContract_ModelsRoutesSendBasicAuth (US-29.7) verifies that the
// ListModels handler — which makes a direct HTTP call to opencode
// port 4096 — includes Basic auth.
func TestContract_ModelsRoutesSendBasicAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const testPassword = "contract-models-pw"
	var gotAuth bool
	var authMu sync.Mutex

	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skip("port 4096 not available:", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		authMu.Lock()
		gotAuth = ok && user == "opencode" && pass == testPassword
		authMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"connected":[],"all":[]}`)
	}))
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	handler := newTestModelsHandler(testPassword)

	clearModelCache()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.GET("/api/v1/workspaces/:id/models", handler.ListModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-models/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "ListModels should succeed")
	authMu.Lock()
	assert.True(t, gotAuth, "ListModels must send Basic auth to opencode")
	authMu.Unlock()
}

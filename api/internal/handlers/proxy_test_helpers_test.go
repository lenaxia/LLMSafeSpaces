// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// registerLegacyMessageTransport registers the exact transport call the
// pre-#828-batch-1 SendMessage made (write-op, session-scoped,
// bufferable, chat-error enrichment closure). After batch 1 the raw-proxy
// transport's write-op, connection-ceiling, request-buffer, SSE-streaming,
// and error-body arms have no production caller — the transport ships
// until #828's final batch deletes it — so the suites pinning those arms
// exercise them through this seam.
func registerLegacyMessageTransport(r gin.IRouter, h *ProxyHandler) {
	r.POST("/api/v1/workspaces/:id/legacy-message/:sessionId", func(c *gin.Context) {
		var errBodyTransform func(statusCode int, body []byte) []byte
		if h.agentStateChecker != nil {
			wid := c.Param("id")
			errBodyTransform = func(_ int, body []byte) []byte {
				changedAt, checkerErr := h.agentStateChecker.GetLastCredentialChangedAt(c.Request.Context(), wid)
				if checkerErr != nil || changedAt.IsZero() {
					return EnrichChatErrorBody(body, false, time.Time{}, wid)
				}
				return EnrichChatErrorBody(body, true, changedAt, wid)
			}
		}
		h.proxyToWorkspaceWithErrBody(c,
			"/session/"+c.Param("sessionId")+"/message",
			true, c.Param("sessionId"), errBodyTransform, true)
	})
}

// newMockK8sWithWorkspace creates a mock K8s client whose workspace CRD
// lookup returns an Active workspace at the given pod IP. Extracted from
// the deleted proxy_queue_test.go (US-63.7) for reuse by V2 tests.
func newMockK8sWithWorkspace(t *testing.T, workspaceID, podIP string) *k8smocks.MockKubernetesClient {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus(workspaceID, podIP, string(v1.WorkspacePhaseActive), workspaceID)
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(ws, nil).Maybe()
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset).Maybe()
	return k8sMock
}

// routingTransport routes HTTP requests to different backend hosts based
// on the URL path. Used by V2 integration tests that need to separate
// /event and /v1/statusz traffic from /api/session/* traffic.
// Extracted from the deleted proxy_queue_drain_miss_test.go (US-63.7).
type routingTransport struct {
	eventHost  string
	promptHost string
}

func (rt *routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	if req.URL.Path == "/event" || req.URL.Path == "/v1/statusz" || req.URL.Path == "/v1/healthz" {
		r.URL.Host = rt.eventHost
	} else {
		r.URL.Host = rt.promptHost
	}
	return http.DefaultTransport.RoundTrip(r)
}

// ---------------------------------------------------------------------------
// V2-era harness retained for the #828 batch-2 guard rows: the V2 tails
// are deleted, so these wires only exist to prove the guards fire before
// any legacy behavior could have (the red state was a working V2 server).
// ---------------------------------------------------------------------------

// startV2TestServer starts an httptest.Server on a DYNAMIC port that routes
// by path: /prompt → 200 with admission body, /interrupt → 204. Enforces
// Basic auth. Dynamic port eliminates the port 4096 contention that caused
// non-deterministic CI failures.
func startV2TestServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Defense-in-depth (#707): include the real opencode 1.18.10
			// timeCreated-as-number shape in the canned body so any future
			// re-introduction of a typed V2PromptResponse.TimeCreated field
			// fails at the integration layer too, not just at the unit layer.
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_v2_1","sessionID":"ses-1","timeCreated":1786316936471}}`))
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newV2TestHandler creates a ProxyHandler with V2 flag on, backed by the
// given test server. The v2ClientFactory is injected so V2 methods talk to
// the test server's dynamic port, not port 4096. Routes are registered on a
// real gin router so gin's response lifecycle (WriteHeaderNow flush) runs
// correctly — calling handler methods directly with gin.CreateTestContext
// skips the flush, causing bare c.Status(204) to never reach the recorder.
func newV2TestHandler(t *testing.T, srv *httptest.Server) (*gin.Engine, *ProxyHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "127.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetCachedPasswordForTest("ws-1", "test-pw")
	handler.userBroker = eventbroker.NewUserEventBroker()

	router := gin.New()
	router.POST("/:id/sessions/:sessionId/queue", handler.EnqueueMessage)
	router.POST("/:id/sessions/:sessionId/prompt_async", handler.SendPromptAsync)
	router.POST("/:id/sessions/:sessionId/abort", handler.AbortSession)
	return router, handler
}

// registerLegacyReadTransport pins the pre-batch-2 GetSession transport
// call (non-write, session-scoped, unbuffered) after #828 batch 2 removed
// GetSession/GetHistory as production callers of the generic transport.
// Any-method so DELETE-flavored rows route through it too.
func registerLegacyReadTransport(r gin.IRouter, h *ProxyHandler) {
	r.Any("/api/v1/workspaces/:id/legacy-read/:sessionId", func(c *gin.Context) {
		h.proxyToWorkspaceWithErrBody(c, "/session/"+c.Param("sessionId"), false, c.Param("sessionId"), nil, false)
	})
}

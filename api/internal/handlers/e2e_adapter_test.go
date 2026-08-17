// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
)

// US-65 e2e integration test: exercises the real handler stack with a
// real Adapter against a mock opencode backend. Verifies the full
// pipeline: gin router -> ProxyHandler -> Adapter -> HTTP -> translate
// -> contract JSON response.

func TestE2E_Adapter_GetHistory_FullPipeline(t *testing.T) {
	// Mock opencode backend returning opencode-shaped history.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return opencode-shaped array (info+parts).
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{
				"info": {"role":"user","id":"msg_1"},
				"parts": [{"type":"text","text":"hello"}]
			},
			{
				"info": {"role":"assistant","id":"msg_2","modelID":"gpt-4o"},
				"parts": [
					{"type":"text","text":"hi there"},
					{"type":"step-start"},
					{"type":"step-finish"}
				]
			}
		]`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)

	require.Equal(t, http.StatusOK, w.Code, "handler must return 200")

	// Verify contract-shaped JSON (NOT opencode-shaped).
	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs), "response must be valid contract JSON")
	require.Len(t, msgs, 2, "both messages must survive translation")

	// First message: user text.
	assert.Equal(t, "msg_1", msgs[0].ID)
	assert.Equal(t, session.MessageUser, msgs[0].Type)
	require.Len(t, msgs[0].Parts, 1, "user message has one text part")
	assert.Equal(t, "hello", msgs[0].Parts[0].Text)

	// Second message: assistant text, step-start/step-finish dropped.
	assert.Equal(t, "msg_2", msgs[1].ID)
	assert.Equal(t, session.MessageAssistant, msgs[1].Type)
	require.Len(t, msgs[1].Parts, 1, "step-start and step-finish must be dropped by translator")
	assert.Equal(t, "hi there", msgs[1].Parts[0].Text)
}

func TestE2E_Adapter_ListSessions_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"id":"ses_1","title":"First","status":{"type":"idle"}},
			{"id":"ses_2","title":"Second","status":{"type":"busy"}}
		]`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)

	require.Equal(t, http.StatusOK, w.Code)
	var sessions []session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sessions))
	require.Len(t, sessions, 2)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, "First", sessions[0].Title)
	assert.Equal(t, session.StatusIdle, sessions[0].Status)
	assert.Equal(t, session.StatusBusy, sessions[1].Status)
}

func TestE2E_Adapter_CreateSession_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"ses_new","title":"New","status":{"type":"idle"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions", strings.NewReader(`{}`))

	require.Equal(t, http.StatusOK, w.Code)
	var s session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	assert.Equal(t, "ses_new", s.ID)
	assert.Equal(t, "New", s.Title)
}

func TestE2E_Adapter_SendMessage_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/message") {
			// Read the request body to verify text extraction.
			body, _ := io.ReadAll(r.Body)
			assert.Contains(t, string(body), "hello world")

			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"info": {"role":"assistant","id":"msg_reply"},
				"parts": [{"type":"text","text":"I received your message"}]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello world"}]}`)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message", body)

	require.Equal(t, http.StatusOK, w.Code)
	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg))
	assert.Equal(t, "msg_reply", msg.ID)
	assert.Equal(t, session.MessageAssistant, msg.Type)
	require.Len(t, msg.Parts, 1)
	assert.Equal(t, "I received your message", msg.Parts[0].Text)
}

func TestE2E_Adapter_SendMessage_Error_IncludesCredentialHint(t *testing.T) {
	// When adapter.Send fails AND credentials are stale, the error
	// response must include the needsRefresh hint (same as legacy path).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)

	// Wire agentStateChecker that reports stale credentials.
	env.handler.agentStateChecker = stubAgentStateChecker{
		changedAt: time.Now().Add(-5 * time.Minute),
	}

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message", body)

	require.Equal(t, http.StatusBadGateway, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["agentNeedsRefresh"],
		"enrichment must add agentNeedsRefresh when credentials are stale")
	assert.NotEmpty(t, resp["credentialsPendingSince"],
		"enrichment must include timestamp when credentials are stale")
}

func TestE2E_Adapter_GetHistory_Backend500_Returns502(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// TestE2E_Adapter_GetHistory_FlatToolShape_Returns200 pins the exact
// production code path that 502'd in issue #730: a flat-string tool
// part (opencode 1.18.10 wire shape) from the backend must surface as
// a correct session.ToolPart in the JSON API response.
func TestE2E_Adapter_GetHistory_FlatToolShape_Returns200(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{
				"info": {"role":"assistant","id":"msg_flat"},
				"parts": [
					{"type":"tool","callID":"call_e2e_1","tool":"bash","state":{"status":"completed","input":{"command":"echo hi"},"output":"hi\n","time":{"start":1786374885930,"end":1786374894033}}}
				]
			}
		]`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)

	require.Equal(t, http.StatusOK, w.Code, "flat-string tool shape must NOT 502 (issue #730)")

	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs))
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Parts, 1)
	assert.Equal(t, session.PartTool, msgs[0].Parts[0].Type)
	require.NotNil(t, msgs[0].Parts[0].Tool)
	assert.Equal(t, "bash", msgs[0].Parts[0].Tool.Name)
	assert.Equal(t, "call_e2e_1", msgs[0].Parts[0].Tool.CallID)
	assert.Equal(t, session.ToolStatusCompleted, msgs[0].Parts[0].Tool.State.Status)
}

// TestE2E_Adapter_GetHistory_MalformedMessage_Returns200WithSystemNotice
// pins the Fix 2 decode-resilience path through the handler: one
// undecodable message downgrades to a system notice (200, not 502),
// while well-formed messages survive.
func TestE2E_Adapter_GetHistory_MalformedMessage_Returns200WithSystemNotice(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{
				"info": {"role":"assistant","id":"msg_good"},
				"parts": [{"type":"text","text":"before"}]
			},
			{
				"info": {"role":"assistant","id":"msg_bad"},
				"parts": [{"type":"tool","tool":42}]
			},
			{
				"info": {"role":"assistant","id":"msg_good2"},
				"parts": [{"type":"text","text":"after"}]
			}
		]`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)

	require.Equal(t, http.StatusOK, w.Code, "one malformed message must NOT 502 the whole history (Fix 2)")

	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs))
	require.Len(t, msgs, 3, "all three messages must be present (bad one downgraded)")

	// Good messages survive.
	assert.Equal(t, "msg_good", msgs[0].ID)
	assert.Equal(t, "msg_good2", msgs[2].ID)

	// Bad message downgraded to system notice.
	assert.Equal(t, session.MessageSystem, msgs[1].Type, "undecodable message must be downgraded")
	assert.NotEmpty(t, msgs[1].Text, "downgrade notice must carry explanatory text")
	assert.NotContains(t, msgs[1].Text, "42", "raw malformed bytes must not leak")
}

// TestE2E_Adapter_GetHistory_LargeBodyOver16MiB_No502 is the integration
// regression test for #737. The pre-fix code called readBody(resp, 16<<20)
// inside Adapter.GetHistory, which silently truncated any history body
// larger than 16 MiB — the subsequent json.Unmarshal hit "unexpected end
// of JSON input" and the handler returned 502.
//
// This test stands up a fake opencode backend that streams a ~17 MiB
// JSON array, then drives the FULL request path: gin router →
// ProxyHandler.GetHistory → adapter.GetHistory → real HTTP → streaming
// json.Decoder → contract JSON response. It must return 200 with all
// messages intact.
//
// A revert to readBody(resp, 16<<20)+json.Unmarshal would truncate the
// body, the parse would fail, and the handler would return 502 —
// failing this test at the status-code assertion.
func TestE2E_Adapter_GetHistory_LargeBodyOver16MiB_No502(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Stream a JSON array of 10 messages, each ~1.7 MiB → ~17 MiB total.
		const numMessages = 10
		const textLen = 1700000
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("["))
		for i := 0; i < numMessages; i++ {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			// Write the JSON prefix: info + opening of the text part.
			_, _ = fmt.Fprintf(w, `{"info":{"role":"assistant","id":"msg_%d"},"parts":[{"type":"text","text":"`, i)
			// Write textLen bytes of filler (no escaping needed — 'x' is literal).
			chunk := []byte(strings.Repeat("x", 4096))
			written := 0
			for written < textLen {
				n := textLen - written
				if n > len(chunk) {
					n = len(chunk)
				}
				_, _ = w.Write(chunk[:n])
				written += n
			}
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte(`"}]}`))
		}
		_, _ = w.Write([]byte("]"))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)

	require.Equal(t, http.StatusOK, w.Code,
		"large history body must NOT 502 — streaming decoder must handle >16 MiB (issue #737)")

	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs), "response must be valid contract JSON")
	require.Len(t, msgs, 10, "all 10 messages must survive the streaming decode")
	assert.Equal(t, "msg_0", msgs[0].ID)
	assert.Equal(t, "msg_9", msgs[9].ID)
	require.Len(t, msgs[0].Parts, 1)
	assert.Equal(t, 1700000, len(msgs[0].Parts[0].Text),
		"first message text must be intact (not truncated at the 16 MiB readBody cap)")
}

// TestE2E_Adapter_GetHistory_EmptySession_ReturnsArrayNotNull is the
// integration regression test for the null-history crash. opencode
// returns "[]" for a session with no messages; ParseHistoryStream
// returns a nil slice for empty input (named return, nothing appended).
// paginateContractHistory(nil, ...) also returns nil. Without the
// explicit nil→[] guard at proxy_handlers.go:366-368, the handler
// emits "null" — the frontend's .filter() crashes ("Cannot read
// properties of null").
//
// This test wires a REAL Adapter (so the adapter path is taken, not
// the legacy path), feeds the backend an empty "[]" body, and asserts
// the wire response is byte-identical to "[]", not "null".
//
// Note: TestGetHistory_EmptySession_ReturnsEmptyArrayNotNull (in
// proxy_history_pagination_test.go) looks like it covers this but
// does NOT — its harness (newTestEnvWithBackend) leaves h.adapter
// nil, so the request takes the legacy paginateOpencodeHistory path
// which is structurally immune. Only THIS test exercises the actual
// null-guard in the adapter code path.
func TestE2E_Adapter_GetHistory_EmptySession_ReturnsArrayNotNull(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// opencode returns a bare empty array for empty sessions.
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "[]", w.Body.String(),
		"empty adapter history must serialize as JSON array '[]', not 'null' (frontend .filter crash)")
}

// TestE2E_Adapter_AbortSession_UsesV1AbortNotV2Interrupt is the regression
// test for the V2 interrupt removal in opencode 1.18.10. The entire v2/
// route group was deleted from opencode 1.18.10 — the V2 interrupt
// endpoint (POST /api/session/:id/interrupt) returns 204 from a catch-all
// stub but does nothing. AbortSession must use the V1 /abort endpoint
// (POST /session/:id/abort) which actually stops the in-flight turn.
//
// Verified live on opencode 1.18.10: V2 interrupt returned 204 but a
// long V1 turn kept running; V1 /abort returned 200 and the session
// transitioned to idle within 3s.
//
// This test asserts:
//  1. V1 /abort endpoint is hit exactly once.
//  2. V2 /interrupt endpoint is NEVER hit.
//
// A revert to InterruptV2 would fail both assertions.
func TestE2E_Adapter_AbortSession_UsesV1AbortNotV2Interrupt(t *testing.T) {
	var v1AbortHits, v2InterruptHits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/abort"):
			v1AbortHits++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interrupt"):
			v2InterruptHits++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/abort", nil)

	assert.Equal(t, http.StatusNoContent, w.Code, "AbortSession must return 204")
	assert.Equal(t, 1, v1AbortHits, "V1 POST /session/:id/abort must be called exactly once")
	assert.Equal(t, 0, v2InterruptHits, "V2 POST /api/session/:id/interrupt must NEVER be called (endpoint removed in 1.18.10)")
}

// --- E2E test environment ---

type e2eEnv struct {
	handler *ProxyHandler
	router  *gin.Engine
}

func newE2EEnv(t *testing.T, backend *httptest.Server) *e2eEnv {
	t.Helper()
	backendHost, backendPortStr, ok := strings.Cut(strings.TrimPrefix(backend.URL, "http://"), ":")
	require.True(t, ok, "backend URL must contain a port: %s", backend.URL)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	wsCRD := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: backendHost},
	}
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(wsCRD, nil).Maybe()

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)
	secret := makePasswordSecret("ws-1", "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	port := extractPort(t, backend.URL)
	_ = backendPortStr // used for assertion only
	adapter := opencode.NewAdapter(
		handler.AdapterPasswordResolver(),
		handler.AdapterPodIPResolver(),
		nil,
		opencode.WithAdapterHTTPClient(backend.Client()),
		opencode.WithAdapterPort(port),
	)
	handler.SetAdapter(adapter)
	require.NotNil(t, handler.adapter, "adapter must be wired")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	{
		proxy.POST("/sessions", handler.CreateSession)
		proxy.GET("/sessions", handler.ListSessions)
		proxy.POST("/sessions/:sessionId/message", handler.SendMessage)
		proxy.POST("/sessions/:sessionId/prompt", handler.SendPromptAsync)
		proxy.GET("/sessions/:sessionId/message", handler.GetHistory)
		proxy.GET("/sessions/:sessionId", handler.GetSession)
		proxy.POST("/sessions/:sessionId/abort", handler.AbortSession)
		proxy.DELETE("/sessions/:sessionId", handler.DeleteSession)
	}

	return &e2eEnv{handler: handler, router: router}
}

func (e *e2eEnv) do(method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	e.router.ServeHTTP(w, req)
	return w
}

func extractPort(t *testing.T, url string) int {
	t.Helper()
	// url is "http://127.0.0.1:PORT"
	idx := strings.LastIndex(url, ":")
	require.Greater(t, idx, 0, "URL must contain a port: %s", url)
	var port int
	_, err := fmt.Sscanf(url[idx+1:], "%d", &port)
	require.NoError(t, err)
	return port
}

// Ensure wsstate import stays alive (used in proxy_handler construction).
var _ = wsstate.NewInMemoryStore

// TestE2E_Adapter_SendPromptAsync_ModelForwarding is the full-pipeline pin
// for per-prompt model forwarding (PR #909 review round): a request body
// carrying {"model":{"modelID","providerID"}} must arrive at the opencode
// backend as the fully-qualified "model":"providerID/modelID" — through the
// REAL handler → extractPromptModel → SendOpts → adapter → qualifiedModelID
// chain. Field-mapping mistakes at any seam (e.g. swapped modelID/providerID)
// are only catchable here, not in the seam-isolated unit tests.
func TestE2E_Adapter_SendPromptAsync_ModelForwarding(t *testing.T) {
	var gotBodies []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			gotBodies = append(gotBodies, body)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"info":{"role":"assistant","id":"msg_fwd_1","time":{"created":1786400000000}},"parts":[{"type":"text","text":"ok"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)

	t.Run("qualified override reaches backend verbatim", func(t *testing.T) {
		gotBodies = nil
		w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt",
			strings.NewReader(`{"model":{"modelID":"glm-5.3","providerID":"thekaocloud"},"parts":[{"type":"text","text":"hi"}]}`))
		require.Equal(t, http.StatusOK, w.Code)
		require.Len(t, gotBodies, 1)
		assert.Equal(t, "thekaocloud/glm-5.3", gotBodies[0]["model"],
			"full pipeline must deliver providerID/modelID to opencode")
	})

	t.Run("no selector sends no model key", func(t *testing.T) {
		gotBodies = nil
		w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt",
			strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
		require.Equal(t, http.StatusOK, w.Code)
		require.Len(t, gotBodies, 1)
		_, present := gotBodies[0]["model"]
		assert.False(t, present, "absent selector must omit the model key (session default)")
	})

	t.Run("malformed selector degrades to session default", func(t *testing.T) {
		gotBodies = nil
		w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt",
			strings.NewReader(`{"model":{"providerID":"thekaocloud"},"parts":[{"type":"text","text":"hi"}]}`))
		require.Equal(t, http.StatusOK, w.Code)
		require.Len(t, gotBodies, 1)
		_, present := gotBodies[0]["model"]
		assert.False(t, present, "empty modelID must degrade to default, not fail the prompt")
	})
}

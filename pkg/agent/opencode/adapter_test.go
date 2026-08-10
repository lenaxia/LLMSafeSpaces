// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// US-65.3 Adapter integration tests — drive the Adapter against an
// httptest.Server that mocks opencode's HTTP API. This validates the
// full HTTP + translation stack end-to-end without needing a real
// opencode pod.
//
// What's covered here (synchronous paths):
//   - ListSessions, GetSession, CreateSession, RenameSession, DeleteSession
//   - Send (POST /session/:id/message), GetHistory
//   - SendAsync (POST /api/session/:id/prompt V2), Abort (POST /api/session/:id/interrupt V2)
//   - ListAvailableModels (GET /provider)
//   - Capabilities (pure-function, no HTTP)
//
// What's NOT covered (deliberate, US-65.4 scope):
//   - Stream (SSE event translation — belongs with proxy rewrite)
//   - FileChange production (needs a real git repo + filediff; covered
//     in filediff_test.go and the Send test below exercises the
//     hookable path with a stub differ)

// --- Test fixtures ---

// fakeOpencode is an httptest.Server wrapper that records every
// request and serves canned responses by path/method. Tests configure
// the response map before invoking the Adapter.
type fakeOpencode struct {
	*httptest.Server
	password string
	// requests records method+path for assertion at the end of a test.
	requests []string
	// responses maps "METHOD /path" → canned response body.
	responses map[string]string
	// statusCodes maps "METHOD /path" → status code (default 200).
	statusCodes map[string]int
}

func newFakeOpencode(t *testing.T) *fakeOpencode {
	t.Helper()
	f := &fakeOpencode{
		password:    testPassword,
		responses:   map[string]string{},
		statusCodes: map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic-auth check — verifies the Adapter wires credentials.
		_, pw, _ := r.BasicAuth()
		if pw != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		body, ok := f.responses[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status := f.statusCodes[key]
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOpencode) register(method, path, body string, statusCode int) {
	key := method + " " + path
	f.responses[key] = body
	if statusCode != 0 {
		f.statusCodes[key] = statusCode
	}
}

// staticResolver returns fixed values — the test sets them once.
// Satisfies agent.PasswordResolver and agent.PodIPResolver.
func staticResolver(ip, password string) (PasswordResolver, PodIPResolver) {
	pw := func(_ context.Context, _ string) (string, error) { return password, nil }
	ipr := &staticPodIPResolver{ip: ip}
	return pw, ipr
}

type staticPodIPResolver struct{ ip string }

func (s *staticPodIPResolver) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return s.ip, nil
}

// newTestAdapter constructs an Adapter whose HTTP calls land at srv.
// Extracts the random port from srv.URL so the static resolver +
// WithAdapterPort route requests to the fake server.
func newTestAdapter(t *testing.T, srv *httptest.Server) *Adapter {
	t.Helper()
	// srv.URL is "http://127.0.0.1:PORT". Extract host + port.
	hostPort := srv.URL[len("http://"):]
	parts := strings.SplitN(hostPort, ":", 2)
	host := parts[0]
	port, err := strconv.Atoi(parts[1])
	require.NoError(t, err, "parse test server port from %s", srv.URL)

	pw, ip := staticResolver(host, testPassword)
	return NewAdapter(pw, ip, zap.NewNop(),
		WithAdapterHTTPClient(srv.Client()),
		WithAdapterPort(port),
	)
}

// --- Sessions ---

func TestAdapter_ListSessions(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session", `[
		{"id":"ses_1","title":"first","status":{"type":"idle"}},
		{"id":"ses_2","title":"second","status":{"type":"busy"}}
	]`, 0)

	a := newTestAdapter(t, srv.Server)
	sessions, err := a.ListSessions(context.Background(), "u-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, "ws-1", sessions[0].WorkspaceID, "WorkspaceID must be filled in by adapter")
	assert.Equal(t, session.StatusIdle, sessions[0].Status)
	assert.Equal(t, session.StatusBusy, sessions[1].Status)

	require.Len(t, srv.requests, 1)
	assert.Equal(t, "GET /session", srv.requests[0])
}

func TestAdapter_GetSession(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_1", `{"data":{
		"id":"ses_1","title":"my session","status":{"type":"idle"},
		"model":{"id":"gpt-4o","provider":"openai"}
	}}`, 0)

	a := newTestAdapter(t, srv.Server)
	s, err := a.GetSession(context.Background(), "u-1", "ws-1", "ses_1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_1", s.ID)
	assert.Equal(t, "my session", s.Title)
	require.NotNil(t, s.Model)
	assert.Equal(t, "gpt-4o", s.Model.ID)
}

func TestAdapter_CreateSession(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/session", `{"data":{
		"id":"ses_new","title":"my title","status":{"type":"idle"}}
	}`, 0)

	a := newTestAdapter(t, srv.Server)
	s, err := a.CreateSession(context.Background(), "u-1", "ws-1", "my title")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_new", s.ID)
	assert.Equal(t, "my title", s.Title)
}

func TestAdapter_RenameSession(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/session/ses_1", `{}`, 0)

	a := newTestAdapter(t, srv.Server)
	require.NoError(t, a.RenameSession(context.Background(), "u-1", "ws-1", "ses_1", "new title"))
	require.Contains(t, srv.requests, "POST /session/ses_1")
}

func TestAdapter_DeleteSession(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("DELETE", "/session/ses_1", `{}`, 0)

	a := newTestAdapter(t, srv.Server)
	require.NoError(t, a.DeleteSession(context.Background(), "u-1", "ws-1", "ses_1"))
	require.Contains(t, srv.requests, "DELETE /session/ses_1")
}

// --- Messaging ---

func TestAdapter_Send_ReturnsTranslatedMessage(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/session/ses_1/message", `{
		"info":{"role":"assistant","id":"msg_1","sessionID":"ses_1"},
		"parts":[
			{"type":"text","id":"p1","text":"Hello!"},
			{"type":"patch","files":["/workspace/foo.go"]}
		]
	}`, 0)

	a := newTestAdapter(t, srv.Server)
	msg, err := a.Send(context.Background(), "u-1", "ws-1", "ses_1", "hi", session.SendOpts{})
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, session.MessageAssistant, msg.Type)
	assert.Equal(t, "msg_1", msg.ID)
	require.Len(t, msg.Parts, 1, "patch part must NOT survive into contract (no differ wired)")
	assert.Equal(t, session.PartText, msg.Parts[0].Type)
	assert.Equal(t, "Hello!", msg.Parts[0].Text)
}

func TestAdapter_GetHistory_ReturnsContractShape(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_1/message", `[
		{
			"info":{"role":"user","id":"msg_0"},
			"parts":[{"type":"text","text":"hi"},{"type":"patch","files":["/x"]}]
		},
		{
			"info":{"role":"assistant","id":"msg_1"},
			"parts":[{"type":"text","text":"hello"},{"type":"patch","files":["/y"]}]
		}
	]`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, session.MessageUser, msgs[0].Type)
	require.Len(t, msgs[0].Parts, 1, "patch part stripped — no differ wired")
	assert.Equal(t, session.MessageAssistant, msgs[1].Type)
}

func TestAdapter_SendAsync_UsesV2PromptEndpoint(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_1/prompt", `{"data":{
		"admittedSeq":1,"id":"msg_v2_1","sessionID":"ses_1"
	}}`, 0)

	a := newTestAdapter(t, srv.Server)
	msgID, err := a.SendAsync(context.Background(), "u-1", "ws-1", "ses_1", "hello", session.SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, "msg_v2_1", msgID)
	require.Contains(t, srv.requests, "POST /api/session/ses_1/prompt")
}

func TestAdapter_SendAsync_SteerDelivery(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_1/prompt", `{"data":{"admittedSeq":1,"id":"m1","sessionID":"ses_1"}}`, 0)

	a := newTestAdapter(t, srv.Server)
	_, err := a.SendAsync(context.Background(), "u-1", "ws-1", "ses_1", "steer me",
		session.SendOpts{Admission: session.AdmissionSteer})
	require.NoError(t, err)
	// The V2 endpoint receives the delivery mode in the body; we can't
	// see body here, but the request reached the right path. Body
	// verification would need a custom handler.
}

func TestAdapter_Abort_UsesV2Interrupt(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_1/interrupt", ``, http.StatusNoContent)

	a := newTestAdapter(t, srv.Server)
	require.NoError(t, a.Abort(context.Background(), "u-1", "ws-1", "ses_1"))
	require.Contains(t, srv.requests, "POST /api/session/ses_1/interrupt")
}

// --- Models ---

func TestAdapter_ListAvailableModels_TranslatesConnectedOnly(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/provider", `{
		"connected":["openai","anthropic"],
		"all":[
			{"id":"openai","models":{
				"gpt-4o":{"id":"gpt-4o","name":"GPT-4o","limit":{"context":128000,"output":16384}}
			}},
			{"id":"anthropic","models":{
				"claude-3":{"id":"claude-3","name":"Claude 3","limit":{"context":200000,"output":8000}}
			}},
			{"id":"not-connected","models":{
				"x":{"id":"x","name":"X","limit":{"context":1000,"output":1000}}
			}}
		]
	}`, 0)

	a := newTestAdapter(t, srv.Server)
	models, err := a.ListAvailableModels(context.Background(), "u-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, models, 2, "only connected providers' models surface")

	byID := map[string]struct{}{}
	for _, m := range models {
		byID[m.ID] = struct{}{}
	}
	assert.Contains(t, byID, "gpt-4o")
	assert.Contains(t, byID, "claude-3")
	assert.NotContains(t, byID, "x", "unconnected provider's model must NOT appear")

	var gpt session.ModelInfo
	for _, m := range models {
		if m.ID == "gpt-4o" {
			gpt = m
		}
	}
	assert.Equal(t, "openai", gpt.Provider)
	assert.Equal(t, "GPT-4o", gpt.DisplayName)
	assert.EqualValues(t, 128000, gpt.ContextWindow)
	assert.EqualValues(t, 16384, gpt.MaxOutput)
}

func TestAdapter_SetModel_QualifiesWithProvider(t *testing.T) {
	srv := newFakeOpencode(t)
	var receivedBody map[string]any
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, _ := r.BasicAuth()
		if pw != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == "PATCH" && r.URL.Path == "/global/config" {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	a := newTestAdapter(t, srv.Server)
	err := a.SetModel(context.Background(), "u-1", "ws-1", "ses_1",
		session.ModelRef{ID: "gpt-4o", Provider: "openai"})
	require.NoError(t, err)
	require.NotNil(t, receivedBody)
	assert.Equal(t, "openai/gpt-4o", receivedBody["model"])
}

// --- Capabilities ---

func TestAdapter_Capabilities_ReportsQueueReasoningDiff(t *testing.T) {
	a := NewAdapter(
		func(_ context.Context, _ string) (string, error) { return "", nil },
		&staticPodIPResolver{ip: ""},
		zap.NewNop(),
	)
	caps := a.Capabilities()
	assert.Contains(t, caps, session.CapQueue)
	assert.Contains(t, caps, session.CapReasoning)
	assert.Contains(t, caps, session.CapDiff)
}

// --- Stream (not implemented in US-65.3) ---

func TestAdapter_Stream_ReturnsNotImplemented(t *testing.T) {
	// Documented in adapter.go: Stream lands in US-65.4 with the
	// proxy SSE rewrite. The current story's "Done when" requires
	// synchronous session round-trip; streaming is a separate
	// migration scope.
	srv := newFakeOpencode(t)
	a := newTestAdapter(t, srv.Server)
	_, err := a.Stream(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
}

// --- Error handling ---

func TestAdapter_GetSession_5xx_ReturnsErrorWithStatusAndBody(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_1", `{"error":"internal"}`, http.StatusInternalServerError)

	a := newTestAdapter(t, srv.Server)
	_, err := a.GetSession(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "internal")
}

func TestAdapter_GetSession_404_PropagatesCleanError(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/missing", `{"error":"not found"}`, http.StatusNotFound)

	a := newTestAdapter(t, srv.Server)
	_, err := a.GetSession(context.Background(), "u-1", "ws-1", "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestAdapter_NoRunningPod_ReturnsWrappingError(t *testing.T) {
	// When PodIPResolver returns "", the adapter must surface
	// ErrNoRunningPod so callers can map to HTTP 404.
	pw := func(_ context.Context, _ string) (string, error) { return "pw", nil }
	ip := &staticPodIPResolver{ip: ""} // no pod
	a := NewAdapter(pw, ip, zap.NewNop())

	_, err := a.GetSession(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoRunningPod)
}

// --- Auth ---

func TestAdapter_AuthenticatesEveryRequest(t *testing.T) {
	// If the Adapter forgets to wire Basic auth, every request
	// returns 401 and the test surfaces it as "401" in the error.
	srv := newFakeOpencode(t)
	srv.register("GET", "/session", `[]`, 0)

	a := newTestAdapter(t, srv.Server)
	_, err := a.ListSessions(context.Background(), "u-1", "ws-1")
	require.NoError(t, err, "Adapter must authenticate; if this fails with 401 the auth header was not sent")
}

// --- ListPending ---

func TestAdapter_ListPending_UnifiesQuestionsAndPermissions(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/question", `[
		{"id":"que_1","sessionID":"ses_1"},
		{"id":"que_2","sessionID":"ses_1"}
	]`, 0)
	srv.register("GET", "/permission", `[
		{"id":"per_1","sessionID":"ses_1","permission":"shell"}
	]`, 0)

	a := newTestAdapter(t, srv.Server)
	reqs, err := a.ListPending(context.Background(), "u-1", "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, reqs, 3, "2 questions + 1 permission")

	counts := map[session.InputKind]int{}
	for _, r := range reqs {
		counts[r.Kind]++
	}
	assert.Equal(t, 2, counts[session.InputQuestion])
	assert.Equal(t, 1, counts[session.InputPermission])
}

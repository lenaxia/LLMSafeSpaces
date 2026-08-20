// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/filediff"
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
	// bodies records the last request body per "METHOD /path".
	bodies map[string][]byte
	// responses maps "METHOD /path" → canned response body.
	responses map[string]string
	// statusCodes maps "METHOD /path" → status code (default 200).
	statusCodes map[string]int
}

func newFakeOpencode(t *testing.T) *fakeOpencode {
	t.Helper()
	f := &fakeOpencode{
		password:    testPassword,
		bodies:      map[string][]byte{},
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
		if reqBody, err := io.ReadAll(r.Body); err == nil {
			f.bodies[key] = reqBody
			// #909 regression net (structural): POST /session/:id/message
			// mirrors opencode 1.18.10's schema — model must be an OBJECT
			// (modelID+providerID) or absent, NEVER the
			// "providerID/modelID" string, which real opencode rejects
			// with this exact error. A reverted string-form send fails the
			// adapter's error path here, not just a shape assertion. (The
			// V2 prompt endpoint is NOT covered by this net — it posts to
			// /api/session/{sid}/prompt with model nested under
			// prompt.model, and its own OpenAPI at 1.18.10 exposes no
			// model property at all — the object form there is a
			// revival-safety mirror of V1, pinned by the explicit
			// assertions in TestAdapter_SendAsync_ModelReferenceForm.)
			if r.Method == http.MethodPost &&
				(strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/message")) {
				var payload map[string]any
				if err := json.Unmarshal(reqBody, &payload); err == nil {
					if m, exists := payload["model"]; exists {
						switch v := m.(type) {
						case map[string]any:
							_, hasID := v["modelID"]
							_, hasProv := v["providerID"]
							if !hasID || !hasProv {
								w.WriteHeader(http.StatusBadRequest)
								_, _ = w.Write([]byte(`{"name":"BadRequest","data":{"message":"Expected object with modelID+providerID","kind":"Payload"}}`))
								return
							}
						default:
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprintf(w, `{"name":"BadRequest","data":{"message":"Expected object | null, got %q at [\"model\"]","kind":"Payload"}}`, fmt.Sprint(v))
							return
						}
					}
				}
			}
		}
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

// TestAdapter_GetHistory_FlatToolShape_ReturnsContractShape is an
// integration test that sends a flat-string tool part (opencode 1.18.10
// wire shape) through the Adapter's real HTTP round-trip and verifies
// a correct session.ToolPart in the output. Pins the production code
// path that 502'd in issue #730.
func TestAdapter_GetHistory_FlatToolShape_ReturnsContractShape(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_1/message", `[
		{
			"info":{"role":"user","id":"msg_0"},
			"parts":[{"type":"text","text":"run ls"}]
		},
		{
			"info":{"role":"assistant","id":"msg_1"},
			"parts":[
				{"type":"text","text":"running it"},
				{"type":"tool","callID":"call_flat_1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"file.go\n","time":{"start":1786374885930,"end":1786374894033}}}
			]
		}
	]`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	assistant := msgs[1]
	require.Equal(t, session.MessageAssistant, assistant.Type)

	var tp *session.ToolPart
	for _, p := range assistant.Parts {
		if p.Type == session.PartTool {
			tp = p.Tool
		}
	}
	require.NotNil(t, tp, "flat-string tool part must survive the HTTP round-trip")
	assert.Equal(t, "bash", tp.Name)
	assert.Equal(t, "call_flat_1", tp.CallID)
	assert.Equal(t, session.ToolStatusCompleted, tp.State.Status)
	require.NotNil(t, tp.State.StartedAt)
	assert.Equal(t, int64(1786374885930), tp.State.StartedAt.UnixMilli())
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

// TestAdapter_SendAsync_ModelReferenceForm extends the model-form contract
// (see TestAdapter_Send_ModelReferenceForm) to the V2 prompt path: a
// qualified override must reach the V2 body in the OBJECT form
// {"modelID","providerID"}; bare/unqualified is omitted. The V2 path is
// dead on opencode 1.18.10 (queue never drained, #755) but remains wired
// for revival — leaving it model-blind would replay the incident the day
// it comes back.
func TestAdapter_SendAsync_ModelReferenceForm(t *testing.T) {
	t.Run("qualified override is forwarded", func(t *testing.T) {
		srv := newFakeOpencode(t)
		srv.register("POST", "/api/session/ses_1/prompt", `{"data":{"admittedSeq":1,"id":"m1","sessionID":"ses_1"}}`, 0)

		a := newTestAdapter(t, srv.Server)
		_, err := a.SendAsync(context.Background(), "u-1", "ws-1", "ses_1", "hi",
			session.SendOpts{Model: &session.ModelRef{ID: "glm-5.3", Provider: "thekaocloud"}})
		require.NoError(t, err)

		var sent map[string]any
		require.NoError(t, json.Unmarshal(srv.bodies["POST /api/session/ses_1/prompt"], &sent))
		prompt, ok := sent["prompt"].(map[string]any)
		require.True(t, ok, "V2 body nests fields under prompt")
		m, ok := prompt["model"].(map[string]any)
		require.True(t, ok, "V2 model must be the OBJECT wire form, matching V1 (opencode 1.18.10 schema)")
		assert.Equal(t, "glm-5.3", m["modelID"])
		assert.Equal(t, "thekaocloud", m["providerID"])
	})

	t.Run("bare override omitted", func(t *testing.T) {
		srv := newFakeOpencode(t)
		srv.register("POST", "/api/session/ses_1/prompt", `{"data":{"admittedSeq":1,"id":"m1","sessionID":"ses_1"}}`, 0)

		a := newTestAdapter(t, srv.Server)
		_, err := a.SendAsync(context.Background(), "u-1", "ws-1", "ses_1", "hi",
			session.SendOpts{Model: &session.ModelRef{ID: "glm-5.3"}})
		require.NoError(t, err)

		var sent map[string]any
		require.NoError(t, json.Unmarshal(srv.bodies["POST /api/session/ses_1/prompt"], &sent))
		prompt, ok := sent["prompt"].(map[string]any)
		require.True(t, ok)
		_, hasModel := prompt["model"]
		assert.False(t, hasModel)
	})
}

func TestAdapter_Abort_UsesV1AbortEndpoint(t *testing.T) {
	// Abort must use V1 POST /session/:id/abort, not the V2 interrupt
	// endpoint which was removed in opencode 1.18.10. Register both
	// endpoints; only V1 /abort should be hit.
	srv := newFakeOpencode(t)
	srv.register("POST", "/session/ses_1/abort", ``, http.StatusOK)
	srv.register("POST", "/api/session/ses_1/interrupt", ``, http.StatusNoContent)

	a := newTestAdapter(t, srv.Server)
	require.NoError(t, a.Abort(context.Background(), "u-1", "ws-1", "ses_1"))
	require.Contains(t, srv.requests, "POST /session/ses_1/abort",
		"Abort must use V1 /session/:id/abort (the only interrupt endpoint on opencode 1.18.10+)")
	require.NotContains(t, srv.requests, "POST /api/session/ses_1/interrupt",
		"Abort must NOT use V2 /api/session/:id/interrupt (endpoint removed in opencode 1.18.10)")
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
	assert.NotContains(t, caps, session.CapDiff, "CapDiff must not be advertised when differ is nil (#745)")
}

func TestAdapter_Capabilities_WithDiffer_ReportsCapDiff(t *testing.T) {
	a := NewAdapter(
		func(_ context.Context, _ string) (string, error) { return "", nil },
		&staticPodIPResolver{ip: ""},
		zap.NewNop(),
		WithFileDiffProducer(&filediff.Producer{}),
	)
	caps := a.Capabilities()
	assert.Contains(t, caps, session.CapDiff, "CapDiff must be advertised when differ is wired")
}

// --- Stream (not implemented in US-65.3) ---

func TestAdapter_Stream_HappyPath_TranslatesEvents(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, _ := r.BasicAuth()
		if pw != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"properties\":{\"sessionID\":\"ses_1\",\"status\":{\"type\":\"busy\"}}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"question.asked\",\"properties\":{\"id\":\"que_1\",\"sessionID\":\"ses_1\",\"questions\":[{\"question\":\"Continue?\"}]}}\n\n")
		flusher.Flush()
		time.Sleep(50 * time.Millisecond)
	})

	a := newTestAdapter(t, srv.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := a.Stream(ctx, "u-1", "ws-1", "ses_1")
	require.NoError(t, err)

	var events []session.Event
	for evt := range ch {
		events = append(events, evt)
		if len(events) >= 2 {
			break
		}
	}
	cancel()

	require.GreaterOrEqual(t, len(events), 1)
	assert.Equal(t, session.EventSessionStatus, events[0].Type)
	assert.Equal(t, "ses_1", events[0].SessionID)
	assert.Equal(t, session.StatusBusy, events[0].Status)

	if len(events) >= 2 {
		assert.Equal(t, session.EventInputRequest, events[1].Type)
		require.NotNil(t, events[1].Input)
		assert.Equal(t, "que_1", events[1].Input.ID)
		assert.Equal(t, session.InputQuestion, events[1].Input.Kind)
		assert.Equal(t, "Continue?", events[1].Input.Question)
	}
}

func TestAdapter_Stream_Non200Response_ReturnsError(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	a := newTestAdapter(t, srv.Server)
	_, err := a.Stream(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
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
	// Full question shape matching what dialect.ParseQuestionRequest expects
	// (from the question.asked event shape — see dialect.go ocQuestionEvent).
	srv.register("GET", "/question", `[
		{
			"id":"que_1","sessionID":"ses_1",
			"questions":[{"question":"Which option?","header":"Choose","options":[{"label":"A","description":"first"},{"label":"B","description":"second"}],"multiple":false,"custom":true}],
			"tool":{"messageID":"msg_1","callID":"call_1"}
		},
		{
			"id":"que_2","sessionID":"ses_1",
			"questions":[{"question":"Another?","header":"Header2","options":[]}]
		}
	]`, 0)
	// Full permission shape matching dialect.ParsePermissionRequest.
	srv.register("GET", "/permission", `[
		{"id":"per_1","sessionID":"ses_1","permission":"shell","patterns":["bash"],"always":["/workspace"]}
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

	// Verify full content survived the parse (PR #717 review critical
	// bug: stub parser discarded everything except id+sessionID).
	var q1 *session.InputRequest
	for i := range reqs {
		if reqs[i].ID == "que_1" {
			q1 = &reqs[i]
		}
	}
	require.NotNil(t, q1, "que_1 must be present")
	assert.Equal(t, "Which option?", q1.Question, "question text must survive parse")
	assert.Equal(t, "Choose", q1.Header)
	assert.True(t, q1.Custom)
	require.Len(t, q1.Options, 2)
	assert.Equal(t, "A", q1.Options[0].Label)
	assert.Equal(t, "first", q1.Options[0].Description)
	require.NotNil(t, q1.Tool, "tool ref must survive parse")
	assert.Equal(t, "msg_1", q1.Tool.MessageID)
	assert.Equal(t, "call_1", q1.Tool.CallID)

	var p1 *session.InputRequest
	for i := range reqs {
		if reqs[i].ID == "per_1" {
			p1 = &reqs[i]
		}
	}
	require.NotNil(t, p1, "per_1 must be present")
	assert.Equal(t, "shell", p1.Permission)
	assert.Contains(t, p1.Patterns, "bash")
	assert.Contains(t, p1.Always, "/workspace")
}

// --- Resolve (PR #714 review follow-up: was untested) ---

func TestAdapter_Resolve_QuestionReply_HappyPath(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/question/que_1/reply", `{}`, 0)

	a := newTestAdapter(t, srv.Server)
	err := a.Resolve(context.Background(), "u-1", "ws-1", "que_1", "option-a")
	require.NoError(t, err)
	require.Contains(t, srv.requests, "POST /question/que_1/reply")
	require.NotContains(t, srv.requests, "POST /permission",
		"question reply succeeded → must NOT fall through to permission")
}

func TestAdapter_Resolve_FallsBackToPermissionOn404(t *testing.T) {
	// When /question/:id/reply returns 404, the adapter must try
	// /permission/:id/reply. This is the core complexity of Resolve.
	srv := newFakeOpencode(t)
	srv.register("POST", "/question/que_1/reply", `not found`, http.StatusNotFound)
	srv.register("POST", "/permission/que_1/reply", `{}`, 0)

	a := newTestAdapter(t, srv.Server)
	err := a.Resolve(context.Background(), "u-1", "ws-1", "que_1", "allow")
	require.NoError(t, err)
	require.Contains(t, srv.requests, "POST /permission/que_1/reply",
		"404 on question → must fall through to permission reply")
}

func TestAdapter_Resolve_QuestionReply5xx_ReturnsError(t *testing.T) {
	// A 5xx on question-reply must surface as an error (not fall
	// through to permission — that would mask a server failure).
	srv := newFakeOpencode(t)
	srv.register("POST", "/question/que_1/reply", `internal`, http.StatusInternalServerError)

	a := newTestAdapter(t, srv.Server)
	err := a.Resolve(context.Background(), "u-1", "ws-1", "que_1", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	require.NotContains(t, srv.requests, "POST /permission",
		"5xx on question must NOT fall through to permission")
}

// --- FormatProviderConfig (PR #714 review follow-up: was untested) ---

func TestAdapter_FormatProviderConfig_ProducesValidConfig(t *testing.T) {
	a := NewAdapter(
		func(_ context.Context, _ string) (string, error) { return "", nil },
		&staticPodIPResolver{ip: ""},
		zap.NewNop(),
	)
	out, err := a.FormatProviderConfig([]agent.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "sk-test"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, out)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &cfg))
	require.Contains(t, cfg, "provider", "formatted config must have provider map")

	var providers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["provider"], &providers))
	require.Contains(t, providers, "openai")
}

// --- ValidateCredentials (PR #714 review follow-up: was untested) ---

func TestAdapter_ValidateCredentials_AllStates(t *testing.T) {
	a := NewAdapter(
		func(_ context.Context, _ string) (string, error) { return "", nil },
		&staticPodIPResolver{ip: ""},
		zap.NewNop(),
	)
	cases := []struct {
		name      string
		input     []byte
		wantState agent.CredentialState
	}{
		{"empty bytes", []byte(``), agent.CredentialStateMissing},
		{"empty object", []byte(`{}`), agent.CredentialStateMissing},
		{"present provider", []byte(`{"provider":{"openai":{}}}`), agent.CredentialStatePresent},
		{"invalid json", []byte(`not json`), agent.CredentialStateInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := a.ValidateCredentials(c.input)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.Equal(t, c.wantState, res.State)
			assert.Equal(t, agent.AgentTypeOpenCode, res.Agent)
		})
	}
}

// --- FileChange production via WithFileDiffProducer (PR #714 review follow-up) ---

func TestAdapter_Send_WithFileDiffProducer_ProducesFileChangeParts(t *testing.T) {
	// The reviewer flagged that the FileChange production path through
	// the Adapter was untested. This test wires a real filediff.Producer
	// against a temp git repo, sends a message with a patch part, and
	// verifies the response carries PartFileChange parts with the
	// correct ChangeStatus detection.
	dir := t.TempDir()
	runGitAdapterHelper(t, dir, "init", "-b", "main")
	runGitAdapterHelper(t, dir, "config", "user.email", "test@example.com")
	runGitAdapterHelper(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package foo\n"), 0o644))
	runGitAdapterHelper(t, dir, "add", "foo.go")
	runGitAdapterHelper(t, dir, "commit", "-m", "baseline")
	// Modify the file so diff has something to report.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package foo // modified\n"), 0o644))

	producer, err := filediff.NewProducer(dir)
	require.NoError(t, err)

	srv := newFakeOpencode(t)
	srv.register("POST", "/session/ses_1/message", `{
		"info":{"role":"assistant","id":"msg_1","sessionID":"ses_1"},
		"parts":[
			{"type":"text","id":"p1","text":"made changes"},
			{"type":"patch","files":["foo.go"]}
		]
	}`, 0)

	a := newTestAdapter(t, srv.Server)
	// Override with the wired producer.
	a.differ = producer

	msg, err := a.Send(context.Background(), "u-1", "ws-1", "ses_1", "edit foo.go", session.SendOpts{})
	require.NoError(t, err)
	require.NotNil(t, msg)

	// Find the FileChange part.
	var fc *session.FileDiff
	for i := range msg.Parts {
		if msg.Parts[i].Type == session.PartFileChange {
			fc = msg.Parts[i].FileChange
			break
		}
	}
	require.NotNil(t, fc, "Send with differ wired must produce a FileChange part")
	assert.Equal(t, "foo.go", fc.Path)
	assert.Equal(t, session.ChangeModified, fc.Status,
		"modified existing file → ChangeStatus must be ChangeModified")
	assert.Contains(t, fc.Patch, "package foo")
	assert.Contains(t, fc.Patch, "-package foo")
	assert.Contains(t, fc.Patch, "+package foo // modified")
}

// runGitAdapterHelper is a local git-invocation helper for the
// FileChange integration test. Lives here (not in filediff/) because
// it's only needed for adapter_test.go's cross-package integration.
func runGitAdapterHelper(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// --- Send error path (PR #714 review follow-up) ---

func TestAdapter_Send_5xx_ReturnsErrorWithStatus(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/session/ses_1/message", `{"error":"internal"}`, http.StatusInternalServerError)

	a := newTestAdapter(t, srv.Server)
	_, err := a.Send(context.Background(), "u-1", "ws-1", "ses_1", "hi", session.SendOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "internal")
}

// TestAdapter_Send_LargeResponseOver4MB_NoTruncation is the regression
// test for the Send response cap fix. The previous code used
// readBody(resp, 4<<20) which silently truncated any assistant turn
// with verbose tool output (>4 MB). This test feeds a >5 MB response
// body (a valid assistant message with a very large text part) and
// asserts it decodes without error and the text is intact.
//
// Reverting to readBody(resp, 4<<20)+json.Unmarshal would truncate the
// body at 4 MB, the JSON parse would fail with "unexpected end of JSON
// input", and Send would return an error — failing this test.
func TestAdapter_Send_LargeResponseOver4MB_NoTruncation(t *testing.T) {
	// Build a valid opencode message with >5 MB of text content.
	// 5 MB > the old 4 MB cap but well under the new 64 MB cap.
	largeText := strings.Repeat("x", 5<<20)
	body := `{"info":{"role":"assistant","id":"msg_big"},"parts":[{"type":"text","text":"` +
		largeText + `"}]}`

	srv := newFakeOpencode(t)
	srv.register("POST", "/session/ses_1/message", body, 0)

	a := newTestAdapter(t, srv.Server)
	msg, err := a.Send(context.Background(), "u-1", "ws-1", "ses_1", "hi", session.SendOpts{})
	require.NoError(t, err, "Send must not fail on response bodies >4 MB")
	require.NotNil(t, msg)
	assert.Equal(t, "msg_big", msg.ID)
	require.Len(t, msg.Parts, 1)
	assert.Equal(t, 5<<20, len(msg.Parts[0].Text), "full text must survive (not truncated at the old 4 MB cap)")
}

// --- SendAsync error paths (PR #714 review follow-up) ---

func TestAdapter_SendAsync_404_NotFound(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_missing/prompt", `not found`, http.StatusNotFound)

	a := newTestAdapter(t, srv.Server)
	_, err := a.SendAsync(context.Background(), "u-1", "ws-1", "ses_missing", "hi", session.SendOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrV2SessionNotFound,
		"404 from V2 prompt must surface as ErrV2SessionNotFound so callers can distinguish missing-session from transient errors")
}

func TestAdapter_SendAsync_409_Conflict(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("POST", "/api/session/ses_1/prompt", `conflict`, http.StatusConflict)

	a := newTestAdapter(t, srv.Server)
	_, err := a.SendAsync(context.Background(), "u-1", "ws-1", "ses_1", "hi", session.SendOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrV2PromptConflict,
		"409 from V2 prompt must surface as ErrV2PromptConflict so callers can retry or surface to user")
}

// --- ParseSessionListWire empty wrapped (PR #714 review C2 regression) ---

func TestParseSessionListWire_EmptyWrapped_ReturnsEmptyNotError(t *testing.T) {
	// C2 regression: {"data": []} is a valid wrapped response with
	// zero sessions. The previous logic fell through to the bare-array
	// parse, which failed because the body is an object, not an array.
	body := []byte(`{"data":[]}`)
	sessions, err := ParseSessionListWire(body, "ws-1")
	require.NoError(t, err)
	assert.Empty(t, sessions, "empty wrapped response must return empty slice, not error")
}

// --- ListPending error path (PR #714 review R1 follow-up; PR #852 C1 fix) ---

// TestAdapter_ListPending_ServerError_ReturnsPendingUnavailable verifies the
// fetch-failure contract: 5xx from the agent's pending-input endpoints means
// "pending set UNKNOWN", never "authoritative empty". Callers (the SSE input
// snapshot) rely on a non-nil error to mark the snapshot ok:false — returning
// nil here made failed fetches commit empty pending sets over live prompts.
func TestAdapter_ListPending_ServerError_ReturnsPendingUnavailable(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/question", `internal`, http.StatusInternalServerError)
	srv.register("GET", "/permission", `internal`, http.StatusInternalServerError)

	a := newTestAdapter(t, srv.Server)
	reqs, err := a.ListPending(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err, "5xx must surface as an error — the pending set is unknown")
	assert.ErrorIs(t, err, ErrPendingUnavailable)
	assert.Empty(t, reqs)
}

func TestAdapter_ListPending_TransportError_ReturnsPendingUnavailable(t *testing.T) {
	// Server closed before the call — connection refused on both endpoints.
	srv := newFakeOpencode(t)
	a := newTestAdapter(t, srv.Server)
	srv.Close()

	_, err := a.ListPending(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err, "transport failure must surface as an error — the pending set is unknown")
	assert.ErrorIs(t, err, ErrPendingUnavailable)
}

func TestAdapter_ListPending_PermissionsServerError_ReturnsPendingUnavailable(t *testing.T) {
	// Partial failure: questions list fine, permissions 5xx. The snapshot is
	// not a complete picture of the workspace's pending set — still an error.
	srv := newFakeOpencode(t)
	srv.register("GET", "/question", `[]`, http.StatusOK)
	srv.register("GET", "/permission", `internal`, http.StatusInternalServerError)

	a := newTestAdapter(t, srv.Server)
	_, err := a.ListPending(context.Background(), "u-1", "ws-1", "ses_1")
	require.Error(t, err, "partial fetch failure must surface as an error")
	assert.ErrorIs(t, err, ErrPendingUnavailable)
}

func TestAdapter_ListPending_NotImplemented404_ReturnsEmptyNoError(t *testing.T) {
	// 404 = endpoint not implemented in this agent version — no pending
	// input is possible via that endpoint; empty is authoritative, not a
	// failure.
	srv := newFakeOpencode(t)
	srv.register("GET", "/question", `missing`, http.StatusNotFound)
	srv.register("GET", "/permission", `missing`, http.StatusNotFound)

	a := newTestAdapter(t, srv.Server)
	reqs, err := a.ListPending(context.Background(), "u-1", "ws-1", "ses_1")
	require.NoError(t, err, "404 not-implemented is an authoritative empty, not a failure")
	assert.Empty(t, reqs)
}

// TestAdapter_Send_ModelReferenceForm pins the model wire form at the
// adapter seam across both incidents:
//
//   - 2026-08-16: a bare ID parses as providerID-with-empty-modelID and
//     fails EVERY prompt ("ProviderModelNotFoundError: <bare>/.").
//   - 2026-08-17: the "providerID/modelID" STRING form fails opencode
//     1.18.10's schema decode ("Expected object | null, got string") —
//     the all-sessions-502 regression from #909, missed because mocked
//     tests asserted the string shape. The per-prompt model field is the
//     OBJECT {"modelID", "providerID"} (both required,
//     additionalProperties false — packages/sdk/openapi.json @v1.18.10,
//     POST /session/{id}/message).
//
// The adapter must send the object (Provider-authoritative split rules per
// #913 round-5), or omit the field entirely for unexpressible shapes —
// never a bare ID, never a string.
func TestAdapter_Send_ModelReferenceForm(t *testing.T) {
	sendAndInspect := func(t *testing.T, opts session.SendOpts) map[string]any {
		t.Helper()
		srv := newFakeOpencode(t)
		srv.register("POST", "/session/ses_1/message", `{
			"info":{"role":"assistant","id":"msg_1"},
			"parts":[{"type":"text","text":"ok"}]
		}`, 0)

		a := newTestAdapter(t, srv.Server)
		_, err := a.Send(context.Background(), "u-1", "ws-1", "ses_1", "hi", opts)
		require.NoError(t, err)

		var sent map[string]any
		require.NoError(t, json.Unmarshal(srv.bodies["POST /session/ses_1/message"], &sent))

		// Schema-shape guard (upstream openapi @v1.18.10): when present,
		// model is an object with EXACTLY modelID+providerID, both
		// strings. Fails on any regression to the string form — the
		// failure mode that let #909 ship.
		if raw, present := sent["model"]; present {
			m, ok := raw.(map[string]any)
			require.True(t, ok,
				"model must be the OBJECT wire form ({modelID, providerID}) — a string fails opencode 1.18.10 schema decode")
			require.Len(t, m, 2, "additionalProperties: false — exactly modelID+providerID")
			mid, midOK := m["modelID"].(string)
			prov, provOK := m["providerID"].(string)
			require.True(t, midOK && provOK, "modelID+providerID are required strings")
			require.NotEmpty(t, mid, "empty modelID is the incident parse shape (<bare>/.)")
			require.NotEmpty(t, prov, "empty providerID routes nowhere")
		}
		return sent
	}

	modelObj := func(t *testing.T, sent map[string]any) (modelID, providerID string) {
		t.Helper()
		raw, present := sent["model"]
		require.True(t, present, "expressible override must send the model object")
		m := raw.(map[string]any) // shape already enforced by the helper guard
		return m["modelID"].(string), m["providerID"].(string)
	}

	t.Run("provider and id form the object", func(t *testing.T) {
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "glm-5.3", Provider: "thekaocloud"}})
		mid, prov := modelObj(t, sent)
		assert.Equal(t, "glm-5.3", mid)
		assert.Equal(t, "thekaocloud", prov)
	})

	t.Run("provider-less slashed id splits first segment", func(t *testing.T) {
		// opencode's own routing rule (proven by the 2026-08-16
		// incident): the FIRST '/' separates provider from model.
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "thekaocloud/glm-5.3"}})
		mid, prov := modelObj(t, sent)
		assert.Equal(t, "glm-5.3", mid)
		assert.Equal(t, "thekaocloud", prov)
	})

	t.Run("slash-bearing id with provider is never double-prefixed", func(t *testing.T) {
		// PR #909 review round: Provider="x" + ID="x/y" previously
		// produced "x/x/y" — a guaranteed per-prompt failure reachable
		// via hand-crafted /prompt bodies.
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "thekaocloud/glm-5.3", Provider: "thekaocloud"}})
		mid, prov := modelObj(t, sent)
		assert.Equal(t, "glm-5.3", mid)
		assert.Equal(t, "thekaocloud", prov,
			"a slash-bearing ID already carrying the authoritative prefix is stripped, never re-prefixed")
	})

	t.Run("provider is authoritative for the frontend double form", func(t *testing.T) {
		// Round 5: the frontend sends the advertised (slashed) catalog ID
		// plus the routing providerID. Provider is authoritative: the
		// vendor namespace ("anthropic") must land in modelID, never in
		// providerID — opencode would otherwise route via a nonexistent
		// provider.
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "anthropic/claude-sonnet-4.5", Provider: "openrouter"}})
		mid, prov := modelObj(t, sent)
		assert.Equal(t, "anthropic/claude-sonnet-4.5", mid,
			"the advertised slashed catalog ID is the modelID verbatim")
		assert.Equal(t, "openrouter", prov,
			"the explicit routing provider prefixes routing; the vendor namespace is never the provider")
	})

	t.Run("bare id without provider is omitted", func(t *testing.T) {
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "glm-5.3"}})
		_, present := sent["model"]
		assert.False(t, present,
			"a bare ID cannot be expressed in opencode form and must be omitted so the session default applies")
	})

	t.Run("empty tail without provider is omitted", func(t *testing.T) {
		// "a/" is the 2026-08-16 incident's own parse shape: provider "a"
		// with an empty modelID.
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "thekaocloud/"}})
		_, present := sent["model"]
		assert.False(t, present, "an empty-tail ID is unexpressible")
	})

	t.Run("provider-less multi-segment empty tail is omitted", func(t *testing.T) {
		// "a/b/" splits at the first "/" into modelID "b/" — a degenerate
		// empty tail again. The empty-tail contract must hold after any
		// split, not just for the single-segment "a/" shape.
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "thekaocloud/glm/"}})
		_, present := sent["model"]
		assert.False(t, present, "a trailing slash leaves a degenerate empty modelID after the split — omit")
	})

	t.Run("empty tail with matching provider is omitted", func(t *testing.T) {
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "thekaocloud/", Provider: "thekaocloud"}})
		_, present := sent["model"]
		assert.False(t, present, "provider + empty tail leaves no modelID — omit")
	})

	t.Run("empty tail with non-matching provider is omitted", func(t *testing.T) {
		// TrimPrefix("x/","y/") does not match, so the degenerate empty
		// modelID would otherwise be forwarded verbatim — the empty-tail
		// contract must hold regardless of provider match.
		sent := sendAndInspect(t, session.SendOpts{Model: &session.ModelRef{ID: "thekaocloud/", Provider: "openrouter"}})
		_, present := sent["model"]
		assert.False(t, present, "any empty-tail ID is unexpressible, even with a non-matching provider")
	})

	t.Run("no model sends no model key", func(t *testing.T) {
		sent := sendAndInspect(t, session.SendOpts{})
		_, present := sent["model"]
		assert.False(t, present)
	})
}

// --- ContextUsageFromEvent (epic 65 S3: live context occupancy) ---

func TestAdapterContextUsageFromEvent(t *testing.T) {
	a := NewAdapter(nil, nil, zap.NewNop())

	t.Run("step-finish part event maps to prompt occupancy", func(t *testing.T) {
		raw := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_a","part":{"type":"step-finish","reason":"tool-calls","cost":0,"tokens":{"total":3950,"input":2310,"output":285,"reasoning":75,"cache":{"read":1280,"write":0}}}}}`
		sid, usage, ok := a.ContextUsageFromEvent("message.part.updated.1", raw)
		assert.True(t, ok)
		assert.Equal(t, "ses_a", sid)
		require.NotNil(t, usage)
		assert.Equal(t, int64(2310+1280+0), usage.Used, "Used = input + cache.read + cache.write")
		assert.Zero(t, usage.Window, "Window comes from ListAvailableModels, not usage events")
	})

	t.Run("legacy step.ended event still decodes (mixed fleet)", func(t *testing.T) {
		raw := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_old","tokens":{"input":800,"output":400,"reasoning":100,"cache":{"read":200,"write":50}}}}`
		sid, usage, ok := a.ContextUsageFromEvent("session.next.step.ended", raw)
		assert.True(t, ok)
		assert.Equal(t, "ses_old", sid)
		require.NotNil(t, usage)
		assert.Equal(t, int64(1050), usage.Used)
	})

	t.Run("text part events carry no usage", func(t *testing.T) {
		raw := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_a","part":{"type":"text","text":"hi"}}}`
		_, _, ok := a.ContextUsageFromEvent("message.part.updated.1", raw)
		assert.False(t, ok)
	})

	t.Run("delta events are not usage", func(t *testing.T) {
		raw := `{"type":"message.part.delta","properties":{"sessionID":"ses_a","part":{"type":"text"}}}`
		_, _, ok := a.ContextUsageFromEvent("message.part.delta", raw)
		assert.False(t, ok)
	})

	t.Run("step-finish without tokens is drift, not usage", func(t *testing.T) {
		raw := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_a","part":{"type":"step-finish"}}}`
		_, _, ok := a.ContextUsageFromEvent("message.part.updated.1", raw)
		assert.False(t, ok, "claims-usage-but-undecodable must surface as ok=false (logged as drift)")
	})

	t.Run("golden fixture step-finish events all decode", func(t *testing.T) {
		for _, fixture := range []string{"testdata/sse_events_1_18_10_live.jsonl", "testdata/event_store_1_18_10.jsonl"} {
			data, err := os.ReadFile(fixture)
			require.NoError(t, err)
			decoded, total := 0, 0
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var env struct {
					Type string `json:"type"`
				}
				require.NoError(t, json.Unmarshal([]byte(line), &env))
				if !strings.HasPrefix(env.Type, "message.part.updated") {
					continue
				}
				var probe struct {
					Part struct {
						Type string `json:"type"`
					} `json:"part"`
				}
				var wrapper struct {
					Properties json.RawMessage `json:"properties"`
				}
				_ = json.Unmarshal([]byte(line), &wrapper)
				if json.Unmarshal(wrapper.Properties, &probe) == nil && probe.Part.Type == "step-finish" {
					total++
					sid, usage, ok := a.ContextUsageFromEvent(env.Type, line)
					assert.True(t, ok, "%s: golden step-finish must decode: %s", fixture, line[:min(80, len(line))])
					assert.NotEmpty(t, sid)
					assert.NotNil(t, usage)
					assert.Positive(t, usage.Used)
					decoded++
				}
			}
			assert.Positive(t, decoded, "%s must contain decodable step-finish events", fixture)
			assert.Equal(t, total, decoded, "%s: every golden step-finish must decode", fixture)
		}
	})
}

// --- MeteringFromEvent (the seam's billing decode contract) ---

func TestAdapterMeteringFromEvent(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	a := NewAdapter(nil, nil, zap.New(core))

	t.Run("non-metering event is nil/false, silent", func(t *testing.T) {
		logs.TakeAll()
		u, ok, err := a.MeteringFromEvent("message.part.updated", []byte(`{"sessionID":"s","part":{"type":"text"}}`))
		assert.NoError(t, err)
		assert.False(t, ok)
		assert.Nil(t, u)
		assert.Empty(t, logs.All(), "non-metering traffic must never warn")
	})

	t.Run("info-less session.updated routes to zero-value/true (incomplete-fields path)", func(t *testing.T) {
		// The round-2 contract: ok=false is reserved for "not a metering
		// event". An info-less session.updated IS one, with missing
		// fields — the caller's incomplete-fields warn depends on this.
		logs.TakeAll()
		u, ok, err := a.MeteringFromEvent("session.updated", []byte(`{"properties":{"sessionID":"s"}}`))
		require.NoError(t, err)
		require.True(t, ok)
		require.NotNil(t, u)
		assert.Empty(t, *u, "zero-value usage: caller's incomplete-fields warn fires")
	})

	t.Run("undecodable metering event errors AND warns (sole drift signal)", func(t *testing.T) {
		logs.TakeAll()
		// The adapter takes envelope-stripped properties; a non-object
		// properties value is the drift case.
		u, ok, err := a.MeteringFromEvent("session.updated", []byte(`[1,2]`))
		require.Error(t, err)
		assert.False(t, ok)
		assert.Nil(t, u)
		found := false
		for _, e := range logs.All() {
			if e.Message == "opencode metering event claims usage but fails to decode — wire drift?" {
				found = true
			}
		}
		assert.True(t, found, "the adapter warn is the sole surviving metering-drift signal (tracker returns silently)")
	})

	t.Run("full decode carries attribution", func(t *testing.T) {
		logs.TakeAll()
		raw := `{"sessionID":"ses_a","info":{"id":"ses_a","cost":0.042,"tokens":{"input":1000,"output":500},"model":{"id":"glm-5.3","providerID":"thekaocloud"}}}`
		u, ok, err := a.MeteringFromEvent("session.updated", []byte(raw))
		require.NoError(t, err)
		require.True(t, ok)
		require.NotNil(t, u)
		assert.Equal(t, "ses_a", u.SessionID)
		assert.Equal(t, "glm-5.3", u.ModelID)
		assert.Equal(t, "thekaocloud", u.ProviderID)
		assert.Equal(t, int64(1000), u.InputTokens)
		assert.Equal(t, int64(500), u.OutputTokens)
		assert.InDelta(t, 0.042, u.CostUSD, 1e-9)
		assert.False(t, u.CostMalformed)
	})

	t.Run("suffixed type decodes (store surface)", func(t *testing.T) {
		raw := `{"sessionID":"s","info":{"id":"s","cost":0,"tokens":{"input":5,"output":5},"model":{"id":"m","provider":"p"}}}`
		u, ok, err := a.MeteringFromEvent("session.updated.1", []byte(raw))
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "m", u.ModelID)
		assert.Equal(t, "p", u.ProviderID, "provider fallback applies")
	})
}

// --- ClientEventsFromEvent / RetryFromEvent (US-65.8 client bridge) ---

func TestAdapterClientEventsFromEvent(t *testing.T) {
	a := NewAdapter(nil, nil, zap.NewNop())

	t.Run("part delta maps to contract part.delta", func(t *testing.T) {
		raw := `{"id":"evt1","type":"message.part.delta","properties":{"sessionID":"ses_a","messageID":"msg_1","partID":"prt_1","field":"text","delta":"The"}}`
		evts := a.ClientEventsFromEvent("message.part.delta", raw)
		require.Len(t, evts, 1)
		e := evts[0]
		assert.Equal(t, session.EventPartDelta, e.Type)
		assert.Equal(t, "ses_a", e.SessionID)
		assert.Equal(t, "msg_1", e.MessageID)
		assert.Equal(t, "prt_1", e.PartID)
		assert.Equal(t, "The", e.Delta)
	})

	t.Run("part update maps to contract part.end with translated part", func(t *testing.T) {
		raw := `{"id":"evt2","type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"id":"prt_2","sessionID":"ses_a","messageID":"msg_1","type":"text","text":"hello world"}}}`
		evts := a.ClientEventsFromEvent("message.part.updated", raw)
		require.Len(t, evts, 1)
		e := evts[0]
		assert.Equal(t, session.EventPartEnd, e.Type)
		assert.Equal(t, "ses_a", e.SessionID)
		assert.Equal(t, "msg_1", e.MessageID)
		assert.Equal(t, "prt_2", e.PartID)
		require.NotNil(t, e.Part)
		assert.Equal(t, session.PartText, e.Part.Type)
		assert.Equal(t, "hello world", e.Part.Text)
	})

	t.Run("step-finish part update yields context usage session update", func(t *testing.T) {
		raw := `{"id":"evt3","type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"id":"prt_3","type":"step-finish","tokens":{"input":2310,"cache":{"read":1280,"write":0}}}}}`
		evts := a.ClientEventsFromEvent("message.part.updated", raw)
		require.Len(t, evts, 1)
		e := evts[0]
		assert.Equal(t, session.EventSessionUpdated, e.Type)
		require.NotNil(t, e.Session)
		require.NotNil(t, e.Session.ContextUsage)
		assert.Equal(t, int64(3590), e.Session.ContextUsage.Used)
	})

	t.Run("step-start part update yields nothing", func(t *testing.T) {
		raw := `{"id":"evt4","type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"id":"prt_4","type":"step-start"}}}`
		assert.Empty(t, a.ClientEventsFromEvent("message.part.updated", raw))
	})

	t.Run("session.updated yields title/parent session update", func(t *testing.T) {
		raw := `{"id":"evt5","type":"session.updated","properties":{"sessionID":"ses_a","info":{"id":"ses_a","title":"New title","parentID":"ses_root"}}}`
		evts := a.ClientEventsFromEvent("session.updated", raw)
		require.Len(t, evts, 1)
		e := evts[0]
		assert.Equal(t, session.EventSessionUpdated, e.Type)
		require.NotNil(t, e.Session)
		assert.Equal(t, "ses_a", e.Session.ID)
		assert.Equal(t, "New title", e.Session.Title)
		assert.Equal(t, "ses_root", e.Session.ParentID)
		assert.Nil(t, e.Session.ContextUsage, "session.updated tokens are CUMULATIVE — never map to ContextUsage (per-step semantics)")
	})

	t.Run("error events map to contract error", func(t *testing.T) {
		raw := `{"id":"evt6","type":"session.error","properties":{"sessionID":"ses_a","error":{"name":"ContextOverflowError","data":{"message":"too long"}}}}`
		evts := a.ClientEventsFromEvent("session.error", raw)
		require.Len(t, evts, 1)
		assert.Equal(t, session.EventError, evts[0].Type)
		require.NotNil(t, evts[0].Error)
		assert.Equal(t, "ContextOverflowError", evts[0].Error.Code)
		assert.Equal(t, "too long", evts[0].Error.Message)

		raw2 := `{"id":"evt7","type":"session.next.step.failed","properties":{"sessionID":"ses_a","error":{"type":"unknown","message":"HTTP 401"}}}`
		evts2 := a.ClientEventsFromEvent("session.next.step.failed", raw2)
		require.Len(t, evts2, 1)
		assert.Equal(t, session.EventError, evts2[0].Type)
		assert.Equal(t, "HTTP 401", evts2[0].Error.Message)
	})

	t.Run("non-client events yield nothing (hot path)", func(t *testing.T) {
		for _, typ := range []string{"session.status", "server.heartbeat", "plugin.added", "file.edited"} {
			assert.Empty(t, a.ClientEventsFromEvent(typ, `{"id":"e","type":"`+typ+`","properties":{"sessionID":"s"}}`), typ)
		}
	})
}

func TestAdapterRetryFromEvent(t *testing.T) {
	a := NewAdapter(nil, nil, zap.NewNop())

	t.Run("retry status decodes", func(t *testing.T) {
		raw := `{"id":"evt8","type":"session.status","properties":{"sessionID":"ses_a","status":{"type":"retry","attempt":2,"message":"provider 429","next":1787023325114,"action":"retry"}}}`
		sid, retry, ok := a.RetryFromEvent("session.status", raw)
		require.True(t, ok)
		assert.Equal(t, "ses_a", sid)
		require.NotNil(t, retry)
		assert.Equal(t, 2, retry.Attempt)
		assert.Equal(t, "provider 429", retry.Message)
		assert.Equal(t, int64(1787023325114), retry.NextAt)
		assert.Equal(t, "retry", retry.Action)
	})

	t.Run("busy and idle are not retries", func(t *testing.T) {
		_, _, ok := a.RetryFromEvent("session.status", `{"properties":{"sessionID":"s","status":{"type":"busy"}}}`)
		assert.False(t, ok)
	})

	t.Run("other event types are not retries", func(t *testing.T) {
		_, _, ok := a.RetryFromEvent("session.updated", `{"properties":{}}`)
		assert.False(t, ok)
	})
}

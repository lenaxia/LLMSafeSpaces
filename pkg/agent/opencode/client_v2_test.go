// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v2TestServer is a minimal httptest.Server that records the last request
// and returns a canned response. It enforces Basic auth (matching the real
// opencode contract) so tests catch auth-header regressions.
type v2TestServer struct {
	tb         testing.TB
	server     *httptest.Server
	lastReq    *http.Request
	lastBody   []byte
	respStatus int
	respBody   string
}

func newV2TestServer(tb testing.TB, password string) *v2TestServer {
	ts := &v2TestServer{tb: tb, respStatus: http.StatusOK}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Enforce Basic auth (same as real opencode).
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		ts.lastReq = r
		ts.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ts.respStatus)
		if ts.respBody != "" {
			_, _ = w.Write([]byte(ts.respBody))
		}
	}))
	tb.Cleanup(ts.server.Close)
	return ts
}

func TestPromptV2_Success(t *testing.T) {
	pw := "testpw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusOK
	// Real opencode 1.18.10 response shape: timeCreated is a NUMBER (epoch
	// millis), not a string. The previous test used an ISO-8601 string and
	// masked the V2PromptResponse.timeCreated decode bug (#707).
	ts.respBody = `{"data":{"admittedSeq":3,"id":"msg_abc","sessionID":"ses_123","prompt":{"text":"hi"},"delivery":"queue","timeCreated":1723204800000}}`

	c := NewClient(ts.server.URL, pw, nil)

	resp, err := c.PromptV2(context.Background(), "ses_123", "hi", V2DeliveryQueue)
	if err != nil {
		t.Fatalf("PromptV2: %v", err)
	}
	if resp.AdmittedSeq != 3 {
		t.Fatalf("AdmittedSeq = %d, want 3", resp.AdmittedSeq)
	}
	if resp.ID != "msg_abc" {
		t.Fatalf("ID = %q, want msg_abc", resp.ID)
	}
	if resp.SessionID != "ses_123" {
		t.Fatalf("SessionID = %q, want ses_123", resp.SessionID)
	}

	// Verify path.
	if ts.lastReq.URL.Path != "/api/session/ses_123/prompt" {
		t.Fatalf("path = %q, want /api/session/ses_123/prompt", ts.lastReq.URL.Path)
	}
	if ts.lastReq.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", ts.lastReq.Method)
	}

	// F18: verify body uses {prompt:{text}} NOT {prompt:{parts:[...]}}.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(ts.lastBody, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, hasParts := body["parts"]; hasParts {
		t.Fatalf("body must NOT contain top-level parts (V1 shape); got %s", ts.lastBody)
	}
	promptRaw, ok := body["prompt"]
	if !ok {
		t.Fatalf("body missing prompt key: %s", ts.lastBody)
	}
	var promptObj map[string]json.RawMessage
	if err := json.Unmarshal(promptRaw, &promptObj); err != nil {
		t.Fatalf("prompt is not an object: %v", err)
	}
	if _, hasParts := promptObj["parts"]; hasParts {
		t.Fatalf("F18: prompt must NOT contain parts array (1.18.10 rejects it); got %s", ts.lastBody)
	}
	textRaw, ok := promptObj["text"]
	if !ok || strings.Trim(string(textRaw), `"`) != "hi" {
		t.Fatalf("prompt.text missing or wrong: %s", ts.lastBody)
	}
	// Verify delivery.
	if d := body["delivery"]; strings.Trim(string(d), `"`) != "queue" {
		t.Fatalf("delivery = %s, want queue", d)
	}

	// F17: verify NO id field on the request (caller must not supply one).
	if _, hasID := body["id"]; hasID {
		t.Fatalf("F17: body must NOT contain caller-supplied id (PromptConflictError risk); got %s", ts.lastBody)
	}
}

// TestPromptV2_RealShapeTimeCreatedAsNumber pins the regression for #707:
// opencode 1.18.10 returns timeCreated as an epoch-millis NUMBER. Any
// future change that re-introduces a typed V2PromptResponse.TimeCreated
// field as a string MUST fail here — the previous "spike-verified" claim
// was never validated against real opencode output and shipped a latent
// decode-failure 500 that only surfaced when LLMSAFESPACES_V2_SESSION_QUEUE
// was enabled in v0.13.0.
//
// This test exists specifically to prevent that failure mode from
// recurring: it asserts the response DECODES successfully when
// timeCreated is a number, which it will only do as long as the struct
// either omits the field or types it as a number-compatible type.
func TestPromptV2_RealShapeTimeCreatedAsNumber(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusOK
	// Verbatim shape observed from opencode 1.18.10 in production (issue #707).
	ts.respBody = `{"data":{"admittedSeq":7,"id":"msg_015cbf1c","sessionID":"ses_015cbf1c","prompt":{"text":"."},"delivery":"queue","timeCreated":1786316936471}}`

	c := NewClient(ts.server.URL, pw, nil)
	resp, err := c.PromptV2(context.Background(), "ses_015cbf1c", ".", V2DeliveryQueue)
	if err != nil {
		t.Fatalf("decode must succeed with timeCreated as number (#707 regression): %v", err)
	}
	if resp.ID != "msg_015cbf1c" {
		t.Fatalf("ID = %q, want msg_015cbf1c", resp.ID)
	}
}

func TestPromptV2_DeliverySteer(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusOK
	ts.respBody = `{"data":{"admittedSeq":1,"id":"msg_x","sessionID":"ses_y"}}`

	c := NewClient(ts.server.URL, pw, nil)
	_, err := c.PromptV2(context.Background(), "ses_y", "steer me", V2DeliverySteer)
	if err != nil {
		t.Fatalf("PromptV2: %v", err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(ts.lastBody, &body)
	if d := body["delivery"]; strings.Trim(string(d), `"`) != "steer" {
		t.Fatalf("delivery = %s, want steer", d)
	}
}

func TestPromptV2_Conflict409(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusConflict
	ts.respBody = `{"error":{"type":"PromptConflictError","message":"id already exists"}}`

	c := NewClient(ts.server.URL, pw, nil)
	_, err := c.PromptV2(context.Background(), "ses_1", "hi", V2DeliveryQueue)
	if err == nil {
		t.Fatal("expected error on 409")
	}
	if !errors.Is(err, ErrV2PromptConflict) {
		t.Fatalf("expected ErrV2PromptConflict, got: %v", err)
	}
}

func TestPromptV2_SessionNotFound404(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusNotFound
	ts.respBody = `{"error":{"type":"SessionNotFound","message":"session does not exist"}}`

	c := NewClient(ts.server.URL, pw, nil)
	_, err := c.PromptV2(context.Background(), "bogus", "hi", V2DeliveryQueue)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !errors.Is(err, ErrV2SessionNotFound) {
		t.Fatalf("expected ErrV2SessionNotFound, got: %v", err)
	}
}

func TestPromptV2_BadRequest400(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusBadRequest
	ts.respBody = `{"error":{"type":"InvalidRequestError","message":"Missing key"}}`

	c := NewClient(ts.server.URL, pw, nil)
	_, err := c.PromptV2(context.Background(), "ses_1", "hi", V2DeliveryQueue)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	// 400 is a client error but not conflict or not-found; it should NOT
	// match either sentinel.
	if errors.Is(err, ErrV2PromptConflict) || errors.Is(err, ErrV2SessionNotFound) {
		t.Fatalf("400 should not map to conflict or not-found: %v", err)
	}
}

func TestPromptV2_AuthHeaderSent(t *testing.T) {
	pw := "secretPw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusOK
	ts.respBody = `{"data":{"admittedSeq":1,"id":"m","sessionID":"s"}}`

	c := NewClient(ts.server.URL, pw, nil)
	_, _ = c.PromptV2(context.Background(), "s", "hi", V2DeliveryQueue)

	user, pass, ok := ts.lastReq.BasicAuth()
	if !ok || user != "opencode" || pass != pw {
		t.Fatalf("Basic auth = %q/%q, want opencode/%s", user, pass, pw)
	}
}

func TestInterruptV2_Success204(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusNoContent
	ts.respBody = ""

	c := NewClient(ts.server.URL, pw, nil)
	err := c.InterruptV2(context.Background(), "ses_123")
	if err != nil {
		t.Fatalf("InterruptV2: %v", err)
	}
	if ts.lastReq.URL.Path != "/api/session/ses_123/interrupt" {
		t.Fatalf("path = %q, want /api/session/ses_123/interrupt", ts.lastReq.URL.Path)
	}
	if ts.lastReq.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", ts.lastReq.Method)
	}
}

func TestInterruptV2_IdleSession204(t *testing.T) {
	// The spike confirmed interrupt on idle returns 204 (no-op success).
	// The client must treat 204 as success, not an error.
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusNoContent

	c := NewClient(ts.server.URL, pw, nil)
	err := c.InterruptV2(context.Background(), "idle_ses")
	if err != nil {
		t.Fatalf("interrupt on idle should be 204 success, got: %v", err)
	}
}

func TestInterruptV2_ServerError(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusInternalServerError

	c := NewClient(ts.server.URL, pw, nil)
	err := c.InterruptV2(context.Background(), "ses_1")
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestInterruptV2_AuthHeaderSent(t *testing.T) {
	pw := "secret"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusNoContent

	c := NewClient(ts.server.URL, pw, nil)
	_ = c.InterruptV2(context.Background(), "s")

	user, pass, _ := ts.lastReq.BasicAuth()
	if user != "opencode" || pass != pw {
		t.Fatalf("Basic auth = %q/%q, want opencode/%s", user, pass, pw)
	}
}

// --- Client.Abort (V1 /abort) tests ---
// The following tests mirror the InterruptV2 coverage: success, server
// error, and auth-header assertion. Client.Abort hits POST /session/:id/abort
// which is the only interrupt endpoint on opencode 1.18.10+.

func TestClient_Abort_Success200(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusOK
	ts.respBody = ""

	c := NewClient(ts.server.URL, pw, nil)
	err := c.Abort(context.Background(), "ses_123")
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if ts.lastReq.URL.Path != "/session/ses_123/abort" {
		t.Fatalf("path = %q, want /session/ses_123/abort", ts.lastReq.URL.Path)
	}
	if ts.lastReq.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", ts.lastReq.Method)
	}
}

func TestClient_Abort_ServerError(t *testing.T) {
	pw := "pw"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusInternalServerError

	c := NewClient(ts.server.URL, pw, nil)
	err := c.Abort(context.Background(), "ses_1")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error must contain status code 500, got: %v", err)
	}
}

func TestClient_Abort_AuthHeaderSent(t *testing.T) {
	pw := "secret"
	ts := newV2TestServer(t, pw)
	ts.respStatus = http.StatusOK

	c := NewClient(ts.server.URL, pw, nil)
	_ = c.Abort(context.Background(), "s")

	user, pass, _ := ts.lastReq.BasicAuth()
	if user != "opencode" || pass != pw {
		t.Fatalf("Basic auth = %q/%q, want opencode/%s", user, pass, pw)
	}
}

// TestMessagesV2_RealShape drives MessagesV2 against the golden V2
// history fixture (testdata/v2_messages_1_18_15.json — captured live
// from opencode 1.18.15, IDs redacted; provenance in testdata/REFRESH.md).
func TestMessagesV2_RealShape(t *testing.T) {
	fixture, err := os.ReadFile("testdata/v2_messages_1_18_15.json")
	require.NoError(t, err)

	ts := newV2TestServer(t, "pw")
	ts.respBody = string(fixture)
	ts.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/session/ses_1/message", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture)
	})

	c := &Client{baseURL: ts.server.URL, password: "pw", httpClient: ts.server.Client()}
	parsed, err := c.MessagesV2(context.Background(), "ses_1")
	require.NoError(t, err)
	require.Len(t, parsed, 6)

	// Newest-first ordering: index 0 is the newest assistant, the last
	// index is the oldest user message.
	assert.Equal(t, "assistant", parsed[0].Type)
	assert.Equal(t, "user", parsed[5].Type)
	assert.Equal(t, "v2 fixture message one", parsed[5].Text)

	// The tool-carrying assistant message: content[1] is the bash call.
	var toolMsg *V2Message
	for i := range parsed {
		for _, cp := range parsed[i].Content {
			if cp.Type == "tool" {
				toolMsg = &parsed[i]
			}
		}
	}
	require.NotNil(t, toolMsg, "fixture must carry a tool content part")
	var found bool
	for _, cp := range toolMsg.Content {
		if cp.Type == "tool" {
			found = true
			assert.Equal(t, "bash", cp.Name)
			require.NotNil(t, cp.State)
			assert.Equal(t, "completed", cp.State.Status)
		}
	}
	assert.True(t, found)
}

// TestMessagesV2_ErrorStatus ensures non-2xx surfaces a typed error with
// the status and body — the outbox's error classification depends on it.
func TestMessagesV2_ErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`agent starting`))
	}))
	t.Cleanup(ts.Close)

	c := &Client{baseURL: ts.URL, password: "pw", httpClient: ts.Client()}
	_, err := c.MessagesV2(context.Background(), "ses_1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
	assert.Contains(t, err.Error(), "agent starting")
}

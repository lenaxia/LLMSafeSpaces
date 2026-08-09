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
	"strings"
	"testing"
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
	ts.respBody = `{"data":{"admittedSeq":3,"id":"msg_abc","sessionID":"ses_123","prompt":{"text":"hi"},"delivery":"queue","timeCreated":"2026-08-09T12:00:00Z"}}`

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

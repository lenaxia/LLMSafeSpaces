// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// inputRecorder captures method+path per request for route assertions.
type inputRecorder struct {
	method string
	path   string
	body   map[string]any
}

func newInputClient(t *testing.T, status int, payload string, rec *inputRecorder) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.method = r.Method
			rec.path = r.URL.Path
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&rec.body)
			}
		}
		w.WriteHeader(status)
		if payload != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, WithAPIKey("lsp_test"))
}

func TestInputRequests_ListQuestions_TypedContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/question" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id": "que_18b28260affeoxXrX1iwPH8wFg",
			"sessionId": "ses_1",
			"rootSessionId": "ses_root",
			"kind": "question",
			"question": "What language?",
			"header": "Choose language",
			"options": [{"label": "Go", "description": "Fast"}, {"label": "Python"}],
			"multiple": false,
			"custom": true,
			"tool": {"messageId": "msg_abc", "callId": "call_xyz"}
		}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	reqs, err := c.InputRequests.ListQuestions(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	q := reqs[0]
	if q.ID != "que_18b28260affeoxXrX1iwPH8wFg" || q.SessionID != "ses_1" || q.RootSessionID != "ses_root" {
		t.Errorf("identity fields not decoded: %+v", q)
	}
	if q.Kind != "question" || q.Question != "What language?" || q.Header != "Choose language" {
		t.Errorf("question fields not decoded: %+v", q)
	}
	if !q.Custom || q.Multiple {
		t.Errorf("flags not decoded: %+v", q)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "Go" || q.Options[0].Description != "Fast" {
		t.Errorf("options not decoded: %+v", q.Options)
	}
	if q.Tool == nil || q.Tool.MessageID != "msg_abc" || q.Tool.CallID != "call_xyz" {
		t.Errorf("tool not decoded: %+v", q.Tool)
	}
}

func TestInputRequests_ListQuestions_LegacyEnvelopeIsNotTheContract(t *testing.T) {
	// A server speaking the RETIRED legacy envelope (questions[] array,
	// snake_case session_id) must not decode into the typed contract —
	// kind would be empty, proving the SDK consumes only InputRequest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"que_x","session_id":"ses_1","questions":[{"question":"Q?","options":[]}]}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	reqs, err := c.InputRequests.ListQuestions(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(reqs))
	}
	if reqs[0].Kind != "" || reqs[0].SessionID != "" || reqs[0].Question != "" {
		t.Errorf("legacy envelope fields leaked into the contract type: %+v", reqs[0])
	}
}

func TestInputRequests_ListPermissions_TypedContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/permission" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id": "per_1",
			"kind": "permission",
			"permission": "bash",
			"patterns": ["/workspace/src/main.go"],
			"always": ["/workspace/*"],
			"metadata": {"command": "go build"}
		}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	reqs, err := c.InputRequests.ListPermissions(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	p := reqs[0]
	if p.Kind != "permission" || p.Permission != "bash" {
		t.Errorf("permission fields not decoded: %+v", p)
	}
	if len(p.Patterns) != 1 || p.Patterns[0] != "/workspace/src/main.go" {
		t.Errorf("patterns not decoded: %+v", p.Patterns)
	}
	if len(p.Always) != 1 || p.Always[0] != "/workspace/*" {
		t.Errorf("always not decoded: %+v", p.Always)
	}
	if p.Metadata == nil || p.Metadata["command"] != "go build" {
		t.Errorf("metadata not decoded: %+v", p.Metadata)
	}
}

func TestInputRequests_ReplyQuestion_LiveAnswer(t *testing.T) {
	rec := &inputRecorder{}
	c := newInputClient(t, http.StatusOK, "", rec)
	late, err := c.InputRequests.ReplyQuestion(context.Background(), "ws-1", "que_1", [][]string{{"Go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if late != nil {
		t.Errorf("live answer (200) must return a nil late-answer, got %+v", late)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/workspaces/ws-1/question/que_1/reply" {
		t.Errorf("wrong route: %s %s", rec.method, rec.path)
	}
	answers, ok := rec.body["answers"].([]any)
	if !ok || len(answers) != 1 {
		t.Fatalf("answers not sent as the wire body: %+v", rec.body)
	}
	if inner, ok := answers[0].([]any); !ok || len(inner) != 1 || inner[0] != "Go" {
		t.Errorf("answers wire shape wrong: %+v", answers)
	}
}

// The r1 review's misclassification scenario: a server that (against the
// published contract) answers 200 WITH a body must not be classified as
// a late answer — live-vs-late is decided by the status code, never by
// body presence.
func TestInputRequests_ReplyQuestion_LiveAnswerWithBodyIsNotLate(t *testing.T) {
	c := newInputClient(t, http.StatusOK, `{"status":"answered"}`, nil)
	late, err := c.InputRequests.ReplyQuestion(context.Background(), "ws-1", "que_1", [][]string{{"Go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if late != nil {
		t.Errorf("200-with-body must still classify as a live answer, got %+v", late)
	}
}

func TestInputRequests_ReplyQuestion_LateAnswer202(t *testing.T) {
	rec := &inputRecorder{}
	c := newInputClient(t, http.StatusAccepted,
		`{"status":"queued","clientMessageID":"inbox-que_1-answer","messageID":"msg_out_1","duplicate":true}`, rec)
	late, err := c.InputRequests.ReplyQuestion(context.Background(), "ws-1", "que_1", [][]string{{"Go"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if late == nil {
		t.Fatal("202 must return the InboxLateAnswerAccepted body")
	}
	if late.Status != "queued" || late.ClientMessageID != "inbox-que_1-answer" ||
		late.MessageID != "msg_out_1" || !late.Duplicate {
		t.Errorf("late-answer body not decoded: %+v", late)
	}
}

func TestInputRequests_ReplyPermission_LiveAnswer(t *testing.T) {
	rec := &inputRecorder{}
	c := newInputClient(t, http.StatusOK, "", rec)
	late, err := c.InputRequests.ReplyPermission(context.Background(), "ws-1", "per_1", "once", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if late != nil {
		t.Errorf("live answer (200) must return a nil late-answer, got %+v", late)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/workspaces/ws-1/permission/per_1/reply" {
		t.Errorf("wrong route: %s %s", rec.method, rec.path)
	}
	if rec.body["reply"] != "once" {
		t.Errorf("reply vocabulary not sent: %+v", rec.body)
	}
	if _, has := rec.body["message"]; has {
		t.Errorf("empty message must be omitted: %+v", rec.body)
	}
}

func TestInputRequests_ReplyPermission_LateAnswer202(t *testing.T) {
	c := newInputClient(t, http.StatusAccepted,
		`{"status":"queued","clientMessageID":"inbox-per_1-answer","messageID":"msg_out_2"}`, nil)
	late, err := c.InputRequests.ReplyPermission(context.Background(), "ws-1", "per_1", "always", "context note")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if late == nil || late.Status != "queued" || late.MessageID != "msg_out_2" {
		t.Fatalf("late-answer body not decoded: %+v", late)
	}
}

func TestInputRequests_Reply_ConflictAndUnavailable(t *testing.T) {
	c := newInputClient(t, http.StatusConflict, `{"error":"record dismissed"}`, nil)
	_, err := c.InputRequests.ReplyQuestion(context.Background(), "ws-1", "que_1", [][]string{{"Go"}})
	if err == nil || !IsConflict(err) {
		t.Errorf("409 must map to Conflict, got %v", err)
	}
	c2 := newInputClient(t, http.StatusServiceUnavailable, `{"error":"pending set unknown"}`, nil)
	_, err = c2.InputRequests.ReplyPermission(context.Background(), "ws-1", "per_1", "reject", "")
	if err == nil || !IsServiceUnavailable(err) {
		t.Errorf("503 must map to ServiceUnavailable, got %v", err)
	}
}

func TestInputRequests_RejectQuestion(t *testing.T) {
	rec := &inputRecorder{}
	c := newInputClient(t, http.StatusOK, "", rec)
	if err := c.InputRequests.RejectQuestion(context.Background(), "ws-1", "que_1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/workspaces/ws-1/question/que_1/reject" {
		t.Errorf("wrong route: %s %s", rec.method, rec.path)
	}
}

func TestInputRequests_DismissInboxRecord(t *testing.T) {
	rec := &inputRecorder{}
	c := newInputClient(t, http.StatusNoContent, "", rec)
	if err := c.InputRequests.DismissInboxRecord(context.Background(), "ws-1", "ses_1", "que_1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.method != http.MethodDelete || rec.path != "/api/v1/workspaces/ws-1/sessions/ses_1/inbox/que_1" {
		t.Errorf("wrong route: %s %s", rec.method, rec.path)
	}
}

func TestInputRequests_RequestInputSnapshot(t *testing.T) {
	rec := &inputRecorder{}
	c := newInputClient(t, http.StatusAccepted, "", rec)
	if err := c.InputRequests.RequestInputSnapshot(context.Background(), "ws-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/workspaces/ws-1/input-snapshot" {
		t.Errorf("wrong route: %s %s", rec.method, rec.path)
	}
}

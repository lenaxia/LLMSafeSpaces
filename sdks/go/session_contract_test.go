// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sessionContractJSON mirrors what the live getSession handler emits
// (proxy_handlers.go GetSession → adapter.GetSession → pkg/session
// Session marshal): camelCase contract keys; id/workspaceId/status are
// the always-present core.
const sessionContractJSON = `{
	"id": "ses_7f9d",
	"workspaceId": "ws-1",
	"parentId": "ses_root",
	"title": "Refactor the adapter",
	"agentId": "plan",
	"model": {"id": "claude-sonnet-4.5", "provider": "anthropic"},
	"status": "busy",
	"cost": {"inputTokens": 120, "outputTokens": 80, "totalTokens": 200},
	"contextUsage": {"used": 45000, "window": 200000},
	"time": {"startedAt": "2026-09-15T10:00:00Z", "completedAt": null},
	"summary": "Wire the seam",
	"archived": false
}`

func TestSessions_Get_TypedContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/sessions/ses_7f9d" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sessionContractJSON))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	s, err := c.Sessions.Get(context.Background(), "ws-1", "ses_7f9d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.ID != "ses_7f9d" || s.WorkspaceID != "ws-1" || s.Title != "Refactor the adapter" {
		t.Errorf("identity fields not decoded: %+v", s)
	}
	if s.ParentID != "ses_root" || s.AgentID != "plan" || s.Summary != "Wire the seam" {
		t.Errorf("metadata fields not decoded: %+v", s)
	}
	if s.Status != "busy" {
		t.Errorf("status not decoded: %q", s.Status)
	}
	if s.Model == nil || s.Model.ID != "claude-sonnet-4.5" || s.Model.Provider != "anthropic" {
		t.Errorf("model not decoded: %+v", s.Model)
	}
	if s.Cost == nil || s.Cost.InputTokens != 120 || s.Cost.TotalTokens != 200 {
		t.Errorf("cost not decoded: %+v", s.Cost)
	}
	if s.ContextUsage == nil || s.ContextUsage.Used != 45000 || s.ContextUsage.Window != 200000 {
		t.Errorf("contextUsage not decoded: %+v", s.ContextUsage)
	}
	if s.Time == nil || s.Time.StartedAt.IsZero() || s.Time.CompletedAt != nil {
		t.Errorf("time not decoded: %+v", s.Time)
	}
}

func TestSessions_Get_RawUpstreamShapeIsNotTheContract(t *testing.T) {
	// The RETIRED raw passthrough shape (opencode's own session object:
	// no workspaceId, no contract status vocabulary) must not decode as
	// the contract — the structural fields stay zero. (parentID, the
	// raw shape's casing, does tolerant-decode into parentId — Go JSON
	// unmarshal is case-insensitive — so the structural core is the
	// discriminator, not the casing.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ses_1","title":"t","parentID":"ses_0","time":{"created":1700000000}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	s, err := c.Sessions.Get(context.Background(), "ws-1", "ses_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.WorkspaceID != "" || s.Status != "" {
		t.Errorf("raw upstream shape decoded into contract fields: %+v", s)
	}
}

func TestSessions_SendPromptAsync_ReturnsAcceptedReceipt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageID":"msg_9","clientMessageID":"cmid-1","status":"queued"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	rcpt, err := c.Sessions.SendPromptAsync(context.Background(), "ws-1", "ses_1", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rcpt == nil || rcpt.MessageID != "msg_9" || rcpt.ClientMessageID != "cmid-1" || rcpt.Status != "queued" {
		t.Errorf("receipt not decoded: %+v", rcpt)
	}
}

func TestSessions_SendPromptAsync_DuplicateReturnsOriginalReceipt(t *testing.T) {
	// A retried clientMessageID answers 200 with the ORIGINAL accepted
	// entry (proxy_handlers.go SendPromptAsync duplicate arm) — the
	// receipt classifies by payload status, never by body presence.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messageID":"msg_orig","clientMessageID":"cmid-1","status":"duplicate"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	rcpt, err := c.Sessions.SendPromptAsync(context.Background(), "ws-1", "ses_1", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rcpt == nil || rcpt.MessageID != "msg_orig" || rcpt.Status != "duplicate" {
		t.Errorf("duplicate receipt not decoded: %+v", rcpt)
	}
}

func TestSessions_AbortAndDelete_AreNoContent(t *testing.T) {
	// The live handlers answer 204 with no body (proxy_handlers.go
	// AbortSession/DeleteSession) — the SDK must surface success, not
	// a decode error, on the empty body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	if err := c.Sessions.Abort(context.Background(), "ws-1", "ses_1"); err != nil {
		t.Errorf("abort: unexpected error: %v", err)
	}
	if err := c.Sessions.Delete(context.Background(), "ws-1", "ses_1"); err != nil {
		t.Errorf("delete: unexpected error: %v", err)
	}
}

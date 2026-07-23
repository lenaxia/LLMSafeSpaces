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

func TestClient_ListWorkspaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer lsp_test" {
			t.Errorf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(WorkspaceListResult{
			Items: []WorkspaceListItem{{ID: "ws-1", Name: "test"}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	result, err := c.Workspaces.List(context.Background(), 20, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "ws-1" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestClient_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "workspace not found"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Workspaces.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsNotFound(err) {
		t.Errorf("expected NotFound, got: %v", err)
	}
}

func TestClient_AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_bad"))
	_, err := c.Auth.Me(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsAuth(err) {
		t.Errorf("expected Auth error, got: %v", err)
	}
}

func TestClient_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Auth.Me(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsRateLimit(err) {
		t.Errorf("expected RateLimit error, got: %v", err)
	}
	if IsAuth(err) {
		t.Errorf("IsAuth should be false for 429")
	}
	if IsRateLimit(nil) {
		t.Errorf("IsRateLimit(nil) should be false")
	}
}

func TestClient_SendMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg-1",
			"role": "assistant",
			"parts": []map[string]string{
				{"type": "text", "text": "Hello "},
				{"type": "text", "text": "world!"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	resp, err := c.Sessions.SendMessage(context.Background(), "ws-1", "sess-1", "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello world!" {
		t.Errorf("expected 'Hello world!', got: %q", resp.Content)
	}
}

func TestClient_Suspend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Workspaces.Suspend(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestClient_Suspend_204EmptyBody guards the do() 204/empty-body path shared
// with RefreshCompute: a response with no body must return nil without
// attempting to decode an empty stream.
func TestClient_Suspend_204EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	if err := c.Workspaces.Suspend(context.Background(), "ws-1"); err != nil {
		t.Fatalf("204 with empty body must not error, got: %v", err)
	}
}

// TestClient_RefreshCompute_202Body verifies the 202-with-body path: the
// response must be decoded into RefreshWorkspaceResult rather than discarded.
func TestClient_RefreshCompute_202Body(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(RefreshWorkspaceResult{RestartGeneration: 7})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	res, err := c.Workspaces.RefreshCompute(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RestartGeneration != 7 {
		t.Fatalf("expected RestartGeneration 7, got %d (202 body was discarded)", res.RestartGeneration)
	}
}

// TestClient_RefreshCompute_APIError verifies a non-2xx surfaces as an error.
func TestClient_RefreshCompute_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Workspaces.RefreshCompute(context.Background(), "ws-1")
	if err == nil {
		t.Fatal("expected error for 409, got nil")
	}
}

func TestClient_TerminalTicket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(TerminalTicket{Ticket: "tkt_abc", ExpiresAt: "2026-05-29T18:00:00Z"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	ticket, err := c.Terminal.GetTicket(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ticket.Ticket != "tkt_abc" {
		t.Errorf("expected tkt_abc, got: %s", ticket.Ticket)
	}
}

func TestClient_AutoLogin(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/api/v1/auth/login" {
			json.NewEncoder(w).Encode(map[string]any{"token": "jwt-abc"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer jwt-abc" {
			t.Errorf("expected jwt-abc token, got: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]string{"id": "u1"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithCredentials("test@example.com", "pass"))
	_, err := c.Auth.Me(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls (login + me), got %d", callCount)
	}
}

func TestClient_DeleteSession(t *testing.T) {
	var capturedMethod, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Sessions.Delete(context.Background(), "ws-1", "sess-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedMethod != "DELETE" {
		t.Errorf("expected DELETE, got: %s", capturedMethod)
	}
	if capturedPath != "/api/v1/workspaces/ws-1/sessions/sess-1" {
		t.Errorf("unexpected path: %s", capturedPath)
	}
}

func TestClient_DeleteSession_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Sessions.Delete(context.Background(), "ws-1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !IsNotFound(err) {
		t.Errorf("expected NotFound, got: %v", err)
	}
}

// TestProviderCredentialsService_Create_WireFormat pins the Epic 55 JSON
// shape that the SDK's Create method sends to the API. A revert that
// puts `provider` back, or a typo in the kind/slug JSON tags, would
// be caught by this test.
//
// This is a wire-format unit test — distinct from the canary scenario
// (which is a live-cluster smoke test). The canary asserts behavior on
// real DBs; this test asserts that the SDK speaks the right protocol.
func TestProviderCredentialsService_Create_WireFormat(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/provider-credentials" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		// Return a credential response that exercises the read path too.
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":                 "cred-1",
			"name":               "Test Cred",
			"kind":               "openai_compatible",
			"slug":               "test-cred",
			"baseURL":            "https://api.example.com/v1",
			"modelAllowlist":     []string{},
			"modelContextLimits": map[string]int{},
			"modelOutputLimits":  map[string]int{},
			"createdAt":          "2026-06-27T00:00:00Z",
			"updatedAt":          "2026-06-27T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	resp, err := c.ProviderCredentials.Create(
		context.Background(),
		"Test Cred",
		"openai_compatible",
		"test-cred",
		"sk-abc",
		"https://api.example.com/v1",
	)
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}

	// Wire-format invariants: the body MUST send the Epic 55 fields and
	// MUST NOT send the legacy `provider` field.
	if captured["name"] != "Test Cred" {
		t.Errorf("name: got %v, want %q", captured["name"], "Test Cred")
	}
	if captured["kind"] != "openai_compatible" {
		t.Errorf("kind: got %v, want %q", captured["kind"], "openai_compatible")
	}
	if captured["slug"] != "test-cred" {
		t.Errorf("slug: got %v, want %q", captured["slug"], "test-cred")
	}
	if captured["apiKey"] != "sk-abc" {
		t.Errorf("apiKey: got %v, want %q", captured["apiKey"], "sk-abc")
	}
	if captured["baseURL"] != "https://api.example.com/v1" {
		t.Errorf("baseURL: got %v, want %q", captured["baseURL"], "https://api.example.com/v1")
	}
	if _, present := captured["provider"]; present {
		t.Errorf("legacy `provider` field must NOT be in the request body (got: %v)", captured["provider"])
	}

	// Read path: the decoded response struct exposes Kind+Slug.
	if resp.Kind != "openai_compatible" {
		t.Errorf("resp.Kind: got %q, want openai_compatible", resp.Kind)
	}
	if resp.Slug != "test-cred" {
		t.Errorf("resp.Slug: got %q, want test-cred", resp.Slug)
	}
	if resp.Name != "Test Cred" {
		t.Errorf("resp.Name: got %q, want Test Cred", resp.Name)
	}
}

// TestProviderCredentialsService_Create_OmitsEmptyBaseURL verifies the
// SDK omits the baseURL field entirely (not "" — actually missing from
// the JSON) when the caller passes an empty string. The server uses
// JSON-key-presence rather than empty-string to distinguish "not set"
// from "explicitly cleared", so this is a real wire-format invariant.
func TestProviderCredentialsService_Create_OmitsEmptyBaseURL(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":   "cred-1",
			"name": "X",
			"kind": "openai",
			"slug": "x",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.ProviderCredentials.Create(
		context.Background(),
		"X",
		"openai",
		"x",
		"sk-abc",
		"", // empty baseURL — must be omitted from request body
	)
	if err != nil {
		t.Fatalf("Create error: %v", err)
	}
	if _, present := captured["baseURL"]; present {
		t.Errorf("baseURL must be omitted from request body when empty; got: %v", captured["baseURL"])
	}
}

// TestAdminProviderCredentialsService_List_WireFormat exercises the
// response decode path used by the canary scenario. Free-tier seed has
// kind="opencode" and slug="opencode-free-tier"; the canary depends on
// being able to find that exact pair in the list response.
func TestAdminProviderCredentialsService_List_WireFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/provider-credentials" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":                 "cred-free",
				"name":               "opencode-free-tier",
				"kind":               "opencode",
				"slug":               "opencode-free-tier",
				"modelAllowlist":     []string{},
				"modelContextLimits": map[string]int{},
				"modelOutputLimits":  map[string]int{},
				"createdAt":          "2026-06-27T00:00:00Z",
				"updatedAt":          "2026-06-27T00:00:00Z",
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	list, err := c.AdminProviderCredentials.List(context.Background())
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len: got %d, want 1", len(list))
	}
	if list[0].Kind != "opencode" {
		t.Errorf("Kind: got %q, want opencode", list[0].Kind)
	}
	if list[0].Slug != "opencode-free-tier" {
		t.Errorf("Slug: got %q, want opencode-free-tier", list[0].Slug)
	}
}

func TestClient_SessionsEnqueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/sessions/sess-1/queue" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"messageID": "qmsg-1"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	msgID, err := c.Sessions.Enqueue(context.Background(), "ws-1", "sess-1", "hello")
	if err != nil {
		t.Fatalf("Enqueue error: %v", err)
	}
	if msgID != "qmsg-1" {
		t.Errorf("got %q, want qmsg-1", msgID)
	}
}

func TestClient_SessionsListQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"id": "qmsg-1", "text": "hello", "session_id": "sess-1", "workspace_id": "ws-1", "enqueued_at": "2026-07-22T00:00:00Z", "retry_count": 0},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	msgs, err := c.Sessions.ListQueue(context.Background(), "ws-1", "sess-1")
	if err != nil {
		t.Fatalf("ListQueue error: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "qmsg-1" {
		t.Errorf("unexpected: %+v", msgs)
	}
}

func TestClient_SessionsMarkSeen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/sessions/sess-1/seen" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	if err := c.Sessions.MarkSeen(context.Background(), "ws-1", "sess-1"); err != nil {
		t.Fatalf("MarkSeen error: %v", err)
	}
}

func TestClient_SessionsDismissQueued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/sessions/sess-1/queue/qmsg-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "DELETE" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	if err := c.Sessions.DismissQueued(context.Background(), "ws-1", "sess-1", "qmsg-1"); err != nil {
		t.Fatalf("DismissQueued error: %v", err)
	}
}

func TestClient_UsageGetWorkspace_Path(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage/workspaces/ws-1" {
			t.Errorf("unexpected path: %s (expected /api/v1/usage/workspaces/ws-1)", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"usage": 100})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Usage.GetWorkspace(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("GetWorkspace error: %v", err)
	}
}

func TestClient_UsageGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"events": []any{}})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Usage.Get(context.Background())
	if err != nil {
		t.Fatalf("Usage.Get error: %v", err)
	}
}

func TestClient_UsageGetQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage/quota" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"quotas": []any{}})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Usage.GetQuota(context.Background())
	if err != nil {
		t.Fatalf("Usage.GetQuota error: %v", err)
	}
}

func TestClient_InputRequests_ListQuestions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/question" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.InputRequests.ListQuestions(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("ListQuestions error: %v", err)
	}
}

func TestClient_InputRequests_ReplyQuestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/question/que_123/reply" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.InputRequests.ReplyQuestion(context.Background(), "ws-1", "que_123", map[string]any{"answer": "yes"})
	if err != nil {
		t.Fatalf("ReplyQuestion error: %v", err)
	}
}

func TestClient_InputRequests_RejectQuestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/question/que_123/reject" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.InputRequests.RejectQuestion(context.Background(), "ws-1", "que_123")
	if err != nil {
		t.Fatalf("RejectQuestion error: %v", err)
	}
}

func TestClient_InputRequests_ListPermissions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/permission" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.InputRequests.ListPermissions(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("ListPermissions error: %v", err)
	}
}

func TestClient_InputRequests_ReplyPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/permission/per_123/reply" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.InputRequests.ReplyPermission(context.Background(), "ws-1", "per_123", map[string]any{"decision": "allow"})
	if err != nil {
		t.Fatalf("ReplyPermission error: %v", err)
	}
}

func TestClient_AuthRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/register" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"token": "jwt"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Auth.Register(context.Background(), "user1", "u@x.com", "password123")
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
}

func TestClient_AuthLogout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/logout" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Auth.Logout(context.Background())
	if err != nil {
		t.Fatalf("Logout error: %v", err)
	}
}

func TestClient_AuthLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/lookup" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{"redirectUrl": "https://org.example.com"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	url, err := c.Auth.Lookup(context.Background(), "u@x.com")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if url != "https://org.example.com" {
		t.Errorf("got %q, want https://org.example.com", url)
	}
}

func TestClient_AuthUnlockDek(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/unlock-dek" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Auth.UnlockDek(context.Background(), "password123")
	if err != nil {
		t.Fatalf("UnlockDek error: %v", err)
	}
}

func TestClient_WorkspacesReloadAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/ws-1/agent/reload" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Workspaces.ReloadAgent(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("ReloadAgent error: %v", err)
	}
}

func TestClient_ProbeModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/probe-models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Probe.ProbeModels(context.Background(), "sk-test", "https://api.openai.com/v1")
	if err != nil {
		t.Fatalf("ProbeModels error: %v", err)
	}
}

func TestClient_AuthRequestPasswordReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/password-reset/request" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Auth.RequestPasswordReset(context.Background(), "u@x.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset error: %v", err)
	}
}

func TestClient_AuthConfirmPasswordReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/password-reset/confirm" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Auth.ConfirmPasswordReset(context.Background(), "token", "newpass123")
	if err != nil {
		t.Fatalf("ConfirmPasswordReset error: %v", err)
	}
}

func TestClient_AuthVerifyEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/verify-email" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Auth.VerifyEmail(context.Background(), "token")
	if err != nil {
		t.Fatalf("VerifyEmail error: %v", err)
	}
}

func TestClient_AuthResendVerification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/verify-email/resend" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Auth.ResendVerification(context.Background(), "u@x.com")
	if err != nil {
		t.Fatalf("ResendVerification error: %v", err)
	}
}

func TestClient_ProviderCredentials_Create_207PartialSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		json.NewEncoder(w).Encode(map[string]any{
			"credential": map[string]any{
				"id":        "cred-1",
				"name":      "my-key",
				"kind":      "openai",
				"slug":      "my-key",
				"createdAt": "2026-07-22T00:00:00Z",
				"updatedAt": "2026-07-22T00:00:00Z",
			},
			"bindWarning": "failed to auto-bind",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	cred, err := c.ProviderCredentials.Create(context.Background(), "my-key", "openai", "my-key", "sk-test", "")
	if err != nil {
		t.Fatalf("Create (207) error: %v", err)
	}
	if cred.ID != "cred-1" {
		t.Errorf("got ID %q, want cred-1", cred.ID)
	}
}

func TestClient_SessionsEnqueue_EmptyText_400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "text must not be empty"})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	_, err := c.Sessions.Enqueue(context.Background(), "ws-1", "sess-1", "")
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

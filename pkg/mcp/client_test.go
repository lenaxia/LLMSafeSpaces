// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func newTestHTTPClient(handler http.Handler) (*HTTPClient, *httptest.Server) {
	ts := httptest.NewServer(handler)
	client := &HTTPClient{
		BaseURL:    ts.URL,
		HTTPClient: ts.Client(),
		APIKey:     "test-key",
	}
	return client, ts
}

// contractMsg builds a contract-shaped history Message (design 0049:
// id/type/parts) for test fixtures.
func contractMsg(id, typ, text string) Message {
	return Message{ID: id, Type: typ, Parts: []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: text}}}
}

// ===== CreateWorkspace =====

func TestHTTPClient_CreateWorkspace_HappyPath(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/workspaces", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var req CreateWorkspaceReq
		json.NewDecoder(r.Body).Decode(&req)
		assert.Equal(t, "python:3.10", req.Runtime)

		json.NewEncoder(w).Encode(WorkspaceResp{ID: "ws-1", Runtime: "python:3.10", Phase: "Active"})
	}))
	defer ts.Close()

	resp, err := client.CreateWorkspace(context.Background(), CreateWorkspaceReq{Runtime: "python:3.10"})
	require.NoError(t, err)
	assert.Equal(t, "ws-1", resp.ID)
}

func TestHTTPClient_CreateWorkspace_APIError(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"workspace limit reached"}`))
	}))
	defer ts.Close()

	_, err := client.CreateWorkspace(context.Background(), CreateWorkspaceReq{Runtime: "python:3.10"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "409")
	assert.Contains(t, err.Error(), "workspace limit reached")
}

// ===== CreateCredential (Epic 55 wire format) =====

func TestHTTPClient_CreateCredential_PostsKindSlugShape(t *testing.T) {
	var capturedBody []byte
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/secrets", r.URL.Path)
		capturedBody, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(CredentialResp{ID: "cred-1", Name: "my-anthropic", Type: "llm-provider"})
	}))
	defer ts.Close()

	resp, err := client.CreateCredential(context.Background(), CreateCredentialReq{
		Kind:    "anthropic",
		Slug:    "my-anthropic",
		APIKey:  "sk-test",
		BaseURL: "https://api.anthropic.test",
		Default: "anthropic/claude-sonnet-4-5",
	})
	require.NoError(t, err)
	assert.Equal(t, "cred-1", resp.ID)

	var posted createSecretRequest
	require.NoError(t, json.Unmarshal(capturedBody, &posted))
	assert.Equal(t, "llm-provider", posted.Type)
	// Name defaults to the slug when not supplied.
	assert.Equal(t, "my-anthropic", posted.Name)

	var val llmProviderValue
	require.NoError(t, json.Unmarshal([]byte(posted.Value), &val))
	assert.Equal(t, "anthropic", val.Kind)
	assert.Equal(t, "my-anthropic", val.Slug)
	assert.Equal(t, "sk-test", val.APIKey)
	assert.Equal(t, "https://api.anthropic.test", val.BaseURL)
	assert.Equal(t, "anthropic/claude-sonnet-4-5", val.Default)
	// The legacy `provider` field must not leak into the Epic 55 value shape.
	assert.NotContains(t, posted.Value, `"provider"`)
}

func TestHTTPClient_CreateCredential_AutoBindsWorkspace(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CredentialResp{ID: "cred-1", Type: "llm-provider"})
	})
	bindCalled := false
	mux.HandleFunc("/api/v1/workspaces/ws-1/bindings", func(w http.ResponseWriter, r *http.Request) {
		bindCalled = true
		assert.Equal(t, "PUT", r.Method)
		w.WriteHeader(http.StatusNoContent)
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	_, err := client.CreateCredential(context.Background(), CreateCredentialReq{
		Kind: "openai", Slug: "openai", APIKey: "sk-test", WorkspaceID: "ws-1",
	})
	require.NoError(t, err)
	assert.True(t, bindCalled, "auto-bind should fire when WorkspaceID is set")
}

// TestLLMProviderValue_DeserializesIntoLLMProviderData is the root-cause
// regression test for issue #435. The bug was a JSON-tag contract mismatch:
// the MCP emitted a value shape that secrets.LLMProviderData could not
// deserialize into, so LLMProviderData.Validate() rejected it ("kind is
// required") and the API returned 400. This test pins the contract directly —
// marshal the MCP's value type and confirm the server's type accepts it and
// validates. A future JSON-tag rename on either side that breaks the wire
// format fails here, not in production. It is the tag-drift analog of
// TestValidCredentialKinds_MatchesSecretsValidKinds (which pins enum drift).
func TestLLMProviderValue_DeserializesIntoLLMProviderData(t *testing.T) {
	value := llmProviderValue{
		Kind:    "anthropic",
		Slug:    "my-anthropic",
		APIKey:  "sk-test",
		BaseURL: "https://api.anthropic.test",
		Default: "anthropic/claude-sonnet-4-5",
	}
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	var serverData secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(raw, &serverData),
		"llmProviderValue must deserialize into secrets.LLMProviderData")

	assert.Equal(t, value.Kind, serverData.Kind)
	assert.Equal(t, value.Slug, serverData.Slug)
	assert.Equal(t, value.APIKey, serverData.APIKey)
	assert.Equal(t, value.BaseURL, serverData.BaseURL)
	assert.Equal(t, value.Default, serverData.Default)

	require.NoError(t, serverData.Validate(),
		"the server-side llm-provider validator must accept this value shape")

	// Negative pin: a value missing kind reproduces the exact #435 failure —
	// the server rejects it with "kind is required". Confirms the test actually
	// exercises the validation gate, not just deserialization.
	var rejected secrets.LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(`{"slug":"x","apiKey":"sk"}`), &rejected))
	err = rejected.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind is required")
}

// ===== ActivateWorkspace =====

func TestHTTPClient_ActivateWorkspace_HappyPath(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/workspaces/ws-1/activate", r.URL.Path)
		json.NewEncoder(w).Encode(ActivateResp{Resumed: "ws-1"})
	}))
	defer ts.Close()

	resp, err := client.ActivateWorkspace(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "ws-1", resp.Resumed)
}

// ===== SuspendWorkspace =====

func TestHTTPClient_SuspendWorkspace_HappyPath(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/workspaces/ws-1/suspend", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	err := client.SuspendWorkspace(context.Background(), "ws-1")
	assert.NoError(t, err)
}

// ===== RefreshWorkspace =====

func TestHTTPClient_RefreshWorkspace_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/refresh-compute", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		json.NewEncoder(w).Encode(RefreshWorkspaceResp{RestartGeneration: 9})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.RefreshWorkspace(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, int64(9), resp.RestartGeneration)
}

func TestHTTPClient_RefreshWorkspace_APIError(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer ts.Close()

	_, err := client.RefreshWorkspace(context.Background(), "ws-1")
	assert.Error(t, err)
}

// ===== CreateSession =====

func TestHTTPClient_CreateSession_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/new", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		json.NewEncoder(w).Encode(SessionResp{ID: "sess-1"})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.CreateSession(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "sess-1", resp.ID)
}

// ===== GetHistory =====

func TestHTTPClient_GetHistory_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/message", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		json.NewEncoder(w).Encode([]Message{
			contractMsg("msg_1", "user", "hi"), contractMsg("msg_2", "assistant", "hello"),
		})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	msgs, err := client.GetHistory(context.Background(), "ws-1", "sess-1")
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
	assert.Equal(t, "hello", msgs[1].TextContent())
}

// ===== SendMessage =====

func TestHTTPClient_SendMessage_SSEResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// The real broker shape (#1053): contract events ride
		// session.event envelopes, and streamed text arrives as
		// part.delta payloads inside data.
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-1\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"Hello \"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-1\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"world!\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "Hello world!", resp)
}

// TestHTTPClient_SendMessage_SSEOtherSessionDelta_Ignored: deltas from
// another session on the same workspace stream must not bleed into this
// call's response.
func TestHTTPClient_SendMessage_SSEOtherSessionDelta_Ignored(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-2\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-2\",\"delta\":\"other session\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-1\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"mine\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "mine", resp)
}

func TestHTTPClient_SendMessage_FallbackToHistory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		// SSE stream closes immediately without session.idle
		w.Header().Set("Content-Type", "text/event-stream")
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/message", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Message{
			contractMsg("msg_1", "user", "hi"), contractMsg("msg_2", "assistant", "fallback response"),
		})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "fallback response", resp)
}

func TestHTTPClient_SendMessage_PromptReturns429(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"active session limit reached"}`))
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	_, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "429")
}

func TestHTTPClient_SendMessage_Timeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		// Block until context canceled (simulates timeout)
		w.Header().Set("Content-Type", "text/event-stream")
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/message", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Message{contractMsg("msg_1", "assistant", "timeout fallback")})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "timeout fallback", resp)
}

// ===== Context cancellation =====

func TestHTTPClient_ContextCancelled(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // will be canceled
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := client.CreateWorkspace(ctx, CreateWorkspaceReq{Runtime: "python:3.10"})
	assert.Error(t, err)
}

// ===== Malformed responses =====

func TestHTTPClient_MalformedJSONResponse(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`<html>502 Bad Gateway</html>`))
	}))
	defer ts.Close()

	_, err := client.CreateWorkspace(context.Background(), CreateWorkspaceReq{Runtime: "python:3.10"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode response")
}

// ===== SSE with keepalive comments and retry directives =====

func TestHTTPClient_SendMessage_SSEWithKeepalives(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Real SSE streams have comments, retry directives, and event types
		fmt.Fprintf(w, ":keepalive\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "retry: 3000\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-1\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"answer\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: not-json-at-all\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "answer", resp)
}

// ===== Input validation (path traversal) =====

func TestHTTPClient_InvalidSessionID(t *testing.T) {
	client := &HTTPClient{BaseURL: "http://localhost", HTTPClient: http.DefaultClient}

	_, err := client.GetHistory(context.Background(), "ws-1", "../../../etc/passwd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid characters")
}

// ===== Message size limit =====

func TestHTTPClient_MessageTooLarge(t *testing.T) {
	client := &HTTPClient{BaseURL: "http://localhost", HTTPClient: http.DefaultClient}

	bigMessage := strings.Repeat("x", maxMessageSize+1)
	_, err := client.SendMessage(context.Background(), "ws-1", "sess-1", bigMessage, 5*time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "message too large")
}

// ===== Response body size limit =====

func TestHTTPClient_HugeResponseTruncated(t *testing.T) {
	// Server returns a response larger than maxResponseBody
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write valid JSON start, then pad with spaces (won't parse as valid WorkspaceResp)
		w.Write([]byte(`{"id":"ws-1","runtime":"python:3.10","phase":"Active","name":"`))
		// Write enough to exceed limit
		for i := 0; i < maxResponseBody/1024; i++ {
			w.Write(bytes.Repeat([]byte("x"), 1024))
		}
		w.Write([]byte(`"}`))
	}))
	defer ts.Close()

	_, err := client.CreateWorkspace(context.Background(), CreateWorkspaceReq{Runtime: "python:3.10"})
	// Should fail with decode error (truncated JSON) rather than OOM
	assert.Error(t, err)
}

// ===== Error message sanitization =====

func TestHTTPClient_LongErrorTruncated(t *testing.T) {
	client, ts := newTestHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Simulate a stack trace leak
		w.Write([]byte(strings.Repeat("internal error details with paths /var/lib/secrets ", 100)))
	}))
	defer ts.Close()

	_, err := client.CreateWorkspace(context.Background(), CreateWorkspaceReq{Runtime: "python:3.10"})
	assert.Error(t, err)
	// Error should be truncated, not contain the full 5000+ char body
	assert.Less(t, len(err.Error()), 600)
	assert.Contains(t, err.Error(), "truncated")
}

// ===== US-16.0: SendMessage ignores idle for other sessions =====

func TestHTTPClient_SendMessage_IgnoresIdleForOtherSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Idle for a DIFFERENT session — should be ignored
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-OTHER\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
		// Content for our session
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-1\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"result\"}}\n\n")
		flusher.Flush()
		// Idle for OUR session — should break
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "result", resp)
}

func TestHTTPClient_SendMessage_IgnoresBusyEvents(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Busy event — should be ignored
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"busy\"}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.event\",\"session_id\":\"sess-1\",\"data\":{\"type\":\"part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"done\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "done", resp)
}

// ===== US-16.0: validID accepts underscores (opencode IDs) =====

func TestValidateID_AcceptsUnderscoreIDs(t *testing.T) {
	tests := []struct {
		id      string
		wantErr bool
	}{
		{"ses_18b28260affeoxXrX1iwPH8wFg", false},
		{"que_e74d7e6db001ZI3VDSHthsee0g", false},
		{"per_1748012345000_xyz", false},
		{"msg_e74d7da37001Nw4A59Ndzegm3A", false},
		{"sess-1", false},       // existing hyphen format still works
		{"ws.test.123", false},  // dots still work
		{"../etc/passwd", true}, // path traversal rejected
		{"", true},              // empty rejected
		{".leading-dot", true},  // must start with alphanumeric
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			err := validateID(tt.id, "test_field")
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHTTPClient_GetHistory_AcceptsOpenCodeSessionID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/ses_18b28260affeoxXrX1iwPH8wFg/message", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Message{contractMsg("msg_1", "assistant", "ok")})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	msgs, err := client.GetHistory(context.Background(), "ws-1", "ses_18b28260affeoxXrX1iwPH8wFg")
	require.NoError(t, err)
	assert.Len(t, msgs, 1)
}

// ===== US-16.7: SendMessage Question Detection =====

func TestHTTPClient_SendMessage_QuestionDetected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Agent asks a question
		questionData := `{"id":"que_abc","session_id":"sess-1","questions":[{"question":"What language?","header":"Choose","options":[{"label":"Go","description":"Fast"}]}]}`
		fmt.Fprintf(w, "data: {\"type\":\"agent.question\",\"data\":%s}\n\n", questionData)
		flusher.Flush()
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "create project", 5*time.Second)
	require.NoError(t, err)

	// Should return structured question result
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(resp), &result))
	assert.Equal(t, "question", result["type"])
	// request should contain the question data
	reqData, _ := json.Marshal(result["request"])
	assert.Contains(t, string(reqData), "que_abc")
	assert.Contains(t, string(reqData), "What language?")
}

func TestHTTPClient_SendMessage_QuestionForDifferentSession_Ignored(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Question for a DIFFERENT session — should be ignored
		fmt.Fprintf(w, "data: {\"type\":\"agent.question\",\"data\":{\"id\":\"que_abc\",\"session_id\":\"sess-OTHER\",\"questions\":[]}}\n\n")
		flusher.Flush()
		// Then idle for our session
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/message", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Message{contractMsg("msg_1", "assistant", "done")})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "hi", 5*time.Second)
	require.NoError(t, err)
	// Should NOT return question data — should get normal response
	assert.Equal(t, "done", resp)
}

func TestHTTPClient_SendMessage_PermissionAutoApproved(t *testing.T) {
	var permissionReplied atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/session-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Permission event
		fmt.Fprintf(w, "data: {\"type\":\"agent.permission\",\"data\":{\"id\":\"per_xyz\",\"session_id\":\"sess-1\",\"permission\":\"shell\",\"patterns\":[\"ls\"]}}\n\n")
		flusher.Flush()
		// Then idle
		fmt.Fprintf(w, "data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n")
		flusher.Flush()
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/permission/per_xyz/reply", func(w http.ResponseWriter, r *http.Request) {
		permissionReplied.Store(true)
		w.Write([]byte(`true`))
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/sess-1/message", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Message{contractMsg("msg_1", "assistant", "done after permission")})
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "sess-1", "run ls", 5*time.Second)
	require.NoError(t, err)
	// Should continue to idle and return normal response
	assert.Equal(t, "done after permission", resp)
	// Give the goroutine time to fire
	time.Sleep(50 * time.Millisecond)
	assert.True(t, permissionReplied.Load(), "permission should have been auto-approved")
}

// #880/#905: the API wraps the secrets list ({"secrets": [...]}); the
// client previously decoded a bare array, failing EVERY credential_list
// MCP call. Red on the pre-fix client (decode error), green after.
func TestHTTPClient_ListCredentials_WrapperDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/secrets", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secrets":[
			{"id":"cred-1","type":"llm-provider","name":"a"},
			{"id":"cred-2","type":"mcp","name":"b"}
		]}`))
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: "key"}

	creds, err := c.ListCredentials(context.Background())
	require.NoError(t, err, "wrapper decode must succeed (pre-fix: json cannot unmarshal object into []CredentialResp)")
	require.Len(t, creds, 1, "non-llm-provider entries filtered")
	assert.Equal(t, "cred-1", creds[0].ID)
}

func TestHTTPClient_ListCredentials_EmptyWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"secrets":[]}`))
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: "key"}

	creds, err := c.ListCredentials(context.Background())
	require.NoError(t, err)
	assert.Empty(t, creds)
}

func TestHTTPClient_ListCredentials_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: "key"}

	_, err := c.ListCredentials(context.Background())
	require.Error(t, err)
}

//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// The #1465 freeze pin: the origin-injection plugin
// (runtimes/opencode/plugins/llmsafespaces-origin.js) against the REAL
// pinned opencode binary. A stub agentd-MCP server stands in for the
// platform tool surface; a mock provider makes the model emit exactly
// one llmsafespaces_send_message tool call; the harness then executes
// it. This test empirically settles the two assumptions the decompiled
// evidence could not fully close:
//
//  1. tool.execute.before hooks can MUTATE the outgoing arguments (the
//     output object is passed by reference and IS the args source) —
//     the positive leg fails if any layer strips unknown keys.
//  2. The injected from_session_id arrives on the MCP wire carrying
//     the CALLING session's ID — and does NOT arrive without the
//     plugin (negative leg: the injection is the plugin's doing, not
//     harness behavior).
//
// Run: OPENCODE_BINARY=/opencode/usr/local/bin/opencode go test -tags=integration \
//      -run TestOriginPlugin ./pkg/agent/opencode/ -timeout 300s
// CI runs it via the origin-plugin-pin job (downloads the pinned binary).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubAgentdMCP is the minimal agentd /v1/mcp stand-in: JSON-RPC over
// plain JSON (the same response shape agentd serves), advertising ONE
// tool (send_message) and recording every tools/call's arguments.
type stubAgentdMCP struct {
	mu    sync.Mutex
	calls []map[string]any // recorded tools/call params (name + arguments)
}

func (s *stubAgentdMCP) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// NEVER fail the test from inside the handler: a Goexit
		// mid-request writes no response and the harness MCP client
		// retries forever (wedged boot). Every input gets SOME
		// response, like agentd itself.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeResult := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		// JSON-RPC notifications are id-less requests (initialized,
		// cancellation, progress): acknowledge with a bare 202 and move
		// on — the StreamableHTTP transport accepts that.
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			writeResult(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "llmsafespaces-workspace", "version": "test"},
			})
		case "tools/list":
			writeResult(map[string]any{"tools": []map[string]any{{
				"name":        "send_message",
				"description": "test stub",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			}}})
		case "tools/call":
			var params map[string]any
			_ = json.Unmarshal(req.Params, &params)
			s.mu.Lock()
			s.calls = append(s.calls, params)
			s.mu.Unlock()
			writeResult(map[string]any{
				"content": []map[string]any{{"type": "text", "text": `{"status":"delivering"}`}},
			})
		default:
			writeResult(nil)
		}
	}
}

func (s *stubAgentdMCP) sendCalls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, c := range s.calls {
		if c["name"] == "send_message" {
			out = append(out, c)
		}
	}
	return out
}

// toolCallMockProvider answers the FIRST completion with one streamed
// llmsafespaces_send_message tool call, every later one with plain
// text — enough agentic loop for the harness to execute the tool once
// and finish the turn.
type toolCallMockProvider struct {
	mu       sync.Mutex
	requests int
	targetID string
}

func (m *toolCallMockProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.mu.Lock()
		m.requests++
		first := m.requests == 1
		target := m.targetID
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		emit := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if first {
			args, _ := json.Marshal(map[string]string{"session_id": target, "message": "stub hello"})
			firstChunk, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
						"index": 0, "id": "call_stub_1", "type": "function",
						"function": map[string]any{"name": "llmsafespaces_send_message", "arguments": string(args)},
					}}},
				}},
			})
			emit(string(firstChunk))
			finishChunk, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "delta": map[string]any{}}},
				"usage":   map[string]int{"prompt_tokens": 84, "completion_tokens": 9, "total_tokens": 93},
			})
			emit(string(finishChunk))
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		textChunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": nil,
				"delta": map[string]any{"role": "assistant", "content": "MOCK-REPLY"},
			}},
		})
		emit(string(textChunk))
		doneChunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "delta": map[string]any{}}},
			"usage":   map[string]int{"prompt_tokens": 84, "completion_tokens": 9, "total_tokens": 93},
		})
		emit(string(doneChunk))
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// originPluginConfig builds the boot config: mock provider + the
// llmsafespaces MCP entry (mirroring injectAgentdMCPServer's shape)
// +, when pluginPath is non-empty, the plugin entry under test.
func originPluginConfig(mockBaseURL, mcpURL string, pluginPath string) string {
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			"mock": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"options": map[string]any{"apiKey": "test", "baseURL": mockBaseURL},
				"models": map[string]any{"mockmodel": map[string]any{
					"name": "Mock Model", "attachment": true,
					"limit": map[string]int{"context": 100000, "output": 4096},
				}},
			},
		},
		"model": "mock/mockmodel",
		"mcp": map[string]any{
			"llmsafespaces": map[string]any{
				"type":    "remote",
				"url":     mcpURL,
				"enabled": true,
				"headers": map[string]any{"Authorization": "Basic dGVzdDp0ZXN0"}, // test:test
			},
		},
	}
	if pluginPath != "" {
		cfg["plugin"] = []string{"file://" + pluginPath}
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(out)
}

// bootOriginHarness boots the pinned binary against the mock provider
// and stub MCP server, sends one prompt through the seam, and returns
// the stub's recorded send_message calls plus the created session ID.
func bootOriginHarness(t *testing.T, ocPort, mockPort int, withPlugin bool) (string, []map[string]any) {
	t.Helper()

	pluginPath := ""
	if withPlugin {
		p, err := filepath.Abs(filepath.Join("..", "..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js"))
		require.NoError(t, err)
		_, statErr := os.Stat(p)
		require.NoError(t, statErr, "plugin source must exist at %s", p)
		pluginPath = p
	}

	ocPort, mockPort = claimTwoPorts(t, ocPort, mockPort)

	mcpStub := &stubAgentdMCP{}
	mcpListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mockPort))
	require.NoError(t, err)
	mcpSrv := &httptest.Server{Listener: mcpListener, Config: &http.Server{Handler: mcpStub.handler(t)}}
	mcpSrv.Start()
	t.Cleanup(mcpSrv.Close)

	provider := &toolCallMockProvider{}
	provListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mockPort+100))
	require.NoError(t, err)
	provSrv := &httptest.Server{Listener: provListener, Config: &http.Server{Handler: provider.handler(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := startOpencodeServerWithConfig(t, ocPort,
		originPluginConfig(provSrv.URL+"/v1", mcpSrv.URL, pluginPath))
	client := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sessionID, err := client.SessionCreate(ctx, "origin-pin")
	require.NoError(t, err)
	provider.mu.Lock()
	provider.targetID = sessionID
	provider.mu.Unlock()

	_, err = client.SessionSend(ctx, sessionID, "call the send_message tool now", "", nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(mcpStub.sendCalls()) == 1 },
		30*time.Second, 250*time.Millisecond, "the harness must execute the stub send_message tool exactly once")
	return sessionID, mcpStub.sendCalls()
}

// TestOriginPlugin_InjectsCallingSessionID — the freeze pin. The
// plugin's from_session_id must arrive on the MCP tools/call carrying
// the session that RAN the tool (the caller), proving: hook mutation
// works end-to-end (residual #4: no layer strips injected args), and
// the value is the harness's own session identity.
func TestOriginPlugin_InjectsCallingSessionID(t *testing.T) {
	sessionID, calls := bootOriginHarness(t, 14210, 14250, true)
	require.Len(t, calls, 1)
	args, ok := calls[0]["arguments"].(map[string]any)
	require.True(t, ok, "arguments must be an object: %v", calls[0])
	assert.Equal(t, sessionID, args["lsp_injected_session"],
		"the plugin must inject the calling session's ID (platform namespace key) into the outgoing arguments")
	// The model-supplied args survive alongside the injection.
	assert.Equal(t, sessionID, args["session_id"], "mock emitted session_id == caller for the self-referential pin")
	assert.Equal(t, "stub hello", args["message"])
}

// TestOriginPlugin_NoPluginNoInjection — the negative leg: without the
// plugin entry, from_session_id must NOT appear (the injection is the
// plugin's doing; nothing in the harness adds session identity — the
// #1465 wire-format finding, held as a regression pin).
func TestOriginPlugin_NoPluginNoInjection(t *testing.T) {
	_, calls := bootOriginHarness(t, 14270, 14310, false)
	require.Len(t, calls, 1)
	args, ok := calls[0]["arguments"].(map[string]any)
	require.True(t, ok, "arguments must be an object: %v", calls[0])
	_, present := args["lsp_injected_session"]
	assert.False(t, present,
		"no harness-injected identity without the plugin — if this fails, the harness grew native injection and the plugin can be retired")
}

//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// The production-handoff integration leg (#1469 review r1): REAL
// opencode (pinned binary + the origin-injection plugin) talking to the
// REAL agentd MCP handler over the REAL seam loopback — no stub MCP, no
// fake agent. The freeze pin (pkg/agent/opencode) proves the plugin's
// injection reaches a stub; this test proves the whole production chain:
// model tool call → plugin injects from_session_id → mcpHandler
// validates by-ID and composes the sentinel → the delivered message
// lands in the TARGET session's transcript carrying the CALLER's
// origin, and the tool result echoes it.
//
// Run: OPENCODE_BINARY=/opencode/usr/local/bin/opencode \
//      go test -tags=integration -run TestOriginE2E ./cmd/workspace-agentd/ -timeout 600s

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

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// e2eMCPPassword matches the pkg harness's hardcoded boot password
// (startOpencodeServerWithConfig sets OPENCODE_SERVER_PASSWORD) — the
// client, the MCP entry's Basic header, and the boot must agree.
const e2eMCPPassword = "test-password"

// e2eMockProvider emits ONE llmsafespaces_send_message tool call on the
// first completion (the caller session's turn), plain text on every
// later one (the delivery turn + the caller's post-tool round).
type e2eMockProvider struct {
	mu        sync.Mutex
	requests  int
	targetID  string
	messageTx string
}

func (m *e2eMockProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.mu.Lock()
		m.requests++
		first := m.requests == 1
		target, msg := m.targetID, m.messageTx
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
		var chunk, done string
		if first {
			args, _ := json.Marshal(map[string]string{"session_id": target, "message": msg})
			chunkB, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
						"index": 0, "id": "call_e2e_1", "type": "function",
						"function": map[string]any{"name": "llmsafespaces_send_message", "arguments": string(args)},
					}}},
				}},
			})
			chunk = string(chunkB)
			doneB, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "delta": map[string]any{}}},
				"usage":   map[string]int{"prompt_tokens": 84, "completion_tokens": 9, "total_tokens": 93},
			})
			done = string(doneB)
		} else {
			chunkB, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "content": "MOCK-REPLY"},
				}},
			})
			chunk = string(chunkB)
			doneB, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "delta": map[string]any{}}},
				"usage":   map[string]int{"prompt_tokens": 84, "completion_tokens": 9, "total_tokens": 93},
			})
			done = string(doneB)
		}
		emit(chunk)
		emit(done)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// bootOriginE2E boots the pinned opencode with the plugin and the REAL
// agentd MCP handler as its llmsafespaces server; returns a seam client
// and the tool-result recorder.
func bootOriginE2E(t *testing.T) (*opencode.Client, *[]string, *e2eMockProvider) {
	t.Helper()

	binary := os.Getenv("OPENCODE_BINARY")
	if binary == "" {
		t.Skip("OPENCODE_BINARY not set — this leg needs the pinned opencode binary (see the file comment)")
	}

	pluginPath, err := filepath.Abs(filepath.Join("..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js"))
	require.NoError(t, err)
	_, statErr := os.Stat(pluginPath)
	require.NoError(t, statErr)

	// BOTH sidecar servers MUST sit on pinned low ports: the pinned
	// binary's outbound fetch fails against this pod's ephemeral port
	// range (live-debugged 2026-09-13 — the pkg L2 harness note; raw
	// httptest ephemeral ports wedge the boot).
	claim := func(p int) int {
		for i := 0; i < 50; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+i))
			if err != nil {
				continue
			}
			_ = l.Close()
			return p + i
		}
		t.Fatalf("no free port near %d", p)
		return 0
	}
	mcpPort := claim(14135)
	provPort := claim(14145)
	ocPort := claim(14155)

	// The REAL agentd MCP surface on an in-process server, seam pointed
	// at the (soon-to-boot) real opencode via the shared agent addr.
	// Listener-first construction on pinned low ports (the startLoopbackL2
	// pattern): the pinned binary cannot reach this pod's ephemeral port
	// range, and swapping .Listener after NewServer leaves both the accept
	// loop and .URL stale.
	toolResults := &[]string{}
	mcpListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mcpPort))
	require.NoError(t, err)
	mcpSrv := &httptest.Server{Listener: mcpListener, Config: &http.Server{
		Handler: realAgentdMCCHandler(t, toolResults),
	}}
	mcpSrv.Start()
	t.Cleanup(mcpSrv.Close)
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d", mcpPort)

	provider := &e2eMockProvider{}
	provListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", provPort))
	require.NoError(t, err)
	provSrv := &httptest.Server{Listener: provListener, Config: &http.Server{
		Handler: provider.handler(t),
	}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)
	provURL := fmt.Sprintf("http://127.0.0.1:%d", provPort)

	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			"mock": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"options": map[string]any{"apiKey": "test", "baseURL": provURL + "/v1"},
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
				"headers": map[string]any{"Authorization": "Basic " + basicAuth(e2eMCPPassword)},
			},
		},
		"plugin": []string{"file://" + pluginPath},
	}
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)

	// Boot via the pkg harness export: the EXACT boot path the passing
	// freeze pin uses (env discipline, detach, log-to-file, readiness
	// wait). A bespoke boot here drifted once and wedged — never again.
	srv := opencode.StartIntegrationServer(t, ocPort, string(cfgJSON))
	baseURL := srv.IntegrationBaseURL()

	// Point the seam (and thus the REAL mcpHandler's send_message) at
	// the real opencode, exactly as withAgentServer does for fakes.
	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(baseURL)
	t.Cleanup(func() { agentAddrAtomic.Store(old) })

	// StartIntegrationServer already waits for /global/health.
	return opencode.NewLoopbackClient(baseURL, e2eMCPPassword), toolResults, provider
}

// TestOriginE2E_RealHarnessRealAgentd — the full production chain.
func TestOriginE2E_RealHarnessRealAgentd(t *testing.T) {
	client, toolResults, provider := bootOriginE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "origin-e2e-caller")
	targetID := createWithBootstrapRetry(t, client, "origin-e2e-target")

	// Arm the mock's tool-call emission with the real target.
	provider.mu.Lock()
	provider.targetID = targetID
	provider.messageTx = "send the status report"
	provider.mu.Unlock()

	// The caller's turn runs the tool via the model: the harness plugin
	// injects lsp_injected_session=callerID, the REAL mcpHandler validates
	// both sessions by-ID, composes the sentinel, and delivers to the
	// target (idle → immediate).
	_, err := client.SessionSend(ctx, callerID, "send the status report", "", nil)
	require.NoError(t, err, "the caller's turn (tool call + post-tool round) must complete")

	// The tool result echoed the origin to the CALLING model.
	require.NotEmpty(t, *toolResults, "the real mcpHandler must have executed send_message")
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("send_message failed inside the real dispatch: %s", (*toolResults)[0])
	}
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte((*toolResults)[0]), &result))
	assert.Equal(t, callerID, result["origin"], "the tool result must echo the stamped origin")
	assert.Equal(t, "injected", result["origin_mode"], "the plugin-injected origin is labeled injected")
	assert.Equal(t, targetID, result["session_id"])
	assert.Equal(t, "delivering", result["status"], "idle target delivers immediately")

	// The delivered message carries the sentinel with the CALLER's ID.
	require.Eventually(t, func() bool {
		body, _, err := client.SessionMessagesRaw(ctx, targetID, 50, "")
		if err != nil {
			return false
		}
		return strings.Contains(string(body), "agent-message-v1")
	}, 30*time.Second, 500*time.Millisecond, "the sentineled message must land in the target transcript")

	body, _, err := client.SessionMessagesRaw(ctx, targetID, 50, "")
	require.NoError(t, err)
	var msgs []struct {
		Info struct {
			Role string `json:"role"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &msgs))
	var delivered string
	for _, m := range msgs {
		if m.Info.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == "text" && strings.HasPrefix(p.Text, "<!-- lsp:agent-message-v1 ") {
				delivered = p.Text
			}
		}
	}
	require.NotEmpty(t, delivered, "the target transcript must carry the sentineled user message")
	assert.Contains(t, delivered, `"fromSession":"`+callerID+`"`)
	assert.Contains(t, delivered, `"mode":"injected"`)
	assert.Contains(t, delivered, "send the status report")
}

// realAgentdMCCHandler serves the REAL agentd MCP dispatch (the same
// logic mcpHandler runs) with tool results captured for assertions.
// NEVER fails the test from inside the handler: a Goexit mid-request
// writes no response and the harness client retries forever — every
// input gets SOME response (400 on garbage), like agentd itself.
func realAgentdMCCHandler(t *testing.T, toolResults *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Type at entry, exactly like agentd's real mcpHandler
		// and the pkg pin stub: without it Go sniffs text/plain and the
		// streamable transport rejects the initialize response (the
		// r2 wedge — POST then GET then silence).
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			writeMCPResult(w, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "llmsafespaces-workspace", "version": "test"},
			})
		case "tools/list":
			// Advertise send_message exactly as the production schema
			// does (the model can only call registered tools).
			writeMCPResult(w, req.ID, map[string]any{"tools": []map[string]any{{
				"name":        "send_message",
				"description": "test surface",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id":      map[string]any{"type": "string"},
						"message":         map[string]any{"type": "string"},
						"from_session_id": map[string]any{"type": "string"},
					},
					"required": []string{"session_id", "message"},
				},
			}}})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			out, err := callMCPTool(r.Context(), e2eMCPPassword, params.Name, params.Arguments)
			if err != nil {
				*toolResults = append(*toolResults, fmt.Sprintf("ERROR: %v", err))
				writeMCPResult(w, req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("Error: %v", err)}},
					"isError": true,
				})
				return
			}
			*toolResults = append(*toolResults, out)
			writeMCPResult(w, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": out}},
			})
		default:
			writeMCPResult(w, req.ID, nil)
		}
	})
}

// createWithBootstrapRetry rides out the harness's bootstrap window:
// /global/health answers BEFORE the bootstrap completes (~seconds with
// live stubs; observed 8.5s in the child log), and stateful calls
// racing that window are reset. Bounded retry, not a correctness
// dependency of the production path.
func createWithBootstrapRetry(t *testing.T, client *opencode.Client, title string) string {
	t.Helper()
	var id string
	var lastErr error
	for i := 0; i < 120; i++ {
		id, lastErr = client.SessionCreate(context.Background(), title)
		if lastErr == nil {
			return id
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("SessionCreate never succeeded: %v", lastErr)
	return ""
}

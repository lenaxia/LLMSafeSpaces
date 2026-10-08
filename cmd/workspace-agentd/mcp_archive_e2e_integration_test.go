//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// #1627 e2e legs: the archive/metadata/delete tool workflows through
// the REAL production chain — pinned opencode + the origin-injection
// plugin + agentd's REAL mcpHandler dispatch (the bootOriginE2E
// skeleton; the provider and tools/list are generalized to the tool
// under test). The unit/dispatcher tests prove the pieces; these legs
// prove the chain: model tool call → plugin stamps
// lsp_injected_session → callMCPTool dispatch → tool result.
//
// Run: OPENCODE_BINARY=/opencode/usr/local/bin/opencode \
//      go test -tags=integration -run TestOriginE2E1627 ./cmd/workspace-agentd/ -timeout 600s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// e2e1627Provider emits ONE configurable llmsafespaces tool call on the
// first completion, plain text after.
type e2e1627Provider struct {
	mu       sync.Mutex
	requests int
	tool     string
	args     string
}

func (m *e2e1627Provider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.mu.Lock()
		m.requests++
		first := m.requests == 1
		tool, args := m.tool, m.args
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
			chunkB, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
						"index": 0, "id": "call_1627_1", "type": "function",
						"function": map[string]any{"name": tool, "arguments": args},
					}}},
				}},
			})
			chunk = string(chunkB)
		} else {
			chunkB, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "content": "MOCK-REPLY"},
				}},
			})
			chunk = string(chunkB)
		}
		doneB, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "delta": map[string]any{}}},
			"usage":   map[string]int{"prompt_tokens": 84, "completion_tokens": 9, "total_tokens": 93},
		})
		done = string(doneB)
		emit(chunk)
		emit(done)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// bootOriginE2E1627 mirrors bootOriginE2E with a configurable provider
// and a tools/list serving the REAL mcpToolCatalog (the model can only
// call advertised tools; the production catalog is the honest surface).
func bootOriginE2E1627(t *testing.T) (*opencode.Client, *[]string, *e2e1627Provider) {
	t.Helper()

	binary := os.Getenv("OPENCODE_BINARY")
	if binary == "" {
		t.Skip("OPENCODE_BINARY not set — this leg needs the pinned opencode binary (see the file comment)")
	}

	pluginPath, err := filepath.Abs(filepath.Join("..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js"))
	require.NoError(t, err)
	_, statErr := os.Stat(pluginPath)
	require.NoError(t, statErr)

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
	mcpPort := claim(14175)
	provPort := claim(14185)
	ocPort := claim(14195)

	toolResults := &[]string{}
	mcpListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mcpPort))
	require.NoError(t, err)
	mcpSrv := &httptest.Server{Listener: mcpListener, Config: &http.Server{
		Handler: realAgentdMCCatalogHandler(t, toolResults),
	}}
	mcpSrv.Start()
	t.Cleanup(mcpSrv.Close)
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d", mcpPort)

	provider := &e2e1627Provider{}
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

	srv := opencode.StartIntegrationServer(t, ocPort, string(cfgJSON))
	baseURL := srv.IntegrationBaseURL()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(baseURL)
	t.Cleanup(func() { agentAddrAtomic.Store(old) })

	return opencode.NewLoopbackClient(baseURL, e2eMCPPassword), toolResults, provider
}

// realAgentdMCCatalogHandler is realAgentdMCCHandler with the ONE
// difference the #1627 legs need: tools/list serves the REAL
// mcpToolCatalog (serialized to the wire shape) instead of the
// send_message-only stub — opencode will not emit calls for tools the
// MCP server has not advertised, so the harness must advertise the
// production surface for session_metadata/delete_session/
// session_archive to be callable.
func realAgentdMCCatalogHandler(t *testing.T, toolResults *[]string) http.Handler {
	inner := realAgentdMCCHandler(t, toolResults)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			inner.ServeHTTP(w, r)
			return
		}
		var probe struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			inner.ServeHTTP(w, r)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := json.Unmarshal(body, &probe); err == nil && probe.Method == "tools/list" {
			w.Header().Set("Content-Type", "application/json")
			writeMCPResult(w, probe.ID, map[string]any{"tools": mcpToolCatalog()})
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// The #1627 default, end to end: the model calls session_metadata with
// NO session_id; the plugin stamps the caller; the REAL dispatch scopes
// the result to exactly the calling session.
func TestOriginE2E1627_SessionMetadataDefaultIsCaller(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21627-caller")
	otherID := createWithBootstrapRetry(t, client, "e21627-other")

	provider.mu.Lock()
	provider.tool = "llmsafespaces_session_metadata"
	provider.args = `{}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "how am I doing", "", nil)
	require.NoError(t, err, "the caller's turn must complete")

	require.NotEmpty(t, *toolResults)
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("session_metadata failed inside the real dispatch: %s", (*toolResults)[0])
	}
	var result struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte((*toolResults)[0]), &result))
	require.Len(t, result.Sessions, 1, "the plugin-stamped caller is the default scope — the other session must NOT appear")
	assert.Equal(t, callerID, result.Sessions[0].SessionID)
	assert.NotEqual(t, otherID, result.Sessions[0].SessionID)
}

// The self-delete refusal, end to end: the model names its OWN session
// (copied from the metadata above); the plugin stamps the same id; the
// refusal fires inside the REAL dispatch — no platform call.
func TestOriginE2E1627_DeleteOwnSessionRefused(t *testing.T) {
	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21627-selfdel")

	provider.mu.Lock()
	provider.tool = "llmsafespaces_delete_session"
	provider.args = `{"session_id":"` + callerID + `"}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "clean me up", "", nil)
	require.NoError(t, err, "the caller's turn must complete (the refusal is a tool error, not a turn failure)")

	require.NotEmpty(t, *toolResults)
	first := (*toolResults)[0]
	assert.True(t, strings.Contains(first, "your own current session"),
		"the self-delete refusal must surface through the real chain: %s", first)
}

// The archive tool, end to end through the platform half: a stub
// pod-identity API receives the internal call; the tool result reports
// the archive.
func TestOriginE2E1627_SessionArchiveReachesPlatform(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotBody string
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		gotPath, gotBody = r.URL.Path, string(buf)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer platform.Close()
	t.Setenv("WORKSPACE_ID", "ws-e21627")
	t.Setenv("LLMSAFESPACE_API_URL", platform.URL)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)

	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21627-archiver")
	targetID := createWithBootstrapRetry(t, client, "e21627-archivetarget")

	provider.mu.Lock()
	provider.tool = "llmsafespaces_session_archive"
	provider.args = `{"session_id":"` + targetID + `","archived":true}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "archive the other one", "", nil)
	require.NoError(t, err, "the caller's turn must complete")

	require.NotEmpty(t, *toolResults)
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("session_archive failed inside the real dispatch: %s", (*toolResults)[0])
	}
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte((*toolResults)[0]), &result))
	assert.Equal(t, "archived", result["status"])
	assert.Equal(t, targetID, result["session_id"])

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "/internal/v1/session-archive", gotPath, "the platform half must receive the internal call")
	assert.Contains(t, gotBody, `"sessionID":"`+targetID+`"`)
	assert.Contains(t, gotBody, `"archived":true`)
	assert.Contains(t, gotBody, `"workspaceID":"ws-e21627"`)
}

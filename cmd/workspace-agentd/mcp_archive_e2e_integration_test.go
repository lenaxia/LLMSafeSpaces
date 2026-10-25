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
	var archivedIDs []string
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/session-archived" {
			// #1879: the post-write confirmation read — echo the id the
			// archive write carried, so the tool's verified verdict has
			// a fresh set to agree with.
			mu.Lock()
			ids := archivedIDs
			mu.Unlock()
			body, err := json.Marshal(map[string]any{"archived": ids})
			if err != nil {
				body = []byte(`{"archived":[]}`)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		gotPath, gotBody = r.URL.Path, string(buf)
		var req struct {
			SessionID string `json:"sessionID"`
			Archived  *bool  `json:"archived"`
		}
		_ = json.Unmarshal(buf, &req)
		archivedIDs = nil
		if req.Archived != nil && *req.Archived {
			archivedIDs = []string{req.SessionID}
		}
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
	assert.Equal(t, "verified", result["archived_visibility"],
		"#1879: the confirmation read agrees with the write — the verdict surfaces through the real dispatch chain")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "/internal/v1/session-archive", gotPath, "the platform half must receive the internal call")
	assert.Contains(t, gotBody, `"sessionID":"`+targetID+`"`)
	assert.Contains(t, gotBody, `"archived":true`)
	assert.Contains(t, gotBody, `"workspaceID":"ws-e21627"`)
}

// --- #1836 bulk legs: the batch twins through the REAL chain ----------
//
// Review r1's e2e mandate: both new workflows prove the chain end to
// end — model tool call → origin plugin (stamps lsp_injected_session
// for the delete bulk) → REAL mcpHandler dispatch → per-item result
// array. Named TestOriginE2E… so CI's origin-e2e selector runs them.

// The archive bulk, end to end: a mixed batch (two live targets + one
// platform-miss) renders the per-item array — done/verified for the
// landed writes, the bare not_found for the miss — and the platform
// half receives each item's canonical id.
func TestOriginE2E1836_ArchiveBulkMixedBatch(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	archivedIDs := map[string]bool{}
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/session-archived":
			mu.Lock()
			ids := make([]string, 0, len(archivedIDs))
			for id, on := range archivedIDs {
				if on {
					ids = append(ids, id)
				}
			}
			mu.Unlock()
			body, _ := json.Marshal(map[string]any{"archived": ids})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case "/internal/v1/session-archive":
			var req struct {
				SessionID string `json:"sessionID"`
				Archived  bool   `json:"archived"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("bad body: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			writes = append(writes, req.SessionID)
			if strings.Contains(req.SessionID, "ghost") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprintf(w, `{"error":"not found: session %s"}`, req.SessionID)
				return
			}
			archivedIDs[req.SessionID] = req.Archived
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected platform path %s", r.URL.Path)
		}
	}))
	defer platform.Close()
	t.Setenv("WORKSPACE_ID", "ws-e21836")
	t.Setenv("LLMSAFESPACE_API_URL", platform.URL)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)

	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21836-archiver")
	targetA := createWithBootstrapRetry(t, client, "e21836-target-a")
	targetB := createWithBootstrapRetry(t, client, "e21836-target-b")

	provider.mu.Lock()
	provider.tool = "llmsafespaces_session_archive_bulk"
	provider.args = `{"sessionIds":["` + targetA + `","ses_ghost_miss","` + targetB + `"],"archived":true}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "archive the finished ones", "", nil)
	require.NoError(t, err, "the caller's turn must complete (a partial-failure item is not a turn failure)")

	require.NotEmpty(t, *toolResults)
	if strings.HasPrefix((*toolResults)[0], "ERROR: ") {
		t.Fatalf("session_archive_bulk failed inside the real dispatch: %s", (*toolResults)[0])
	}
	var result struct {
		Results []struct {
			ID                 string `json:"id"`
			Status             string `json:"status"`
			ArchivedVisibility string `json:"archived_visibility"`
			Error              *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte((*toolResults)[0]), &result))
	require.Len(t, result.Results, 3)

	byID := map[string]struct {
		Status             string
		ArchivedVisibility string
		Code               string
	}{}
	for _, r := range result.Results {
		entry := struct {
			Status             string
			ArchivedVisibility string
			Code               string
		}{Status: r.Status, ArchivedVisibility: r.ArchivedVisibility}
		if r.Error != nil {
			entry.Code = r.Error.Code
		}
		byID[r.ID] = entry
	}
	assert.Equal(t, "done", byID[targetA].Status, "target A lands through the real chain")
	assert.Equal(t, "verified", byID[targetA].ArchivedVisibility, "#1879 verdict surfaces per item through the real chain")
	assert.Equal(t, "done", byID[targetB].Status)
	assert.Equal(t, "verified", byID[targetB].ArchivedVisibility)
	assert.Equal(t, "failed", byID["ses_ghost_miss"].Status, "the miss is a per-item failure, never a turn failure")
	assert.Equal(t, "not_found", byID["ses_ghost_miss"].Code)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{targetA, "ses_ghost_miss", targetB}, writes,
		"per-item delegation in order — the platform half receives each canonical id")
}

// The delete bulk, end to end: the batch [own session, other session]
// through the REAL plugin chain — the plugin stamps the calling
// session (the injection half this lane adds to INJECTED_SESSION_TOOLS),
// the self item renders its per-item refusal, the other item deletes,
// and the platform half is never asked for the caller's own session.
func TestOriginE2E1836_DeleteBulkSelfGuard(t *testing.T) {
	var mu sync.Mutex
	var asks []string
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-delete" {
			t.Errorf("unexpected platform path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad body: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		asks = append(asks, req.SessionID)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer platform.Close()
	t.Setenv("WORKSPACE_ID", "ws-e21836d")
	t.Setenv("LLMSAFESPACE_API_URL", platform.URL)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)

	client, toolResults, provider := bootOriginE2E1627(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	callerID := createWithBootstrapRetry(t, client, "e21836-selfdel")
	otherID := createWithBootstrapRetry(t, client, "e21836-othertarget")

	provider.mu.Lock()
	provider.tool = "llmsafespaces_session_delete_bulk"
	provider.args = `{"sessionIds":["` + callerID + `","` + otherID + `"]}`
	provider.mu.Unlock()

	_, err := client.SessionSend(ctx, callerID, "clean up me and the leftover", "", nil)
	require.NoError(t, err, "the caller's turn must complete (the self refusal is per-item)")

	require.NotEmpty(t, *toolResults)
	first := (*toolResults)[0]
	require.False(t, strings.HasPrefix(first, "ERROR: "),
		"the batch returns its per-item array, not a tool error: %s", first)
	var result struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(first), &result))
	require.Len(t, result.Results, 2)

	assert.Equal(t, callerID, result.Results[0].ID)
	assert.Equal(t, "failed", result.Results[0].Status, "the plugin-stamped caller is refused per item")
	require.NotNil(t, result.Results[0].Error)
	assert.Equal(t, "self_delete_refused", result.Results[0].Error.Code)

	assert.Equal(t, otherID, result.Results[1].ID)
	assert.Equal(t, "done", result.Results[1].Status, "the rest of the batch still processes")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{otherID}, asks,
		"the platform half is never asked to delete the caller's own session — the injection half works through the real plugin")
}

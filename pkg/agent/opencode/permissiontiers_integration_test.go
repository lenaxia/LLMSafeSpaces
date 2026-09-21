//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// The #1493 live legs: the permission-tier floor rendered into a REAL
// pinned-opencode boot, denied through the REAL matcher in both
// semantics the ruling mandates — the READ tool (canonical/resolved
// symlink paths) and BASH (typed paths). The mock provider emits one
// tool call per turn; the tool result's error text proves the denial
// fired (or fails the test proving a tier leaks).
//
// Run: OPENCODE_BINARY=/opencode/usr/local/bin/opencode \
//      go test -tags=integration -run TestPermissionTier \
//      ./pkg/agent/opencode/ -timeout 600s

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

// tierProbeProvider emits a sequence of tool calls — one per request —
// and records nothing else. Each turn's tool result comes back in the
// session transcript where the test asserts the denial.
type tierProbeProvider struct {
	mu    sync.Mutex
	calls []string // JSON argument blobs queued by the test, one per turn
}

func (m *tierProbeProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.mu.Lock()
		var call *string
		if len(m.calls) > 0 {
			c := m.calls[0]
			m.calls = m.calls[1:]
			call = &c
		}
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
		if call == nil {
			// Post-tool round: a plain-text finish so the turn completes.
			text, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "content": "done"},
				}},
			})
			emit(string(text))
			done, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "delta": map[string]any{}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
			emit(string(done))
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		chunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": nil,
				"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
					"index": 0, "id": "call_tier_1", "type": "function",
					"function": map[string]any{"name": "read", "arguments": *call},
				}}},
			}},
		})
		emit(string(chunk))
		done, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "delta": map[string]any{}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
		emit(string(done))
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// tierBoot writes a config whose mode.permissions.external_directory is
// EXACTLY the platform floor (as the ConfigWriter renders it) and boots
// the pinned binary via the shared harness.
func tierBoot(t *testing.T, prov *httptest.Server) *opencodeServer {
	t.Helper()
	extDir := make(map[string]string, len(platformPermissionTiers))
	for k, v := range platformPermissionTiers {
		extDir[k] = v
	}
	perms := map[string]any{"external_directory": extDir}
	mode := map[string]any{"permissions": perms}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			"mock": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"options": map[string]any{"apiKey": "test", "baseURL": prov.URL + "/v1"},
				"models": map[string]any{"mockmodel": map[string]any{
					"name": "Mock Model", "attachment": true,
					"limit": map[string]int{"context": 100000, "output": 4096},
				}},
			},
		},
		"model": "mock/mockmodel",
		"mode":  mode,
	}
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)
	return StartIntegrationServer(t, 14420, string(cfgJSON))
}

// lastToolResult scans the newest session message for a tool part whose
// callID matches the emitted call and returns its error/output text.
func lastToolResult(t *testing.T, c *Client, sessionID string) string {
	t.Helper()
	body, _, err := c.SessionMessagesRaw(context.Background(), sessionID, 10, "")
	require.NoError(t, err)
	var msgs []struct {
		Parts []struct {
			Type  string `json:"type"`
			Tool  string `json:"tool"`
			State struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, p := range msgs[i].Parts {
			if p.Type == "tool" && p.Tool == "read" {
				if p.State.Error != "" {
					return p.State.Error
				}
				return p.State.Output
			}
		}
	}
	return ""
}

// TestPermissionTier_ReadDeniesCanonicalTargets: the read tool resolves
// symlinks to canonical paths before the permission gate — the
// resolved-target denies (/sandbox-runtime/rt/secrets/*) must fire.
func TestPermissionTier_ReadDeniesCanonicalTargets(t *testing.T) {
	prov := tierProbeProvider{}
	prov.calls = []string{`{"filePath":"/sandbox-runtime/rt/secrets/leak.txt"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14490), Config: &http.Server{Handler: prov.handler(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-read")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "read the file", "", nil)
	require.NoError(t, err, "the turn (denied tool + finish) must complete")

	res := lastToolResult(t, c, id)
	t.Logf("read tool result: %s", res)
	assert.Contains(t, strings.ToLower(res), "denied",
		"the read of a resolved secret target must be DENIED by the tier floor through the real matcher")
}

// TestPermissionTier_BashDeniesTypedEtcPath: bash command parsing feeds
// TYPED paths to the external_directory gate — the /etc deny must fire
// for a typed absolute path.
func TestPermissionTier_BashDeniesTypedEtcPath(t *testing.T) {
	// Reuse the provider shape with a bash call.
	prov := tierProbeProvider{}
	prov.calls = []string{`{"command":"cat /etc/passwd"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14495), Config: &http.Server{Handler: prov.handlerBash(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-bash")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "run the command", "", nil)
	require.NoError(t, err)

	res := lastBashResult(t, c, id)
	t.Logf("bash tool result: %s", res)
	assert.Contains(t, strings.ToLower(res), "denied",
		"bash with a typed /etc path must be DENIED by the tier floor")
}

func (m *tierProbeProvider) handlerBash(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.mu.Lock()
		var call *string
		if len(m.calls) > 0 {
			c := m.calls[0]
			m.calls = m.calls[1:]
			call = &c
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if call == nil {
			text, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "content": "done"},
				}},
			})
			fmt.Fprintf(w, "data: %s\n\n", text)
			done, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "delta": map[string]any{}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
			fmt.Fprintf(w, "data: %s\n\n", done)
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		chunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": nil,
				"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
					"index": 0, "id": "call_tier_b", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": *call},
				}}},
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		done, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "delta": map[string]any{}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
		fmt.Fprintf(w, "data: %s\n\n", done)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func lastBashResult(t *testing.T, c *Client, sessionID string) string {
	t.Helper()
	body, _, err := c.SessionMessagesRaw(context.Background(), sessionID, 10, "")
	require.NoError(t, err)
	var msgs []struct {
		Parts []struct {
			Type  string `json:"type"`
			Tool  string `json:"tool"`
			State struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, p := range msgs[i].Parts {
			if p.Type == "tool" && p.Tool == "bash" {
				if p.State.Error != "" {
					return p.State.Error
				}
				return p.State.Output
			}
		}
	}
	return ""
}

func mustListener(t *testing.T, port int) net.Listener {
	t.Helper()
	for i := 0; i < 50; i++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+i))
		if err == nil {
			return l
		}
	}
	t.Fatalf("no free port near %d", port)
	return nil
}

var _ = os.Getenv // keep os import if fixture paths land later
var _ = filepath.Join

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// dev_preview_state_test.go — #1617: the space's dev-preview state must be
// live in-pod. The boot env (WORKSPACE_DEV_PREVIEW_ENABLED) is a snapshot
// frozen at pod creation; the API pushes every toggle to the running pod
// (POST /v1/dev-preview-state) and feature_status / dev_preview_url read
// the push first, the boot env second, the skew note last.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetDevPreviewState restores the process-wide live-push store after a
// test mutates it (the swapDefaultDevPreviewHeaders pattern).
func resetDevPreviewState(t *testing.T) {
	t.Helper()
	old := devPreviewPushedAtomic.Load()
	t.Cleanup(func() { devPreviewPushedAtomic.Store(old) })
	devPreviewPushedAtomic.Store("")
}

func devPreviewStateRequest(method, body string) *http.Request {
	req := httptest.NewRequest(method, "/v1/dev-preview-state", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth("ws-pass"))
	return req
}

func TestDevPreviewStateHandler_AuthRequired(t *testing.T) {
	resetDevPreviewState(t)
	h := devPreviewStateHandler("ws-pass", "agentd-pass")

	req := httptest.NewRequest(http.MethodPost, "/v1/dev-preview-state", strings.NewReader(`{"enabled":true}`))
	w := httptest.NewRecorder()
	h(w, req)
	assert.Equal(t, 401, w.Code)
}

func TestDevPreviewStateHandler_MethodNotAllowed(t *testing.T) {
	resetDevPreviewState(t)
	h := devPreviewStateHandler("ws-pass", "agentd-pass")

	w := httptest.NewRecorder()
	h(w, devPreviewStateRequest(http.MethodGet, ""))
	assert.Equal(t, 405, w.Code)
}

func TestDevPreviewStateHandler_BadBodyRejected(t *testing.T) {
	resetDevPreviewState(t)
	h := devPreviewStateHandler("ws-pass", "agentd-pass")

	for _, body := range []string{`{}`, `not json`, `{"enabled":"yes"}`} {
		w := httptest.NewRecorder()
		h(w, devPreviewStateRequest(http.MethodPost, body))
		assert.Equal(t, 400, w.Code, "body %q must be rejected — enabled must be an explicit boolean, never guessed", body)
	}
}

func TestDevPreviewStateHandler_StoresBothDirections(t *testing.T) {
	resetDevPreviewState(t)
	h := devPreviewStateHandler("ws-pass", "agentd-pass")

	for _, enabled := range []bool{true, false} {
		w := httptest.NewRecorder()
		h(w, devPreviewStateRequest(http.MethodPost, fmt.Sprintf(`{"enabled":%t}`, enabled)))
		require.Equal(t, 200, w.Code)

		active, reported, live := devPreviewState()
		assert.Equal(t, enabled, active)
		assert.True(t, reported)
		assert.True(t, live)
	}
}

// --- the tool surface reads live push over boot env (#1617's exact bug) ---

func TestDevPreviewURL_LivePushTrueOverridesEnvFalse(t *testing.T) {
	resetDevPreviewState(t)
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("WORKSPACE_DEV_PREVIEW_ENABLED", "false")

	devPreviewPushedAtomic.Store("true")

	req := mcpRequest{JSONRPC: "2.0", ID: 31, Method: "tools/call", Params: mcpMustMarshal(t, map[string]any{
		"name": "dev_preview_url", "arguments": map[string]any{"port": 5173},
	})}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	isErr, _ := result["isError"].(bool)
	assert.False(t, isErr, "a live enable must mint even though the boot env still says false")
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, text, "LSP_DEV_PREVIEW_V1")
	assert.NotContains(t, text, "controller did not report", "a pushed state is reported — no skew note")
}

func TestDevPreviewURL_LivePushFalseOverridesEnvTrue(t *testing.T) {
	resetDevPreviewState(t)
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("WORKSPACE_DEV_PREVIEW_ENABLED", "true")

	devPreviewPushedAtomic.Store("false")

	req := mcpRequest{JSONRPC: "2.0", ID: 32, Method: "tools/call", Params: mcpMustMarshal(t, map[string]any{
		"name": "dev_preview_url", "arguments": map[string]any{"port": 5173},
	})}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.True(t, result["isError"].(bool), "a live disable must refuse even though the boot env still says true")
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, strings.ToLower(text), "disabled")
	assert.NotContains(t, text, "LSP_DEV_PREVIEW_V1")
}

func TestDevPreviewURL_LivePushCoversControllerSkew(t *testing.T) {
	resetDevPreviewState(t)
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("WORKSPACE_DEV_PREVIEW_ENABLED", "")

	devPreviewPushedAtomic.Store("true")

	req := mcpRequest{JSONRPC: "2.0", ID: 33, Method: "tools/call", Params: mcpMustMarshal(t, map[string]any{
		"name": "dev_preview_url", "arguments": map[string]any{"port": 5173},
	})}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	isErr, _ := result["isError"].(bool)
	assert.False(t, isErr)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, text, "LSP_DEV_PREVIEW_V1")
	assert.NotContains(t, text, "controller did not report", "the push is authoritative over an absent boot env")
}

func TestDevPreviewURL_NoPushStillReadsBootEnv(t *testing.T) {
	resetDevPreviewState(t)
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("WORKSPACE_DEV_PREVIEW_ENABLED", "true")

	req := mcpRequest{JSONRPC: "2.0", ID: 34, Method: "tools/call", Params: mcpMustMarshal(t, map[string]any{
		"name": "dev_preview_url", "arguments": map[string]any{"port": 5173},
	})}
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	isErr, _ := result["isError"].(bool)
	assert.False(t, isErr, "no push — the boot env remains the fallback")
}

func TestFeatureStatus_DevPreviewLivePush(t *testing.T) {
	resetDevPreviewState(t)
	t.Setenv("WORKSPACE_DEV_PREVIEW_ENABLED", "false")

	devPreviewPushedAtomic.Store("true")

	out, err := mcpFeatureStatus()
	require.NoError(t, err)
	var statuses []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &statuses))

	var devPreview map[string]any
	for _, st := range statuses {
		if st["feature"] == "dev_preview" {
			devPreview = st
		}
	}
	require.NotNil(t, devPreview)
	assert.Equal(t, true, devPreview["active"], "the live push must win over the stale boot env")
	detail, _ := devPreview["source_detail"].(string)
	assert.Contains(t, detail, "live", "the source detail must say the state came from the live push")
}

func TestFeatureStatus_DevPreviewUnreportedWithPush(t *testing.T) {
	resetDevPreviewState(t)
	t.Setenv("WORKSPACE_DEV_PREVIEW_ENABLED", "")

	devPreviewPushedAtomic.Store("false")

	out, err := mcpFeatureStatus()
	require.NoError(t, err)
	var statuses []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &statuses))

	var devPreview map[string]any
	for _, st := range statuses {
		if st["feature"] == "dev_preview" {
			devPreview = st
		}
	}
	require.NotNil(t, devPreview)
	assert.Equal(t, false, devPreview["active"])
	detail, _ := devPreview["source_detail"].(string)
	assert.NotContains(t, detail, "UNREPORTED", "a pushed state is reported — the skew note belongs to the absent-env-only case")
}

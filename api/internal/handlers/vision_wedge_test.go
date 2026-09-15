// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// wedgeBackend answers the synchronous message POST with the exact #1307
// wedge 400 (live-verified litellm surface) — every turn replays the
// image-bearing history into a text-only model.
func wedgeBackend(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"name":"ProviderServerError","data":{"message":"Provider request failed with HTTP 400: litellm.BadRequestError: ZaiException - messages.content.type is invalid, allowed values: ['text']"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`[]`))
}

// setupWedgeEnv wires a real opencode adapter against the wedge backend.
func setupWedgeEnv(t *testing.T) *testEnv {
	t.Helper()
	log, _ := logger.NewObserved()
	env := newTestEnvWithBackendAndLogger(t, wedgeBackend, log)
	env.setupWorkspacePodWithT(t, "ws-wedge", "10.0.0.1", "Active", "ws-wedge")
	env.setupPasswordWithT(t, "ws-wedge", "test-password")
	env.setupWorkspaceWithT(t, "ws-wedge", 5)
	adapter := agentoc.NewAdapter(
		env.handler.AdapterPasswordResolver(),
		env.handler.AdapterPodIPResolver(),
		nil,
		agentoc.WithAdapterHTTPClient(env.handler.httpClient),
	)
	env.handler.adapter = adapter
	return env
}

// TestSendMessage_TextOnlyWedgeReturnsTargetedError pins the #1307 send
// surface: instead of the raw generic 502, the classified wedge returns a
// structured 422 whose body names the cause and the escape (switch to a
// vision-capable model) — the user must not need the provider log to
// recover.
func TestSendMessage_TextOnlyWedgeReturnsTargetedError(t *testing.T) {
	env := setupWedgeEnv(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-wedge/sessions/ses_wedge/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"continue"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "text_only_model_image_history")
	require.Contains(t, body, "vision")
	require.NotContains(t, body, "litellm.BadRequestError", "raw provider body must not be the user surface")
}

// TestSendMessage_GenericAdapterErrorStill502 pins that unrelated adapter
// failures keep the existing generic 502 — the classification is tight.
func TestSendMessage_GenericAdapterErrorStill502(t *testing.T) {
	log, _ := logger.NewObserved()
	env := newTestEnvWithBackendAndLogger(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream exploded"}`, http.StatusBadGateway)
	}, log)
	env.setupWorkspacePodWithT(t, "ws-502", "10.0.0.1", "Active", "ws-502")
	env.setupPasswordWithT(t, "ws-502", "test-password")
	env.setupWorkspaceWithT(t, "ws-502", 5)
	adapter := agentoc.NewAdapter(
		env.handler.AdapterPasswordResolver(),
		env.handler.AdapterPodIPResolver(),
		nil,
		agentoc.WithAdapterHTTPClient(env.handler.httpClient),
	)
	env.handler.adapter = adapter

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-502/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadGateway, w.Code)
	require.NotContains(t, w.Body.String(), "text_only_model_image_history")
}

// TestSyncSend_TextOnlyWedgeReturnsTargetedError pins the same structured
// surface on the /prompt synchronous fallback path (no outbox wired).
func TestSyncSend_TextOnlyWedgeReturnsTargetedError(t *testing.T) {
	env := setupWedgeEnv(t)
	env.handler.outbox = nil

	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	proxy.POST("/sessions/:sessionId/prompt", env.handler.SendPromptAsync)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-wedge/sessions/ses_wedge/prompt",
		strings.NewReader(`{"parts":[{"type":"text","text":"continue"}]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	require.Contains(t, w.Body.String(), "text_only_model_image_history")
}

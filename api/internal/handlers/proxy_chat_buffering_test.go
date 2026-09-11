// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// stubAgentStateChecker is a minimal AgentStateChecker for US-27b.5 tests.
type stubAgentStateChecker struct {
	changedAt time.Time
	err       error
}

func (s stubAgentStateChecker) GetLastCredentialChangedAt(_ context.Context, _ string) (time.Time, error) {
	return s.changedAt, s.err
}

// TestSendMessage_4xxWithErrorBuffering_PendingCredentials_EnrichesResponseBody
// closes the US-27b.5 Definition-of-Done gap: the chat error body rewrite
// that was previously deferred (logged server-side only) must now actually
// reach the client.
//
// Flow:
//   - User POSTs /sessions/:sid/message
//   - Pod returns 4xx with a structured opencode error body
//   - agentStateChecker reports a pending credential change
//   - Client receives the SAME status code but with EnrichChatErrorBody's
//     agentNeedsRefresh / credentialsPendingSince / hint fields added
func TestSendMessage_4xxWithErrorBuffering_PendingCredentials_EnrichesResponseBody(t *testing.T) {
	podResp := `{"_tag":"ProviderNotFoundError","message":"provider 'openai' not configured","providerID":"openai"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, podResp)
	}))
	defer backend.Close()

	env := newTestEnvWithBackend(t, backend.Config.Handler.(http.HandlerFunc))
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-password")

	since := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	env.handler.SetAgentStateChecker(stubAgentStateChecker{changedAt: since})

	// env.router has the legacy-message transport route registered by the
	// shared harness (proxy_test_helpers_test.go).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/legacy-message/ses-1",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, "status code must pass through unchanged")

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ProviderNotFoundError", got["_tag"], "original allowlisted fields preserved")
	assert.Equal(t, "openai", got["providerID"])
	assert.Equal(t, true, got["agentNeedsRefresh"], "US-27b.5 enrichment must reach the client")
	assert.Equal(t, since.Format(time.RFC3339), got["credentialsPendingSince"])
	assert.Contains(t, got["hint"].(string), "ws-1")
	assert.Contains(t, got["hint"].(string), "reload")
}

// TestSendMessage_4xxWithErrorBuffering_NoPendingCredentials_PassesBodyThroughUnchanged
// verifies the negative path: a 4xx without pending credentials must not
// invent enrichment fields. The body should round-trip unchanged (allowlist
// still applies since EnrichChatErrorBody always rewrites through the
// allowlist — but no hint / agentNeedsRefresh fields are added).
func TestSendMessage_4xxWithErrorBuffering_NoPendingCredentials_PassesBodyThroughUnchanged(t *testing.T) {
	podResp := `{"_tag":"SessionBusyError","message":"busy","sessionID":"s1"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, podResp)
	}))
	defer backend.Close()

	env := newTestEnvWithBackend(t, backend.Config.Handler.(http.HandlerFunc))
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-password")

	// No agentStateChecker wired — no enrichment.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/legacy-message/ses-1",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "SessionBusyError", got["_tag"])
	assert.Nil(t, got["agentNeedsRefresh"], "no enrichment without pending credentials")
	assert.Nil(t, got["hint"])
}

// TestSendMessage_2xxResponse_StreamsNormally_Unbuffered proves the buffering
// path is gated to 4xx ONLY: a 2xx streaming response (the normal chat path)
// must continue to stream chunk-by-chunk without buffering.
func TestSendMessage_2xxResponse_StreamsNormally_Unbuffered(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Write multiple chunks with explicit flushes.
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: chunk1\n\n"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: chunk2\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer backend.Close()

	env := newTestEnvWithBackend(t, backend.Config.Handler.(http.HandlerFunc))
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-password")
	// Wire a checker to prove it's NOT invoked on 2xx.
	called := false
	env.handler.SetAgentStateChecker(stubAgentStateCheckerFunc(func(ctx context.Context, wsID string) (time.Time, error) {
		called = true
		return time.Time{}, nil
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/legacy-message/ses-1",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "data: chunk1")
	assert.Contains(t, body, "data: chunk2")
	assert.False(t, called, "checker must not be invoked on 2xx (no buffering)")
}

// TestGetHistory_4xx_NoEnrichment proves the buffering is scoped to chat
// (SendMessage) only — GetHistory 4xx responses must pass through unchanged.
func TestGetHistory_4xx_NoEnrichment(t *testing.T) {
	podResp := `{"_tag":"NotFoundError","message":"session gone"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, podResp)
	}))
	defer backend.Close()

	env := newTestEnvWithBackend(t, backend.Config.Handler.(http.HandlerFunc))
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.SetAgentStateChecker(stubAgentStateChecker{changedAt: time.Now()})

	// GetHistory is NOT in the default proxy group's write routes but IS
	// registered for /sessions/:sessionId/message GET in the default harness.
	// Re-use the existing route.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/workspaces/ws-1/sessions/ses-1/message", nil)
	env.router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	// Body passed through verbatim — no enrichment fields.
	assert.Contains(t, rec.Body.String(), "NotFoundError")
	assert.NotContains(t, rec.Body.String(), "agentNeedsRefresh")
	assert.NotContains(t, rec.Body.String(), "hint")
}

// TestSendMessage_4xxWithErrorBuffering_LargeBodyTruncated proves the buffer
// has an upper bound (chat errors are small structured payloads; a runaway
// upstream must not consume unbounded memory).
func TestSendMessage_4xxWithErrorBuffering_LargeBodyTruncated(t *testing.T) {
	// 256 KB body — exceeds the 64 KB buffer cap.
	huge := strings.Repeat("x", 256*1024)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, huge)
	}))
	defer backend.Close()

	env := newTestEnvWithBackend(t, backend.Config.Handler.(http.HandlerFunc))
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.SetAgentStateChecker(stubAgentStateChecker{changedAt: time.Now()})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/legacy-message/ses-1",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	// The non-JSON body should be wrapped by EnrichChatErrorBody with a
	// truncated "message" field. Response body must be << 256 KB.
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "enricher must produce valid JSON even on truncation")
	assert.Less(t, rec.Body.Len(), 8*1024, "response must be bounded (< 8KB)")
}

// TestSendMessage_RetryAfterConnError_4xxStillEnriched is documented as a
// known gap: the retry path at proxy.go:285 passes onErrorBody to the second
// doProxy call (verified by code inspection). A full integration test
// requires mocking the workspace CRD Get to return a different PodIP
// mid-reconcile, which is disproportionate to the risk — the onErrorBody
// parameter flows through transparently and the non-retry 4xx path is
// covered by the 5 tests above.

// --- helpers ---

// stubAgentStateCheckerFunc is a function-adapter for AgentStateChecker.
type stubAgentStateCheckerFunc func(ctx context.Context, workspaceID string) (time.Time, error)

func (f stubAgentStateCheckerFunc) GetLastCredentialChangedAt(ctx context.Context, wsID string) (time.Time, error) {
	return f(ctx, wsID)
}

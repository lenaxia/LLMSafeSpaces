// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// phrase_ids_test.go — #1816 platform-MCP phrase pins (red-first):
// the SDK MCP server's session surfaces emit phrases with degraded
// markers (session_create's registration-degraded window), accept
// phrase refs without the contract-stream timeout burn (the resolved
// id bridge), never leak the harness id in question results, and
// attach the #1756 §2 deprecation notice on deprecated-form inputs
// (parity with the REST Deprecation/Sunset headers).

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// ===== session_create: degraded window carries the marker (#1816 G1) =====

func TestSessionCreate_DegradedCarriesDeprecatedMarker(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	const harness = "ses_deadbeefcafe"
	mockClient.On("CreateSession", ctx, "ws-1").Return(&SessionResp{
		ID:                  harness,
		DeprecatedSessionID: harness,
	}, nil)

	result, err := h.sessionCreate(ctx, makeReq("session_create", map[string]any{"workspace_id": "ws-1"}))
	require.NoError(t, err)
	assert.False(t, result.IsError)

	text := result.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, `"sessionId":`+`"ses_deadbeefcafe"`)
	assert.Contains(t, text, `"session_id_deprecated":true`,
		"the degraded window must FLAG the harness id, never ship it silently")
	require.Len(t, result.Content, 2, "the guidance rides a second content block")
	assert.Contains(t, result.Content[1].(mcp.TextContent).Text, "DEPRECATED",
		"the §2-style guidance rides the degraded create result")
	mockClient.AssertExpectations(t)
}

func TestSessionCreate_MintedPathCarriesNoMarker(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("CreateSession", ctx, "ws-1").Return(&SessionResp{
		ID: "amber-falcon-glides",
	}, nil)

	result, err := h.sessionCreate(ctx, makeReq("session_create", map[string]any{"workspace_id": "ws-1"}))
	require.NoError(t, err)
	assert.False(t, result.IsError)

	text := result.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "amber-falcon-glides")
	assert.NotContains(t, text, "session_id_deprecated")
	assert.NotContains(t, text, "DEPRECATED")
	mockClient.AssertExpectations(t)
}

// ===== session_message/session_history: §2 notice block on deprecated inputs =====

func TestSessionMessage_DeprecatedInputCarriesNotice(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	const legacy = "ses_fulllegacyid1234567890ab"
	mockClient.On("SendMessage", ctx, "ws-1", legacy, "hello", 300*time.Second).
		Return("Hello!", nil)

	result, err := h.sessionMessage(ctx, makeReq("session_message", map[string]any{
		"workspace_id": "ws-1",
		"session_id":   legacy,
		"message":      "hello",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	require.Len(t, result.Content, 2,
		"a deprecated-form input carries the notice as a second content block")
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "Hello!")
	notice := result.Content[1].(mcp.TextContent).Text
	assert.Contains(t, notice, "DEPRECATED")
	assert.Contains(t, notice, "phrase")
	mockClient.AssertExpectations(t)
}

func TestSessionMessage_PhraseInputCarriesNoNotice(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("SendMessage", ctx, "ws-1", "amber-falcon-glides", "hello", 300*time.Second).
		Return("Hello!", nil)

	result, err := h.sessionMessage(ctx, makeReq("session_message", map[string]any{
		"workspace_id": "ws-1",
		"session_id":   "amber-falcon-glides",
		"message":      "hello",
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	require.Len(t, result.Content, 1, "a phrase input carries NO notice block")
	mockClient.AssertExpectations(t)
}

func TestSessionHistory_DeprecatedInputCarriesNotice(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	const legacy = "ses_fulllegacyid1234567890ab"
	mockClient.On("GetHistory", ctx, "ws-1", legacy).Return([]Message{}, nil)

	result, err := h.sessionHistory(ctx, makeReq("session_history", map[string]any{
		"workspace_id": "ws-1",
		"session_id":   legacy,
	}))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	require.Len(t, result.Content, 2,
		"a deprecated-form input carries the notice as a second content block")
	assert.Contains(t, result.Content[1].(mcp.TextContent).Text, "DEPRECATED")
	mockClient.AssertExpectations(t)
}

// ===== HTTPClient: phrase targets match the contract stream via the resolved id =====

// TestHTTPClient_SendMessage_PhraseTargetMatchesStreamOnResolvedID pins the
// #1816 resolved-id bridge: the API tells the caller which canonical id the
// phrase mapped to (X-Session-Ref-Resolved on the prompt response), and the
// stream wait matches THAT id — a phrase-addressed send completes from live
// frames instead of burning the whole timeout and falling back to history.
func TestHTTPClient_SendMessage_PhraseTargetMatchesStreamOnResolvedID(t *testing.T) {
	const canonical = "ses_canonical_aaa"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/amber-falcon-glides/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Session-Ref-Resolved", canonical)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/contract-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeContractFrame(t, w, snapshotFrame(1, sessionSnap(canonical, abiv1.SessionStatus_SESSION_STATUS_BUSY)))
		flusher.Flush()
		writeContractFrame(t, w, eventFrame(2, deltaEvent(canonical, "live ")))
		flusher.Flush()
		writeContractFrame(t, w, eventFrame(3, deltaEvent(canonical, "text")))
		flusher.Flush()
		writeContractFrame(t, w, eventFrame(4, statusEvent(canonical, abiv1.SessionStatus_SESSION_STATUS_IDLE)))
		flusher.Flush()
	})
	// The history fallback 404s: with the bridge, it must never be reached.
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/amber-falcon-glides/message", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	start := time.Now()
	resp, err := client.SendMessage(context.Background(), "ws-1", "amber-falcon-glides", "hi", 3*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "live text", resp, "the response must come from live stream frames")
	assert.Less(t, time.Since(start), 2*time.Second,
		"a phrase-targeted send must complete from the stream, not the timeout+fallback")
}

// TestHTTPClient_SendMessage_QuestionResultRewritesSessionID: the pending-input
// question result must carry the CALLER-FACING id (the phrase), never the
// harness id the ABI frame carries (#1816 G3).
func TestHTTPClient_SendMessage_QuestionResultRewritesSessionID(t *testing.T) {
	const canonical = "ses_canonical_bbb"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/amber-falcon-glides/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Session-Ref-Resolved", canonical)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/contract-events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeContractFrame(t, w, snapshotFrame(1, sessionSnap(canonical, abiv1.SessionStatus_SESSION_STATUS_BUSY)))
		flusher.Flush()
		evt := &abiv1.Event{
			Type:      abiv1.EventType_EVENT_TYPE_INPUT_REQUEST,
			SessionId: canonical,
			Input: &abiv1.InputRequest{
				Id:        "que_123",
				SessionId: canonical,
				Kind:      abiv1.InputKind_INPUT_KIND_QUESTION,
				Question:  "Continue?",
			},
		}
		writeContractFrame(t, w, eventFrame(2, evt))
		flusher.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v1/workspaces/ws-1/sessions/amber-falcon-glides/message", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	client, ts := newTestHTTPClient(mux)
	defer ts.Close()

	resp, err := client.SendMessage(context.Background(), "ws-1", "amber-falcon-glides", "hi", 3*time.Second)
	require.NoError(t, err)
	assert.NotContains(t, resp, canonical, "the harness id must not leak into the question result")
	assert.Contains(t, resp, "que_123")
	assert.Contains(t, resp, "amber-falcon-glides",
		"the question result carries the caller-facing phrase")
}

// ===== §2 notice parity on ERROR results (review r1 finding 2) =====

// TestSessionMessage_DeprecatedInputErrorCarriesNotice: a deprecated
// full-id input whose send FAILS still carries the notice — REST rides
// errors too, and agentd covers its error envelope (#1816 r1 parity).
func TestSessionMessage_DeprecatedInputErrorCarriesNotice(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	const legacy = "ses_fulllegacyid1234567890ab"
	mockClient.On("SendMessage", ctx, "ws-1", legacy, "hello", 300*time.Second).
		Return("", fmt.Errorf("API error 503: unavailable"))

	result, err := h.sessionMessage(ctx, makeReq("session_message", map[string]any{
		"workspace_id": "ws-1",
		"session_id":   legacy,
		"message":      "hello",
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError, "the send error surfaces as a tool error")
	require.Len(t, result.Content, 2, "the notice rides the ERROR result too")
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "failed to send message")
	assert.Contains(t, result.Content[1].(mcp.TextContent).Text, "DEPRECATED")
	mockClient.AssertExpectations(t)
}

func TestSessionHistory_DeprecatedInputErrorCarriesNotice(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	const legacy = "ses_fulllegacyid1234567890ab"
	mockClient.On("GetHistory", ctx, "ws-1", legacy).
		Return(nil, fmt.Errorf("API error 500: boom"))

	result, err := h.sessionHistory(ctx, makeReq("session_history", map[string]any{
		"workspace_id": "ws-1",
		"session_id":   legacy,
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError)
	require.Len(t, result.Content, 2, "the notice rides the ERROR result too")
	assert.Contains(t, result.Content[1].(mcp.TextContent).Text, "DEPRECATED")
	mockClient.AssertExpectations(t)
}

// TestSessionRefResolvedHeaderConstantShared pins the seam the bridge
// joins on: the middleware (api) and the platform MCP client (pkg/mcp)
// MUST agree on the exact header literal — a rename on either side
// silently re-breaks phrase sends. The constant lives in pkg/types so
// both sides import ONE definition (review r1 finding 1).
func TestSessionRefResolvedHeaderConstantShared(t *testing.T) {
	assert.Equal(t, "X-Session-Ref-Resolved", types.HeaderSessionRefResolved)
}

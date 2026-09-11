// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #828 batch 1: CreateSession, ListSessions, and SendMessage are
// adapter-only. With the adapter unset they must refuse with a typed
// 503 — never fall back to the legacy dialect proxy. Every guard row
// wires a WORKING legacy backend (newTestEnv redirects the proxy
// transport there), so a regression to passthrough surfaces as a 200,
// not a transport-error 503 that coincidentally passes.

func TestCreateSession_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: legacy env, adapter unset")

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions", nil)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestListSessions_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: legacy env, adapter unset")

	w := env.doRequestWithT(t, http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestSendMessage_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: legacy env, adapter unset")

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`)
	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions/s1/message", body)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

// Shared input validation runs before the adapter guard: a malformed
// request is a 400 regardless of adapter wiring — the guard is a
// configuration failure, not an input failure, and must not mask the
// more specific client error.
func TestSendMessage_NilAdapter_ValidationPrecedesGuard(t *testing.T) {
	env := newTestEnv(t)
	require.Nil(t, env.handler.adapter, "precondition: adapter unset")

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions/bad!id/message", strings.NewReader(`{}`))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid sessionId")
}

// Port of TestProxy_CreateSessionBypassesLimit onto the adapter path:
// CreateSession must not enforce the active-session ceiling. The
// ceiling guards concurrent SENDS (checkAdapterSessionLimit in
// SendMessage); session creation itself is unbounded because a created
// session is inert until a send arrives.
func TestCreateSession_AdapterPath_BypassesActiveSessionLimit(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 1)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.adapter = &mockAdapter{
		createSessionFn: func(_ context.Context, _, _ string, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_new"}, nil
		},
	}
	env.handler.SetActiveSessionsForTest("ws-1", []string{"s-at-ceiling"})

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions", nil)

	assert.Equal(t, http.StatusOK, w.Code, "create session should bypass limit")
	assert.Contains(t, w.Body.String(), "ses_new")
	assert.Equal(t, 1, env.handler.activeSessionCount(context.Background(), "ws-1"),
		"CreateSession must not add to the active-session set")
}

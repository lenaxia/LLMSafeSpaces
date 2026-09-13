// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #828 batch 1: CreateSession, ListSessions, and SendMessage are
// adapter-only. With the adapter unset they must refuse with a typed
// 503 — never fall back to the legacy dialect proxy. Every guard row
// wires a WORKING legacy backend (newTestEnv redirects the proxy
// transport there), so a regression to passthrough surfaces as a 200,
// not a transport-error 503 that coincidentally passes.

// TestCreateSession_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestListSessions_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestSendMessage_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestSendMessage_NilAdapter_ValidationPrecedesGuard was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): superseded — validation-first ordering is inherent; bad input 400s before any adapter call with the ctor-required adapter.

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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1882 Bug 2 pins, proxy half. PropagateSessionDeleted is THE
// convergence seam: the engine's authorized deletes and
// agentd-reported ephemeral teardowns ride the exact teardown the
// user-route delete performs — tombstone FIRST (a late pod event
// racing the row removal must find the tombstone set, or the
// phrase-bearing upsert re-births the row, #1881), then the
// session_index row removal, then the session.status:deleted
// broadcast on workspace + user streams.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPropagateSessionDeleted_Tombstone_IndexRow_SSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newProxyHandlerForAdapterTest(t)
	idx := newMockSessionIndex()
	h.sessionIndex = idx
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	sub, err := h.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer h.userBroker.UnsubscribeWorkspace("ws-1", sub)

	h.PropagateSessionDeleted(context.Background(), "ws-1", "ses_ghost")

	// Effect 1: tombstone (late-event suppression).
	assert.True(t, h.state().IsSessionDeleted(context.Background(), "ws-1", "ses_ghost"),
		"the tombstone must be set — a late pod event would otherwise re-birth the row")

	// Effect 2: session_index row removal (the sidebar's only source).
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"],
		"the session_index row must be removed")

	// Effect 3: session.status:deleted on the workspace stream (the
	// sidebar's cache-drop handler).
	evt := recvTypeWithTimeout(t, sub, "session.status")
	assert.Equal(t, "deleted", evt.Status, "the deleted status event clears the frontend sidebar")
}

// TestPropagateSessionDeleted_ConvergenceWithUserRoute: HardDeleteSession
// and the propagator must produce the SAME teardown effects — the user
// route's pins (tombstone + row + SSE) hold through the convergence.
func TestPropagateSessionDeleted_ConvergenceWithUserRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newProxyHandlerForAdapterTest(t)
	idx := newMockSessionIndex()
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	sub, err := h.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer h.userBroker.UnsubscribeWorkspace("ws-1", sub)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_u1"}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	h.DeleteSession(c)
	c.Writer.WriteHeaderNow()

	require.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, h.state().IsSessionDeleted(context.Background(), "ws-1", "ses_u1"))
	assert.True(t, idx.deletedTree["ws-1/ses_u1"], "the user route still removes the row")
	evt := recvTypeWithTimeout(t, sub, "session.status")
	assert.Equal(t, "deleted", evt.Status)
}

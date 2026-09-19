// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1340: the session_gone surface. A harness-side 404 on GetSession or
// GetHistory means the session no longer exists at the agent — the API's
// session_index row is a stale ghost. The handler classifies the
// adapter's agent.ErrSessionNotFound, reaps the index row (+tree), and
// returns the typed 410 the frontend renders as a gone-state (never a
// generic fetch-failure banner).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func goneAdapterErr() error {
	return fmt.Errorf("%w: GET /session/ses_ghost returned 404: {\"error\":\"not found\"}", agent.ErrSessionNotFound)
}

func TestGetSession_Harness404_Returns410AndReapsIndexRow(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_ghost", 0) // count 0: guard admits
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, goneAdapterErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_ghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
	assert.NotEqual(t, "session_gone", body.Error, "error carries a human message, not a duplicated code")
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "the stale index row must be reaped")
}

func TestGetSession_OtherAdapterError_Still502_NoReap(t *testing.T) {
	idx := newMockSessionIndex()
	require.NoError(t, idx.UpsertTitle(context.Background(), "ws-1", "ses_1", "Alive"))
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	assert.Equal(t, http.StatusBadGateway, w.Code, "non-not-found adapter errors keep the generic 502")
	assert.Empty(t, idx.deletedTree, "transient errors must never reap index rows")
}

func TestGetHistory_Harness404_Returns410AndReapsIndexRow(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_ghost", 0) // count 0: guard admits
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return nil, goneAdapterErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_ghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/?limit=50", nil)
	h.GetHistory(c)

	require.Equal(t, http.StatusGone, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"])
}

func TestGetHistory_OtherAdapterError_Still502_NoReap(t *testing.T) {
	idx := newMockSessionIndex()
	require.NoError(t, idx.UpsertTitle(context.Background(), "ws-1", "ses_1", "Alive"))
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/?limit=50", nil)
	h.GetHistory(c)

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Empty(t, idx.deletedTree)
}

// The triage guard on the read path: a history-bearing row answers the
// typed 410 (the session is unreadable either way) but the row is NOT
// deleted — a vanished session with history may indicate a harness
// store reset; operator review, never silent destruction.
func TestGetSession_Harness404_HistoryBearingRowKeptButStill410(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_vanished", 12)
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, goneAdapterErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_vanished"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code, "the gone-state answers regardless of the guard")
	assert.Empty(t, idx.deletedTree, "message_count>0 rows are never auto-deleted on the read path")
}

// Real classification chain through the mounted routes: the fake
// opencode backend answers 404, the REAL adapter classifies
// agent.ErrSessionNotFound, the handler reaps the guard-admitted row
// and answers the typed 410 — no synthetic errors injected.
func TestE2E_SessionGone_RealAdapterClassifies(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Session not found"}`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_ghost", 0)
	env.handler.SetSessionIndex(idx)

	rec := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_ghost", nil)
	require.Equal(t, http.StatusGone, rec.Code)
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "the real chain reaps the guard-admitted row")
}

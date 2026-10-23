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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
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

	// r2: the human message must be TRUE for the guard outcome — the
	// kept row is still listed; saying "removed from your session list"
	// here was a user-visible untruth.
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
	assert.Contains(t, body.Error, "remains in your session list")
	assert.NotContains(t, body.Error, "has been removed", "the kept case must not claim removal")
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

// r3: the 410's human message must be TRUE on EVERY leg — removal is
// claimed only when a reap was CONFIRMED. The uncertain legs
// (delete-failed, lookup-error, nil-index) say "may take a short
// while" instead.
func goneSessionRequest() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_ghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, w
}

func TestGetSession_Harness404_DeleteFailed_UncertainMessage(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_ghost", 0)
	idx.failDelete = true
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
		return nil, goneAdapterErr()
	}}

	c, w := goneSessionRequest()
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "short while", "an unconfirmed reap must not claim removal")
	assert.NotContains(t, body.Error, "has been removed")
}

func TestGetSession_Harness404_LookupError_UncertainMessage(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_ghost", 0)
	idx.failList = true
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
		return nil, goneAdapterErr()
	}}

	c, w := goneSessionRequest()
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "short while")
	assert.NotContains(t, body.Error, "has been removed")
}

func TestGetSession_Harness404_NilIndex_UncertainMessage(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
		return nil, goneAdapterErr()
	}}

	c, w := goneSessionRequest()
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "short while")
	assert.NotContains(t, body.Error, "has been removed")
}

// #1756 post-union finding 4 pin: the reap verdict must match on
// InternalID, not the post-mint row ID (the phrase). A row minted to
// amber-falcon-glides with harness id ses_ghost must still reap when
// the canonicalized path id is ses_ghost — the r1-era code compared
// row.ID, so minted rows never matched and every reap reported
// "uncertain".
func TestGetSession_Harness404_ReapMatchesInternalIDNotPhrase(t *testing.T) {
	idx := newMockSessionIndex()
	idx.mu.Lock()
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "amber-falcon-glides", InternalID: "ses_ghost", MessageCount: 0,
	})
	idx.mu.Unlock()

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
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"],
		"reap must match the row by InternalID even when its ID is the phrase")
}

// #1850 read-path grace pin (review r1's blocking finding): a FRESH
// count-zero row — the create_session spawn window; CreatedAt feeds
// the reap guard through ListByWorkspace → ListSessionIndex — meets a
// harness 404 and must answer the typed 410 with the KEPT body while
// the row SURVIVES. The 404-on-a-minutes-old-session is list/store
// lag exactly often enough that deleting the row orphans the phrase
// minted at creation (the five reproductions' mechanism). This pins
// the composition in reapSessionIfGone: the guard sees the row's
// CreatedAt and refuses — a future row source that drops the feed
// goes red here while every unit-tier guard test stays green.
func TestGetSession_Harness404_FreshCountZeroRow_KeptBySpawnGrace(t *testing.T) {
	idx := newMockSessionIndex()
	fresh := time.Now()
	idx.mu.Lock()
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "amber-trout-glides", InternalID: "ses_newworker", MessageCount: 0, CreatedAt: &fresh,
	})
	idx.mu.Unlock()

	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, goneAdapterErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_newworker"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code, "the gone-state answers regardless of the guard")
	assert.Empty(t, idx.deletedTree, "a spawn-window row is never reaped on the read path — its phrase must survive")

	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
	assert.Contains(t, body.Error, "remains in your session list",
		"the kept body must be truthful — the row is still listed")
	assert.NotContains(t, body.Error, "has been removed")
}

// The aged twin: the same count-zero row past the grace window IS
// reaped on the read path (ghost hygiene converges; the grace is a
// deferral, not a pardon).
func TestGetSession_Harness404_AgedCountZeroRow_StillReaped(t *testing.T) {
	idx := newMockSessionIndex()
	aged := time.Now().Add(-16 * time.Minute)
	idx.mu.Lock()
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "fallow-mist-rises", InternalID: "ses_oldghost", MessageCount: 0, CreatedAt: &aged,
	})
	idx.mu.Unlock()

	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, goneAdapterErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_oldghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	assert.True(t, idx.deletedTree["ws-1/ses_oldghost"],
		"an aged count-zero row keeps the #1340 reap verdict")
}

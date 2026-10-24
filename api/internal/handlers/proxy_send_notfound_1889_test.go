// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1889 F2 — a send addressed to a session that does not exist (an
// orphaned phrase whose row died, a deleted session, a typo'd id) must
// surface the TYPED not-found verdict with the §4.6 did-you-mean
// payload — never a raw 5xx "failed to send message". The read paths
// (GetSession/GetHistory) already classify the adapter's definitive
// not-found via reapSessionIfGone; the send paths wrap EVERY failure in
// a bare 502, which is exactly the "degradation marker becomes a hard
// 500" class #1850's expectation section forbids.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// goneSendProbeErr is what probeSessionAliveByID returns for a
// definitively absent session (the READ route's 404 — the one verdict
// the house design rules definitive).
func goneSendProbeErr() error {
	return fmt.Errorf("%w: GET /session/ses_gone returned 404: {\"error\":\"not found\"}", agent.ErrSessionNotFound)
}

// seedRowPhrase1889 seeds an index row carrying a MINTED phrase (the
// #1889 fixtures' phrase-bearing-row shape).
func (m *mockSessionIndex) seedRowPhrase(workspaceID, sessionID, phrase string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[workspaceID] = append(m.rows[workspaceID], types.SessionListItem{
		ID:      sessionID,
		InternalID: sessionID,
		Title:      sessionID,
		ShortID:    phrase,
		MessageCount: count,
	})
}

// phraseResolver satisfies sessionindex.RefResolver over the mock
// index's seeded rows (phrase-exact only — enough for the §4.6 payload
// path).
type phraseResolver struct{ idx *mockSessionIndex }

func (r *phraseResolver) FindSessionByShortID(_ context.Context, workspaceID, shortID string) (string, error) {
	rows, err := r.idx.ListByWorkspace(context.Background(), workspaceID)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if strings.EqualFold(row.ShortID, shortID) {
			return row.InternalID, nil
		}
	}
	return "", nil
}

func (r *phraseResolver) FindSessionsByAlias(_ context.Context, _ string, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}

func (r *phraseResolver) ListSessionIndex(ctx context.Context, workspaceID string) ([]types.SessionListItem, error) {
	return r.idx.ListByWorkspace(ctx, workspaceID)
}

func (r *phraseResolver) ListWorkspaceIDsByOwner(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (r *phraseResolver) FindRefCandidatesAcrossWorkspaces(_ context.Context, _ []string, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}

var _ sessionindex.RefResolver = (*phraseResolver)(nil)

// TestSendMessage_TargetGone_ReturnsTyped410WithSuggestions: the send
// fails AND the by-ID probe confirms the session does not exist → the
// caller gets the typed session_gone body (410) with did-you-mean
// suggestions, and the ghost row is reaped — consistent with the read
// paths' surface for the same verdict.
func TestSendMessage_TargetGone_ReturnsTyped410WithSuggestions(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowPhrase("ws-1", "ses_gone", "amber-falcon-glides", 0) // count 0: the guard admits the reap
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.sessionRefs = &phraseResolver{idx: idx}
	// A live neighbor so the did-you-mean payload has a candidate.
	idx.seedRowPhrase("ws-1", "ses_live", "linen-orchard-chirps", 2)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("agent.ErrHTTPStatus: POST /session/ses_gone/message returned 500: {\"name\":\"UnknownError\"}")
		},
		getSessionFn: func(_ context.Context, _, _, sid string) (*session.Session, error) {
			assert.Equal(t, "ses_gone", sid, "the not-found probe must be by-ID")
			return nil, goneSendProbeErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_gone"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)

	require.Equal(t, http.StatusGone, w.Code,
		"a definitively-absent send target answers the typed gone surface, not a raw 502")
	var body struct {
		Code        string `json:"code"`
		Error       string `json:"error"`
		Suggestions []any  `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
	assert.NotEmpty(t, body.Error)
	require.NotEmpty(t, body.Suggestions, "the §4.6 did-you-mean payload must ride the typed verdict")
	assert.True(t, idx.deletedTree["ws-1/ses_gone"], "the ghost row is reaped like the read paths reap it")
}

// TestSendMessage_PhraseTargetGone_ReturnsTyped410: the #1889 scenario
// verbatim — the send is addressed to a PHRASE whose row died (the
// middleware passes the unresolved id-shaped ref through) and the
// probe's definitive not-found answers the typed surface.
func TestSendMessage_PhraseTargetGone_ReturnsTyped410(t *testing.T) {
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.sessionRefs = &phraseResolver{idx: idx}
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, sid string, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("agent.ErrHTTPStatus: POST /session/%s/message returned 500: {\"name\":\"UnknownError\"}", sid)
		},
		getSessionFn: func(_ context.Context, _, _, sid string) (*session.Session, error) {
			return nil, goneSendProbeErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "amber-falcon-glides"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)

	require.Equal(t, http.StatusGone, w.Code,
		"an orphaned-phrase send must answer the typed not-found, never a raw 5xx (#1889 F2)")
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
}

// TestSendMessage_TargetAliveKeepsRaw502: a send failure on a session
// that IS alive (busy turn, transport hiccup mid-turn) keeps the
// existing 502 — the probe vetoes the not-found surface, exactly like
// the reconcile's reap_vetoed_live.
func TestSendMessage_TargetAliveKeepsRaw502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("context deadline exceeded mid-turn")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_live"}, nil // alive by ID
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_live"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code, "a live target's send failure keeps the generic 502")
}

// TestSendMessage_ProbeIndeterminateKeepsRaw502: a probe that cannot
// reach a verdict (transport error) must not manufacture a not-found —
// uncertainty never answers the gone surface.
func TestSendMessage_ProbeIndeterminateKeepsRaw502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_x"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code, "an indeterminate probe keeps the generic 502")
}

// TestSyncSend_TargetGone_ReturnsTyped410: the no-outbox fallback send
// (SendPromptAsync → syncSend) answers the same typed surface.
func TestSyncSend_TargetGone_ReturnsTyped410(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowPhrase("ws-1", "ses_gone", "amber-falcon-glides", 0)
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.sessionRefs = &phraseResolver{idx: idx}
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("agent.ErrHTTPStatus: POST /session/ses_gone/message returned 404")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, goneSendProbeErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_gone"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	h.syncSend(c, "ws-1", "ses_gone", "hello", nil)

	require.Equal(t, http.StatusGone, w.Code, "the fallback sync send answers the typed surface too")
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_gone", body.Code)
}

// TestSyncSend_TargetAliveKeepsRaw502: same veto discipline on the
// fallback path.
func TestSyncSend_TargetAliveKeepsRaw502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("context deadline exceeded mid-turn")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_live"}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_live"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	h.syncSend(c, "ws-1", "ses_live", "hello", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

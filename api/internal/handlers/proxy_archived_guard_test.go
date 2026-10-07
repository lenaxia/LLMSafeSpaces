// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #1627 read-only enforcement at the API proxy layer. Archived
// sessions reject CHAT SENDS on every send surface (/message sync,
// /prompt outbox-accepted, /queue outbox-accepted, and the sync
// fallback) with a typed 409; reads (history) stay open; a failed
// archive check fails OPEN (UX guard, not a security boundary — the
// in-pod peer path bypasses the proxy by the owner's ruling).

func requireArchivedBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "session_archived", body.Code)
	assert.NotEmpty(t, body.Error, "the rejection must name the recovery (unarchive)")
}

func TestSendMessage_ArchivedSession_Rejected409(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si
	adapterCalled := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			adapterCalled = true
			return &session.Message{ID: "must-not-reach"}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)
	requireArchivedBody(t, w)
	assert.False(t, adapterCalled, "the rejection must fire before any adapter call")
}

func TestSendMessage_ArchiveCheckFails_SendsProceed(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newMockSessionIndex()
	si.failArchived = true
	h.sessionIndex = si
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return &session.Message{ID: "msg_1"}, nil
		},
		// The post-send title fetch is detached; it must not panic the
		// test binary when the send succeeds.
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, assert.AnError
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)
	require.Equal(t, http.StatusOK, w.Code, "fail-open on infra error: body %s", w.Body.String())
}

func TestSendMessage_UnarchivedSession_SendsNormally(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", false))
	h.sessionIndex = si
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return &session.Message{ID: "msg_1"}, nil
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, assert.AnError
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))

	h.SendMessage(c)
	require.Equal(t, http.StatusOK, w.Code, "unarchive restores chat instantly: body %s", w.Body.String())
}

func TestPrompt_OutboxPath_ArchivedSession_Rejected409(t *testing.T) {
	env := newOutboxTestEnv(t)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si

	w := postPrompt(t, env, `{"clientMessageID":"cm-arch","parts":[{"type":"text","text":"hello"}]}`)
	requireArchivedBody(t, w)
	assert.Equal(t, 0, len(listOutbox(t, env)), "nothing may be accepted into the outbox for an archived session")
}

func TestQueue_OutboxPath_ArchivedSession_Rejected409(t *testing.T) {
	env := newOutboxTestEnv(t)
	env.router.POST("/api/v1/workspaces/:id/sessions/:sessionId/queue", env.handler.EnqueueMessage)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si

	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue", strings.NewReader(`{"text":"hello"}`))
	requireArchivedBody(t, w)
	assert.Equal(t, 0, len(listOutbox(t, env)))
}

func TestPrompt_SyncFallback_ArchivedSession_Rejected409(t *testing.T) {
	// No outbox wired — SendPromptAsync degrades to syncSend, which
	// must enforce the same guard.
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	})
	// The fixture router already registers /prompt (no outbox wired on
	// this env — SendPromptAsync degrades to syncSend).
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt", strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))
	requireArchivedBody(t, w)
}

func TestGetHistory_ArchivedSession_StillViewable(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si
	h.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{{ID: "m1"}}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h.GetHistory(c)
	require.Equal(t, http.StatusOK, w.Code, "history remains viewable on archived sessions: %s", w.Body.String())
}

// #1627 ruling (b): an entry accepted BEFORE archive must not deliver
// AFTER it — the worker refuses with the terminal archived error (no
// retry; unarchive + new send is the recovery).
func TestOutboxDeliver_ArchivedSession_TerminalRefusal(t *testing.T) {
	var hits int32
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si

	err := env.handler.outboxDeliver(context.Background(), "ws-1", "ses_1", outbox.Entry{Text: "late message"})
	require.Error(t, err)
	var terminal *outbox.TerminalDeliveryError
	require.ErrorAs(t, err, &terminal, "archived refusal must be terminal (never retried): %v", err)
	assert.Contains(t, err.Error(), "session_archived")
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "no bytes may reach the pod for an archived session")
}

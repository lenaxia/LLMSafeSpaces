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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
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

// --- review round 1+2 findings: the remaining write surfaces ---

// Finding 1: the terminus regime gates EVERY entry — prior-attempt
// entries must refuse BEFORE the ledger path re-POSTs (outbox_terminus
// re-admits when the prior row is failed/not-found).
func TestOutboxDeliver_TerminusMode_PriorAttemptArchived_Refused(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.SetAgentdTerminus(true)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si

	err := h.outboxDeliver(context.Background(), "ws-1", "ses_1", outbox.Entry{
		Text:     "prior attempt existed",
		Attempts: 1,
	})
	require.Error(t, err)
	var terminal *outbox.TerminalDeliveryError
	require.ErrorAs(t, err, &terminal, "the terminus regime must refuse archived sessions for prior-attempt entries too: %v", err)
}

func TestOutboxDeliver_TerminusMode_FreshEntryArchived_Refused(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.SetAgentdTerminus(true)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si

	err := h.outboxDeliver(context.Background(), "ws-1", "ses_1", outbox.Entry{Text: "fresh"})
	var terminal *outbox.TerminalDeliveryError
	require.ErrorAs(t, err, &terminal)
}

// Finding 4: retry-while-archived must 409, not re-arm a dead letter.
func TestRetryQueueMessage_ArchivedSession_Rejected409(t *testing.T) {
	env := newOutboxTestEnv(t)
	env.router.POST("/api/v1/workspaces/:id/sessions/:sessionId/queue/:messageId/retry", env.handler.RetryQueueMessage)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si

	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue/msg-1/retry", nil)
	requireArchivedBody(t, w)
}

// Finding 2: the reply surfaces are chat writes — live question replies
// into an archived session must 409.
func TestQuestionReply_LiveAsk_ArchivedSession_Rejected409(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si
	answered := false
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{ID: "que_live", SessionID: "ses_1"}}, nil
		},
		answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error {
			answered = true
			return nil
		},
	}

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/question/que_live/reply",
		strings.NewReader(`{"answers":[["yes"]]}`))
	requireArchivedBody(t, w)
	assert.False(t, answered, "no reply may reach the agent for an archived session")
}

// Finding 2: the late-answer path must refuse BEFORE accepting and
// BEFORE resolving the inbox record — the user learns the truth, the
// ask stays pending, and unarchive-then-retry works.
func TestInbox_LateAnswer_ArchivedSession_RejectedAndRecordKept(t *testing.T) {
	h, in, ob, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil // ask is dead — late-answer path
		},
	}
	rec := inbox.Record{
		ID: "que_dead", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Deploy?", Options: []inbox.Option{{Label: "Yes"}}, RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "requestID", Value: "que_dead"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"answers":[["Yes"]]}`))
	h.QuestionReply(c)

	require.Equal(t, http.StatusConflict, w.Code, "the late answer must 409, not 202")
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "session_archived", body.Code)

	entries, err := ob.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be accepted into the outbox")
	pending, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the ask must stay pending — the record is NOT resolved on refusal")
}

// Review round 1 finding 3: the USER-stream leg of the archive
// announcement — the #786 cross-tab copy — pinned with the workspace
// owner recorded so PublishToUser actually fires.
func TestPublishSessionArchived_UserStreamCarriesWorkspaceID(t *testing.T) {
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-7")

	wsub, err := env.handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", wsub)
	usub, err := env.handler.userBroker.SubscribeUser("user-7")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeUser("user-7", usub)

	env.handler.PublishSessionArchived("ws-1", "s1", true)

	select {
	case evt := <-wsub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "archived", evt.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("workspace-stream copy missing")
	}
	select {
	case evt := <-usub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "archived", evt.Status)
		assert.Equal(t, "ws-1", evt.WorkspaceID, "the user-stream copy carries WorkspaceID for routing (#786)")
	case <-time.After(2 * time.Second):
		t.Fatal("user-stream copy missing — every other tab depends on it")
	}
}

// --- review r-next: the four missing pins + disposition tests ---

// Missing test 1: PermissionReply's gate (changed line, zero coverage).
func TestPermissionReply_LiveAsk_ArchivedSession_Rejected409(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si
	replied := false
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{ID: "per_live", SessionID: "ses_1"}}, nil
		},
		replyPermissionFn: func(_ context.Context, _, _, _, _, _ string) error {
			replied = true
			return nil
		},
	}

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/permission/per_live/reply",
		strings.NewReader(`{"reply":"once"}`))
	requireArchivedBody(t, w)
	assert.False(t, replied)
}

// Missing test 2: autoApprovePermission skips archived sessions; the
// fail-open leg (IsArchived error) lets the bridge proceed.
func TestAutoApprovePermission_ArchivedSkips_FailOpenProceeds(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	approved := func() *mockAdapter {
		return &mockAdapter{
			listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
				return []session.InputRequest{{ID: "per_1", SessionID: "ses_1"}}, nil
			},
			replyPermissionFn: func(_ context.Context, _, _, _, _, _ string) error { return nil },
		}
	}

	t.Run("archived skips the write", func(t *testing.T) {
		called := false
		env.handler.sessionIndex = func() *mockSessionIndex {
			si := newMockSessionIndex()
			require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
			return si
		}()
		adapter := approved()
		adapter.replyPermissionFn = func(_ context.Context, _, _, _, _, _ string) error {
			called = true
			return nil
		}
		env.handler.adapter = adapter
		env.handler.autoApprovePermission("ws-1", "per_1")
		assert.False(t, called, "the bridge must not write into an archived session")
	})

	t.Run("check error fails open", func(t *testing.T) {
		si := newMockSessionIndex()
		si.failArchived = true
		env.handler.sessionIndex = si
		called := false
		adapter := approved()
		adapter.replyPermissionFn = func(_ context.Context, _, _, _, _, _ string) error {
			called = true
			return nil
		}
		env.handler.adapter = adapter
		env.handler.autoApprovePermission("ws-1", "per_1")
		assert.True(t, called, "an infra failure must not wedge the bridge (fail-open, same posture as the gates)")
	})
}

// Missing test 3: the non-terminus prior-attempt refusal — the branch
// the reviewer proved unpinned by mutation. The sendFn must never fire.
func TestOutboxDeliver_NonTerminus_PriorAttemptArchived_Refused(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si
	sendCalled := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			sendCalled = true
			return &session.Message{ID: "m"}, nil
		},
	}

	err := h.outboxDeliver(context.Background(), "ws-1", "ses_1", outbox.Entry{
		Text:     "prior attempt existed",
		Attempts: 1,
	})
	require.Error(t, err)
	var terminal *outbox.TerminalDeliveryError
	require.ErrorAs(t, err, &terminal, "not-delivered prior attempts must terminally refuse, not re-send: %v", err)
	assert.False(t, sendCalled)
}

// Missing test 4 (finding A disposition): reject/dismiss are GATED on
// the live arm like their PermissionReply sibling.
func TestQuestionReject_LiveAsk_ArchivedSession_Rejected409(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si
	rejected := false
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{ID: "que_live", SessionID: "ses_1"}}, nil
		},
		rejectInputFn: func(_ context.Context, _, _, _ string) error {
			rejected = true
			return nil
		},
	}

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/question/que_live/reject", nil)
	requireArchivedBody(t, w)
	assert.False(t, rejected, "the live reject must not reach the agent for an archived session")
}

// Finding B disposition pins: abort + rename stay OPEN on archived
// sessions (lifecycle/content-neutral carve-out, documented in
// proxy_archived.go) — pinned so the carve-out is contract, not drift.
func TestLifecycleCarveOuts_ArchivedSession_AbortAndRenameStayOpen(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.sessionIndex = si
	env.handler.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error { return nil },
		renameSessionFn: func(_ context.Context, _, _, _, _ string) error {
			return nil
		},
	}

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/abort", nil)
	require.Equal(t, http.StatusNoContent, w.Code, "abort is the documented lifecycle carve-out (peer turns may run): %s", w.Body.String())

	err := env.handler.RenameSessionInAgent(context.Background(), "ws-1", "ses_1", "new title")
	require.NoError(t, err, "rename is cosmetic metadata — the documented carve-out")
}

func TestDismissInboxRecord_LiveAsk_ArchivedSession_Rejected409(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si
	rejected := false
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			// The ask is LIVE — the dismiss would drive the harness reject.
			return []session.InputRequest{{ID: "que_live", SessionID: "ses_1"}}, nil
		},
		rejectInputFn: func(_ context.Context, _, _, _ string) error {
			rejected = true
			return nil
		},
	}
	rec := inbox.Record{
		ID: "que_live", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Deploy?", Options: []inbox.Option{{Label: "Yes"}}, RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
		{Key: "requestID", Value: "que_live"},
	}
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	h.DismissInboxRecord(c)

	requireArchivedBody(t, w)
	assert.False(t, rejected)
	pending, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the record must stay pending — the dismiss did not terminalize it")
}

// --- review round 4: SessionAction gate + DeleteSession disposition ---

// Finding 1: the typed-action union carries the same mutating
// answer/reject vocabulary the REST reply surfaces gate — archived
// sessions must not take actions.
func TestSessionAction_ArchivedSession_Rejected409_NoActCall(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.SetAgentdTerminus(true)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	h.sessionIndex = si

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"answerQuestion":{"inputId":"que_1","reply":"Yes"}}`))

	h.SessionAction(c)
	// requireArchivedBody distinguishes the gate's typed body from the
	// unresolved-endpoint 409 (which carries a nested error object, no
	// top-level code) — the pin stays mutation-honest.
	requireArchivedBody(t, w)
}

// Finding 2 disposition: delete is the documented lifecycle carve-out
// (explicit destruction, nothing written INTO the conversation) —
// pinned so the carve-out is contract, not drift.
func TestDeleteSession_ArchivedSession_CarveOutStillDeletes(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	si := newMockSessionIndex()
	require.NoError(t, si.SetArchived(context.Background(), "ws-1", "ses_1", true))
	env.handler.SetSessionIndex(si)
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, http.MethodDelete, "/api/v1/workspaces/ws-1/sessions/ses_1", nil)
	require.Equal(t, http.StatusNoContent, w.Code, "delete is the documented carve-out (explicit destruction, no conversation write): %s", w.Body.String())
}

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

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// epic-71 / 3a (#1313): unanswered-question inbox wiring — record at ask
// time, snapshot union (live ∪ inbox, whileAway-tagged), late answers
// through the outbox, dismiss as the second exit (S11).

func newInboxBackend(t *testing.T) (*ProxyHandler, *inbox.Service, *outbox.Service, *miniredis.Miniredis) {
	t.Helper()
	h := newProxyHandlerForAdapterTest(t)
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	in := inbox.New(client)
	ob := outbox.New(client)
	h.SetInboxStoreForTest(in)
	h.SetOutboxForTest(ob)
	return h, in, ob, mr
}

func inboxAbiQuestion(id, ses string) *abiv1.InputRequest {
	return &abiv1.InputRequest{
		Id:        id,
		SessionId: ses,
		Kind:      abiv1.InputKind_INPUT_KIND_QUESTION,
		Question:  "Deploy the widget?",
		Header:    "Deploy",
		Options: []*abiv1.InputOption{
			{Label: "Yes", Description: "ship it"},
			{Label: "No", Description: "hold"},
		},
		Custom: true,
		Tool:   &abiv1.ToolRef{MessageId: "msg_1", CallId: "call_1"},
	}
}

func inboxAbiPermission(id, ses string) *abiv1.InputRequest {
	return &abiv1.InputRequest{
		Id:         id,
		SessionId:  ses,
		Kind:       abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: "bash",
		Patterns:   []string{"rm -rf *"},
		Always:     []string{"rm *"},
		Tool:       &abiv1.ToolRef{MessageId: "msg_2", CallId: "call_2"},
	}
}

func TestInbox_InputRequested_RecordsQuestion(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	(&usageBridge{h: h}).InputRequested("ws-1", inboxAbiQuestion("que_1", "ses_1"))

	got, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "que_1", got[0].ID)
	assert.Equal(t, inbox.KindQuestion, got[0].Kind)
	assert.Equal(t, "Deploy the widget?", got[0].Question)
	assert.Equal(t, "Deploy", got[0].Header)
	require.Len(t, got[0].Options, 2)
	assert.Equal(t, "Yes", got[0].Options[0].Label)
	assert.True(t, got[0].Custom)
	require.NotNil(t, got[0].Tool)
	assert.Equal(t, "msg_1", got[0].Tool.MessageID)
	assert.Equal(t, inbox.StatusPending, got[0].Status)
}

func TestInbox_InputRequested_RecordsPermission(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	(&usageBridge{h: h}).InputRequested("ws-1", inboxAbiPermission("per_1", "ses_1"))

	got, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, inbox.KindPermission, got[0].Kind)
	assert.Equal(t, "bash", got[0].Permission)
	assert.Equal(t, []string{"rm -rf *"}, got[0].Patterns)
}

func TestInbox_InputRequested_AutoApproveDoesNotRecord(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{AutoApprovePermissions: true})
	h.adapter = &mockAdapter{
		resolveFn: func(_ context.Context, _, _, _, _ string) error { return nil },
	}

	(&usageBridge{h: h}).InputRequested("ws-1", inboxAbiPermission("per_1", "ses_1"))

	got, err := in.ListWorkspace(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, got, "auto-approved permissions never surface; they must not be recorded")
}

func TestInbox_InputResolved_KeepsRecordPending(t *testing.T) {
	// INPUT_RESOLVED means "the live ask is gone" — answered, timed out,
	// dropped, or lease-resolved (#1310). Only the reply routes know the
	// user actually answered. Resolution alone must never clear the
	// record: the walk-away scenario depends on a timed-out ask STILL
	// re-presenting as whileAway.
	h, in, _, _ := newInboxBackend(t)
	(&usageBridge{h: h}).InputRequested("ws-1", inboxAbiQuestion("que_1", "ses_1"))

	(&usageBridge{h: h}).InputResolved("ws-1", "ses_1", "que_1")

	got, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, got, 1, "resolution event alone must keep the record pending")
}

func TestInbox_EmitPending_UnionLiveAndInbox(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_live", SessionID: "ses_1", Kind: session.InputQuestion,
				Question: "Live?", Options: []session.InputOption{{Label: "A", Description: "a"}},
			}}, nil
		},
	}
	// Inbox-only record: its live ask is gone (the walk-away case).
	stale := inbox.Record{
		ID: "que_stale", SessionID: "ses_2", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "While away?", RecordedAt: time.Now().UTC().Add(-time.Minute),
		Options: []inbox.Option{{Label: "Sure", Description: ""}},
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", stale))

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	var live, away bool
	for i := 0; i < 2; i++ {
		evt := recvWithTimeout(t, userSub, "agent.question")
		req, ok := evt.Data.(*agent.QuestionRequest)
		if !ok {
			b, _ := json.Marshal(evt.Data)
			var qr agent.QuestionRequest
			require.NoError(t, json.Unmarshal(b, &qr))
			req = &qr
		}
		switch req.ID {
		case "que_live":
			live = true
			assert.False(t, req.WhileAway, "live asks are not whileAway")
		case "que_stale":
			away = true
			assert.True(t, req.WhileAway, "inbox-only records must be whileAway-tagged")
			assert.Equal(t, "While away?", req.Questions[0].Question)
			assert.Equal(t, "ses_2", req.SessionID)
		default:
			t.Fatalf("unexpected ask %s", req.ID)
		}
	}
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.True(t, *marker.SnapshotOK)

	assert.True(t, live, "live ask must still be emitted")
	assert.True(t, away, "inbox-only ask must be emitted as whileAway")

	// The snapshot path is the recovering write (decision 3): the live
	// ask must now ALSO be an inbox record.
	recs, err := in.ListWorkspace(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Len(t, recs, 2, "live ask upserted + stale record retained")
}

func TestInbox_EmitPending_NoDuplicateForLiveRecord(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_live", SessionID: "ses_1", Kind: session.InputQuestion,
				Question: "Live?", Options: []session.InputOption{{Label: "A", Description: "a"}},
			}}, nil
		},
	}
	// The same ask, already recorded (event path fired first).
	rec := inbox.Record{
		ID: "que_live", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Live?", RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	count := 0
	for i := 0; i < 1; i++ {
		evt := recvWithTimeout(t, userSub, "agent.question")
		count++
		assert.False(t, evt.Data.(*agent.QuestionRequest).WhileAway)
	}
	_ = count
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.True(t, *marker.SnapshotOK)
}

func TestInbox_EmitPending_TerminalNotRePresented(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}
	rec := inbox.Record{
		ID: "que_done", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Answered?", RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))
	require.NoError(t, in.Resolve(context.Background(), "ws-1", "ses_1", "que_done", inbox.StatusAnswered))

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.True(t, *marker.SnapshotOK, "empty live + empty pending inbox = authoritative empty")
}

func TestInbox_QuestionReply_LateAnswerThroughOutbox(t *testing.T) {
	h, in, ob, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil // nothing live — the ask is dead (walk-away)
		},
	}
	rec := inbox.Record{
		ID: "que_dead", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Deploy the widget?", Header: "Deploy",
		Options:    []inbox.Option{{Label: "Yes", Description: "ship"}, {Label: "No", Description: "hold"}},
		RecordedAt: time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "requestID", Value: "que_dead"}}
	c.Request = jsonRequest(t, http.MethodPost, "/workspaces/ws-1/question/que_dead/reply", `{"answers":[["Yes"]]}`)

	h.QuestionReply(c)

	assert.Equal(t, http.StatusAccepted, c.Writer.Status(), "late answer is accepted (queued), not proxied")

	entries, err := ob.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, entries, 1, "the Q&A message must land in the outbox exactly once")
	e := entries[0]
	assert.Equal(t, "inbox-que_dead-answer", e.ClientMessageID, "S2 dedupe key rides the cmid")
	assert.Contains(t, e.Text, "Deploy the widget?", "Q&A text carries the original question")
	assert.Contains(t, e.Text, "Yes", "Q&A text carries the answer")

	answered, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, answered, "record must be terminal (answered) after the late answer")
}

func TestInbox_QuestionReply_LateAnswerDuplicateIsIdempotent(t *testing.T) {
	h, in, ob, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}
	rec := inbox.Record{
		ID: "que_dead", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Deploy?", Options: []inbox.Option{{Label: "Yes", Description: ""}}, RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	for i := 0; i < 2; i++ {
		c, _ := gin.CreateTestContext(recorderFor(t))
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "requestID", Value: "que_dead"}}
		c.Request = jsonRequest(t, http.MethodPost, "/workspaces/ws-1/question/que_dead/reply", `{"answers":[["Yes"]]}`)
		h.QuestionReply(c)
	}

	entries, err := ob.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, entries, 1, "double-click must not mint a second Q&A entry (outbox cmid dedupe)")
}

func TestInbox_QuestionReply_LiveAskStillProxies(t *testing.T) {
	env := newInputTestEnv(t)
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	env.handler.SetInboxStoreForTest(inbox.New(client))
	env.handler.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_abc123", SessionID: "ses_1", Kind: session.InputQuestion,
				Question: "Go?", Options: []session.InputOption{{Label: "Go", Description: ""}},
			}}, nil
		},
	}
	in := inbox.New(client)
	require.NoError(t, in.Record(context.Background(), "ws-1", inbox.Record{
		ID: "que_abc123", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Go?", RecordedAt: time.Now().UTC(),
	}))

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply", strings.NewReader(`{"answers":[["Go"]]}`))
	assert.Equal(t, http.StatusOK, w.Code, "live ask: the proxy path is unchanged")

	left, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, left, "successful live reply terminalizes the record as answered")
}

func TestInbox_PermissionReply_LateAnswerNotifiesModel(t *testing.T) {
	h, in, ob, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}
	rec := inbox.Record{
		ID: "per_dead", SessionID: "ses_1", Kind: inbox.KindPermission, Status: inbox.StatusPending,
		Permission: "bash", Patterns: []string{"rm -rf *"}, RecordedAt: time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "requestID", Value: "per_dead"}}
	c.Request = jsonRequest(t, http.MethodPost, "/workspaces/ws-1/permission/per_dead/reply", `{"reply":"reject"}`)

	h.PermissionReply(c)

	assert.Equal(t, http.StatusAccepted, c.Writer.Status())

	entries, err := ob.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "inbox-per_dead-answer", entries[0].ClientMessageID)
	assert.Contains(t, entries[0].Text, "bash", "permission Q&A carries the permission kind")
	assert.Contains(t, entries[0].Text, "reject", "permission Q&A carries the decision")

	answered, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, answered)
}

func TestInbox_QuestionReject_LiveRejectTerminalizesDismissed(t *testing.T) {
	env := newInputTestEnv(t)
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	in := inbox.New(client)
	env.handler.SetInboxStoreForTest(in)
	env.handler.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_abc123", SessionID: "ses_1", Kind: session.InputQuestion, Question: "Go?",
			}}, nil
		},
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", inbox.Record{
		ID: "que_abc123", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Go?", RecordedAt: time.Now().UTC(),
	}))

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	left, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, left, "a user-rejected live ask is the dismiss exit — no whileAway re-presentation")
}

func TestInbox_Dismiss_StaleRecordClears(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}
	rec := inbox.Record{
		ID: "que_stale", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Gone?", RecordedAt: time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}, {Key: "requestID", Value: "que_stale"}}
	c.Request = jsonRequest(t, http.MethodDelete, "/workspaces/ws-1/sessions/ses_1/inbox/que_stale", "")

	h.DismissInboxRecord(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())

	left, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, left)

	resolved := recvWithTimeout(t, userSub, "agent.question.resolved")
	assert.Equal(t, "que_stale", resolved.RequestID, "other tabs clear via the resolved event")
}

func TestInbox_Dismiss_LiveAskRejectsFirst(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	rejected := false
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_ring", SessionID: "ses_1", Kind: session.InputQuestion, Question: "Still ringing?",
			}}, nil
		},
		rejectInputFn: func(_ context.Context, _, _, rid string) error {
			rejected = true
			assert.Equal(t, "que_ring", rid)
			return nil
		},
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", inbox.Record{
		ID: "que_ring", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Still ringing?", RecordedAt: time.Now().UTC(),
	}))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}, {Key: "requestID", Value: "que_ring"}}
	c.Request = jsonRequest(t, http.MethodDelete, "/workspaces/ws-1/sessions/ses_1/inbox/que_ring", "")

	h.DismissInboxRecord(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.True(t, rejected, "dismiss of a live ask must reject the live ask first (two-exits hole)")
	left, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestInbox_Dismiss_LiveRejectFailureSurfaces(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_ring", SessionID: "ses_1", Kind: session.InputQuestion, Question: "Still ringing?",
			}}, nil
		},
		rejectInputFn: func(_ context.Context, _, _, _ string) error {
			return assert.AnError
		},
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", inbox.Record{
		ID: "que_ring", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Still ringing?", RecordedAt: time.Now().UTC(),
	}))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}, {Key: "requestID", Value: "que_ring"}}
	c.Request = jsonRequest(t, http.MethodDelete, "/workspaces/ws-1/sessions/ses_1/inbox/que_ring", "")

	h.DismissInboxRecord(c)

	assert.Equal(t, http.StatusBadGateway, c.Writer.Status(), "reject failure must surface, not silently clear")
	left, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, left, 1, "record must stay pending when the live reject fails")
}

func TestInbox_Dismiss_UnknownRecord404(t *testing.T) {
	h, _, _, _ := newInboxBackend(t)
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}
	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}, {Key: "requestID", Value: "que_none"}}
	c.Request = jsonRequest(t, http.MethodDelete, "/x", "")

	h.DismissInboxRecord(c)

	assert.Equal(t, http.StatusNotFound, c.Writer.Status())
}

func TestInbox_QAComposition_Permission(t *testing.T) {
	rec := inbox.Record{
		Kind:       inbox.KindPermission,
		Permission: "bash",
		Patterns:   []string{"rm -rf *"},
	}
	text := composeQA(rec, "reject")
	assert.Contains(t, text, "bash")
	assert.Contains(t, text, "rm -rf *")
	assert.Contains(t, text, "reject")
}

func recorderFor(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return httptest.NewRecorder()
}

func jsonRequest(t *testing.T, method, target string, body string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestInbox_QAComposition_Question(t *testing.T) {
	rec := inbox.Record{
		Kind:     inbox.KindQuestion,
		Question: "Deploy now?",
		Header:   "Deploy",
	}
	text := composeQA(rec, `"Yes, deploy"`)
	assert.Contains(t, text, "Deploy now?")
	assert.Contains(t, text, `"Yes, deploy"`)
}

// --- r1 findings 1-3: liveness-error semantics ---

func TestInbox_Reply_HarnessUnknownStillLateAnswers(t *testing.T) {
	// r1 finding 1: ListPending error (unreachable harness — e.g. the
	// suspended window) must degrade the REPLY to the late-answer flow:
	// an answer is safe against a possibly-live ask by construction.
	h, in, ob, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	}
	rec := inbox.Record{
		ID: "que_gone", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Deploy?", Options: []inbox.Option{{Label: "Yes", Description: ""}}, RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "requestID", Value: "que_gone"}}
	c.Request = jsonRequest(t, http.MethodPost, "/x", `{"answers":[["Yes"]]}`)

	h.QuestionReply(c)

	assert.Equal(t, http.StatusAccepted, c.Writer.Status(), "unknown liveness degrades to the late answer (r1 f1)")
	entries, err := ob.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	left, _ := in.List(context.Background(), "ws-1", "ses_1")
	assert.Empty(t, left)
}

func TestInbox_Dismiss_HarnessUnknownFailsClosed(t *testing.T) {
	// r1 finding 2: dismiss under unknown liveness must NOT terminalize —
	// a possibly-ringing doorbell plus an immutable dismissed record
	// reopens the two-exits hole. 503, record stays pending.
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	}
	rec := inbox.Record{
		ID: "que_maybe", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Still ringing?", RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}, {Key: "requestID", Value: "que_maybe"}}
	c.Request = jsonRequest(t, http.MethodDelete, "/x", "")

	h.DismissInboxRecord(c)

	assert.Equal(t, http.StatusServiceUnavailable, c.Writer.Status(), "dismiss fails closed on unknown liveness (r1 f2)")
	left, err := in.List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Len(t, left, 1, "record must stay pending")
}

func TestInbox_Dismiss_NoAdapterFailsClosed(t *testing.T) {
	h, in, _, _ := newInboxBackend(t)
	h.adapter = nil
	rec := inbox.Record{
		ID: "que_x", SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Q?", RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}, {Key: "requestID", Value: "que_x"}}
	c.Request = jsonRequest(t, http.MethodDelete, "/x", "")

	h.DismissInboxRecord(c)
	assert.Equal(t, http.StatusServiceUnavailable, c.Writer.Status())
}

func TestInbox_EmitPending_ListPendingErrorStillEmitsInbox(t *testing.T) {
	// r1 finding 3: the union must survive the live leg failing — the
	// suspension window is the exact scenario the inbox exists for. The
	// marker stays ok=false (non-authoritative; clients never wipe live
	// prompts), and the inbox half still emits.
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	}
	stale := inbox.Record{
		ID: "que_away", SessionID: "ses_2", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Away?", RecordedAt: time.Now().UTC().Add(-time.Minute),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", stale))

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	found := false
	for i := 0; i < 3 && !found; i++ {
		evt := recvWithTimeout(t, userSub, "agent.question")
		if evt.RequestID == "que_away" {
			found = true
		}
	}
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.False(t, *marker.SnapshotOK, "failed live fetch must stay non-authoritative")
	assert.True(t, found, "inbox-only record must still emit on the failed-live flight (r1 f3)")
}

func TestInbox_EmitPending_InboxStoreErrorNoPanicLiveOnly(t *testing.T) {
	// r1 missing-case 5: ListWorkspace error must degrade to a live-only
	// snapshot with a warn — never panic, never fail the flight.
	h, _, _, mr := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID: "que_live", SessionID: "ses_1", Kind: session.InputQuestion,
				Question: "Live?", Options: []session.InputOption{{Label: "A", Description: "a"}},
			}}, nil
		},
	}
	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	mr.Close() // the inbox store now errors on every read

	assert.NotPanics(t, func() {
		h.emitPendingInputRequests(context.Background(), "ws-1")
	})
	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	evt := recvWithTimeout(t, userSub, "agent.question")
	assert.Equal(t, "que_live", evt.RequestID, "live half unaffected by an inbox store failure")
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.True(t, *marker.SnapshotOK)
}

func TestInbox_Dismiss_RouterLevelBinding(t *testing.T) {
	// r1 missing-case 2: drive dismiss through a real gin router (param
	// names, method, middleware) instead of hand-set c.Params. The
	// route's OpenAPI↔router registration is pinned by the server
	// package's contract test; this pins the handler-side binding.
	env := newInputTestEnv(t)
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	in := inbox.New(client)
	env.handler.SetInboxStoreForTest(in)
	env.handler.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil // dead ask — dismiss proceeds without a live reject
		},
	}
	rec := inbox.Record{
		ID: "que_router", SessionID: "ses_router", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Bound?", RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec))

	env.router.DELETE("/api/v1/workspaces/:id/sessions/:sessionId/inbox/:requestID", env.handler.DismissInboxRecord)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/ses_router/inbox/que_router", nil)
	assert.Equal(t, http.StatusNoContent, w.Code, "router-level binding must reach the handler with all three params")

	// Wrong session in the path: same ask ID, different session scoping
	// → 404 (the record belongs to another session).
	rec2 := inbox.Record{
		ID: "que_scoped", SessionID: "ses_owner", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Scoped?", RecordedAt: time.Now().UTC(),
	}
	require.NoError(t, in.Record(context.Background(), "ws-1", rec2))
	w2 := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/ses_other/inbox/que_scoped", nil)
	assert.Equal(t, http.StatusNotFound, w2.Code, "session mismatch must 404, not dismiss another session's record")
}

func TestInbox_WhileAwayStack_MultipleRecordsAllRePresent(t *testing.T) {
	// The #1313 vitest "stack" row, server side: several pending records
	// re-present together, oldest first, all whileAway-tagged.
	h, in, _, _ := newInboxBackend(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}
	base := time.Now().UTC().Add(-time.Hour)
	ids := []string{"que_s1", "que_s2", "que_s3"}
	for i, id := range ids {
		rec := inbox.Record{
			ID: id, SessionID: "ses_1", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
			Question: "Q" + id, RecordedAt: base.Add(time.Duration(i) * time.Minute),
		}
		require.NoError(t, in.Record(context.Background(), "ws-1", rec))
	}

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	var order []string
	for i := 0; i < 3; i++ {
		evt := recvWithTimeout(t, userSub, "agent.question")
		b, _ := json.Marshal(evt.Data)
		var qr agent.QuestionRequest
		require.NoError(t, json.Unmarshal(b, &qr))
		assert.True(t, qr.WhileAway)
		order = append(order, qr.ID)
	}
	assert.Equal(t, ids, order, "stack re-presents oldest-first")
}

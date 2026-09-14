// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #1365: every reply path must publish the input-resolved event — the
// resolve-by-absence completion (200), the inbox late-answer (202), the
// typed action, and dismiss. The incident: a dead ask clicked from a
// stale pill resolved server-side (200) but emitted nothing, so every
// tab kept the pill forever. The event leg is part of S6's completion
// invariant, not an optional extra.

func livePermission(id, ses string) []session.InputRequest {
	return []session.InputRequest{{
		ID: id, SessionID: ses, Kind: session.InputPermission,
		Permission: "external_directory", Patterns: []string{"/tmp/*"},
	}}
}

// TestInputResolved_AbsenceResolve_NoRecord_PublishesPermissionEvent is
// the exact incident shape: the pod's projection still lists the ask
// (stale), the Act succeeds (agentd converts the harness 404 via
// resolve-by-absence), and NO inbox record exists — the click returned
// 200 yet zero events reached the user stream.
func TestInputResolved_AbsenceResolve_NoRecord_PublishesPermissionEvent(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return livePermission("per_norec", "ses_incident"), nil
		},
	})
	// Deliberately NO inbox record — the incident's orphaned ask.
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_norec/reply", `{"reply":"always"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	evt := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_norec", evt.RequestID)
	assert.Equal(t, "ses_incident", evt.SessionID, "the live-set session addresses the event")
	reason, _ := evt.Data.(map[string]string)
	assert.Equal(t, "answered", reason["reason"])
}

// TestInputResolved_AbsenceResolve_NoRecord_PublishesQuestionEvent —
// the question twin: kind derives from the validated que_ prefix when
// no record carries it.
func TestInputResolved_AbsenceResolve_NoRecord_PublishesQuestionEvent(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_norec", "ses_q"), nil
		},
	})
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_norec/reply", `{"answers":[["Go"]]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	evt := recvWithTimeout(t, userSub, "agent.question.resolved")
	assert.Equal(t, "que_norec", evt.RequestID)
}

// TestInputResolved_Reject_NoRecord_PublishesDismissedEvent — question
// reject (the dismiss exit) with no record still clears every tab.
func TestInputResolved_Reject_NoRecord_PublishesDismissedEvent(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return liveQuestion("que_dismiss", "ses_d"), nil
		},
	})
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/question/que_dismiss/reject", `{}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	evt := recvWithTimeout(t, userSub, "agent.question.resolved")
	assert.Equal(t, "que_dismiss", evt.RequestID)
	reason, _ := evt.Data.(map[string]string)
	assert.Equal(t, "dismissed", reason["reason"])
}

// TestInputResolved_AdapterPath_NoRecord_PublishesEvent — the flag-off
// regime's live reply publishes too: the record is an accelerator, not
// a precondition.
func TestInputResolved_AdapterPath_NoRecord_PublishesEvent(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	})
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
		replyPermissionFn: func(_ context.Context, _, _, _, _, _ string) error { return nil },
	}
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_adapter/reply", `{"reply":"once"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	evt := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_adapter", evt.RequestID)
}

// TestInputResolved_OwnerUnknown_PublishesToAuthenticatedClicker — a
// stale owner map (fresh replica, informer not yet caught up) must not
// silently drop the event: the authenticated clicker is a publish
// target too.
func TestInputResolved_OwnerUnknown_PublishesToAuthenticatedClicker(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return livePermission("per_noowner", "ses_o"), nil
		},
		configure: func(h *ProxyHandler) {
			h.userBroker = eventbroker.NewUserEventBroker()
		},
	})
	require.Empty(t, env.handler.userBroker.WorkspaceOwner("ws-act"), "precondition: owner map cold")

	clickerSub, _ := env.handler.userBroker.SubscribeUser("user-77")
	defer env.handler.userBroker.UnsubscribeUser("user-77", clickerSub)

	c, _ := gin.CreateTestContext(recorderFor(t))
	c.Params = gin.Params{{Key: "id", Value: "ws-act"}, {Key: "requestID", Value: "per_noowner"}}
	c.Request = jsonRequest(t, http.MethodPost, "/x", `{"reply":"always"}`)
	c.Set("userID", "user-77")
	env.handler.PermissionReply(c)
	require.Equal(t, http.StatusOK, c.Writer.Status())

	evt := recvWithTimeout(t, clickerSub, "agent.permission.resolved")
	assert.Equal(t, "per_noowner", evt.RequestID)
}

// TestInputResolved_SecondClickAfterResolution_Idempotent — the issue's
// integration scenario: click 1 resolves (200, one event); click 2 on
// the same dead pill is 2xx and publishes exactly one more event — no
// error, no duplicate burst, no 5xx.
func TestInputResolved_SecondClickAfterResolution_Idempotent(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	live := true
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			if live {
				return livePermission("per_twice", "ses_t"), nil
			}
			return nil, nil
		},
	})
	require.NoError(t, env.inbox.Record(context.Background(), "ws-act", inbox.Record{
		ID: "per_twice", SessionID: "ses_t", Kind: inbox.KindPermission, Status: inbox.StatusPending,
		Permission: "bash", RecordedAt: time.Now().UTC(),
	}))
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w1 := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_twice/reply", `{"reply":"always"}`)
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	evt1 := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_twice", evt1.RequestID)

	live = false // resolve-by-absence dropped the projection server-side
	w2 := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/permission/per_twice/reply", `{"reply":"always"}`)
	require.Equal(t, http.StatusOK, w2.Code, "re-click on the terminal record is idempotent, not an error", w2.Body.String())
	evt2 := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_twice", evt2.RequestID, "exactly one event per click — the second click re-publishes, never a burst")

	assertNoPendingEvents(t, userSub)
}

// TestInputResolved_SessionAction_NoRecord_PublishesEvent — the typed
// actions route (MCP/SDK replies) publishes without a record too.
func TestInputResolved_SessionAction_NoRecord_PublishesEvent(t *testing.T) {
	stub := newAnswerActStubPod(t, "")
	env := newInputActEnv(t, inputActOpts{
		terminus: true, podURL: stub.server.URL,
		listFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	})
	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.do(t, http.MethodPost, "/api/v1/workspaces/ws-act/sessions/ses_mcp/actions",
		`{"answerQuestion":{"inputId":"per_mcp","reply":"once"}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	evt := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "per_mcp", evt.RequestID)
}

func assertNoPendingEvents(t *testing.T, sub *eventbroker.Subscriber) {
	t.Helper()
	select {
	case evt := <-sub.Ch:
		t.Fatalf("unexpected extra event: %+v", evt)
	case <-time.After(300 * time.Millisecond):
	}
}

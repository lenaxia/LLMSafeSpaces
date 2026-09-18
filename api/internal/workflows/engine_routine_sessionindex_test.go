// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1452: preserved routine sessions must land in the platform's
// session_index. GET /workspaces/:id/sessions serves only that index;
// the routine fire path never goes through the proxy (whose adapter
// routes are the only index writers), so a session preserved by a
// routine trigger was invisible on the platform surface even though
// opencode's own store and live list carried it. These tests pin the
// fire-completion write: same condition as RecordSessionOrigin — the
// session must still exist (PreserveAlways, or PreserveOnFailure whose
// delete failed).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

type sessionIndexTitleCall struct {
	workspaceID, sessionID, title string
}

type sessionIndexMessageCall struct {
	workspaceID, sessionID, title string
	at                            time.Time
}

// recordingSessionIndex captures the session_index writes the routine
// path emits. upsertErr, when set, is returned by UpsertTitle.
type recordingSessionIndex struct {
	mu        sync.Mutex
	titles    []sessionIndexTitleCall
	messages  []sessionIndexMessageCall
	upsertErr error
}

func (r *recordingSessionIndex) UpsertTitle(_ context.Context, workspaceID, sessionID, title string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.titles = append(r.titles, sessionIndexTitleCall{workspaceID, sessionID, title})
	return r.upsertErr
}

func (r *recordingSessionIndex) RecordMessage(workspaceID, sessionID, title string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, sessionIndexMessageCall{workspaceID, sessionID, title, at})
}

func (r *recordingSessionIndex) titleCalls() []sessionIndexTitleCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionIndexTitleCall(nil), r.titles...)
}

func (r *recordingSessionIndex) messageCalls() []sessionIndexMessageCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionIndexMessageCall(nil), r.messages...)
}

func TestExecuteRoutine_PreserveAlways_IndexesPreservedSession(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_idx1"}`)
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx1", TriggerID: "trig-idx1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	before := time.Now().UTC()
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx1"], "fire must deliver")

	titles := index.titleCalls()
	require.Len(t, titles, 1, "exactly one title write per preserved session")
	require.Equal(t, sessionIndexTitleCall{workspaceID: "ws-1", sessionID: "ses_idx1", title: "Weather Bot"}, titles[0])

	messages := index.messageCalls()
	require.Len(t, messages, 1, "exactly one message write per fire")
	require.Equal(t, "ws-1", messages[0].workspaceID)
	require.Equal(t, "ses_idx1", messages[0].sessionID)
	require.False(t, messages[0].at.Before(before), "message timestamp must be the fire completion time")

	require.Len(t, store.sessionOrigins, 1, "origin recording is unchanged")
}

func TestExecuteRoutine_PreserveNever_DoesNotIndexSession(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx2", Name: "Chores", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-idx2", TriggerID: "trig-idx2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx2"])
	require.Empty(t, index.titleCalls(), "deleted ephemeral session must not be indexed")
	require.Empty(t, index.messageCalls())
}

func TestExecuteRoutine_PreserveOnFailure_DeleteSucceeds_DoesNotIndexSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_idx3"}`)
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx3", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-idx3", TriggerID: "trig-idx3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-idx"},
		AgentdPort:       port,
		SessionIndex:     index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx3"])
	require.Empty(t, index.titleCalls(), "session deleted on success must not be indexed")
	require.Empty(t, index.messageCalls())
}

func TestExecuteRoutine_PreserveOnFailure_DeleteFails_IndexesPreservedSession(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_idx4"}`)
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx4", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-idx4", TriggerID: "trig-idx4", InputEnvelope: json.RawMessage(`{}`)}
	// mockActivator returns an unreachable pod IP: the authorized delete
	// dials and fails, so the session still exists and must be indexed
	// (mirrors the RecordSessionOrigin fallback semantics, #762).
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-idx"},
		SessionIndex:     index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx4"])
	require.Len(t, index.titleCalls(), 1, "session that survived a failed delete must be indexed")
	require.Len(t, index.messageCalls(), 1)
}

func TestExecuteRoutine_NilSessionIndex_StillDelivers(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_idx5"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx5", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx5", TriggerID: "trig-idx5", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}

	require.NotPanics(t, func() {
		sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	})
	require.Equal(t, "delivered", store.statuses["fire-idx5"])
}

func TestExecuteRoutine_IndexTitleError_NonFatal(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_idx6"}`)
	index := &recordingSessionIndex{upsertErr: errors.New("index down")}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx6", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx6", TriggerID: "trig-idx6", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx6"], "index failure must never fail the fire")
	require.Len(t, index.messageCalls(), 1, "message write is independent of the title write")
	require.Len(t, store.sessionOrigins, 1, "origin write is independent of the title write")
}

// TestExecuteRoutine_SessionIndexIntegration drives the engine against
// the REAL sessionindex.Service (started drainer + mock database) — the
// seam the recording mock bypasses: interface dispatch on the concrete
// service, the bounded queue, and the drain that turns RecordMessage
// into a database UpsertSessionMessage. Stop() drains the queue and
// joins the drainer, so the mock is quiescent when asserted (testify
// mocks are not safe for concurrent reads from the drainer goroutine).
func TestExecuteRoutine_SessionIndexIntegration(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionTitle", mock.Anything, "ws-1", "ses_int1", "Weather Bot").Return(nil)
	db.On("UpsertSessionMessage", mock.Anything, "ws-1", "ses_int1", mock.AnythingOfType("time.Time")).Return(nil)
	index := sessionindex.New(db, nil)
	require.NoError(t, index.Start())

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_int1"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-int1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-int1", TriggerID: "trig-int1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	require.NoError(t, index.Stop())

	require.Equal(t, "delivered", store.statuses["fire-int1"])
	db.AssertCalled(t, "UpsertSessionTitle", mock.Anything, "ws-1", "ses_int1", "Weather Bot")
	db.AssertCalled(t, "UpsertSessionMessage", mock.Anything, "ws-1", "ses_int1", mock.AnythingOfType("time.Time"))
}

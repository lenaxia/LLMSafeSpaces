// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1452: a preserved routine session exists in opencode (row + transcript)
// but was invisible on the platform surface — GET /workspaces/:id/sessions
// serves the PostgreSQL session_index only, and no routine-fire path ever
// wrote a row there (every INSERT site was adapter-route or usage-stream
// gated; visibility was a race on recent interactive traffic). These tests
// pin the fix: executeRoutine writes the session_index row at fire
// completion, at the same site and under the same condition as
// RecordSessionOrigin.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// The production sessionindex.Service must satisfy the scheduler's seam
// implicitly — app.go wires the concrete service without an adapter.
var _ SessionIndexWriter = (*sessionindex.Service)(nil)

type titleCall struct {
	workspaceID string
	sessionID   string
	title       string
}

type messageCall struct {
	workspaceID string
	sessionID   string
	at          time.Time
}

// recordingSessionIndexWriter captures SessionIndexWriter calls from the
// scheduler. Thread-safe: fires can be processed concurrently.
type recordingSessionIndexWriter struct {
	mu       sync.Mutex
	titles   []titleCall
	messages []messageCall
	titleErr error
}

func (r *recordingSessionIndexWriter) RecordMessage(workspaceID, sessionID, _ string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, messageCall{workspaceID: workspaceID, sessionID: sessionID, at: at})
}

func (r *recordingSessionIndexWriter) UpsertTitle(_ context.Context, workspaceID, sessionID, title string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.titles = append(r.titles, titleCall{workspaceID: workspaceID, sessionID: sessionID, title: title})
	return r.titleErr
}

func (r *recordingSessionIndexWriter) titleCalls() []titleCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]titleCall(nil), r.titles...)
}

func (r *recordingSessionIndexWriter) messageCalls() []messageCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]messageCall(nil), r.messages...)
}

func TestExecuteRoutine_PreserveAlways_IndexesSession(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_idx1"}`)
	idx := &recordingSessionIndexWriter{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx1", TriggerID: "trig-idx1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}, SessionIndex: idx}

	before := time.Now().UTC().Add(-time.Second)
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	after := time.Now().UTC().Add(time.Second)

	require.Equal(t, "delivered", store.statuses["fire-idx1"])

	titleCalls := idx.titleCalls()
	require.Len(t, titleCalls, 1, "the index row must carry the trigger name as its title")
	assert.Equal(t, "ws-1", titleCalls[0].workspaceID)
	assert.Equal(t, "ses_idx1", titleCalls[0].sessionID)
	assert.Equal(t, "Weather Bot", titleCalls[0].title)

	messageCalls := idx.messageCalls()
	require.Len(t, messageCalls, 1, "the index row must carry last_message_at so the sidebar ordering includes it")
	assert.Equal(t, "ws-1", messageCalls[0].workspaceID)
	assert.Equal(t, "ses_idx1", messageCalls[0].sessionID)
	assert.False(t, messageCalls[0].at.Before(before), "last_message_at must be stamped at fire completion, not zero")
	assert.False(t, messageCalls[0].at.After(after), "last_message_at must not be in the future")

	assert.Len(t, store.sessionOrigins, 1, "the origin record (badge/linkage) must be unchanged")
}

func TestExecuteRoutine_PreserveOnFailure_DeleteSucceeds_DoesNotIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, portStr, _ := strings.Cut(addr, ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_gone1"}`)
	idx := &recordingSessionIndexWriter{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx2", Name: "Cleanup", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-idx2", TriggerID: "trig-idx2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: idx, PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx2"])
	assert.Empty(t, idx.titleCalls(), "a deleted session must not be indexed")
	assert.Empty(t, idx.messageCalls(), "a deleted session must not be indexed")
	assert.Empty(t, store.sessionOrigins)
}

func TestExecuteRoutine_PreserveNever_DoesNotIndex(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	// agentd sets session_id="" for PreserveNever after deleting the ephemeral session
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	idx := &recordingSessionIndexWriter{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx3", Name: "Ephemeral", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-idx3", TriggerID: "trig-idx3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}, SessionIndex: idx}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx3"])
	assert.Empty(t, idx.titleCalls(), "an ephemeral (deleted) session must not be indexed")
	assert.Empty(t, idx.messageCalls(), "an ephemeral (deleted) session must not be indexed")
	assert.Empty(t, store.sessionOrigins)
}

func TestExecuteRoutine_AgentFailure_DoesNotIndex(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "agent_not_found"
	idx := &recordingSessionIndexWriter{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx4", Name: "Broken", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx4", TriggerID: "trig-idx4", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}, SessionIndex: idx}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-idx4"])
	assert.Empty(t, idx.titleCalls(), "a failed fire has no completed session to index")
	assert.Empty(t, idx.messageCalls(), "a failed fire has no completed session to index")
	assert.Empty(t, store.sessionOrigins)
}

func TestExecuteRoutine_IndexTitleError_NonFatal(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_err1"}`)
	idx := &recordingSessionIndexWriter{titleErr: assert.AnError}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx5", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx5", TriggerID: "trig-idx5", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}, SessionIndex: idx}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx5"], "an index-write failure must not fail the fire (same best-effort class as RecordSessionOrigin)")
	assert.Len(t, idx.messageCalls(), 1, "the ordering write is independent of the title write and must still be attempted")
	assert.Len(t, store.sessionOrigins, 1, "origin recording is unaffected by index-write failures")
}

func TestExecuteRoutine_NilSessionIndex_DoesNotPanic(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_nil1"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx6", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx6", TriggerID: "trig-idx6", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}

	require.NotPanics(t, func() {
		sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	})
	assert.Equal(t, "delivered", store.statuses["fire-idx6"])
	assert.Len(t, store.sessionOrigins, 1)
}

func TestExecuteRoutine_Redrive_RefreshesIndex(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_red1"}`)
	idx := &recordingSessionIndexWriter{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-idx7", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-idx7", TriggerID: "trig-idx7", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}, SessionIndex: idx}

	// processPendingRoutineFire re-drives pending fires through
	// executeRoutine; the index writes are upserts, so a re-drive must
	// refresh (not corrupt) the row.
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-idx7"])
	assert.Len(t, idx.titleCalls(), 2)
	for _, c := range idx.titleCalls() {
		assert.Equal(t, "ws-1", c.workspaceID)
		assert.Equal(t, "ses_red1", c.sessionID)
		assert.Equal(t, "Weather Bot", c.title)
	}
	assert.Len(t, idx.messageCalls(), 2)
	for _, c := range idx.messageCalls() {
		assert.Equal(t, "ws-1", c.workspaceID)
		assert.Equal(t, "ses_red1", c.sessionID)
	}
}

// TestExecuteRoutine_PreserveAlways_RealSessionIndexServiceWiring exercises
// the real production wiring: Scheduler → sessionindex.Service → the DB
// layer (mocked at the SQL boundary). The sync title write lands inside
// executeRoutine; the async ordering write must reach the DB through the
// service's drainer goroutine.
func TestExecuteRoutine_PreserveAlways_RealSessionIndexServiceWiring(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionTitle", mock.Anything, "ws-1", "ses_wire1", "Weather Bot").Return(nil)
	db.On("UpsertSessionMessage", mock.Anything, "ws-1", "ses_wire1", mock.AnythingOfType("time.Time")).Return(nil)

	svc := sessionindex.New(db, nil)
	require.NoError(t, svc.Start())
	defer func() { _ = svc.Stop() }()

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_wire1"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-wire1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-wire1", TriggerID: "trig-wire1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}, SessionIndex: svc}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-wire1"])
	db.AssertCalled(t, "UpsertSessionTitle", mock.Anything, "ws-1", "ses_wire1", "Weather Bot")
	assert.Eventually(t, func() bool {
		for _, c := range db.Calls {
			if c.Method == "UpsertSessionMessage" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "the async ordering write must drain to the DB layer")
	db.AssertCalled(t, "UpsertSessionMessage", mock.Anything, "ws-1", "ses_wire1", mock.AnythingOfType("time.Time"))
}

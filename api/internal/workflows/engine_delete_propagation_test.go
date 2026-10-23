// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1882 Bug 2 pins: every session delete the engine authorizes or
// observes must PROPAGATE platform-side — the same tombstone +
// session_index row removal + session.status:deleted broadcast the
// user-route delete performs (SessionDeletePropagator), plus the
// pod-side short-id replica invalidation agentd owns on its own
// delete legs. Pre-fix, the engine's authorized deletes emitted none
// of the effects, and agentd's ephemeral teardowns were INVISIBLE to
// the engine (the id wiped from the envelope) — together the
// ghost-sidebar root cause: ~75 carrier sessions/hour minted, deleted,
// and never reaped.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// propagationCall is one recorded PropagateSessionDeleted invocation.
type propagationCall struct{ workspaceID, sessionID string }

// recordingPropagator captures the engine's teardown propagation calls.
type recordingPropagator struct {
	mu    sync.Mutex
	calls []propagationCall
}

func (p *recordingPropagator) PropagateSessionDeleted(_ context.Context, workspaceID, sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, propagationCall{workspaceID: workspaceID, sessionID: sessionID})
}

func (p *recordingPropagator) propagated() []propagationCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]propagationCall(nil), p.calls...)
}

// newAuthorizedDeleteServer stands up an agentd-shaped /v1/workflow/
// session/delete endpoint answering 204 (the confirmed-delete shape).
func newAuthorizedDeleteServer(t *testing.T) (port int, deleted *[]string) {
	t.Helper()
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workflow/session/delete", r.URL.Path)
		mu.Lock()
		hits = append(hits, r.URL.Query().Get("sessionId"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	p, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return p, &hits
}

// TestExecuteRoutine_PreserveOnFailureSuccess_PropagatesDelete: the
// engine's own authorized delete (PreserveOnFailure success cleanup)
// must run the platform-side teardown after the pod confirms — the
// carrier was indexed during its life; without propagation its row is
// kept_for_operator forever (DefaultReapGuard refuses history-bearing
// rows).
func TestExecuteRoutine_PreserveOnFailureSuccess_PropagatesDelete(t *testing.T) {
	port, deleted := newAuthorizedDeleteServer(t)
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_pof1"}`)
	prop := &recordingPropagator{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-pof1", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-pof1", TriggerID: "trig-pof1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port,
		SessionIndex: &recordingSessionIndex{}, SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-pof1"])
	require.Equal(t, []string{"ses_pof1"}, *deleted, "the pod-side delete must still run")
	require.Equal(t, []propagationCall{{workspaceID: "ws-1", sessionID: "ses_pof1"}}, prop.propagated(),
		"a confirmed authorized delete must run the platform-side teardown (#1882)")
}

// TestExecuteRoutine_EphemeralTeardown_ReportedDeletedIDPropagates:
// the PreserveNever default — agentd creates a carrier, runs the turn,
// deletes the session, and (post-#1882) reports the deleted id in the
// envelope. The engine must propagate that teardown: the live event
// bridge minted the carrier's index row during its ~seconds of life.
func TestExecuteRoutine_EphemeralTeardown_ReportedDeletedIDPropagates(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_eph1"
	prop := &recordingPropagator{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-eph1", Name: "Chores", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-eph1", TriggerID: "trig-eph1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: &recordingSessionIndex{}, SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-eph1"])
	require.Equal(t, []propagationCall{{workspaceID: "ws-1", sessionID: "ses_eph1"}}, prop.propagated(),
		"an agentd-reported ephemeral teardown must propagate platform-side (#1882)")
}

// TestExecuteRoutine_EphemeralTeardown_FailureEnvelopePropagates: the
// failure path tears ephemerals down too (failAgentNode) — the engine
// must propagate the reported deleted id on the ErrorCode leg exactly
// as on the delivered leg.
func TestExecuteRoutine_EphemeralTeardown_FailureEnvelopePropagates(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "schema_mismatch", Detail: "bad json", DeletedSessionID: "ses_eph2"}},
	}}
	prop := &recordingPropagator{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-eph2", Name: "Chores", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-eph2", TriggerID: "trig-eph2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: &recordingSessionIndex{}, SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-eph2"])
	require.Equal(t, []propagationCall{{workspaceID: "ws-1", sessionID: "ses_eph2"}}, prop.propagated(),
		"a failed fire's ephemeral teardown must propagate too — its row was minted the same way")
}

// TestExecuteRoutine_RetryIntermediateEphemeralTeardown_Propagates:
// the retry loop DISCARDS intermediate attempts' responses — the only
// place an attempt-1 ephemeral teardown is observable is inside the
// loop. A retryable failure envelope carrying a deleted id must
// propagate before the next attempt dispatches.
func TestExecuteRoutine_RetryIntermediateEphemeralTeardown_Propagates(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", DeletedSessionID: "ses_midA"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok","session_id":""}`), DeletedSessionID: "ses_finalB"}},
	}}
	prop := &recordingPropagator{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-eph3", Name: "Chores", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-eph3", TriggerID: "trig-eph3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: &recordingSessionIndex{}, SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-eph3"])
	require.Equal(t, []propagationCall{
		{workspaceID: "ws-1", sessionID: "ses_midA"},
		{workspaceID: "ws-1", sessionID: "ses_finalB"},
	}, prop.propagated(), "every attempt's reported teardown must propagate — the discarded intermediate's row is a ghost too")
}

// TestExecuteRoutine_NilPropagator_NoPanic: unwired engines (embedded
// schedulers, tests) keep the pre-#1882 behavior without panicking.
func TestExecuteRoutine_NilPropagator_NoPanic(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_eph4"
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-eph4", Name: "Chores", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-eph4", TriggerID: "trig-eph4", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: &recordingSessionIndex{}}

	require.NotPanics(t, func() {
		sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	})
	require.Equal(t, "delivered", store.statuses["fire-eph4"])
}

// TestReconciler_DAGAgentEphemeralTeardown_Propagates: the DAG path's
// agent nodes default to ephemeral sessions too — a reported teardown
// must propagate exactly as on the routine path.
func TestReconciler_DAGAgentEphemeralTeardown_Propagates(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()
	agentd.outputs["start"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["start"] = "ses_dag1"
	prop := &recordingPropagator{}

	spec := json.RawMessage(`{
		"nodes": [
			{"id":"start","type":"agent","data":{"prompt":"hi"},"maxAttempts":1}
		],
		"edges": []
	}`)
	store.addRun("run-dag1", "wf-1", "ws-1", spec, json.RawMessage(`{}`), "")

	rec := &Reconciler{Store: store, AgentdClient: agentd, Activator: &mockActivator{}, Logger: noopLogger{},
		SessionDelete: prop}
	rec.cancelMu = sync.Mutex{}
	rec.canceledRuns = make(map[string]struct{})

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag1"])
	require.Equal(t, []propagationCall{{workspaceID: "ws-1", sessionID: "ses_dag1"}}, prop.propagated(),
		"a DAG agent node's ephemeral teardown must propagate platform-side (#1882)")
}

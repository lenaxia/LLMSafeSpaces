// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1882 delete propagation — UNION of both lanes' pins. EVERY
// platform-initiated session delete — agentd's ephemeral carrier
// teardown, the scheduler's authorized deletes (PreserveOnFailure
// success, retry-intermediate), and the DAG reconciler's node-retry
// cleanup — runs the same teardown as the user proxy route (wsstate
// tombstone, durable session_index death mark + row removal,
// session.status:deleted SSE); one propagation point per response
// (onDeleted), the carrier tail origin-marks before and cleans after
// (reconcile self-heal), and main's DeleteEffects wiring is the
// SessionDelete propagator under its union name.

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

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
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

// --- main-lane pins (renamed to the union interface) ---

// recordingSessionDelete captures the engine's delete-effect calls.
type recordingSessionDelete struct {
	mu     sync.Mutex
	called []sessionDeleteCall
}

type sessionDeleteCall struct {
	workspaceID, sessionID string
}

func (r *recordingSessionDelete) PropagateSessionDeleted(_ context.Context, workspaceID, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = append(r.called, sessionDeleteCall{workspaceID, sessionID})
}

func (r *recordingSessionDelete) calls() []sessionDeleteCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionDeleteCall(nil), r.called...)
}

// TestExecuteRoutine_EphemeralCarrierDelete_PropagatesEffects is the
// #1882 primary pin: agentd's envelope reports the deleted ephemeral
// carrier's id (deletedSessionId); the engine must answer with the
// platform delete effects for that id — the ghost-row mint the live
// event bridge performed during the carrier's turn is undone.
func TestExecuteRoutine_EphemeralCarrierDelete_PropagatesEffects(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_carrier_1"
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp1", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp1", TriggerID: "trig-dp1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionDelete: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-dp1"])
	got := effects.calls()
	require.Len(t, got, 1, "the ephemeral carrier delete must propagate exactly one effects call")
	require.Equal(t, sessionDeleteCall{workspaceID: "ws-1", sessionID: "ses_carrier_1"}, got[0])
}

// TestExecuteRoutine_EphemeralCarrierDelete_MarksAndClearsOrigin: the
// carrier is origin-marked BEFORE the effects run (the #1340 reconcile
// bypass mark — if the effects' index write fails, the reconcile still
// reaps the row) and the mark is cleaned after (carriers are deleted
// sessions; origins must not accumulate per fire).
func TestExecuteRoutine_EphemeralCarrierDelete_MarksAndClearsOrigin(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_carrier_2"
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp2", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp2", TriggerID: "trig-dp2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionDelete: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, []string{"ses_carrier_2"}, store.originDeletes,
		"the carrier's origin mark must be cleaned once the effects ran")
	require.Empty(t, store.sessionOrigins, "no origin row may survive the carrier's delete")
}

// TestExecuteRoutine_EphemeralFailureLeg_PropagatesEffects: the
// #1470 failure leg (200+errorCode envelope) carries the deleted id too —
// a failed fire's torn-down carrier ghosts exactly like a delivered one.
func TestExecuteRoutine_EphemeralFailureLeg_PropagatesEffects(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "script_failed"
	agentd.deletedSessionIDs["routine-agent"] = "ses_carrier_3"
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp3", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp3", TriggerID: "trig-dp3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionDelete: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-dp3"])
	got := effects.calls()
	require.Len(t, got, 1, "the failure-leg teardown must propagate effects too")
	require.Equal(t, "ses_carrier_3", got[0].sessionID)
}

// TestExecuteRoutine_AuthorizedDelete_PropagatesEffects: the
// PreserveOnFailure success cleanup (deleteRoutineSessionAuthorized →
// agentd route 204) must run the effects for the deleted session.
func TestExecuteRoutine_AuthorizedDelete_PropagatesEffects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_pof_1"}`)
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp4", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-dp4", TriggerID: "trig-dp4", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-dp"},
		AgentdPort:       port,
		SessionDelete:    effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-dp4"])
	got := effects.calls()
	require.Len(t, got, 1, "the authorized delete must propagate effects")
	require.Equal(t, "ses_pof_1", got[0].sessionID)
	require.Equal(t, "ws-1", got[0].workspaceID)
}

// TestExecuteRoutine_AuthorizedDeleteFailed_NoEffects: a delete the
// agentd route refuses (502 — the session SURVIVED, #1470 reality
// contract) must NOT run delete effects: the session still exists.
func TestExecuteRoutine_AuthorizedDeleteFailed_NoEffects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_pof_2"}`)
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp5", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-dp5", TriggerID: "trig-dp5", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-dp"},
		AgentdPort:       port,
		SessionDelete:    effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Empty(t, effects.calls(), "a survived delete must not broadcast deleted effects")
}

// TestExecuteRoutine_EphemeralDelete_NilEffects_NonFatal: deployments
// without the effectuator wired (tests, partial wiring) must not panic.
func TestExecuteRoutine_EphemeralDelete_NilEffects_NonFatal(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_carrier_4"
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp6", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp6", TriggerID: "trig-dp6", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}

	require.NotPanics(t, func() {
		sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	})
	require.Equal(t, "delivered", store.statuses["fire-dp6"])
}

// TestExecuteRoutine_EphemeralCarrier_SessionIndexIntegrationToZero is
// the #1882 E2E-shaped pin: a routine fire's carrier session — indexed
// by the live event bridge during its turn — returns the sidebar-visible
// row count to zero once the fire's delete effects land. The bridge
// mint is simulated with a direct index write (Register + title +
// message); the effectuator adapter mirrors the production
// ProxyHandler.ApplySessionDeleteEffects index half (mark deleted +
// row delete) over the REAL sessionindex.Service.
func TestExecuteRoutine_EphemeralCarrier_SessionIndexIntegrationToZero(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("EnsureSessionRegistered", mock.Anything, "ws-1", "ses_e2e_carrier", "opencode").Return("oc-e2e00001", nil)
	db.On("UpsertSessionTitle", mock.Anything, "ws-1", "ses_e2e_carrier", mock.Anything).Return(nil)
	db.On("UpsertSessionMessage", mock.Anything, "ws-1", "ses_e2e_carrier", mock.AnythingOfType("time.Time")).Return(nil)
	db.On("MarkSessionIndexDeleted", mock.Anything, "ws-1", "ses_e2e_carrier").Return(nil).Maybe()
	db.On("DeleteSessionTree", mock.Anything, "ws-1", "ses_e2e_carrier").Return(nil)
	index := sessionindex.New(db, nil)
	require.NoError(t, index.Start())

	// The live event bridge's mint during the carrier's brief life.
	index.Register(context.Background(), "ws-1", "ses_e2e_carrier", "opencode")
	require.NoError(t, index.UpsertTitle(context.Background(), "ws-1", "ses_e2e_carrier", "Reply OK"))
	index.RecordMessage("ws-1", "ses_e2e_carrier", "", time.Now().UTC())

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_e2e_carrier"
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-e2e", Name: "Reply Bot", WorkspaceID: &wsID, Prompt: "Reply OK",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-e2e", TriggerID: "trig-e2e", InputEnvelope: json.RawMessage(`{}`)}
	// The effectuator mirrors the production effect tail's index half.
	effects := &indexDeleteEffects{index: index}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionDelete: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	require.NoError(t, index.Stop())

	require.Equal(t, "delivered", store.statuses["fire-e2e"])
	db.AssertCalled(t, "MarkSessionIndexDeleted", mock.Anything, "ws-1", "ses_e2e_carrier")
	db.AssertCalled(t, "DeleteSessionTree", mock.Anything, "ws-1", "ses_e2e_carrier")
}

// indexDeleteEffects is the test mirror of the production
// ProxyHandler.ApplySessionDeleteEffects index half.
type indexDeleteEffects struct { // renamed seam: implements SessionDeletePropagator
	index *sessionindex.Service
}

func (e *indexDeleteEffects) PropagateSessionDeleted(ctx context.Context, workspaceID, sessionID string) {
	_ = e.index.MarkSessionDeleted(ctx, workspaceID, sessionID)
	_ = e.index.DeleteSession(ctx, workspaceID, sessionID)
}

// TestExecuteRoutine_RetryAttempts_CarriersPropagateSeparately is the
// round-1 review's missing-test finding 2: a discarded RETRY attempt's
// carrier (reported on the discarded response, propagated via
// executeWithRetry's intermediateDeleted callback) AND the FINAL
// attempt's carrier (reported on the returned response) each propagate
// their own effects — neither rides the other's envelope.
func TestExecuteRoutine_RetryAttempts_CarriersPropagateSeparately(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{
			ErrorCode: "script_failed", Detail: "opencode returned 500",
			DeletedSessionID: "ses_carrier_mid",
		}},
		{resp: &NodeExecResponse{
			Output:           json.RawMessage(`{"response":"ok","session_id":""}`),
			DeletedSessionID: "ses_carrier_final",
		}},
	}}
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp7", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp7", TriggerID: "trig-dp7", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionDelete: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-dp7"])
	got := effects.calls()
	require.Len(t, got, 2, "the discarded attempt's carrier and the final attempt's carrier each propagate")
	require.Equal(t, sessionDeleteCall{workspaceID: "ws-1", sessionID: "ses_carrier_mid"}, got[0],
		"the discarded retry attempt's carrier propagates first (before the retry dispatches)")
	require.Equal(t, sessionDeleteCall{workspaceID: "ws-1", sessionID: "ses_carrier_final"}, got[1],
		"the final attempt's carrier propagates from the returned response")
	require.ElementsMatch(t, []string{"ses_carrier_mid", "ses_carrier_final"}, store.originDeletes,
		"each carrier's origin mark is cleaned")
}

// TestExecuteRoutine_OriginKind_DiscriminatesFate is the #1901 item-a
// discriminator pin: the two origin writers must record the session's
// FATE. propagateCarrierDelete (agentd-reported-deleted ephemerals)
// writes kind=carrier — reap-admissible (its absence from the harness
// is platform policy). recordRoutineSessionArtifacts (sessions that
// SURVIVED the fire) writes kind=session — NEVER reap-admitted past
// the operator-review guard: a harness store reset is exactly the
// kept_for_operator scenario for a preserved session. Pre-#1901 both
// wrote the same undiscriminated mark, so the blanket bypass could
// reap a preserved session's history row.
func TestExecuteRoutine_OriginKind_DiscriminatesFate(t *testing.T) {
	// Leg 1 — ephemeral carrier (PreserveNever + deletedSessionId):
	// the transient mark must be kind=carrier.
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	agentd.deletedSessionIDs["routine-agent"] = "ses_kind_carrier"
	effects := &recordingSessionDelete{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-kind1", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-kind1", TriggerID: "trig-kind1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionDelete: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-kind1"])
	require.Len(t, store.originWrites, 1, "the carrier's transient mark must be recorded (pre-cleanup)")
	require.Equal(t, types.SessionOriginKindCarrier, store.originWrites[0].Kind,
		"a platform-deleted ephemeral carrier's mark must be kind=carrier (reap-admissible)")

	// Leg 2 — preserved session (PreserveAlways): the persistent mark
	// must be kind=session.
	store2 := newMockSchedulerStore()
	agentd2 := newMockAgentd()
	agentd2.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_kind_preserved"}`)
	wsID2 := "ws-1"
	trigger2 := &wf.TriggerRow{ID: "trig-kind2", Name: "Weather Bot", WorkspaceID: &wsID2, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire2 := &wf.TriggerFireRow{ID: "fire-kind2", TriggerID: "trig-kind2", InputEnvelope: json.RawMessage(`{}`)}
	sched2 := &Scheduler{Store: store2, Activator: &mockActivator{}, AgentdClient: agentd2, Logger: noopLogger{}}

	sched2.executeRoutine(context.Background(), noopLogger{}, trigger2, fire2)

	require.Equal(t, "delivered", store2.statuses["fire-kind2"])
	preserved, ok := store2.sessionOrigins["ses_kind_preserved"]
	require.True(t, ok, "the preserved session's mark must persist")
	require.Equal(t, types.SessionOriginKindSession, preserved.Kind,
		"a survived fire's mark must be kind=session (never reap-admitted past the guard)")
}

// TestExecuteNode_SupersededAttemptCleanup_PropagatesEffects: the DAG
// node-retry authorized delete (#1476) must run the delete effects —
// before #1882 the cleaned orphan's index row lingered as a ghost.
func TestExecuteNode_SupersededAttemptCleanup_PropagatesEffects(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	effects := &recordingSessionDelete{}
	rec.SessionDelete = effects
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_dag_eff"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`), SessionID: "ses_dag_final"}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag-eff", "wf-dag", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag-eff"])
	require.Equal(t, []string{"ses_dag_eff"}, *cleaned)
	got := effects.calls()
	require.Len(t, got, 1, "the node-retry cleanup delete must propagate effects")
	require.Equal(t, sessionDeleteCall{workspaceID: "ws-1", sessionID: "ses_dag_eff"}, got[0])
}

// TestExecuteNode_DAGAgentEphemeral_PropagatesEffects: a workflow DAG
// agent node in ephemeral mode (agentd tears the carrier down) reports
// deletedSessionId — the reconciler must run the effects for it.
func TestExecuteNode_DAGAgentEphemeral_PropagatesEffects(t *testing.T) {
	store, rec, _ := dagCleanupHarness(t, http.StatusNoContent)
	effects := &recordingSessionDelete{}
	rec.SessionDelete = effects
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`), SessionID: "", DeletedSessionID: "ses_dag_eph"}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag-eph", "wf-dag", "ws-1", dagAgentSpec("ephemeral", 1), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag-eph"])
	got := effects.calls()
	require.Len(t, got, 1, "a DAG ephemeral carrier teardown must propagate effects")
	require.Equal(t, sessionDeleteCall{workspaceID: "ws-1", sessionID: "ses_dag_eph"}, got[0])
}

// --- #1915 origin-marking pins (mark → effects → clean on the two
// unmarked legs) ---

// orderingProbePropagator records, at each PropagateSessionDeleted
// call, whether the session's kind='carrier' origin mark was ALREADY
// recorded — the #1915 ordering observable: the re-apability guarantee
// is the mark BEFORE the effects (a mid-effects failure must find the
// mark in place for the #1901 reap net's discriminator).
type orderingProbePropagator struct {
	mu     sync.Mutex
	probe  func(sessionID string) bool
	calls  []string
	marked map[string]bool
}

func (p *orderingProbePropagator) PropagateSessionDeleted(_ context.Context, _, sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, sessionID)
	if p.marked == nil {
		p.marked = make(map[string]bool)
	}
	p.marked[sessionID] = p.probe != nil && p.probe(sessionID)
}

func (p *orderingProbePropagator) markedAtEffects(sessionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.marked[sessionID]
}

// TestExecuteRoutine_AuthorizedDelete_MarksCarrierOriginBeforeEffects:
// the #1915 leg-1 pin (routine PreserveOnFailure-success cleanup →
// deleteSessionAuthorized). A CONFIRMED agentd 204 must origin-mark
// kind='carrier' BEFORE the effects run (the effects' own error return
// is void — an index-write failure is only observable as the ghost row
// surviving; the pre-effects mark is then the ONLY thing letting the
// #1901 reap bypass admit it) and clean the mark after (origins must
// not accumulate per fire).
func TestExecuteRoutine_AuthorizedDelete_MarksCarrierOriginBeforeEffects(t *testing.T) {
	port, deleted := newAuthorizedDeleteServer(t)
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_mark1"}`)
	prop := &orderingProbePropagator{probe: store.carrierMarked}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-mk1", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-mk1", TriggerID: "trig-mk1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port,
		SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-mk1"])
	require.Equal(t, []string{"ses_mark1"}, *deleted, "the pod-side delete is confirmed (204)")
	require.Len(t, prop.calls, 1, "the effects ran exactly once")
	require.True(t, prop.markedAtEffects("ses_mark1"),
		"#1915: the kind=carrier origin mark must be recorded BEFORE the effects run — "+
			"without it, a mid-effects failure leaves an UNMARKED ghost row the #1901 reap net "+
			"cannot admit (ListCarrierOriginMarks empty; kept_for_operator forever)")
	require.Equal(t, []string{"ses_mark1"}, store.originDeletes,
		"the mark must be cleaned once the effects ran")
	require.False(t, store.carrierMarked("ses_mark1"), "no origin row may survive the cleanup")
}

// TestExecuteNode_SupersededCleanup_MarksCarrierOriginBeforeEffects:
// the #1915 leg-1 pin (DAG side): the reconciler's node-retry cleanup
// rides the same caller-neutral deleteSessionAuthorized core and must
// carry the same mark-before-effects / clean-after discipline.
func TestExecuteNode_SupersededCleanup_MarksCarrierOriginBeforeEffects(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	prop := &orderingProbePropagator{probe: store.carrierMarked}
	rec.SessionDelete = prop
	rec.AgentdClient = &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_mark_mid"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`), SessionID: "ses_mark_final"}},
	}}
	store.addRun("run-mk2", "wf-mk", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-mk2"])
	require.Equal(t, []string{"ses_mark_mid"}, *cleaned, "the superseded attempt's delete is confirmed (204)")
	require.True(t, prop.markedAtEffects("ses_mark_mid"),
		"#1915: the DAG node-retry authorized delete must mark kind=carrier BEFORE effects")
	require.Equal(t, []string{"ses_mark_mid"}, store.originDeletes,
		"the DAG leg's mark must be cleaned once the effects ran")
	require.False(t, store.carrierMarked("ses_mark_mid"), "no origin row may survive the cleanup")
}

// TestReconciler_DAGEnvelopeTeardown_MarksCarrierOriginBeforeEffects:
// the #1915 leg-2 pin (propagateDeletedSession) — BOTH envelope legs.
// An agentd-reported ephemeral teardown (deletedSessionId) runs the
// effects directly; each leg must origin-mark kind='carrier' before
// the effects and clean after, exactly like the routine path's
// propagateCarrierDelete.
func TestReconciler_DAGEnvelopeTeardown_MarksCarrierOriginBeforeEffects(t *testing.T) {
	// Delivered leg — a successful node's torn-down carrier.
	storeD, recD, _ := dagCleanupHarness(t, http.StatusNoContent)
	propD := &orderingProbePropagator{probe: storeD.carrierMarked}
	recD.SessionDelete = propD
	recD.AgentdClient = &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`), SessionID: "", DeletedSessionID: "ses_env_ok"}},
	}}
	storeD.addRun("run-mk3a", "wf-mk", "ws-1", dagAgentSpec("ephemeral", 1), json.RawMessage(`{}`), "")

	runEngine(t, recD, storeD)

	require.Equal(t, types.RunStatusSucceeded, storeD.statuses["run-mk3a"])
	require.True(t, propD.markedAtEffects("ses_env_ok"),
		"#1915: the DAG envelope delivered-leg teardown must mark kind=carrier BEFORE effects")
	require.Equal(t, []string{"ses_env_ok"}, storeD.originDeletes)
	require.False(t, storeD.carrierMarked("ses_env_ok"))

	// ErrorCode leg — a FAILED node's torn-down carrier ghosts exactly
	// like a delivered one's.
	storeE, recE, _ := dagCleanupHarness(t, http.StatusNoContent)
	propE := &orderingProbePropagator{probe: storeE.carrierMarked}
	recE.SessionDelete = propE
	recE.AgentdClient = &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", DeletedSessionID: "ses_env_err"}},
	}}
	storeE.addRun("run-mk3b", "wf-mk", "ws-1", dagAgentSpec("ephemeral", 1), json.RawMessage(`{}`), "")

	runEngine(t, recE, storeE)

	require.Equal(t, types.RunStatusFailed, storeE.statuses["run-mk3b"])
	require.True(t, propE.markedAtEffects("ses_env_err"),
		"#1915: the DAG envelope errorCode-leg teardown must mark kind=carrier BEFORE effects")
	require.Equal(t, []string{"ses_env_err"}, storeE.originDeletes)
	require.False(t, storeE.carrierMarked("ses_env_err"))
}

// TestExecuteRoutine_AuthorizedDeleteMidOutage_MarkSurvivesForReap is
// the #1915 discriminator-consequence pin: the harm window is a DB
// outage that fails the effects' index writes (the ghost row survives)
// AND the post-effects mark clean (same pool). In that window the
// kind='carrier' mark must SURVIVE — it is exactly the data the #1901
// reap bypass reads (ListCarrierOriginMarks → proxy_session_index.go
// `marks[sessionID]`) to admit the history-bearing ghost as
// reaped_carrier instead of holding it kept_for_operator. Pre-#1915 no
// mark is ever written, so the same outage strands the ghost unmarked.
func TestExecuteRoutine_AuthorizedDeleteMidOutage_MarkSurvivesForReap(t *testing.T) {
	port, deleted := newAuthorizedDeleteServer(t)
	store := newMockSchedulerStore()
	store.failOriginDeletes = true // the clean fails: same outage as the effects' index writes
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_out1"}`)
	prop := &orderingProbePropagator{probe: store.carrierMarked}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-out1", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-out1", TriggerID: "trig-out1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port,
		SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-out1"])
	require.Equal(t, []string{"ses_out1"}, *deleted)
	require.True(t, prop.markedAtEffects("ses_out1"),
		"the mark must be in place before the effects (the outage window's only guarantee)")
	require.Equal(t, []string{"ses_out1"}, store.originDeletes, "the clean ran (and failed — the outage)")
	require.True(t, store.carrierMarked("ses_out1"),
		"the kind=carrier mark must SURVIVE the failed clean — it is the reap bypass's only "+
			"admission data; without it the ghost row is kept_for_operator until operator review")
}

// TestExecuteRoutine_AuthorizedDeleteFailed_NoOriginMark is the #1915
// invariant pin: a delete the agentd route REFUSES (502 — the session
// SURVIVED, #1470 reality contract) must write no origin mark and run
// no effects — marks exist only for CONFIRMED platform deletes, never
// for live sessions (the #1901 item-b direction: nothing may widen the
// carrier class toward live/preserved sessions).
func TestExecuteRoutine_AuthorizedDeleteFailed_NoOriginMark(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_ref1"}`)
	prop := &orderingProbePropagator{probe: store.carrierMarked}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-ref1", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-ref1", TriggerID: "trig-ref1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port,
		SessionDelete: prop}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-ref1"])
	require.Empty(t, prop.calls, "a survived delete must not run effects")
	require.False(t, store.carrierMarked("ses_ref1"),
		"a survived delete must never write a carrier mark — the carrier class admits only "+
			"CONFIRMED platform deletes (#1901 item-b direction)")
	preserved, ok := store.sessionOrigins["ses_ref1"]
	require.True(t, ok, "the survived session keeps its completion artifacts path")
	require.Equal(t, types.SessionOriginKindSession, preserved.Kind,
		"the failed-delete survivor's mark stays kind=session (never reap-admitted)")
	require.Empty(t, store.originDeletes, "a preserved session's mark is never cleaned")
}

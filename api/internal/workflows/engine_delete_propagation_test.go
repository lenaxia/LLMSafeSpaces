// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1882 delete propagation: EVERY platform-initiated session delete —
// agentd's ephemeral carrier teardown, the scheduler's authorized
// deletes (PreserveOnFailure success, retry-intermediate), and the DAG
// reconciler's node-retry cleanup — must run the same delete effects
// as the user proxy route (session.status:deleted SSE, wsstate
// tombstone, session_index row removal). Before #1882 only the user
// route propagated; every routine fire left a ghost sidebar row minted
// by the live event bridge during the carrier's brief life.

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

// recordingDeleteEffects captures the engine's delete-effect calls.
type recordingDeleteEffects struct {
	mu     sync.Mutex
	called []deleteEffectCall
}

type deleteEffectCall struct {
	workspaceID, sessionID string
}

func (r *recordingDeleteEffects) ApplySessionDeleteEffects(_ context.Context, workspaceID, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = append(r.called, deleteEffectCall{workspaceID, sessionID})
}

func (r *recordingDeleteEffects) calls() []deleteEffectCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deleteEffectCall(nil), r.called...)
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
	effects := &recordingDeleteEffects{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp1", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp1", TriggerID: "trig-dp1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		DeleteEffects: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-dp1"])
	got := effects.calls()
	require.Len(t, got, 1, "the ephemeral carrier delete must propagate exactly one effects call")
	require.Equal(t, deleteEffectCall{workspaceID: "ws-1", sessionID: "ses_carrier_1"}, got[0])
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
	effects := &recordingDeleteEffects{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp2", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp2", TriggerID: "trig-dp2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		DeleteEffects: effects}

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
	effects := &recordingDeleteEffects{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp3", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp3", TriggerID: "trig-dp3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		DeleteEffects: effects}

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
	effects := &recordingDeleteEffects{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp4", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-dp4", TriggerID: "trig-dp4", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-dp"},
		AgentdPort:       port,
		DeleteEffects:    effects}

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
	effects := &recordingDeleteEffects{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp5", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-dp5", TriggerID: "trig-dp5", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-dp"},
		AgentdPort:       port,
		DeleteEffects:    effects}

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
		DeleteEffects: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	require.NoError(t, index.Stop())

	require.Equal(t, "delivered", store.statuses["fire-e2e"])
	db.AssertCalled(t, "MarkSessionIndexDeleted", mock.Anything, "ws-1", "ses_e2e_carrier")
	db.AssertCalled(t, "DeleteSessionTree", mock.Anything, "ws-1", "ses_e2e_carrier")
}

// indexDeleteEffects is the test mirror of the production
// ProxyHandler.ApplySessionDeleteEffects index half.
type indexDeleteEffects struct {
	index *sessionindex.Service
}

func (e *indexDeleteEffects) ApplySessionDeleteEffects(ctx context.Context, workspaceID, sessionID string) {
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
	effects := &recordingDeleteEffects{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-dp7", Name: "Watch", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-dp7", TriggerID: "trig-dp7", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		DeleteEffects: effects}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-dp7"])
	got := effects.calls()
	require.Len(t, got, 2, "the discarded attempt's carrier and the final attempt's carrier each propagate")
	require.Equal(t, deleteEffectCall{workspaceID: "ws-1", sessionID: "ses_carrier_mid"}, got[0],
		"the discarded retry attempt's carrier propagates first (before the retry dispatches)")
	require.Equal(t, deleteEffectCall{workspaceID: "ws-1", sessionID: "ses_carrier_final"}, got[1],
		"the final attempt's carrier propagates from the returned response")
	require.ElementsMatch(t, []string{"ses_carrier_mid", "ses_carrier_final"}, store.originDeletes,
		"each carrier's origin mark is cleaned")
}

// TestExecuteNode_SupersededAttemptCleanup_PropagatesEffects: the DAG
// node-retry authorized delete (#1476) must run the delete effects —
// before #1882 the cleaned orphan's index row lingered as a ghost.
func TestExecuteNode_SupersededAttemptCleanup_PropagatesEffects(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	effects := &recordingDeleteEffects{}
	rec.DeleteEffects = effects
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
	require.Equal(t, deleteEffectCall{workspaceID: "ws-1", sessionID: "ses_dag_eff"}, got[0])
}

// TestExecuteNode_DAGAgentEphemeral_PropagatesEffects: a workflow DAG
// agent node in ephemeral mode (agentd tears the carrier down) reports
// deletedSessionId — the reconciler must run the effects for it.
func TestExecuteNode_DAGAgentEphemeral_PropagatesEffects(t *testing.T) {
	store, rec, _ := dagCleanupHarness(t, http.StatusNoContent)
	effects := &recordingDeleteEffects{}
	rec.DeleteEffects = effects
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`), SessionID: "", DeletedSessionID: "ses_dag_eph"}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag-eph", "wf-dag", "ws-1", dagAgentSpec("ephemeral", 1), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag-eph"])
	got := effects.calls()
	require.Len(t, got, 1, "a DAG ephemeral carrier teardown must propagate effects")
	require.Equal(t, deleteEffectCall{workspaceID: "ws-1", sessionID: "ses_dag_eph"}, got[0])
}

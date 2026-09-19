// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1476: executeWithRetry discards superseded attempts' responses —
// including the sessionId a preserved-mode failure envelope reports
// (#1471). The intermediate session then exists in opencode forever:
// unrecorded, unindexed, never deleted. The retry loop is the only
// place that knows a response is being DISCARDED, so it owns the
// cleanup: when a retry will actually follow and the discarded response
// carries a session, the loop hands it to the caller's cleaner before
// re-dispatching. Final-attempt failures are never cleaned — the #1470
// contract keeps them for inspection.

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

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// seqStep is one scripted agentd outcome in sequenceAgentd.
type seqStep struct {
	resp *NodeExecResponse
	err  error
}

// sequenceAgentd serves scripted outcomes in order (retry-loop tests
// need per-ATTEMPT responses, which the NodeID-keyed mock cannot do).
type sequenceAgentd struct {
	mu    sync.Mutex
	steps []seqStep
	calls int
}

func (s *sequenceAgentd) Execute(_ context.Context, _, _ string, _ *NodeExecRequest) (*NodeExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.steps) {
		return nil, errors.New("sequenceAgentd: no step scripted")
	}
	step := s.steps[s.calls]
	s.calls++
	if step.resp != nil {
		cp := *step.resp
		return &cp, nil
	}
	return nil, step.err
}

func retryableFailure(sessionID string) *NodeExecResponse {
	return &NodeExecResponse{
		ErrorCode: "script_failed",
		Detail:    "opencode returned 500",
		SessionID: sessionID,
	}
}

func TestExecuteWithRetry_CleansSupersededAttemptSession(t *testing.T) {
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_mid1")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok","session_id":"ses_final"}`), SessionID: "ses_final"}},
	}}
	var cleaned []string
	cleanup := func(_ context.Context, sessionID string) bool {
		cleaned = append(cleaned, sessionID)
		return true
	}

	resp, err := executeWithRetry(context.Background(), ex, "ws-1", "10.0.0.1",
		&NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err)
	require.Equal(t, "ses_final", resp.SessionID)
	require.Equal(t, []string{"ses_mid1"}, cleaned, "the superseded attempt's session must be cleaned exactly once")
}

func TestExecuteWithRetry_FinalAttemptFailureNotCleaned(t *testing.T) {
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_mid1")},
		{resp: retryableFailure("ses_mid2")},
		{resp: retryableFailure("ses_final")},
	}}
	var cleaned []string
	cleanup := func(_ context.Context, sessionID string) bool {
		cleaned = append(cleaned, sessionID)
		return true
	}

	resp, err := executeWithRetry(context.Background(), ex, "ws-1", "10.0.0.1",
		&NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err, "an error-code response is a returned response, not a transport error")
	require.Equal(t, "script_failed", resp.ErrorCode)
	require.Equal(t, "ses_final", resp.SessionID)
	require.Equal(t, []string{"ses_mid1", "ses_mid2"}, cleaned,
		"final-attempt failure keeps its session (#1470 contract); only superseded attempts are cleaned")
}

func TestExecuteWithRetry_NonRetryableFailureNotCleaned(t *testing.T) {
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "schema_mismatch", Detail: "bad json", SessionID: "ses_keep"}},
	}}
	var cleaned []string
	cleanup := func(_ context.Context, sessionID string) bool {
		cleaned = append(cleaned, sessionID)
		return true
	}

	resp, err := executeWithRetry(context.Background(), ex, "ws-1", "10.0.0.1",
		&NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err)
	require.Equal(t, "schema_mismatch", resp.ErrorCode)
	require.Empty(t, cleaned, "no retry follows — the response is final, nothing is superseded")
}

func TestExecuteWithRetry_TransportErrorNoSessionToClean(t *testing.T) {
	ex := &sequenceAgentd{steps: []seqStep{
		{err: errors.New("agentd node execute returned 500: boom")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok"}`)}},
	}}
	var cleaned []string
	cleanup := func(_ context.Context, sessionID string) bool {
		cleaned = append(cleaned, sessionID)
		return true
	}

	_, err := executeWithRetry(context.Background(), ex, "ws-1", "10.0.0.1",
		&NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err)
	require.Empty(t, cleaned, "transport errors carry no response object — no session to clean (the #1470 blind residual)")
}

func TestExecuteWithRetry_NilCleanupNoPanic(t *testing.T) {
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_mid1")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok"}`)}},
	}}

	require.NotPanics(t, func() {
		_, _ = executeWithRetry(context.Background(), ex, "ws-1", "10.0.0.1",
			&NodeExecRequest{NodeID: "n1"}, nil)
	})
}

// TestExecuteRoutine_RetryIntermediateCleanedViaAuthorizedDelete wires
// the full routine path: attempt 1 fails transiently (preserved session
// ses_mid in the envelope), attempt 2 delivers — the intermediate must
// reach the authorized delete route and ONLY the final session must be
// recorded + indexed.
func TestExecuteRoutine_RetryIntermediateCleanedViaAuthorizedDelete(t *testing.T) {
	var deleted []string
	var deleteMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workflow/session/delete", r.URL.Path)
		deleteMu.Lock()
		deleted = append(deleted, r.URL.Query().Get("sessionId"))
		deleteMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_mid")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok","session_id":"ses_final"}`), SessionID: "ses_final"}},
	}}
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-rl1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-rl1", TriggerID: "trig-rl1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port, SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-rl1"])
	require.Equal(t, []string{"ses_mid"}, deleted, "the superseded attempt's session must reach the authorized delete route")
	require.Len(t, store.sessionOrigins, 1, "only the final attempt's session is recorded")
	require.Equal(t, "ses_final", index.titleCalls()[0].sessionID)
}

// TestExecuteRoutine_RetryIntermediateCleanupFails_StillDelivers: a
// 502 from the delete route (session survived, #1471 contract) must
// not break the retry — the fire still delivers and records its final
// session; the leaked intermediate is logged, not fatal. The route hit
// is recorded so the test distinguishes "cleanup attempted and 502'd"
// from "cleanup never wired".
func TestExecuteRoutine_RetryIntermediateCleanupFails_StillDelivers(t *testing.T) {
	var deleteHits []string
	var deleteMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleteMu.Lock()
		deleteHits = append(deleteHits, r.URL.Query().Get("sessionId"))
		deleteMu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_leak")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok","session_id":"ses_final"}`), SessionID: "ses_final"}},
	}}
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-rl2", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-rl2", TriggerID: "trig-rl2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port, SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, "delivered", store.statuses["fire-rl2"], "cleanup failure must never fail the fire")
	require.Equal(t, []string{"ses_leak"}, deleteHits, "the cleanup WAS attempted — the 502 is a failed delete, not missing wiring")
	require.Len(t, store.sessionOrigins, 1)
	require.Equal(t, "ses_final", index.titleCalls()[0].sessionID)
}

// TestExecuteRoutine_TimeoutEnvelope_RecordsSurvivingSession_Composition
// pins the Option-A composition end-to-end at the engine: a timed-out
// agent turn now arrives as a 200+errorCode envelope carrying the
// surviving session — the ErrorCode branch records it, the fire fails
// with the standard payload, and the classifier does NOT retry
// script_timeout (single dispatch).
func TestExecuteRoutine_TimeoutEnvelope_RecordsSurvivingSession_Composition(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_timeout", Detail: "agent call timed out", SessionID: "ses_to1"}},
	}}
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-to1", Name: "Watchdog", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-to1", TriggerID: "trig-to1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		SessionIndex: index}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	require.Equal(t, 1, agentd.calls, "script_timeout is non-retryable — exactly one dispatch")
	require.Equal(t, "failed", store.statuses["fire-to1"])
	require.Len(t, store.sessionOrigins, 1, "the timed-out fire's surviving session is recorded")
	require.Equal(t, "ses_to1", index.titleCalls()[0].sessionID)
}

// TestExecuteWithRetry_RetryableWithoutSession_NoCleanup pins the
// remaining guard half explicitly: a retryable envelope that carries no
// session (old-agentd skew, or session_create_failed — no session was
// created) retries without invoking the cleaner.
func TestExecuteWithRetry_RetryableWithoutSession_NoCleanup(t *testing.T) {
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok"}`)}},
	}}
	var cleaned []string
	cleanup := func(_ context.Context, sessionID string) bool {
		cleaned = append(cleaned, sessionID)
		return true
	}

	_, err := executeWithRetry(context.Background(), ex, "ws-1", "10.0.0.1",
		&NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err)
	require.Empty(t, cleaned, "a retryable envelope with no session has nothing to clean")
}

// TestExecuteWithRetry_CtxCanceledAfterCleanup_ScrubsCleanedSession is
// the round-3 F1 pin: the context dying during the post-cleanup backoff
// makes the CLEANED attempt's response final — and a session the loop
// itself deleted must never ride that response (#1471 existence
// contract), or the engine ghost-records it (the async session_index
// write is ctx-free and always lands). The cleaner cancels the context,
// so the window is deterministic — no select race.
func TestExecuteWithRetry_CtxCanceledAfterCleanup_ScrubsCleanedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_mid")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ok"}`)}}, // never reached
	}}
	var cleaned []string
	cleanup := func(_ context.Context, sessionID string) bool {
		cleaned = append(cleaned, sessionID)
		cancel()
		return true
	}

	resp, err := executeWithRetry(ctx, ex, "ws-1", "10.0.0.1", &NodeExecRequest{NodeID: "n1"}, cleanup)

	require.Equal(t, []string{"ses_mid"}, cleaned, "the cleanup ran before cancellation")
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "script_failed", resp.ErrorCode, "the cleaned attempt's failure is the final outcome")
	require.Empty(t, resp.SessionID, "a session the loop deleted must not be returned as surviving")
}

// TestExecuteWithRetry_CtxCanceledAfterFailedCleanup_KeepsSession pins
// the other half of the scrub rule: a cleanup that did NOT confirm the
// delete (session still exists) keeps the id on the canceled-context
// return — the engine records a real survivor, never drops a live one.
func TestExecuteWithRetry_CtxCanceledAfterFailedCleanup_KeepsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_live")},
	}}
	cleanup := func(_ context.Context, _ string) bool {
		cancel()
		return false // delete did not confirm — the session exists
	}

	resp, err := executeWithRetry(ctx, ex, "ws-1", "10.0.0.1", &NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err)
	require.Equal(t, "ses_live", resp.SessionID, "a live session keeps its id — dropping it would hide a real survivor")
}

// TestExecuteRoutine_CtxCanceledAfterCleanup_NoGhostRecord pins the
// full-path F1 consequence: a canceled context between the successful
// cleanup and the backoff returns the scrubbed response — the engine's
// ErrorCode branch sees NO session id: no origin row, no index write
// for the deleted session. Cancellation fires ~100ms after the
// (microsecond-scale) cleanup and ~2s before the backoff elapses —
// deterministic by three orders of magnitude.
func TestExecuteRoutine_CtxCanceledAfterCleanup_NoGhostRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_ghost")},
	}}
	index := &recordingSessionIndex{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-gh1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-gh1", TriggerID: "trig-gh1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}, AgentdPort: port, SessionIndex: index}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	sched.executeRoutine(ctx, noopLogger{}, trigger, fire)

	require.Equal(t, "failed", store.statuses["fire-gh1"])
	require.Empty(t, store.sessionOrigins, "no ghost origin for the deleted session")
	require.Empty(t, index.titleCalls(), "no ghost index row — the scrubbed response carries no session id")
}

// TestExecuteWithRetry_CtxCanceledAfterFailedCleanup_MultiAttempt is
// the round-4 F1′ pin: attempt 1's confirmed delete (S1 gone) must not
// scrub attempt 2's LIVE survivor when its own cleanup failed (the
// honest-502 shape) and the context dies during attempt 2's backoff —
// the returned response keeps S2's id so the engine records the real
// survivor, never orphans it.
func TestExecuteWithRetry_CtxCanceledAfterFailedCleanup_MultiAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: retryableFailure("ses_s1")},
		{resp: retryableFailure("ses_s2")},
	}}
	gone := map[string]bool{"ses_s1": true, "ses_s2": false}
	cleanup := func(_ context.Context, sessionID string) bool {
		if sessionID == "ses_s2" {
			cancel()
		}
		return gone[sessionID]
	}

	resp, err := executeWithRetry(ctx, ex, "ws-1", "10.0.0.1", &NodeExecRequest{NodeID: "n1"}, cleanup)

	require.NoError(t, err)
	require.Equal(t, "ses_s2", resp.SessionID,
		"a prior attempt's confirmed delete must not scrub THIS live survivor — the engine must record it")
	require.Equal(t, "script_failed", resp.ErrorCode)
}

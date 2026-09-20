// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1476 DAG adoption: executeNode's maxAttempts loop supersedes failed
// attempts exactly like executeWithRetry did pre-#1477 — for agent
// nodes whose spec mints a fresh session per dispatch (session:"new"),
// every superseded attempt's preserved session leaks as an invisible
// orphan. The #1477 cleanup pattern applies with two DAG-specific gates
// (Rule-7 enumeration in the worklog): node retries are UNCONDITIONAL
// (any failed non-final attempt is superseded, deterministic errors
// included), and spec-PINNED sessions (data.sessionId) are shared
// across attempts by authorial intent — never cleaned.

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
)

// dagCleanupHarness wires a Reconciler whose delete route records
// cleaned session ids (204 = gone), returning the recorder.
func dagCleanupHarness(t *testing.T, deleteStatus int) (*mockStore, *Reconciler, *[]string) {
	t.Helper()
	var cleaned []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workflow/session/delete" { // goroutine-safe: FailNow is not
			t.Errorf("delete route hit unexpected path %s", r.URL.Path)
		}
		mu.Lock()
		cleaned = append(cleaned, r.URL.Query().Get("sessionId"))
		mu.Unlock()
		w.WriteHeader(deleteStatus)
	}))
	t.Cleanup(srv.Close)
	_, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockStore()
	rec := &Reconciler{
		Store:            store,
		Activator:        &loopbackActivator{},
		Logger:           noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-dag"},
		AgentdPort:       port,
	}
	rec.canceledRuns = make(map[string]struct{})
	return store, rec, &cleaned
}

func dagAgentSpec(session string, maxAttempts int) json.RawMessage {
	return json.RawMessage(`{"nodes":[{"id":"a1","type":"agent","data":{"agent":"build","prompt":"hi","session":"` + session + `"},"maxAttempts":` + strconv.Itoa(maxAttempts) + `}],"edges":[]}`)
}

func dagAgentSpecPinned(sessionID string, maxAttempts int) json.RawMessage {
	return json.RawMessage(`{"nodes":[{"id":"a1","type":"agent","data":{"agent":"build","prompt":"hi","sessionId":"` + sessionID + `"},"maxAttempts":` + strconv.Itoa(maxAttempts) + `}],"edges":[]}`)
}

// TestExecuteNode_SupersededAttemptSessionCleaned: session:"new",
// transient first attempt — the superseded session reaches the
// authorized delete route; the run still succeeds on attempt 2.
func TestExecuteNode_SupersededAttemptSessionCleaned(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_dag_mid"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`), SessionID: "ses_dag_final"}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag1", "wf-dag", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag1"])
	require.Equal(t, []string{"ses_dag_mid"}, *cleaned,
		"the superseded attempt's session is cleaned; the FINAL SUCCESS session survives — nothing records DAG sessions, it is the author's only artifact")
}

// TestExecuteNode_DeterministicErrorIntermediateAlsoCleaned pins the
// breadth difference vs the routine path: node retries are
// UNCONDITIONAL — a schema_mismatch attempt 1 is just as superseded as
// a transient 5xx, and its orphan session is cleaned too.
func TestExecuteNode_DeterministicErrorIntermediateAlsoCleaned(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "schema_mismatch", Detail: "bad json", SessionID: "ses_dag_det"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`)}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag2", "wf-dag", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag2"])
	require.Equal(t, []string{"ses_dag_det"}, *cleaned,
		"the retry supersedes deterministic failures too — the orphan is real regardless of the error class")
}

// TestExecuteNode_FinalAttemptSessionNotCleaned: all attempts fail —
// the FINAL failure's session survives (nothing supersedes it; the
// #1470 keep-on-final-failure contract).
func TestExecuteNode_FinalAttemptSessionNotCleaned(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_d1"}},
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_d2"}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag3", "wf-dag", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusFailed, store.statuses["run-dag3"])
	require.Equal(t, []string{"ses_d1"}, *cleaned, "only the superseded attempt is cleaned; the final failure keeps its session")
}

// TestExecuteNode_PinnedSessionNeverCleaned: data.sessionId is the
// author's SHARED session — every attempt reuses it; cleaning any
// attempt's envelope id would delete the live shared session.
func TestExecuteNode_PinnedSessionNeverCleaned(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_shared"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`)}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag4", "wf-dag", "ws-1", dagAgentSpecPinned("ses_shared", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag4"])
	require.Empty(t, *cleaned, "a spec-pinned session is caller-managed — never cleaned")
}

// TestExecuteNode_EphemeralNoSessionNoop: ephemeral specs tear down
// agent-side (#1471) — no envelope id, no cleanup call.
func TestExecuteNode_EphemeralNoSessionNoop(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`)}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag5", "wf-dag", "ws-1", dagAgentSpec("ephemeral", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag5"])
	require.Empty(t, *cleaned)
}

// TestExecuteNode_NilPasswordProviderSkipsCleanup: unwired reconcilers
// (embedded use, older construction sites) keep today's behavior —
// no cleanup, run outcome unaffected.
func TestExecuteNode_NilPasswordProviderSkipsCleanup(t *testing.T) {
	// The call-site nil-gate is a NOISE guard, and its removal's only
	// observable is the LOG: deleteSessionAuthorized's internal nil
	// check returns before any dial, so a route hit is structurally
	// impossible either way (r2's mutation receipts). With the gate, an
	// unwired reconciler logs NOTHING; without it, every superseded
	// attempt emits "session delete: no password provider configured".
	// The discriminating pin is the captured Error log + zero route
	// hits + unchanged outcome.
	store, rec, cleaned := dagCleanupHarness(t, http.StatusNoContent)
	var errLogs []string
	red := &errorOnlyLogger{errs: &errLogs} // Info excluded: the run-success line is not the observable
	rec.PasswordProvider = nil
	rec.AgentdClient = &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_np"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`)}},
	}}
	store.addRun("run-dag6", "wf-dag", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	// Drive executeRun with the capturing logger (runEngine hardcodes
	// noopLogger — the observable lives in the logger executeNode
	// receives).
	ctx := context.Background()
	runs, _ := store.ClaimQueuedRuns(ctx, 10)
	for _, run := range runs {
		rec.executeRun(ctx, red, run)
	}

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag6"], "no cleanup wiring, no outcome change")
	require.Empty(t, *cleaned, "an unwired reconciler must never reach the delete route")
	require.Empty(t, errLogs, "the gate suppresses the per-attempt no-provider error log — its removal surfaces HERE, not as a route hit")
}

// TestExecuteNode_CleanupFailureRetriesAnyway: a 502 delete (session
// survived, #1471 honest route) is logged best-effort — the retry
// proceeds and the run succeeds.
func TestExecuteNode_CleanupFailureRetriesAnyway(t *testing.T) {
	store, rec, cleaned := dagCleanupHarness(t, http.StatusBadGateway)
	ex := &sequenceAgentd{steps: []seqStep{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500", SessionID: "ses_leak_dag"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"ok":1}`)}},
	}}
	rec.AgentdClient = ex
	store.addRun("run-dag7", "wf-dag", "ws-1", dagAgentSpec("new", 2), json.RawMessage(`{}`), "")

	runEngine(t, rec, store)

	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-dag7"], "cleanup failure must never fail the node")
	require.Equal(t, []string{"ses_leak_dag"}, *cleaned, "the cleanup was attempted — best-effort, not silent")
}

// errorOnlyLogger captures Error lines only (the noise-guard observable);
// Info is the run's normal chatter and is not under assertion.
type errorOnlyLogger struct{ errs *[]string }

func (e *errorOnlyLogger) Info(string, ...any)                 {}
func (e *errorOnlyLogger) Error(_ error, msg string, _ ...any) { *e.errs = append(*e.errs, msg) }

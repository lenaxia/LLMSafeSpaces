// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// engine_identity_test.go — #1327: the logical-execution identity
// (workflow/run) must ride every agentd dispatch that can reach a
// harness transcript write, so agentd can derive its dedupe key
// (msg_wf_<workflow>_<node>_<run>). Workflow retries re-dispatch the
// same run/node pair (engine.go's attempt loop) and pending-fire
// re-drives re-execute the same trigger/fire pair — the identity is
// exactly the retry-stable, execution-distinct tuple the key needs.
// These tests fail against pre-fix code by construction: the fields
// they assert on did not exist.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// identityRecorder captures every NodeExecRequest dispatched through
// the AgentdExecutor seam.
type identityRecorder struct {
	mu   sync.Mutex
	reqs []*NodeExecRequest
}

func (r *identityRecorder) Execute(_ context.Context, _, _ string, req *NodeExecRequest) (*NodeExecResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return &NodeExecResponse{Output: json.RawMessage(`{"response":"ok","session_id":""}`)}, nil
}

func (r *identityRecorder) byNode(nodeID string) *NodeExecRequest {
	for _, req := range r.all() {
		if req.NodeID == nodeID {
			return req
		}
	}
	return nil
}

func (r *identityRecorder) all() []*NodeExecRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*NodeExecRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func TestExecuteNode_DispatchesRunIdentity(t *testing.T) {
	store := newMockStore()
	rec := &identityRecorder{}
	spec := json.RawMessage(`{"nodes":[{"id":"a1","type":"agent","data":{"agent":"build","prompt":"hi"}}],"edges":[]}`)
	store.addRun("run-1", "wf-1", "ws-1", spec, json.RawMessage(`{}`), "")

	r := &Reconciler{Store: store, AgentdClient: rec, Activator: &mockActivator{}, Logger: noopLogger{}}
	r.canceledRuns = make(map[string]struct{})
	runEngine(t, r, store)

	req := rec.byNode("a1")
	require.NotNil(t, req, "agent node must have been dispatched")
	require.Equal(t, "wf-1", req.WorkflowID, "dispatch must carry the run's workflow ID")
	require.Equal(t, "run-1", req.RunID, "dispatch must carry the run ID")
}

// TestExecuteNode_RetryStableIdentity pins the retry-stability half of
// the key contract: the engine's attempt loop re-dispatches the SAME
// run/node identity, so agentd derives the SAME key and the harness
// upsert bounds the transcript to one message per logical execution.
func TestExecuteNode_RetryStableIdentity(t *testing.T) {
	store := newMockStore()
	rec := &identityRecorder{}
	// maxAttempts 2 with a failing first attempt: mockAgentd-style
	// per-node errors are not enough here — the recorder always
	// succeeds, so drive executeNode's retry leg through the error-code
	// response path instead.
	fail := &failingThenSucceedingExecutor{rec: rec, failFirstN: 1}
	spec := json.RawMessage(`{"nodes":[{"id":"a1","type":"agent","data":{"agent":"build","prompt":"hi"},"maxAttempts":2}],"edges":[]}`)
	store.addRun("run-r", "wf-r", "ws-1", spec, json.RawMessage(`{}`), "")

	r := &Reconciler{Store: store, AgentdClient: fail, Activator: &mockActivator{}, Logger: noopLogger{}}
	r.canceledRuns = make(map[string]struct{})
	runEngine(t, r, store)

	require.Equal(t, 2, fail.calls(), "both attempts must dispatch")
	require.Equal(t, types.RunStatusSucceeded, store.statuses["run-r"])
	first, second := fail.reqAt(0), fail.reqAt(1)
	require.Equal(t, first.WorkflowID, second.WorkflowID)
	require.Equal(t, first.RunID, second.RunID)
	require.Equal(t, "wf-r", second.WorkflowID)
	require.Equal(t, "run-r", second.RunID)
}

// failingThenSucceedingExecutor fails the first N dispatches with the
// in-band error-code shape (the engine's retry leg), recording every
// request it saw; later dispatches delegate to rec.
type failingThenSucceedingExecutor struct {
	rec        *identityRecorder
	failFirstN int
	mu         sync.Mutex
	failCalls  int
	failedReqs []*NodeExecRequest
}

func (f *failingThenSucceedingExecutor) calls() int {
	f.mu.Lock()
	failed := f.failCalls
	f.mu.Unlock()
	return failed + len(f.rec.all())
}

func (f *failingThenSucceedingExecutor) reqAt(i int) *NodeExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.failedReqs) {
		return f.failedReqs[i]
	}
	return f.rec.reqs[i-len(f.failedReqs)]
}

func (f *failingThenSucceedingExecutor) Execute(ctx context.Context, workspaceID, podIP string, req *NodeExecRequest) (*NodeExecResponse, error) {
	f.mu.Lock()
	if f.failCalls < f.failFirstN {
		f.failCalls++
		probe := *req
		f.failedReqs = append(f.failedReqs, &probe)
		f.mu.Unlock()
		return &NodeExecResponse{ErrorCode: "script_failed", Detail: "simulated transient failure"}, nil
	}
	f.mu.Unlock()
	return f.rec.Execute(ctx, workspaceID, podIP, req)
}

// TestExecuteRoutine_DispatchesFireIdentity: the routine path shares
// the harness write (routine-agent) and its own retry surface
// (processPendingRoutineFire re-drives pending fires), so it carries
// the same identity tuple — trigger (the reusable definition) + fire
// (the logical execution). The routine-script dispatch carries NO
// identity: script nodes never write to the harness transcript, so the
// key has no consumer there.
func TestExecuteRoutine_DispatchesFireIdentity(t *testing.T) {
	store := newMockSchedulerStore()
	rec := &identityRecorder{}
	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-r1", WorkspaceID: &wsID, Prompt: "test",
		ScriptPath: "/routines/pre.py", CaptureMode: types.CaptureFull,
		PreserveSession: types.PreserveNever,
	}
	fire := &wf.TriggerFireRow{ID: "fire-1", TriggerID: "trig-r1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: rec, Logger: noopLogger{}}

	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	agentReq := rec.byNode("routine-agent")
	require.NotNil(t, agentReq, "routine agent node must have been dispatched")
	require.Equal(t, "trig-r1", agentReq.WorkflowID, "routine identity carries the trigger (definition) ID")
	require.Equal(t, "fire-1", agentReq.RunID, "routine identity carries the fire (execution) ID")

	scriptReq := rec.byNode("routine-script")
	require.NotNil(t, scriptReq, "ScriptPath set — the pre-script must run")
	require.Empty(t, scriptReq.WorkflowID, "script dispatches carry no identity (no harness write)")
	require.Empty(t, scriptReq.RunID, "script dispatches carry no identity (no harness write)")

	require.Equal(t, "delivered", store.statuses["fire-1"])
}

// TestHTTPAgentExecutor_MarshalsIdentityFields pins the WIRE field
// names: a struct-tag rename (workflowId → anything else) would
// silently drop the identity agentd decodes, and the dedupe key with
// it. Decoding the raw body asserts the exact JSON keys.
func TestHTTPAgentExecutor_MarshalsIdentityFields(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	host, port := testServerAddr(t, srv)

	exec := &HTTPAgentExecutor{Port: port, Client: srv.Client(), PasswordProvider: &stubPasswordProvider{password: "pw"}}
	_, err := exec.Execute(context.Background(), "ws-1", host, &NodeExecRequest{
		NodeID: "n1", NodeType: "agent",
		WorkflowID: "wf-9", RunID: "run-9",
	})
	require.NoError(t, err)
	require.Equal(t, "wf-9", got["workflowId"], "wire key must be workflowId (agentd decodes that spelling)")
	require.Equal(t, "run-9", got["runId"], "wire key must be runId (agentd decodes that spelling)")
}

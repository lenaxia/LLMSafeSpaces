package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

type mockReconcilerStore struct {
	mu          sync.Mutex
	claimed     []*wf.WorkflowRunRow
	statuses    map[string]string
	nodeRuns    []*wf.WorkflowNodeRunRow
	triggerFail map[string]int
	runUpdates  map[string]int
}

func newMockReconcilerStore() *mockReconcilerStore {
	return &mockReconcilerStore{
		statuses:    make(map[string]string),
		triggerFail: make(map[string]int),
		runUpdates:  make(map[string]int),
	}
}

func (m *mockReconcilerStore) ClaimQueuedRuns(_ context.Context, limit int) ([]*wf.WorkflowRunRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := m.claimed
	m.claimed = nil
	return runs, nil
}

func (m *mockReconcilerStore) UpdateWorkflowRunStatus(_ context.Context, runID, status string, ec *string, _ json.RawMessage, _ json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[runID] = status
	m.runUpdates[runID]++
	return nil
}

func (m *mockReconcilerStore) CreateNodeRun(_ context.Context, row *wf.WorkflowNodeRunRow) error {
	m.nodeRuns = append(m.nodeRuns, row)
	return nil
}

func (m *mockReconcilerStore) UpdateNodeRunStatus(_ context.Context, nodeRunID, status string, _ json.RawMessage, _ *string, _ *string, _ json.RawMessage) error {
	return nil
}

func (m *mockReconcilerStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggerFail[triggerID]++
	return m.triggerFail[triggerID], nil
}

func (m *mockReconcilerStore) ResetTriggerFailures(_ context.Context, triggerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggerFail[triggerID] = 0
	return nil
}

func (m *mockReconcilerStore) addRun(id, workflowID, workspaceID string, spec json.RawMessage, input json.RawMessage, triggerID string) *wf.WorkflowRunRow {
	run := &wf.WorkflowRunRow{
		ID: id, WorkflowID: workflowID, WorkspaceID: workspaceID,
		SpecSnapshot: spec, Input: input, Status: "queued",
	}
	if triggerID != "" {
		run.TriggerID = &triggerID
	}
	m.claimed = append(m.claimed, run)
	return run
}

type mockAgentdExecutor struct {
	outputs  map[string]json.RawMessage
	branches map[string]string
	errors   map[string]error
	errCodes map[string]string
}

func newMockAgentdExecutor() *mockAgentdExecutor {
	return &mockAgentdExecutor{
		outputs:  make(map[string]json.RawMessage),
		branches: make(map[string]string),
		errors:   make(map[string]error),
		errCodes: make(map[string]string),
	}
}

func (m *mockAgentdExecutor) Execute(_ context.Context, _ string, req *NodeExecRequest) (*NodeExecResponse, error) {
	if err, ok := m.errors[req.NodeID]; ok {
		return nil, err
	}
	resp := &NodeExecResponse{
		Output: m.outputs[req.NodeID],
	}
	if code, ok := m.errCodes[req.NodeID]; ok {
		resp.ErrorCode = code
		resp.Detail = "simulated error"
	}
	if br, ok := m.branches[req.NodeID]; ok {
		resp.Branch = br
	}
	return resp, nil
}

type mockActivator struct{ fail bool }

func (m *mockActivator) EnsureActive(_ context.Context, _ string, _ time.Duration) (string, error) {
	if m.fail {
		return "", fmt.Errorf("workspace activation failed")
	}
	return "10.0.0.1", nil
}

func makeLinearSpec() json.RawMessage {
	return json.RawMessage(`{
		"nodes": [
			{"id":"start","type":"script","data":{"language":"python","handler":"x"}},
			{"id":"end","type":"script","data":{"language":"python","handler":"y"}}
		],
		"edges": [{"source":"start","target":"end"}]
	}`)
}

func makeConditionSpec() json.RawMessage {
	return json.RawMessage(`{
		"nodes": [
			{"id":"start","type":"script","data":{"language":"python","handler":"x"}},
			{"id":"choice","type":"condition","data":{"conditions":[{"id":"skip","expression":"input.skipped == true"}]}},
			{"id":"skip-path","type":"script","data":{"language":"python","handler":"z"}},
			{"id":"else-path","type":"script","data":{"language":"python","handler":"w"}}
		],
		"edges": [
			{"source":"start","target":"choice"},
			{"source":"choice","target":"skip-path","sourceHandle":"skip"},
			{"source":"choice","target":"else-path","sourceHandle":"otherwise"}
		]
	}`)
}

func runReconciler(t *testing.T, rec *WorkflowReconciler, store *mockReconcilerStore) {
	t.Helper()
	ctx := context.Background()
	runs, _ := store.ClaimQueuedRuns(ctx, 10)
	for _, run := range runs {
		rec.executeRun(ctx, noopLogger{}, run)
	}
}

func TestReconciler_HappyPath(t *testing.T) {
	store := newMockReconcilerStore()
	agentd := newMockAgentdExecutor()
	agentd.outputs["start"] = json.RawMessage(`{"x":1}`)
	agentd.outputs["end"] = json.RawMessage(`{"result":"done"}`)

	spec := makeLinearSpec()
	store.addRun("run-1", "wf-1", "ws-1", spec, json.RawMessage(`{}`), "")

	rec := &WorkflowReconciler{
		Store: store, AgentdClient: agentd, K8sClient: &mockActivator{},
		Logger: noopLogger{}, canceledRuns: make(map[string]struct{}),
	}

	runReconciler(t, rec, store)

	if store.statuses["run-1"] != types.RunStatusSucceeded {
		t.Errorf("expected succeeded, got %s", store.statuses["run-1"])
	}
}

func TestReconciler_WorkspaceUnavailable(t *testing.T) {
	store := newMockReconcilerStore()
	agentd := newMockAgentdExecutor()

	spec := makeLinearSpec()
	store.addRun("run-2", "wf-1", "ws-bad", spec, json.RawMessage(`{}`), "")

	rec := &WorkflowReconciler{
		Store: store, AgentdClient: agentd, K8sClient: &mockActivator{fail: true},
		Logger: noopLogger{}, canceledRuns: make(map[string]struct{}),
	}

	runReconciler(t, rec, store)

	if store.statuses["run-2"] != types.RunStatusFailed {
		t.Errorf("expected failed, got %s", store.statuses["run-2"])
	}
}

func TestReconciler_NodeFailure(t *testing.T) {
	store := newMockReconcilerStore()
	agentd := newMockAgentdExecutor()
	agentd.errors["start"] = fmt.Errorf("connection refused")

	spec := makeLinearSpec()
	store.addRun("run-3", "wf-1", "ws-1", spec, json.RawMessage(`{}`), "")

	rec := &WorkflowReconciler{
		Store: store, AgentdClient: agentd, K8sClient: &mockActivator{},
		Logger: noopLogger{}, canceledRuns: make(map[string]struct{}),
	}

	runReconciler(t, rec, store)

	if store.statuses["run-3"] != types.RunStatusFailed {
		t.Errorf("expected failed, got %s", store.statuses["run-3"])
	}
}

func TestReconciler_TriggerFailureIncrement(t *testing.T) {
	store := newMockReconcilerStore()
	agentd := newMockAgentdExecutor()
	agentd.errors["start"] = fmt.Errorf("boom")

	spec := makeLinearSpec()
	store.addRun("run-4", "wf-1", "ws-1", spec, json.RawMessage(`{}`), "trig-1")

	rec := &WorkflowReconciler{
		Store: store, AgentdClient: agentd, K8sClient: &mockActivator{},
		Logger: noopLogger{}, canceledRuns: make(map[string]struct{}),
	}

	runReconciler(t, rec, store)

	if store.triggerFail["trig-1"] != 1 {
		t.Errorf("expected trigger failures=1, got %d", store.triggerFail["trig-1"])
	}
}

func TestReconciler_Cancel(t *testing.T) {
	store := newMockReconcilerStore()

	spec := makeLinearSpec()
	store.addRun("run-5", "wf-1", "ws-1", spec, json.RawMessage(`{}`), "")

	rec := &WorkflowReconciler{
		Store: store, AgentdClient: newMockAgentdExecutor(), K8sClient: &mockActivator{},
		Logger: noopLogger{}, canceledRuns: make(map[string]struct{}),
	}

	rec.Cancel("run-5")
	runReconciler(t, rec, store)

	if store.statuses["run-5"] != types.RunStatusCanceled {
		t.Errorf("expected canceled, got %s", store.statuses["run-5"])
	}
}

func TestTopoSort(t *testing.T) {
	spec := &wf.Spec{
		Nodes: []wf.SpecNode{
			{ID: "a"}, {ID: "b"}, {ID: "c"},
		},
		Edges: []wf.SpecEdge{
			{Source: "a", Target: "b"},
			{Source: "b", Target: "c"},
		},
	}
	order := topoSort(spec)
	if len(order) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(order))
	}
	if spec.Nodes[order[0]].ID != "a" {
		t.Errorf("expected first to be 'a', got %q", spec.Nodes[order[0]].ID)
	}
	if spec.Nodes[order[2]].ID != "c" {
		t.Errorf("expected last to be 'c', got %q", spec.Nodes[order[2]].ID)
	}
}

func TestFindBranchTarget(t *testing.T) {
	spec := &wf.Spec{
		Nodes: []wf.SpecNode{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Edges: []wf.SpecEdge{
			{Source: "a", Target: "b", SourceHandle: "skip"},
			{Source: "a", Target: "c", SourceHandle: "otherwise"},
		},
	}
	idx := findBranchTarget(spec, "a", "skip")
	if idx != 1 {
		t.Errorf("expected index 1 for 'skip', got %d", idx)
	}
	idx = findBranchTarget(spec, "a", "otherwise")
	if idx != 2 {
		t.Errorf("expected index 2 for 'otherwise', got %d", idx)
	}
	idx = findBranchTarget(spec, "a", "nonexistent")
	if idx != -1 {
		t.Errorf("expected -1 for nonexistent handle, got %d", idx)
	}
}

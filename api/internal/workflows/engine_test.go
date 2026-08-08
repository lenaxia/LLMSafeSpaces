// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

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

// --- Mock types ---

type mockStore struct {
	mu          sync.Mutex
	claimed     []*wf.WorkflowRunRow
	statuses    map[string]string
	nodeRuns    []*wf.WorkflowNodeRunRow
	triggerFail map[string]int
	runUpdates  map[string]int
}

func newMockStore() *mockStore {
	return &mockStore{
		statuses:    make(map[string]string),
		triggerFail: make(map[string]int),
		runUpdates:  make(map[string]int),
	}
}

func (m *mockStore) ClaimQueuedRuns(_ context.Context, _ int) ([]*wf.WorkflowRunRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := m.claimed
	m.claimed = nil
	return runs, nil
}

func (m *mockStore) UpdateWorkflowRunStatus(_ context.Context, runID, status string, _ *string, _ json.RawMessage, _ json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[runID] = status
	m.runUpdates[runID]++
	return nil
}

func (m *mockStore) CreateNodeRun(_ context.Context, row *wf.WorkflowNodeRunRow) error {
	m.nodeRuns = append(m.nodeRuns, row)
	return nil
}

func (m *mockStore) UpdateNodeRunStatus(_ context.Context, _, _ string, _ json.RawMessage, _ *string, _ *string, _ json.RawMessage) error {
	return nil
}

func (m *mockStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggerFail[triggerID]++
	return m.triggerFail[triggerID], nil
}

func (m *mockStore) ResetTriggerFailures(_ context.Context, triggerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggerFail[triggerID] = 0
	return nil
}

func (m *mockStore) addRun(id, workflowID, workspaceID string, spec json.RawMessage, input json.RawMessage, triggerID string) *wf.WorkflowRunRow {
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

type mockAgentd struct {
	outputs  map[string]json.RawMessage
	branches map[string]string
	errors   map[string]error
	errCodes map[string]string
}

func newMockAgentd() *mockAgentd {
	return &mockAgentd{
		outputs:  make(map[string]json.RawMessage),
		branches: make(map[string]string),
		errors:   make(map[string]error),
		errCodes: make(map[string]string),
	}
}

func (m *mockAgentd) Execute(_ context.Context, _ string, req *NodeExecRequest) (*NodeExecResponse, error) {
	if err, ok := m.errors[req.NodeID]; ok {
		return nil, err
	}
	resp := &NodeExecResponse{Output: m.outputs[req.NodeID]}
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

func linearSpec() json.RawMessage {
	return json.RawMessage(`{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}},{"id":"end","type":"script","data":{"language":"python","handler":"y"}}],"edges":[{"source":"start","target":"end"}]}`)
}

func runEngine(t *testing.T, rec *Reconciler, store *mockStore) {
	t.Helper()
	ctx := context.Background()
	runs, _ := store.ClaimQueuedRuns(ctx, 10)
	for _, run := range runs {
		rec.executeRun(ctx, noopLogger{}, run)
	}
}

// --- Tests ---

func TestReconciler_HappyPath(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()
	agentd.outputs["start"] = json.RawMessage(`{"x":1}`)
	agentd.outputs["end"] = json.RawMessage(`{"result":"done"}`)
	store.addRun("run-1", "wf-1", "ws-1", linearSpec(), json.RawMessage(`{}`), "")

	rec := &Reconciler{Store: store, AgentdClient: agentd, Activator: &mockActivator{}, Logger: noopLogger{}}

	rec.canceledRuns = make(map[string]struct{})

	runEngine(t, rec, store)
	if store.statuses["run-1"] != types.RunStatusSucceeded {
		t.Errorf("expected succeeded, got %s", store.statuses["run-1"])
	}
}

func TestReconciler_WorkspaceUnavailable(t *testing.T) {
	store := newMockStore()
	store.addRun("run-2", "wf-1", "ws-bad", linearSpec(), json.RawMessage(`{}`), "")
	rec := &Reconciler{Store: store, AgentdClient: newMockAgentd(), Activator: &mockActivator{fail: true}, Logger: noopLogger{}}

	rec.canceledRuns = make(map[string]struct{})
	runEngine(t, rec, store)
	if store.statuses["run-2"] != types.RunStatusFailed {
		t.Errorf("expected failed, got %s", store.statuses["run-2"])
	}
}

func TestReconciler_NodeFailure(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()
	agentd.errors["start"] = fmt.Errorf("connection refused")
	store.addRun("run-3", "wf-1", "ws-1", linearSpec(), json.RawMessage(`{}`), "")
	rec := &Reconciler{Store: store, AgentdClient: agentd, Activator: &mockActivator{}, Logger: noopLogger{}}

	rec.canceledRuns = make(map[string]struct{})
	runEngine(t, rec, store)
	if store.statuses["run-3"] != types.RunStatusFailed {
		t.Errorf("expected failed, got %s", store.statuses["run-3"])
	}
}

func TestReconciler_TriggerFailureIncrement(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()
	agentd.errors["start"] = fmt.Errorf("boom")
	store.addRun("run-4", "wf-1", "ws-1", linearSpec(), json.RawMessage(`{}`), "trig-1")
	rec := &Reconciler{Store: store, AgentdClient: agentd, Activator: &mockActivator{}, Logger: noopLogger{}}

	rec.canceledRuns = make(map[string]struct{})
	runEngine(t, rec, store)
	if store.triggerFail["trig-1"] != 1 {
		t.Errorf("expected trigger failures=1, got %d", store.triggerFail["trig-1"])
	}
}

func TestReconciler_Cancel(t *testing.T) {
	store := newMockStore()
	store.addRun("run-5", "wf-1", "ws-1", linearSpec(), json.RawMessage(`{}`), "")
	rec := &Reconciler{Store: store, AgentdClient: newMockAgentd(), Activator: &mockActivator{}, Logger: noopLogger{}}

	rec.canceledRuns = make(map[string]struct{})
	rec.Cancel("run-5")
	runEngine(t, rec, store)
	if store.statuses["run-5"] != types.RunStatusCanceled {
		t.Errorf("expected canceled, got %s", store.statuses["run-5"])
	}
}

func TestTopoSort(t *testing.T) {
	spec := &wf.Spec{
		Nodes: []wf.SpecNode{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		Edges: []wf.SpecEdge{{Source: "a", Target: "b"}, {Source: "b", Target: "c"}},
	}
	order := topoSort(spec)
	if len(order) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(order))
	}
	if spec.Nodes[order[0]].ID != "a" {
		t.Errorf("expected first to be 'a'")
	}
	if spec.Nodes[order[2]].ID != "c" {
		t.Errorf("expected last to be 'c'")
	}
}

func TestComputeNextFire_Every5Minutes(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"*/5 * * * *"}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	if !next.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("*/5: expected %v, got %v", now.Add(5*time.Minute), next)
	}
}

func TestComputeNextFire_Hourly(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`)}
	now := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	if !next.Equal(now.Add(time.Hour)) {
		t.Errorf("hourly: expected %v, got %v", now.Add(time.Hour), next)
	}
}

func TestComputeNextFire_Daily(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"0 9 * * *"}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("daily 9am: expected %v, got %v", expected, next)
	}
}

func TestComputeNextFire_Weekdays(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"0 9 * * 1-5"}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	if next.Hour() != 9 || next.Minute() != 0 {
		t.Errorf("weekday: expected 9:00, got %02d:%02d", next.Hour(), next.Minute())
	}
	if next.Weekday() == time.Sunday || next.Weekday() == time.Saturday {
		t.Errorf("weekday: expected a weekday, got %v", next.Weekday())
	}
}

func TestComputeNextFire_Empty(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":""}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	if !next.Equal(now.Add(time.Hour)) {
		t.Errorf("empty: expected %v", now.Add(time.Hour))
	}
}

func TestComputeNextFire_Malformed(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"not cron"}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	if !next.Equal(now.Add(time.Hour)) {
		t.Errorf("malformed: expected default 1h")
	}
}

func TestComputeNextFire_TimezoneSupport(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"0 9 * * *","tz":"America/New_York"}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	if next.Hour() != 13 {
		t.Errorf("expected 13:00 UTC (9am EDT), got %02d:%02d", next.Hour(), next.Minute())
	}
}

func TestComputeNextFire_TimezoneInvalid(t *testing.T) {
	trigger := &wf.TriggerRow{SourceConfig: json.RawMessage(`{"expr":"0 14 * * *","tz":"Mars/Olympus"}`)}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	next := computeNextFire(trigger, now)
	expected := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("invalid TZ fallback: expected %v, got %v", expected, next)
	}
}

// --- Scheduler tests ---

type mockSchedulerStore struct {
	mu        sync.Mutex
	triggers  []*wf.TriggerRow
	workflows map[string]*wf.WorkflowRow
	fires     []*wf.TriggerFireRow
	runs      []*wf.WorkflowRunRow
	disabled  map[string]bool
	nextFires map[string]time.Time
}

func newMockSchedulerStore() *mockSchedulerStore {
	return &mockSchedulerStore{
		workflows: make(map[string]*wf.WorkflowRow),
		disabled:  make(map[string]bool),
		nextFires: make(map[string]time.Time),
	}
}

func (m *mockSchedulerStore) ListDueCronTriggers(_ context.Context, _ time.Time, _ int) ([]*wf.TriggerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.triggers, nil
}

func (m *mockSchedulerStore) GetWorkflow(_ context.Context, _, _, id string) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return r, nil
}

func (m *mockSchedulerStore) CreateWorkflowRunWithFire(_ context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error {
	m.fires = append(m.fires, fire)
	m.runs = append(m.runs, run)
	return nil
}

func (m *mockSchedulerStore) CreateTriggerFire(_ context.Context, row *wf.TriggerFireRow) error {
	m.fires = append(m.fires, row)
	return nil
}

func (m *mockSchedulerStore) UpdateTriggerFireTimestamps(_ context.Context, triggerID string, _ time.Time, nextFireAt *time.Time) error {
	if nextFireAt != nil {
		m.nextFires[triggerID] = *nextFireAt
	}
	return nil
}

func (m *mockSchedulerStore) IncrementTriggerFailures(_ context.Context, _ string) (int, error) {
	return 0, nil
}

func (m *mockSchedulerStore) DisableTrigger(_ context.Context, triggerID string) error {
	m.disabled[triggerID] = true
	return nil
}

func makeDueTrigger(id, wfID, wsID string) *wf.TriggerRow {
	now := time.Now().UTC().Add(-5 * time.Second)
	return &wf.TriggerRow{
		ID: id, OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig:     json.RawMessage(`{"expr":"0 * * * *","tz":"UTC"}`),
		TargetType:       types.TriggerTargetRunWorkflow,
		TargetConfig:     json.RawMessage(fmt.Sprintf(`{"workflowId":%q}`, wfID)),
		AutoDisableAfter: 10, NextFireAt: &now,
	}
}

func TestScheduler_FiresDueTrigger(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
	}
	store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-1", "wf-1", "ws-1")}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 || store.fires[0].Status != "fired" {
		t.Fatalf("expected 1 fired, got %d fires", len(store.fires))
	}
	if len(store.runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(store.runs))
	}
}

func TestScheduler_MissedFireSkipped(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-old", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
		TargetType:   types.TriggerTargetRunWorkflow,
		TargetConfig: json.RawMessage(`{"workflowId":"wf-1"}`),
		NextFireAt:   &old,
	}}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 || store.fires[0].Status != "skipped" {
		t.Fatalf("expected 1 skipped, got %+v", store.fires)
	}
}

func TestScheduler_RunScriptTarget(t *testing.T) {
	store := newMockSchedulerStore()
	now := time.Now().UTC().Add(-5 * time.Second)
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-script", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
		TargetType:   types.TriggerTargetRunScript,
		TargetConfig: json.RawMessage(`{"workspaceId":"ws-1","path":"/scripts/backup.sh"}`),
		NextFireAt:   &now,
	}}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 || store.fires[0].Status != "delivered" {
		t.Fatalf("expected 1 delivered, got %+v", store.fires)
	}
}

func TestScheduler_AdvancesNextFireAt(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
	}
	store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-1", "wf-1", "ws-1")}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	next, ok := store.nextFires["trig-1"]
	if !ok {
		t.Fatal("expected next_fire_at to be set")
	}
	if !next.After(time.Now()) {
		t.Error("next_fire_at should be in the future")
	}
}
func strPtr(s string) *string { return &s }

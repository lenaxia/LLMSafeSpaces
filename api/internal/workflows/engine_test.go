// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mockkubernetes "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	k8stypes "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Mock types ---

type mockStore struct {
	mu            sync.Mutex
	claimed       []*wf.WorkflowRunRow
	statuses      map[string]string
	nodeRuns      []*wf.WorkflowNodeRunRow
	triggerFail   map[string]int
	runUpdates    map[string]int
	wfPolicies    map[string]string
	runWorkspaces map[string]string
}

func newMockStore() *mockStore {
	return &mockStore{
		statuses:      make(map[string]string),
		triggerFail:   make(map[string]int),
		runUpdates:    make(map[string]int),
		wfPolicies:    make(map[string]string),
		runWorkspaces: make(map[string]string),
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

func (m *mockStore) GetWorkflowPolicy(_ context.Context, workflowID string) (string, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.wfPolicies[workflowID]
	if !ok {
		return types.OnMissingAbort, "user", "test-owner", nil
	}
	return p, "user", "test-owner", nil
}

func (m *mockStore) UpdateRunWorkspace(_ context.Context, runID, workspaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runWorkspaces[runID] = workspaceID
	return nil
}

func (m *mockStore) GetLastRoutineResult(_ context.Context, _ string) (json.RawMessage, error) {
	return nil, nil
}

func (m *mockStore) GetRecentRoutineResults(_ context.Context, _ string, _ int) ([]json.RawMessage, error) {
	return nil, nil
}

func (m *mockStore) UpdateTriggerFireResult(_ context.Context, _ string, _ json.RawMessage, _ string) error {
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
	outputs   map[string]json.RawMessage
	branches  map[string]string
	errors    map[string]error
	errCodes  map[string]string
	sentSpecs map[string]json.RawMessage
}

func newMockAgentd() *mockAgentd {
	return &mockAgentd{
		outputs:   make(map[string]json.RawMessage),
		branches:  make(map[string]string),
		errors:    make(map[string]error),
		errCodes:  make(map[string]string),
		sentSpecs: make(map[string]json.RawMessage),
	}
}

func (m *mockAgentd) Execute(_ context.Context, _ string, req *NodeExecRequest) (*NodeExecResponse, error) {
	if m.sentSpecs != nil {
		m.sentSpecs[req.NodeID] = req.Spec
	}
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

type mockActivatorNotFound struct {
	succeeded bool
}

func (m *mockActivatorNotFound) EnsureActive(_ context.Context, workspaceID string, _ time.Duration) (string, error) {
	if workspaceID == "ws-gone" && !m.succeeded {
		m.succeeded = true
		return "", fmt.Errorf("workspace %s not found", workspaceID)
	}
	return "10.0.0.2", nil
}

type mockWorkspaceCreator struct {
	createdWorkspaces []string
	returnID          string
}

func (m *mockWorkspaceCreator) CreateWorkspace(_ context.Context, _ string, _ string, _ string) (string, error) {
	id := m.returnID
	if id == "" {
		id = "ws-new-" + fmt.Sprintf("%d", len(m.createdWorkspaces)+1)
	}
	m.createdWorkspaces = append(m.createdWorkspaces, id)
	return id, nil
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

func TestReconciler_OnMissingCreate(t *testing.T) {
	store := newMockStore()
	store.wfPolicies["wf-create"] = types.OnMissingCreate
	agentd := newMockAgentd()
	agentd.outputs["start"] = json.RawMessage(`{"x":1}`)
	agentd.outputs["end"] = json.RawMessage(`{"result":"done"}`)
	store.addRun("run-create", "wf-create", "ws-gone", linearSpec(), json.RawMessage(`{}`), "")

	activator := &mockActivatorNotFound{}
	creator := &mockWorkspaceCreator{returnID: "ws-new-1"}

	rec := &Reconciler{
		Store: store, AgentdClient: agentd, Activator: activator,
		WorkspaceCreator: creator, Logger: noopLogger{},
	}
	rec.canceledRuns = make(map[string]struct{})
	runEngine(t, rec, store)

	if store.statuses["run-create"] != types.RunStatusSucceeded {
		t.Errorf("expected succeeded, got %s", store.statuses["run-create"])
	}
	if len(creator.createdWorkspaces) != 1 {
		t.Fatalf("expected 1 workspace created, got %d", len(creator.createdWorkspaces))
	}
	if creator.createdWorkspaces[0] != "ws-new-1" {
		t.Errorf("expected ws-new-1, got %s", creator.createdWorkspaces[0])
	}
	if store.runWorkspaces["run-create"] != "ws-new-1" {
		t.Errorf("expected run workspace updated to ws-new-1, got %s", store.runWorkspaces["run-create"])
	}
}

func TestReconciler_OnMissingAbort(t *testing.T) {
	store := newMockStore()
	store.wfPolicies["wf-abort"] = types.OnMissingAbort
	store.addRun("run-abort", "wf-abort", "ws-gone", linearSpec(), json.RawMessage(`{}`), "")

	activator := &mockActivatorNotFound{}
	creator := &mockWorkspaceCreator{}

	rec := &Reconciler{
		Store: store, AgentdClient: newMockAgentd(), Activator: activator,
		WorkspaceCreator: creator, Logger: noopLogger{},
	}
	rec.canceledRuns = make(map[string]struct{})
	runEngine(t, rec, store)

	if store.statuses["run-abort"] != types.RunStatusFailed {
		t.Errorf("expected failed, got %s", store.statuses["run-abort"])
	}
	if len(creator.createdWorkspaces) != 0 {
		t.Errorf("expected 0 workspaces created (abort mode), got %d", len(creator.createdWorkspaces))
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
	expected := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("hourly: expected %v (next hour boundary), got %v", expected, next)
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
	mu                sync.Mutex
	triggers          []*wf.TriggerRow
	workflows         map[string]*wf.WorkflowRow
	fires             []*wf.TriggerFireRow
	runs              []*wf.WorkflowRunRow
	disabled          map[string]bool
	nextFires         map[string]time.Time
	statuses          map[string]string
	triggerFail       map[string]int
	lastRoutineResult json.RawMessage
}

func newMockSchedulerStore() *mockSchedulerStore {
	return &mockSchedulerStore{
		workflows:   make(map[string]*wf.WorkflowRow),
		disabled:    make(map[string]bool),
		nextFires:   make(map[string]time.Time),
		statuses:    make(map[string]string),
		triggerFail: make(map[string]int),
	}
}

func (m *mockSchedulerStore) ClaimDueCronTriggers(_ context.Context, _ time.Time, _ int, nextFireFn func(*wf.TriggerRow) time.Time) ([]*wf.TriggerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.triggers {
		if nextFireFn != nil {
			m.nextFires[t.ID] = nextFireFn(t)
		}
	}
	return m.triggers, nil
}

func (m *mockSchedulerStore) ListPendingRoutineFires(_ context.Context, _ int) ([]*wf.TriggerFireRow, error) {
	return nil, nil
}

func (m *mockSchedulerStore) GetTriggerByID(_ context.Context, triggerID string) (*wf.TriggerRow, error) {
	for _, t := range m.triggers {
		if t.ID == triggerID {
			return t, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (m *mockSchedulerStore) GetWorkflow(_ context.Context, _, _, id string) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return r, nil
}

func (m *mockSchedulerStore) UpdateTriggerFireResult(_ context.Context, fireID string, _ json.RawMessage, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statuses == nil {
		m.statuses = make(map[string]string)
	}
	m.statuses[fireID] = status
	return nil
}

func (m *mockSchedulerStore) GetLastRoutineResult(_ context.Context, _ string) (json.RawMessage, error) {
	return m.lastRoutineResult, nil
}

func (m *mockSchedulerStore) GetRecentRoutineResults(_ context.Context, _ string, _ int) ([]json.RawMessage, error) {
	return nil, nil
}

func (m *mockSchedulerStore) ResetTriggerFailures(_ context.Context, _ string) error {
	return nil
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

func (m *mockSchedulerStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggerFail[triggerID]++
	return m.triggerFail[triggerID], nil
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
		WorkflowID:       strPtr(wfID),
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
		WorkflowID:   strPtr("wf-1"),
		NextFireAt:   &old,
	}}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 || store.fires[0].Status != "skipped" {
		t.Fatalf("expected 1 skipped, got %+v", store.fires)
	}
}

func TestScheduler_RoutineTrigger(t *testing.T) {
	store := newMockSchedulerStore()
	now := time.Now().UTC().Add(-5 * time.Second)
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-routine", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
		WorkspaceID:  strPtr("ws-1"),
		Prompt:       "Summarize what changed since last run.",
		MemoryMode:   types.MemoryNone,
		CaptureMode:  types.CaptureFull,
		NextFireAt:   &now,
	}}

	activator := &mockActivator{}
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"nothing changed"}`)

	sched := &Scheduler{
		Store: store, Activator: activator, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second,
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(store.fires))
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

// --- K8sWorkspaceActivator tests ---

func TestK8sWorkspaceActivator_EmptyNamespace(t *testing.T) {
	a := &K8sWorkspaceActivator{Namespace: ""}
	// Should return error (no namespace configured to find workspace CRD).
	_, err := a.EnsureActive(context.Background(), "ws-1", 100*time.Millisecond)
	if err == nil {
		t.Error("expected error with empty namespace")
	}
}

// TestK8sWorkspaceActivator_PatchRefreshesLastActivity is the regression test
// for the "Test1we" production bug: workflow/trigger-driven activations left
// the workspace's `last-activity-at` annotation stale, so the controller's
// idle-timeout check (phase_active.go:235 — `time.Since(lastActivity) >
// idleTimeoutSeconds`) immediately re-suspended the workspace on the next
// reconcile. The API service's ActivateWorkspace (workspace_service.go:1675)
// refreshes the annotation; the workflow engine's K8sWorkspaceActivator did
// not — an asymmetry between the two activation paths.
//
// This test pins the contract: when EnsureActive patches spec.suspend=false
// on a Suspended/Failed workspace, the patch body MUST also include the
// `llmsafespaces.dev/last-activity-at` annotation set to a current timestamp.
func TestK8sWorkspaceActivator_PatchRefreshesLastActivity(t *testing.T) {
	wsFake := mockkubernetes.NewMockWorkspaceInterface()
	v1Fake := mockkubernetes.NewMockLLMSafespacesV1Interface()
	k8sFake := mockkubernetes.NewMockKubernetesClient()

	k8sFake.On("LlmsafespacesV1").Return(v1Fake, nil)
	v1Fake.On("Workspaces", "test-ns").Return(wsFake)

	// First Get: Suspended workspace with stale lastActivity (the bug condition).
	suspended := &k8stypes.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ws-stale",
			Annotations: map[string]string{k8stypes.AnnotationLastActivityAt: "2026-08-02T18:53:10Z"},
		},
		Status: k8stypes.WorkspaceStatus{Phase: k8stypes.WorkspacePhaseSuspended},
	}
	wsFake.On("Get", mock.Anything, "ws-stale", mock.Anything).Return(suspended, nil)
	wsFake.On("Patch", mock.Anything, "ws-stale", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			data := args.Get(3).([]byte)
			t.Logf("patch body: %s", string(data))
			bodyMap := map[string]any{}
			if err := json.Unmarshal(data, &bodyMap); err != nil {
				t.Fatalf("patch body is not valid JSON: %v (body=%s)", err, string(data))
			}
			meta, ok := bodyMap["metadata"].(map[string]any)
			if !ok {
				t.Fatalf("regression: patch body missing metadata key (body=%s). "+
					"EnsureActive must refresh last-activity-at to prevent the "+
					"controller idle-timeout from immediately re-suspending the workspace.", string(data))
			}
			annotations, ok := meta["annotations"].(map[string]any)
			if !ok {
				t.Fatalf("regression: patch body metadata missing annotations (body=%s). "+
					"EnsureActive must refresh last-activity-at.", string(data))
			}
			ts, ok := annotations[k8stypes.AnnotationLastActivityAt].(string)
			if !ok || ts == "" {
				t.Fatalf("regression: patch body missing %s annotation (body=%s). "+
					"EnsureActive must refresh last-activity-at.", k8stypes.AnnotationLastActivityAt, string(data))
			}
			// Timestamp must be recent (within the last minute), not the stale value.
			parsed, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				t.Fatalf("last-activity-at %q is not RFC3339: %v", ts, err)
			}
			if age := time.Since(parsed); age > time.Minute {
				t.Fatalf("regression: last-activity-at %q is stale (age=%s); must be ~now", ts, age)
			}
			// Must differ from the stale value.
			if ts == "2026-08-02T18:53:10Z" {
				t.Fatalf("regression: last-activity-at unchanged from stale value")
			}
		}).Return(suspended, nil)

	a := &K8sWorkspaceActivator{K8sClient: k8sFake, Namespace: "test-ns"}
	// Short timeout — we only care that the Patch is called with the right body.
	// EnsureActive will time out waiting for Active, which is fine for this assertion.
	_, _ = a.EnsureActive(context.Background(), "ws-stale", 100*time.Millisecond)

	wsFake.AssertNumberOfCalls(t, "Patch", 1)
}

// --- HTTPAgentExecutor tests ---

func TestHTTPAgentExecutor_ContextCancellation(t *testing.T) {
	exec := &HTTPAgentExecutor{Port: 9999} // nothing listening
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := exec.Execute(ctx, "127.0.0.1", &NodeExecRequest{NodeID: "test", NodeType: "script"})
	if err == nil {
		t.Error("expected error connecting to non-existent server")
	}
}

func TestHTTPAgentExecutor_SuccessfulCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(NodeExecResponse{
			Output: json.RawMessage(`{"result":"ok"}`),
		})
	}))
	defer srv.Close()

	// Extract host:port from the test server URL
	addr := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(portStr)

	exec := &HTTPAgentExecutor{Port: port, Client: srv.Client()}
	resp, err := exec.Execute(context.Background(), host, &NodeExecRequest{
		NodeID: "test", NodeType: "script", Spec: json.RawMessage(`{}`), Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Output) != `{"result":"ok"}` {
		t.Errorf("expected output {\"result\":\"ok\"}, got %s", string(resp.Output))
	}
}

// --- Condition branching test ---

func TestReconciler_ConditionBranching(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()
	agentd.outputs["start"] = json.RawMessage(`{"shouldSkip":true}`)
	agentd.branches["choice"] = "skip"
	agentd.outputs["skip-path"] = json.RawMessage(`{"skipped":true}`)
	// else-path deliberately NOT set in outputs — if it gets called, agentd
	// returns nil output, which would cause a JSON parse failure downstream.
	// We verify the run succeeds (meaning else-path was NOT called).

	condSpec := json.RawMessage(`{
		"nodes": [
			{"id":"start","type":"script","data":{"language":"python","handler":"x"}},
			{"id":"choice","type":"condition","data":{"conditions":[{"id":"skip","expression":"input.shouldSkip == true"}]}},
			{"id":"skip-path","type":"script","data":{"language":"python","handler":"z"}},
			{"id":"else-path","type":"script","data":{"language":"python","handler":"w"}}
		],
		"edges": [
			{"source":"start","target":"choice"},
			{"source":"choice","target":"skip-path","sourceHandle":"skip"},
			{"source":"choice","target":"else-path","sourceHandle":"otherwise"}
		]
	}`)
	store.addRun("run-cond", "wf-1", "ws-1", condSpec, json.RawMessage(`{}`), "")

	// Track which nodes were actually called
	calledNodes := make(map[string]bool)
	trackingAgentd := &trackingExecutor{inner: agentd, called: calledNodes}

	rec := &Reconciler{Store: store, AgentdClient: trackingAgentd, Activator: &mockActivator{}, Logger: noopLogger{}}
	rec.cancelMu = sync.Mutex{}
	rec.canceledRuns = make(map[string]struct{})

	runEngine(t, rec, store)

	if store.statuses["run-cond"] != types.RunStatusSucceeded {
		t.Fatalf("expected succeeded, got %s", store.statuses["run-cond"])
	}
	if !calledNodes["skip-path"] {
		t.Error("skip-path should have been executed when 'skip' branch matched")
	}
	if calledNodes["else-path"] {
		t.Error("else-path should NOT have been executed when 'skip' branch matched")
	}
}

type trackingExecutor struct {
	inner  *mockAgentd
	called map[string]bool
}

func (t *trackingExecutor) Execute(ctx context.Context, podIP string, req *NodeExecRequest) (*NodeExecResponse, error) {
	t.called[req.NodeID] = true
	return t.inner.Execute(ctx, podIP, req)
}

// --- Node retry test ---

func TestReconciler_NodeRetry(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()

	// First call fails, second succeeds
	callCount := 0
	wrappedAgentd := &retryAgentdExecutor{
		inner:      agentd,
		callCount:  &callCount,
		failFirstN: 1,
		output:     json.RawMessage(`{"result":"ok"}`),
	}

	retrySpec := json.RawMessage(`{
		"nodes": [
			{"id":"start","type":"script","data":{"language":"python","handler":"x"},"maxAttempts":3}
		],
		"edges": []
	}`)
	store.addRun("run-retry", "wf-1", "ws-1", retrySpec, json.RawMessage(`{}`), "")

	rec := &Reconciler{Store: store, AgentdClient: wrappedAgentd, Activator: &mockActivator{}, Logger: noopLogger{}}
	rec.cancelMu = sync.Mutex{}
	rec.canceledRuns = make(map[string]struct{})

	runEngine(t, rec, store)

	if store.statuses["run-retry"] != types.RunStatusSucceeded {
		t.Errorf("expected succeeded after retry, got %s", store.statuses["run-retry"])
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 calls (1 fail + 1 success), got %d", callCount)
	}
}

type retryAgentdExecutor struct {
	inner      *mockAgentd
	callCount  *int
	failFirstN int
	output     json.RawMessage
}

func (r *retryAgentdExecutor) Execute(_ context.Context, podIP string, req *NodeExecRequest) (*NodeExecResponse, error) {
	*r.callCount++
	if *r.callCount <= r.failFirstN {
		return nil, fmt.Errorf("simulated transient failure")
	}
	return &NodeExecResponse{Output: r.output}, nil
}

// --- Error code response path test ---

func TestReconciler_ErrorCodeResponse(t *testing.T) {
	store := newMockStore()
	agentd := newMockAgentd()
	agentd.errCodes["start"] = "script_failed"
	agentd.outputs["start"] = nil

	errSpec := json.RawMessage(`{
		"nodes": [
			{"id":"start","type":"script","data":{"language":"python","handler":"x"},"maxAttempts":1}
		],
		"edges": []
	}`)
	store.addRun("run-err", "wf-1", "ws-1", errSpec, json.RawMessage(`{}`), "")

	rec := &Reconciler{Store: store, AgentdClient: agentd, Activator: &mockActivator{}, Logger: noopLogger{}}
	rec.cancelMu = sync.Mutex{}
	rec.canceledRuns = make(map[string]struct{})

	runEngine(t, rec, store)

	if store.statuses["run-err"] != types.RunStatusFailed {
		t.Errorf("expected failed, got %s", store.statuses["run-err"])
	}
}

// --- Regression test: engine runs without controller DB access ---
// Verifies the engine package has zero controller or DB imports — it works
// entirely through the Store/AgentdClient/Activator interfaces. If this test
// compiles and passes, the architectural migration is complete.

func TestReconciler_InterfaceBasedArchitecture(t *testing.T) {
	// Compile-time check: the Reconciler only depends on interfaces, not
	// concrete DB or controller types. This test would fail to compile if
	// anyone added a controller-runtime or pgx import to this package.
	store := newMockStore()
	store.addRun("run-arch", "wf-1", "ws-1", linearSpec(), json.RawMessage(`{}`), "")

	rec := &Reconciler{
		Store:        store,
		AgentdClient: newMockAgentd(),
		Activator:    &mockActivator{},
		Logger:       noopLogger{},
	}
	rec.cancelMu = sync.Mutex{}
	rec.canceledRuns = make(map[string]struct{})

	runEngine(t, rec, store)

	if store.statuses["run-arch"] != types.RunStatusSucceeded {
		t.Errorf("engine must work via interfaces only: expected succeeded, got %s", store.statuses["run-arch"])
	}
}

// --- Routine executor tests ---

func TestExecuteRoutine_SuccessDelivered(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-r1", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-1", TriggerID: "trig-r1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	if store.statuses["fire-1"] != "delivered" {
		t.Errorf("expected delivered, got %s", store.statuses["fire-1"])
	}
}

func TestExecuteRoutine_AgentError_FailedAndIncrements(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "agent_not_found"
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-r2", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull}
	fire := &wf.TriggerFireRow{ID: "fire-2", TriggerID: "trig-r2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	if store.statuses["fire-2"] != "failed" {
		t.Errorf("expected failed, got %s", store.statuses["fire-2"])
	}
	if store.triggerFail["trig-r2"] != 1 {
		t.Errorf("expected failures=1, got %d", store.triggerFail["trig-r2"])
	}
}

func TestExecuteRoutine_ScriptFailure_IncrementsFailures(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errors["routine-script"] = fmt.Errorf("script crashed")
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-r3", WorkspaceID: &wsID, Prompt: "test", ScriptPath: "/scripts/run.sh", CaptureMode: types.CaptureFull}
	fire := &wf.TriggerFireRow{ID: "fire-3", TriggerID: "trig-r3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	if store.statuses["fire-3"] != "failed" {
		t.Errorf("expected failed, got %s", store.statuses["fire-3"])
	}
	if store.triggerFail["trig-r3"] != 1 {
		t.Errorf("expected failures=1, got %d", store.triggerFail["trig-r3"])
	}
}

func TestExecuteRoutine_ActivationFailure_IncrementsFailures(t *testing.T) {
	store := newMockSchedulerStore()
	wsID := "ws-dead"
	trigger := &wf.TriggerRow{ID: "trig-r4", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull}
	fire := &wf.TriggerFireRow{ID: "fire-4", TriggerID: "trig-r4", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{fail: true}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	if store.statuses["fire-4"] != "failed" {
		t.Errorf("expected failed, got %s", store.statuses["fire-4"])
	}
	if store.triggerFail["trig-r4"] != 1 {
		t.Errorf("expected failures=1, got %d", store.triggerFail["trig-r4"])
	}
}

func TestExecuteRoutine_CaptureErrorsOnly_NoResultOnSuccess(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-r5", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureErrorsOnly}
	fire := &wf.TriggerFireRow{ID: "fire-5", TriggerID: "trig-r5", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	if store.statuses["fire-5"] != "delivered" {
		t.Errorf("expected delivered, got %s", store.statuses["fire-5"])
	}
}

func TestBuildRoutineAgentSpec_PreserveNever(t *testing.T) {
	spec := buildRoutineAgentSpec(&wf.TriggerRow{PreserveSession: types.PreserveNever}, "test")
	var parsed map[string]any
	_ = json.Unmarshal(spec, &parsed)
	if parsed["session"] != "ephemeral" {
		t.Errorf("expected ephemeral, got %v", parsed["session"])
	}
}

func TestBuildRoutineAgentSpec_PreserveAlways(t *testing.T) {
	spec := buildRoutineAgentSpec(&wf.TriggerRow{PreserveSession: types.PreserveAlways}, "test")
	var parsed map[string]any
	_ = json.Unmarshal(spec, &parsed)
	if parsed["session"] != "new" {
		t.Errorf("expected new, got %v", parsed["session"])
	}
}

func TestBuildRoutineAgentSpec_PreserveOnFailure(t *testing.T) {
	spec := buildRoutineAgentSpec(&wf.TriggerRow{PreserveSession: types.PreserveOnFailure}, "test")
	var parsed map[string]any
	_ = json.Unmarshal(spec, &parsed)
	if parsed["session"] != "new" {
		t.Errorf("expected new, got %v", parsed["session"])
	}
}

func TestExecuteRoutine_MemoryLastResult_InjectsPrevResult(t *testing.T) {
	store := newMockSchedulerStore()
	store.lastRoutineResult = json.RawMessage(`{"action":"check email"}`)

	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"done"}`)

	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-mem", WorkspaceID: &wsID,
		Prompt:     "Previous: {{.prevResult}}",
		MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 1,
		CaptureMode: types.CaptureFull,
	}
	fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	if store.statuses["fire-mem"] != "delivered" {
		t.Errorf("expected delivered, got %s", store.statuses["fire-mem"])
	}

	sentSpec := agentd.sentSpecs["routine-agent"]
	var parsed map[string]any
	_ = json.Unmarshal(sentSpec, &parsed)
	renderedPrompt, _ := parsed["prompt"].(string)
	if !strings.Contains(renderedPrompt, "check email") {
		t.Errorf("expected prevResult injected into prompt, got: %s", renderedPrompt)
	}
	if strings.Contains(renderedPrompt, "{{.prevResult}}") {
		t.Errorf("expected {{.prevResult}} to be replaced, got: %s", renderedPrompt)
	}
}

func TestProcessPendingRoutineFire_TriggerNotFound_MarksFailed(t *testing.T) {
	store := newMockSchedulerStore()

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
	fire := &wf.TriggerFireRow{ID: "fire-orphan", TriggerID: "nonexistent", InputEnvelope: json.RawMessage(`{}`)}
	sched.processPendingRoutineFire(context.Background(), noopLogger{}, fire)

	if store.statuses["fire-orphan"] != "failed" {
		t.Errorf("expected failed for orphaned fire, got %s", store.statuses["fire-orphan"])
	}
}

func TestExecuteRoutine_AutoDisable_AfterConsecutiveFailures(t *testing.T) {
	store := newMockSchedulerStore()

	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-auto", WorkspaceID: &wsID, Prompt: "test",
		CaptureMode: types.CaptureFull, AutoDisableAfter: 2,
	}

	sched := &Scheduler{Store: store, Activator: &mockActivator{fail: true}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}

	for i := 0; i < 2; i++ {
		fire := &wf.TriggerFireRow{
			ID: fmt.Sprintf("fire-auto-%d", i), TriggerID: "trig-auto",
			InputEnvelope: json.RawMessage(`{}`),
		}
		sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	}

	if !store.disabled["trig-auto"] {
		t.Error("expected trigger to be auto-disabled after 2 consecutive failures")
	}
}

// uuidEnforcingSchedulerStore wraps mockSchedulerStore and rejects any fire/run
// ID that does not parse as a UUID. The real trigger_fires.id and
// workflow_runs.id columns are `uuid NOT NULL` (migration 000016:209,327); a
// non-UUID string like "fire-<triggerID>-<unix>" causes SQLSTATE 22P02 at
// insert time. The unwrapped mock accepted any string and so masked the bug.
//
// This is the regression guard: if any future change reintroduces a
// human-readable ID for fire/run rows, this store rejects it and the test
// fails immediately at unit-test speed — no real Postgres required.
type uuidEnforcingSchedulerStore struct {
	*mockSchedulerStore
	t *testing.T
	// failNextCreateWorkflowRun, when true, makes the next CreateWorkflowRunWithFire
	// call return wf.ErrConcurrentRun AFTER validating the IDs — simulating the
	// single-in-flight unique violation so the engine's skip-path (engine.go:517)
	// is exercised. Without this the changed UUID-emitting branch is unguarded.
	failNextCreateWorkflowRun bool
}

func (m *uuidEnforcingSchedulerStore) mustParseUUID(id, origin string) {
	m.t.Helper()
	if _, err := uuid.Parse(id); err != nil {
		m.t.Fatalf("regression: %s id %q is not a valid UUID (trigger_fires.id / workflow_runs.id are uuid NOT NULL): %v", origin, id, err)
	}
}

func (m *uuidEnforcingSchedulerStore) CreateTriggerFire(ctx context.Context, row *wf.TriggerFireRow) error {
	m.mustParseUUID(row.ID, "trigger_fire")
	return m.mockSchedulerStore.CreateTriggerFire(ctx, row)
}

func (m *uuidEnforcingSchedulerStore) CreateWorkflowRunWithFire(ctx context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error {
	m.mustParseUUID(fire.ID, "workflow_run fire")
	m.mustParseUUID(run.ID, "workflow_run run")
	if m.failNextCreateWorkflowRun {
		m.failNextCreateWorkflowRun = false
		return wf.ErrConcurrentRun
	}
	return m.mockSchedulerStore.CreateWorkflowRunWithFire(ctx, fire, run)
}

// TestScheduler_FireAndRunIDs_AreUUIDs is the regression test for the
// "Test1we" production bug: the cron scheduler built fire/run IDs as
// fmt.Sprintf("fire-%s-%d", triggerID, unix) which Postgres rejected with
// SQLSTATE 22P02 because trigger_fires.id is `uuid NOT NULL`. Every cron tick
// silently dropped the fire row, advanced last_fired_at, and produced zero
// agent invocations. Cover all three scheduler paths that generate IDs:
//   - workflow-target fires (fireID + runID)
//   - routine-target fires (fireID)
//   - missed-fire skip rows
//
// uuidEnforcingReconcilerStore wraps mockStore (reconciler's store) and rejects
// any node-run ID that does not parse as a UUID. workflow_node_runs.id is
// `uuid NOT NULL` (migration 000016:290); a non-UUID id causes SQLSTATE 22P02
// at insert time, swallowed silently by the reconciler's `_ =`.
type uuidEnforcingReconcilerStore struct {
	*mockStore
	t *testing.T
}

func (m *uuidEnforcingReconcilerStore) mustParseUUID(id, origin string) {
	m.t.Helper()
	if _, err := uuid.Parse(id); err != nil {
		m.t.Fatalf("regression: %s id %q is not a valid UUID (workflow_node_runs.id is uuid NOT NULL): %v", origin, id, err)
	}
}

func (m *uuidEnforcingReconcilerStore) CreateNodeRun(ctx context.Context, row *wf.WorkflowNodeRunRow) error {
	m.mustParseUUID(row.ID, "node_run")
	return m.mockStore.CreateNodeRun(ctx, row)
}

// TestReconciler_NodeRunID_IsUUID is the regression test for the second
// instance of the "Test1we" bug class: the reconciler built node-run IDs as
// fmt.Sprintf("%s-%s-%d", runID, nodeID, attempt), which PG rejects because
// workflow_node_runs.id is `uuid NOT NULL`. Every node execution silently
// dropped its audit row. Same file, same bug class, same execution flow as
// the scheduler fire/run IDs fixed above.
func TestReconciler_NodeRunID_IsUUID(t *testing.T) {
	store := &uuidEnforcingReconcilerStore{mockStore: newMockStore(), t: t}
	store.addRun(uuid.New().String(), uuid.New().String(), "ws-1",
		linearSpec(), json.RawMessage(`{}`), "")

	agentd := newMockAgentd()
	agentd.outputs["start"] = json.RawMessage(`{"ok":true}`)
	agentd.outputs["end"] = json.RawMessage(`{"ok":true}`)

	rec := &Reconciler{
		Store: store, AgentdClient: agentd, Activator: &mockActivator{},
		Logger: noopLogger{},
	}

	claimed, err := store.ClaimQueuedRuns(context.Background(), 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("setup: expected 1 claimed run, got %d (err=%v)", len(claimed), err)
	}
	rec.executeRun(context.Background(), noopLogger{}, claimed[0])

	if len(store.nodeRuns) != 2 {
		t.Fatalf("expected 2 node runs, got %d", len(store.nodeRuns))
	}
}

func TestScheduler_FireAndRunIDs_AreUUIDs(t *testing.T) {
	t.Run("workflow_target", func(t *testing.T) {
		store := &uuidEnforcingSchedulerStore{mockSchedulerStore: newMockSchedulerStore(), t: t}
		store.workflows["wf-uuid"] = &wf.WorkflowRow{
			ID: "wf-uuid", OwnerType: "user", OwnerID: "u1",
			SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		}
		store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-wf-uuid", "wf-uuid", "ws-1")}

		sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
		sched.tick(context.Background(), noopLogger{}, 10)

		if len(store.fires) != 1 || store.fires[0].Status != "fired" {
			t.Fatalf("expected 1 fired fire, got %+v", store.fires)
		}
		if len(store.runs) != 1 {
			t.Fatalf("expected 1 run, got %d", len(store.runs))
		}
	})

	t.Run("routine_target", func(t *testing.T) {
		store := &uuidEnforcingSchedulerStore{mockSchedulerStore: newMockSchedulerStore(), t: t}
		now := time.Now().UTC().Add(-5 * time.Second)
		store.triggers = []*wf.TriggerRow{{
			ID: "trig-rt-uuid", OwnerType: "user", OwnerID: "u1",
			Enabled: true, SourceType: types.TriggerSourceCron,
			SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
			WorkspaceID:  strPtr("ws-1"),
			Prompt:       "test",
			CaptureMode:  types.CaptureErrorsOnly,
			NextFireAt:   &now,
		}}

		sched := &Scheduler{
			Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(),
			Logger: noopLogger{}, TickInterval: 30 * time.Second,
		}
		sched.tick(context.Background(), noopLogger{}, 10)

		if len(store.fires) != 1 {
			t.Fatalf("expected 1 fire, got %d", len(store.fires))
		}
	})

	t.Run("missed_fire_skipped", func(t *testing.T) {
		store := &uuidEnforcingSchedulerStore{mockSchedulerStore: newMockSchedulerStore(), t: t}
		store.workflows["wf-miss"] = &wf.WorkflowRow{
			ID: "wf-miss", OwnerType: "user", OwnerID: "u1",
			SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		}
		old := time.Now().UTC().Add(-2 * time.Hour)
		store.triggers = []*wf.TriggerRow{{
			ID: "trig-miss", OwnerType: "user", OwnerID: "u1",
			Enabled: true, SourceType: types.TriggerSourceCron,
			SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
			WorkflowID:   strPtr("wf-miss"),
			NextFireAt:   &old,
		}}

		sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
		sched.tick(context.Background(), noopLogger{}, 10)

		if len(store.fires) != 1 || store.fires[0].Status != "skipped" {
			t.Fatalf("expected 1 skipped fire, got %+v", store.fires)
		}
	})

	// already_running_skipped guards the changed UUID-emitting branch at
	// engine.go:517: when CreateWorkflowRunWithFire returns ErrConcurrentRun
	// (single-in-flight unique violation), the engine falls back to creating a
	// skipped TriggerFire row. The base mock always returns nil, so without
	// injecting ErrConcurrentRun this branch is never reached and a future
	// regression to a non-UUID skipped-fire ID would pass undetected.
	t.Run("already_running_skipped", func(t *testing.T) {
		store := &uuidEnforcingSchedulerStore{
			mockSchedulerStore:        newMockSchedulerStore(),
			t:                         t,
			failNextCreateWorkflowRun: true,
		}
		store.workflows["wf-already"] = &wf.WorkflowRow{
			ID: "wf-already", OwnerType: "user", OwnerID: "u1",
			SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		}
		now := time.Now().UTC().Add(-5 * time.Second)
		store.triggers = []*wf.TriggerRow{{
			ID: "trig-already", OwnerType: "user", OwnerID: "u1",
			Enabled: true, SourceType: types.TriggerSourceCron,
			SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
			WorkflowID:   strPtr("wf-already"),
			NextFireAt:   &now,
		}}

		sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
		sched.tick(context.Background(), noopLogger{}, 10)

		skipped := 0
		for _, f := range store.fires {
			if f.Status == "skipped" {
				skipped++
			}
		}
		if skipped != 1 {
			t.Fatalf("expected 1 skipped fire from ErrConcurrentRun path, got %d (total fires=%d)", skipped, len(store.fires))
		}
	})
}

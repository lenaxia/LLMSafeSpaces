// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"context"
	"encoding/base64"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mockkubernetes "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	k8stypes "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
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
	// sessionIDs stamps the #1470 first-class envelope field onto the
	// response for the given node (empty map = field never set, the
	// old-agentd shape).
	sessionIDs map[string]string
}

func newMockAgentd() *mockAgentd {
	return &mockAgentd{
		outputs:    make(map[string]json.RawMessage),
		branches:   make(map[string]string),
		errors:     make(map[string]error),
		errCodes:   make(map[string]string),
		sentSpecs:  make(map[string]json.RawMessage),
		sessionIDs: make(map[string]string),
	}
}

func (m *mockAgentd) Execute(_ context.Context, _, _ string, req *NodeExecRequest) (*NodeExecResponse, error) {
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
	if sid, ok := m.sessionIDs[req.NodeID]; ok {
		resp.SessionID = sid
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
	mu                   sync.Mutex
	triggers             []*wf.TriggerRow
	workflows            map[string]*wf.WorkflowRow
	fires                []*wf.TriggerFireRow
	runs                 []*wf.WorkflowRunRow
	disabled             map[string]bool
	nextFires            map[string]time.Time
	statuses             map[string]string
	triggerFail          map[string]int
	lastRoutineResult    json.RawMessage
	recentRoutineResults []json.RawMessage
	// Call-count/last-result mirrors for the #1454 behavior-identical
	// consolidation pins: increments/resets count CALLS (triggerFail
	// stays the running total), results captures the last written
	// result payload per fire. getTriggerByIDErr injects a transient
	// store failure (#1473 fetch split).
	increments        map[string]int
	resets            map[string]int
	results           map[string]json.RawMessage
	getTriggerByIDErr error
	getWorkflowErr    error
	sessionOrigins    map[string]*wf.SessionOriginRow
	overridePending   []*wf.TriggerFireRow
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

func (m *mockSchedulerStore) ClaimDueCronTriggers(_ context.Context, now time.Time, _ int, nextFireFn func(*wf.TriggerRow) time.Time) ([]*wf.TriggerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*wf.TriggerRow
	for _, t := range m.triggers {
		// Store parity: only DUE, ENABLED, CRON-SOURCED triggers are
		// claimed (the real claim filters source_type='cron' AND
		// enabled=true AND next_fire_at due). Without this gate the
		// mock returned every row, double-firing webhook-sourced
		// routine triggers that ride the pending-fire drain instead.
		if t.SourceType != types.TriggerSourceCron || !t.Enabled {
			continue
		}
		if t.NextFireAt == nil || t.NextFireAt.After(now) {
			continue
		}
		if nextFireFn != nil {
			m.nextFires[t.ID] = nextFireFn(t)
		}
		out = append(out, t)
	}
	return out, nil
}

func (m *mockSchedulerStore) ListPendingRoutineFires(_ context.Context, _ int) ([]*wf.TriggerFireRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overridePending != nil {
		return m.overridePending, nil
	}
	// Store parity: only routine fires still in 'fired' AND never
	// result-written drain (the real store filters action_type='routine'
	// AND status='fired' AND result IS NULL). Without the result-write
	// landing on the row, a fire created and completed within one tick
	// re-executed on the same tick's drain — production-impossible.
	var out []*wf.TriggerFireRow
	for _, f := range m.fires {
		if f.ActionType == "routine" && f.Status == "fired" && f.Result == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *mockSchedulerStore) GetTriggerByID(_ context.Context, triggerID string) (*wf.TriggerRow, error) {
	if m.getTriggerByIDErr != nil {
		return nil, m.getTriggerByIDErr
	}
	for _, t := range m.triggers {
		if t.ID == triggerID {
			return t, nil
		}
	}
	// Store parity: the real store maps a missing row to wf.ErrNotFound
	// (pkg/workflows/store.go GetTriggerByID) so the drain's transient-
	// vs-deleted split (#1473) is exercisable at unit speed.
	return nil, wf.ErrNotFound
}

func (m *mockSchedulerStore) GetWorkflow(_ context.Context, _, _, id string) (*wf.WorkflowRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.getWorkflowErr; err != nil {
		return nil, err
	}
	r, ok := m.workflows[id]
	if !ok {
		// Store parity: missing rows are wf.ErrNotFound; anything else is
		// a transient store failure and must NOT be conflated.
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockSchedulerStore) UpdateTriggerFireResult(_ context.Context, fireID string, result json.RawMessage, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statuses == nil {
		m.statuses = make(map[string]string)
	}
	if m.results == nil {
		m.results = make(map[string]json.RawMessage)
	}
	m.statuses[fireID] = status
	m.results[fireID] = result
	// Store parity: the write lands ON THE ROW (result set + status
	// moved out of 'fired'), so a same-tick drain re-list cannot pick
	// it up. Without this the drain filter observed nothing.
	for _, f := range m.fires {
		if f.ID == fireID {
			f.Status = status
			f.Result = json.RawMessage(`{"written":true}`)
		}
	}
	for _, f := range m.overridePending {
		if f.ID == fireID {
			f.Status = status
			f.Result = json.RawMessage(`{"written":true}`)
		}
	}
	return nil
}

func (m *mockSchedulerStore) GetLastRoutineResult(_ context.Context, _ string) (json.RawMessage, error) {
	return m.lastRoutineResult, nil
}

func (m *mockSchedulerStore) GetRecentRoutineResults(_ context.Context, _ string, _ int) ([]json.RawMessage, error) {
	return m.recentRoutineResults, nil
}

func (m *mockSchedulerStore) ResetTriggerFailures(_ context.Context, triggerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resets == nil {
		m.resets = make(map[string]int)
	}
	m.resets[triggerID]++
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
	if m.increments == nil {
		m.increments = make(map[string]int)
	}
	m.increments[triggerID]++
	m.triggerFail[triggerID]++
	return m.triggerFail[triggerID], nil
}

func (m *mockSchedulerStore) DisableTrigger(_ context.Context, triggerID string) error {
	m.disabled[triggerID] = true
	return nil
}

func (m *mockSchedulerStore) RecordSessionOrigin(_ context.Context, row *wf.SessionOriginRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionOrigins == nil {
		m.sessionOrigins = make(map[string]*wf.SessionOriginRow)
	}
	m.sessionOrigins[row.SessionID] = row
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

func TestScheduler_MissingWorkflowRecordsFailedFire(t *testing.T) {
	// A trigger whose workflow was deleted must surface a failed fire and
	// drive the auto-disable counter — not tick silently forever (#1412).
	store := newMockSchedulerStore()
	// No workflow "wf-gone" in the store.
	store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-1", "wf-gone", "ws-1")}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 || store.fires[0].Status != "failed" {
		t.Fatalf("expected 1 failed fire, got %+v", store.fires)
	}
	if store.fires[0].ActionResult == nil || !strings.Contains(string(store.fires[0].ActionResult), "workflow not found") {
		t.Fatalf("expected workflow_not_found detail in action result, got %s", store.fires[0].ActionResult)
	}
	if len(store.runs) != 0 {
		t.Fatalf("no run may be created for a missing workflow, got %d", len(store.runs))
	}
	if store.triggerFail["trig-1"] != 1 {
		t.Fatalf("failure counter must increment, got %d", store.triggerFail["trig-1"])
	}
}

func TestScheduler_MissingWorkflowAutoDisables(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueTrigger("trig-2", "wf-gone", "ws-1")
	trig.AutoDisableAfter = 1
	store.triggers = []*wf.TriggerRow{trig}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if !store.disabled["trig-2"] {
		t.Fatal("auto_disable_after=1 must disable the trigger after one failed fire")
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
	exec := &HTTPAgentExecutor{Port: 9999, PasswordProvider: &stubPasswordProvider{password: "pw"}} // nothing listening
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := exec.Execute(ctx, "ws-1", "127.0.0.1", &NodeExecRequest{NodeID: "test", NodeType: "script"})
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

	exec := &HTTPAgentExecutor{Port: port, Client: srv.Client(), PasswordProvider: &stubPasswordProvider{password: "pw"}}
	resp, err := exec.Execute(context.Background(), "ws-1", host, &NodeExecRequest{
		NodeID: "test", NodeType: "script", Spec: json.RawMessage(`{}`), Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp.Output) != `{"result":"ok"}` {
		t.Errorf("expected output {\"result\":\"ok\"}, got %s", string(resp.Output))
	}
}

// --- HTTPAgentExecutor auth tests (#762) ---

// stubPasswordProvider returns a fixed password and records the workspaceID
// it was asked for.
type stubPasswordProvider struct {
	password string
	gotWS    []string
	err      error
}

func (s *stubPasswordProvider) WorkspacePassword(_ context.Context, workspaceID string) (string, error) {
	s.gotWS = append(s.gotWS, workspaceID)
	return s.password, s.err
}

func testServerAddr(t *testing.T, srv *httptest.Server) (host string, port int) {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	h, portStr, _ := strings.Cut(addr, ":")
	p, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return h, p
}

func TestHTTPAgentExecutor_SetsBasicAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(NodeExecResponse{Output: json.RawMessage(`{}`)})
	}))
	defer srv.Close()
	host, port := testServerAddr(t, srv)

	pw := &stubPasswordProvider{password: "pw-123"}
	exec := &HTTPAgentExecutor{Port: port, Client: srv.Client(), PasswordProvider: pw}
	_, err := exec.Execute(context.Background(), "ws-42", host, &NodeExecRequest{NodeID: "n", NodeType: "script"})
	require.NoError(t, err)

	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:pw-123"))
	require.Equal(t, expected, gotAuth, "executor must send Basic auth on node dispatch")
	require.Equal(t, []string{"ws-42"}, pw.gotWS, "executor must resolve password for the dispatched workspace")
}

func TestHTTPAgentExecutor_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorCode":"unauthorized"}`))
	}))
	defer srv.Close()
	host, port := testServerAddr(t, srv)

	exec := &HTTPAgentExecutor{
		Port: port, Client: srv.Client(),
		PasswordProvider: &stubPasswordProvider{password: "pw"},
	}
	_, err := exec.Execute(context.Background(), "ws-1", host, &NodeExecRequest{NodeID: "n", NodeType: "script"})
	require.Error(t, err, "a 401 from agentd must surface as an explicit error, not a JSON-parse failure")
	require.Contains(t, err.Error(), "401")
}

func TestHTTPAgentExecutor_MissingPasswordProvider(t *testing.T) {
	exec := &HTTPAgentExecutor{Port: 4097}
	_, err := exec.Execute(context.Background(), "ws-1", "127.0.0.1", &NodeExecRequest{NodeID: "n", NodeType: "script"})
	require.Error(t, err, "executor without a PasswordProvider must fail fast (agentd enforces Basic auth)")
}

func TestHTTPAgentExecutor_PasswordProviderError(t *testing.T) {
	exec := &HTTPAgentExecutor{
		Port:             4097,
		PasswordProvider: &stubPasswordProvider{err: fmt.Errorf("secret gone")},
	}
	_, err := exec.Execute(context.Background(), "ws-1", "127.0.0.1", &NodeExecRequest{NodeID: "n", NodeType: "script"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "secret gone")
}

func TestDeleteRoutineSession_SendsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	host, port := testServerAddr(t, srv)

	ok := deleteRoutineSession(context.Background(), noopLogger{}, "pw-9", host, port, "ses_1")
	require.True(t, ok)
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:pw-9"))
	require.Equal(t, expected, gotAuth)
}

func TestDeleteRoutineSession_401IsNotDeleted(t *testing.T) {
	var logged []string
	capturingLogger := &captureLogger{logs: &logged}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	host, port := testServerAddr(t, srv)

	ok := deleteRoutineSession(context.Background(), capturingLogger, "pw-9", host, port, "ses_1")
	require.False(t, ok, "401 must not count as deleted")
	require.NotEmpty(t, logged, "non-2xx delete responses must be logged, not silently swallowed")
}

type captureLogger struct{ logs *[]string }

func (c *captureLogger) Info(msg string, _ ...any)           { *c.logs = append(*c.logs, msg) }
func (c *captureLogger) Error(_ error, msg string, _ ...any) { *c.logs = append(*c.logs, msg) }

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

func (t *trackingExecutor) Execute(ctx context.Context, workspaceID, podIP string, req *NodeExecRequest) (*NodeExecResponse, error) {
	t.called[req.NodeID] = true
	return t.inner.Execute(ctx, workspaceID, podIP, req)
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

func (r *retryAgentdExecutor) Execute(_ context.Context, _, _ string, _ *NodeExecRequest) (*NodeExecResponse, error) {
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
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-r1", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-1", TriggerID: "trig-r1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	if store.statuses["fire-1"] != "delivered" {
		t.Errorf("expected delivered, got %s", store.statuses["fire-1"])
	}
}

func TestExecuteRoutine_PreserveAlways_RecordsSessionOrigin(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_abc123"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-orig1", Name: "Weather Bot", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveAlways}
	fire := &wf.TriggerFireRow{ID: "fire-orig1", TriggerID: "trig-orig1", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	if store.statuses["fire-orig1"] != "delivered" {
		t.Fatalf("expected delivered, got %s", store.statuses["fire-orig1"])
	}
	origin, ok := store.sessionOrigins["ses_abc123"]
	if !ok {
		t.Fatal("expected session origin to be recorded for PreserveAlways")
	}
	if origin.Origin != types.SessionOriginRoutine {
		t.Errorf("expected origin 'routine', got '%s'", origin.Origin)
	}
	if origin.TriggerID == nil || *origin.TriggerID != "trig-orig1" {
		t.Errorf("expected triggerId 'trig-orig1', got %v", origin.TriggerID)
	}
	if origin.FireID == nil || *origin.FireID != "fire-orig1" {
		t.Errorf("expected fireId 'fire-orig1', got %v", origin.FireID)
	}
	if origin.Title != "Weather Bot" {
		t.Errorf("expected title 'Weather Bot', got '%s'", origin.Title)
	}
}

func TestExecuteRoutine_PreserveNever_DoesNotRecordOrigin(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	// agentd sets session_id="" for PreserveNever after deleting the ephemeral session
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":""}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-orig2", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveNever}
	fire := &wf.TriggerFireRow{ID: "fire-orig2", TriggerID: "trig-orig2", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	if len(store.sessionOrigins) != 0 {
		t.Errorf("expected no session origins for PreserveNever, got %d", len(store.sessionOrigins))
	}
}

func TestExecuteRoutine_PreserveOnFailure_DeleteFails_RecordsOrigin(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_def456"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-orig3", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-orig3", TriggerID: "trig-orig3", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw"}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	if store.statuses["fire-orig3"] != "delivered" {
		t.Fatalf("expected delivered, got %s", store.statuses["fire-orig3"])
	}
	// DELETE endpoint unreachable (mockActivator returns 10.0.0.1, the
	// real dial fails) → session still exists → origin IS recorded as a
	// fallback. This documents the robustness fix: sessionDeleted only
	// flips when DELETE succeeds.
	if len(store.sessionOrigins) != 1 {
		t.Errorf("expected 1 session origin when DELETE fails (session still exists), got %d", len(store.sessionOrigins))
	}
}

// TestExecuteRoutine_PreserveOnFailure_DeleteSucceeds_NoOrigin is the
// happy-path counterpart: the authorized delete reaches agentd with the
// workspace Basic credential, agentd returns 2xx, the session is deleted
// and therefore NO session origin is recorded (#762 caller wiring).
func TestExecuteRoutine_PreserveOnFailure_DeleteSucceeds_NoOrigin(t *testing.T) {
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, portStr, _ := strings.Cut(addr, ":")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_ok789"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-ok", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-ok", TriggerID: "trig-ok", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{
		Store: store, Activator: &loopbackActivator{}, AgentdClient: agentd, Logger: noopLogger{},
		PasswordProvider: &stubPasswordProvider{password: "pw-ok"},
		AgentdPort:       port,
	}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	if store.statuses["fire-ok"] != "delivered" {
		t.Fatalf("expected delivered, got %s", store.statuses["fire-ok"])
	}
	if len(store.sessionOrigins) != 0 {
		t.Errorf("expected no session origin after successful delete, got %d", len(store.sessionOrigins))
	}
	require.Equal(t, "/v1/workflow/session/delete", gotPath)
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:pw-ok"))
	require.Equal(t, expected, gotAuth, "PreserveOnFailure delete must authenticate against agentd")
}

// loopbackActivator points the delete dial at the test server.
type loopbackActivator struct{}

func (loopbackActivator) EnsureActive(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "127.0.0.1", nil
}

// TestExecuteRoutine_PreserveOnFailure_ControlCharSessionID_NoCrash pins
// the Go 1.26 nil-request fix in the PreserveOnFailure delete path: a
// session_id from the workspace agent's output containing a control
// character makes the DELETE URL unparseable. This routine runs in the
// scheduler outside gin's recovery middleware — a nil-req Do used to
// crash the API process. Now: logged failure, session origin still
// recorded (session not deleted), fire delivered.
func TestExecuteRoutine_PreserveOnFailure_ControlCharSessionID_NoCrash(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	// Control character in session_id → unparseable DELETE URL.
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok","session_id":"ses_\u0007bad"}`)
	wsID := "ws-1"
	trigger := &wf.TriggerRow{ID: "trig-ctl", WorkspaceID: &wsID, Prompt: "test", CaptureMode: types.CaptureFull, PreserveSession: types.PreserveOnFailure}
	fire := &wf.TriggerFireRow{ID: "fire-ctl", TriggerID: "trig-ctl", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}

	require.NotPanics(t, func() {
		sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)
	})

	if store.statuses["fire-ctl"] != "delivered" {
		t.Fatalf("expected delivered, got %s", store.statuses["fire-ctl"])
	}
	// DELETE could not run (URL build failed) → session still exists →
	// origin recorded as fallback, same as the unreachable-DELETE case.
	if len(store.sessionOrigins) != 1 {
		t.Errorf("expected 1 session origin when DELETE URL is invalid, got %d", len(store.sessionOrigins))
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
	// Real stored shape under captureMode=full: the agent-node output
	// envelope (#1453 — the injected prev must be {response, tokens} only).
	store.lastRoutineResult = json.RawMessage(`{"response":"checked email, nothing urgent","session_id":"ses_1","tokens":{"input":10,"output":5,"total":15},"prompt":"OLD-ROUND-PROMPT","parts":[]}`)

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
	if !strings.Contains(renderedPrompt, "checked email") {
		t.Errorf("expected prevResult injected into prompt, got: %s", renderedPrompt)
	}
	if strings.Contains(renderedPrompt, "{{.prevResult}}") {
		t.Errorf("expected {{.prevResult}} to be replaced, got: %s", renderedPrompt)
	}
	if strings.Contains(renderedPrompt, "OLD-ROUND-PROMPT") {
		t.Errorf("injected prev result must not carry the prior round's prompt (#1453), got: %s", renderedPrompt)
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

// A TRANSIENT store failure is logged and skipped — never recorded as a
// failed fire, never counted toward auto-disable.
func TestScheduler_TransientStoreErrorNotCounted(t *testing.T) {
	store := newMockSchedulerStore()
	store.getWorkflowErr = fmt.Errorf("pool exhausted")
	store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-1", "wf-1", "")}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 0 {
		t.Fatalf("transient outage must not record a failed fire: %+v", store.fires)
	}
	if store.triggerFail["trig-1"] != 0 {
		t.Fatalf("transient outage must not count toward auto-disable: %v", store.triggerFail)
	}
	if store.disabled["trig-1"] {
		t.Fatalf("trigger must stay enabled")
	}
}

// --- 0059: trigger input mapping on the cron fire path -----------------------

// makeMappedTrigger is a due cron trigger wired to a workflow with an
// input mapping (the 0059 opt-in).
func makeMappedTrigger(id, wfID, inputFrom string, input json.RawMessage) *wf.TriggerRow {
	t := makeDueTrigger(id, wfID, "")
	t.InputFrom = inputFrom
	t.Input = input
	return t
}

// TestScheduler_MappedTriggerConformingInput: a schema-bearing workflow
// wired via inputFrom:mapped with a conforming static document queues a
// run whose input IS that document — the #1425 fix.
func TestScheduler_MappedTriggerConformingInput(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		InputSchema: json.RawMessage(`{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`),
	}
	store.triggers = []*wf.TriggerRow{
		makeMappedTrigger("trig-mapped", "wf-schema", "mapped", json.RawMessage(`{"topic":"nightly"}`)),
	}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	require.Len(t, store.runs, 1, "a conforming mapped input must queue a run")
	assert.JSONEq(t, `{"topic":"nightly"}`, string(store.runs[0].Input),
		"mapped mode: the static document IS the run input")
	require.Len(t, store.fires, 1)
	assert.Equal(t, "fired", store.fires[0].Status)
}

// TestScheduler_MappedTriggerDivergentInput: a divergent static document
// records a validation_error fire with typed, location-only violations —
// no run is queued, no node executes, and the #1412 accounting drives
// auto-disable.
func TestScheduler_MappedTriggerDivergentInput(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		InputSchema: json.RawMessage(`{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}},"additionalProperties":false}`),
	}
	store.triggers = []*wf.TriggerRow{
		makeMappedTrigger("trig-bad", "wf-schema", "mapped", json.RawMessage(`{"SECRET-INSTANCE":"x"}`)),
	}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	require.Len(t, store.runs, 0, "no run may be created for a failing input")
	require.Len(t, store.fires, 1)
	fire := store.fires[0]
	assert.Equal(t, types.TriggerFireValidationError, fire.Status)
	assert.NotNil(t, fire.CompletedAt)
	require.NotNil(t, fire.ActionResult)
	payload := string(fire.ActionResult)
	assert.Contains(t, payload, `"code":"schema_mismatch"`)
	assert.Contains(t, payload, `"inputFrom":"mapped"`)
	assert.Contains(t, payload, `"/topic"`)
	assert.Contains(t, payload, `"keyword":"required"`)
	assert.NotContains(t, payload, "SECRET-INSTANCE", "violation payloads must never carry instance values")
	// The audit row records the raw envelope — what the source sent.
	assert.NotNil(t, fire.InputEnvelope)
	assert.Equal(t, 1, store.triggerFail["trig-bad"], "validation_error fires count toward auto-disable")
	assert.False(t, store.disabled["trig-bad"])
}

// TestScheduler_ValidationErrorAutoDisables: the #1412 circuit breaker
// trips at auto_disable_after.
func TestScheduler_ValidationErrorAutoDisables(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		InputSchema: json.RawMessage(`{"type":"object","required":["topic"]}`),
	}
	trig := makeMappedTrigger("trig-ad", "wf-schema", "mapped", json.RawMessage(`{}`))
	trig.AutoDisableAfter = 1
	store.triggers = []*wf.TriggerRow{trig}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.True(t, store.disabled["trig-ad"], "auto_disable_after=1 must disable after one validation_error fire")
}

// TestScheduler_InvalidSchemaFailedFire: a stored schema that no longer
// compiles is a workflow defect — failed fire with
// {"code":"invalid_input_schema"}, never a schema-mismatch payload.
func TestScheduler_InvalidSchemaFailedFire(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-broken"] = &wf.WorkflowRow{
		ID: "wf-broken", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		InputSchema: json.RawMessage(`{"$ref":"#/definitions/missing"}`),
	}
	store.triggers = []*wf.TriggerRow{
		makeMappedTrigger("trig-broken", "wf-broken", "mapped", json.RawMessage(`{"topic":"x"}`)),
	}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	require.Len(t, store.runs, 0)
	require.Len(t, store.fires, 1)
	assert.Equal(t, types.TriggerFireFailed, store.fires[0].Status)
	assert.JSONEq(t, `{"code":"invalid_input_schema"}`, string(store.fires[0].ActionResult))
	assert.Equal(t, 1, store.triggerFail["trig-broken"])
}

// TestScheduler_EnvelopeStaticMerge: envelope mode + static input merges
// (static wins) and the fire's input_envelope stays the raw envelope.
func TestScheduler_EnvelopeStaticMerge(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		InputSchema: json.RawMessage(`{"type":"object","required":["topic"]}`),
	}
	store.triggers = []*wf.TriggerRow{
		makeMappedTrigger("trig-merge", "wf-schema", "envelope", json.RawMessage(`{"topic":"nightly"}`)),
	}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	require.Len(t, store.runs, 1)
	var runInput map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(store.runs[0].Input, &runInput))
	assert.Equal(t, `"nightly"`, string(runInput["topic"]), "static overlay wins")
	assert.NotNil(t, runInput["source"], "the envelope stays reachable under its keys")
	assert.NotNil(t, runInput["received_at"])
	// The audit row keeps the RAW envelope — what the source sent.
	var env map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(store.fires[0].InputEnvelope, &env))
	assert.Nil(t, env["topic"], "input_envelope must NOT carry the resolved input")
}

// TestScheduler_LegacyTriggerByteIdentical: an un-opted trigger against a
// schema-bearing workflow keeps today's behavior exactly — the envelope
// bytes become the run input and NO validation runs (§3.6).
func TestScheduler_LegacyTriggerByteIdentical(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
		InputSchema: json.RawMessage(`{"type":"object","required":["topic"]}`),
	}
	// Raw row as migration 000031 backfills it: input_from envelope, no input.
	store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-legacy", "wf-schema", "")}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	require.Len(t, store.runs, 1)
	require.Len(t, store.fires, 1)
	assert.Equal(t, "fired", store.fires[0].Status)
	assert.Equal(t, string(store.fires[0].InputEnvelope), string(store.runs[0].Input),
		"legacy un-opted triggers: run input must stay the envelope bytes verbatim")
	assert.Equal(t, 0, store.triggerFail["trig-legacy"])
}

// #1440: a targetless trigger (workflow deleted → FK SET NULL, or a
// routine missing workspace_id) must record a FAILED fire, count
// toward auto-disable, and never tick silently.
func TestScheduler_TargetlessTriggerFailsLoudly(t *testing.T) {
	store := newMockSchedulerStore()
	// Post-FK shape: workflow_id NULL (deleted target), no workspace_id.
	due := time.Now().UTC().Add(-5 * time.Second)
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-ghost", OwnerType: "user", OwnerID: "u1",
		Name: "ghost", Enabled: true, SourceType: "cron",
		WorkflowID: nil, WorkspaceID: nil, AutoDisableAfter: 5,
		NextFireAt: &due,
	}}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 || store.fires[0].Status != "failed" {
		t.Fatalf("targetless trigger must record exactly one FAILED fire, got %+v", store.fires)
	}
	if store.triggerFail["trig-ghost"] != 1 {
		t.Fatalf("failure counted, got %v", store.triggerFail)
	}
	if store.runs != nil {
		t.Fatalf("no run may be created")
	}
	var payload map[string]string
	if err := json.Unmarshal(store.fires[0].ActionResult, &payload); err != nil {
		t.Fatalf("action result payload: %v", err)
	}
	if payload["reason"] != "trigger_has_no_target" {
		t.Fatalf("payload names the cause, got %v", payload)
	}
}

// At the threshold the targetless fire disarms the zombie.
func TestScheduler_TargetlessTriggerAutoDisables(t *testing.T) {
	store := newMockSchedulerStore()
	due2 := time.Now().UTC().Add(-5 * time.Second)
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-ghost2", OwnerType: "user", OwnerID: "u1",
		Name: "ghost2", Enabled: true, SourceType: "cron",
		WorkflowID: nil, WorkspaceID: nil, AutoDisableAfter: 1,
		NextFireAt: &due2,
	}}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if !store.disabled["trig-ghost2"] {
		t.Fatalf("targetless zombie must auto-disable at the threshold")
	}
}

// --- #1441: bounded retry on transient upstream 5xx ------------------------

type scriptedExecutor struct {
	calls   int
	results []struct {
		resp *NodeExecResponse
		err  error
	}
}

func (e *scriptedExecutor) Execute(_ context.Context, _, _ string, _ *NodeExecRequest) (*NodeExecResponse, error) {
	i := e.calls
	e.calls++
	if i >= len(e.results) {
		i = len(e.results) - 1
	}
	return e.results[i].resp, e.results[i].err
}

// A provider blip (opencode 500) recovers on retry: the fire succeeds
// and no failure budget is burned.
func TestExecuteWithRetry_Transient5xxRecovers(t *testing.T) {
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ACK"}`)}},
	}}
	resp, err := executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp.Output)
	assert.Equal(t, 2, ex.calls, "exactly one retry")
}

// Exhausted retries surface the failure as before.
func TestExecuteWithRetry_Exhausted(t *testing.T) {
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 503"}},
	}}
	resp, err := executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
	require.NoError(t, err)
	assert.Equal(t, "script_failed", resp.ErrorCode)
	assert.Equal(t, 3, ex.calls, "bounded: three attempts")
}

// Deterministic failures are NOT retried (unsupported language etc.).
func TestExecuteWithRetry_DeterministicNoRetry(t *testing.T) {
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "invalid_node_data", Detail: "unsupported language"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{}`)}},
	}}
	resp, err := executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
	require.NoError(t, err)
	assert.Equal(t, "invalid_node_data", resp.ErrorCode)
	assert.Equal(t, 1, ex.calls, "no retry on deterministic failure")
}

// Transport errors: agentd 5xx retried, non-5xx not.
func TestExecuteWithRetry_TransportShapes(t *testing.T) {
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{err: fmt.Errorf("agentd node execute returned 502: bad gateway")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{}`)}},
	}}
	_, err := executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
	require.NoError(t, err)
	assert.Equal(t, 2, ex.calls)

	ex2 := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{err: fmt.Errorf("agentd node execute returned 404: no route")},
	}}
	_, _ = executeWithRetry(context.Background(), ex2, "ws", "ip", &NodeExecRequest{})
	assert.Equal(t, 1, ex2.calls, "404 transport not retried")
}

// Timeouts (504) are OUT of the retry class: fresh-session retries of a
// timed-out turn risk double execution, and a 10m-timeout retry would
// triple the scheduler's worst-case per-fire latency.
func TestExecuteWithRetry_TimeoutNotRetried(t *testing.T) {
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{err: fmt.Errorf("agentd node execute returned 504: gateway timeout")},
	}}
	_, _ = executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
	assert.Equal(t, 1, ex.calls, "504 transport not retried")

	ex2 := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_timeout", Detail: "agent call timed out"}},
	}}
	_, _ = executeWithRetry(context.Background(), ex2, "ws", "ip", &NodeExecRequest{})
	assert.Equal(t, 1, ex2.calls, "agentd script_timeout not retried")

	ex3 := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 504"}},
	}}
	_, _ = executeWithRetry(context.Background(), ex3, "ws", "ip", &NodeExecRequest{})
	assert.Equal(t, 1, ex3.calls, "opencode 504 wrap not retried")
}

// Wiring pin: executeRoutine actually routes through the retry — a
// transient 5xx from the executor must NOT fail the fire (reverting
// the executeWithRetry call site leaves this red).
func TestScheduler_RoutineFireRetriesTransient5xx(t *testing.T) {
	store := newMockSchedulerStore()
	// A pending WEBHOOK routine fire (the receiver created it; the tick
	// drains it) whose routine workspace is live.
	wsPtr := "ws-rt"
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-rt", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceWebhook, WorkspaceID: &wsPtr,
		Prompt: "ACK", AutoDisableAfter: 10,
	}}
	store.overridePending = []*wf.TriggerFireRow{{
		ID: "fire-rt", TriggerID: "trig-rt", SourceType: "webhook",
		ActionType: "routine", Status: "fired", FiredAt: time.Now().UTC(),
	}}
	// Executor: first call is the transient 500, second succeeds.
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ACK"}`)}},
	}}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, "delivered", store.statuses["fire-rt"], "the transient 500 must not fail the fire")
	assert.Equal(t, 2, ex.calls, "exactly one retry — reverting the executeWithRetry call site leaves this red (0 retries, fire failed)")
}

// Exhausted-retry wiring: a PERSISTENT retryable 5xx fails the fire and
// burns exactly ONE failure — the budget-multiplication regression
// class this retry exists to prevent (per-attempt counting would
// triple-burn consecutiveFailures).
func TestScheduler_RoutineFirePersistent5xxBurnsOneFailure(t *testing.T) {
	store := newMockSchedulerStore()
	wsPtr := "ws-rt2"
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-rt2", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceWebhook, WorkspaceID: &wsPtr,
		Prompt: "ACK", AutoDisableAfter: 10,
	}}
	store.overridePending = []*wf.TriggerFireRow{{
		ID: "fire-rt2", TriggerID: "trig-rt2", SourceType: "webhook",
		ActionType: "routine", Status: "fired", FiredAt: time.Now().UTC(),
	}}
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_failed", Detail: "opencode returned 500"}},
	}}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, "failed", store.statuses["fire-rt2"], "persistent 5xx still fails the fire")
	assert.Equal(t, 3, ex.calls, "bounded at three attempts")
	assert.Equal(t, 1, store.triggerFail["trig-rt2"], "exactly ONE failure burned — not one per attempt")
	assert.False(t, store.disabled["trig-rt2"], "threshold not reached (1 < 10)")
}

// Mock-fidelity pin (review r5/r6): a DUE CRON routine trigger's
// claim-created fire, completed within the same tick, executes exactly
// once — the drain must not re-pick the result-written row. This is
// the exact double-execution shape the round-4 mock allowed (claim
// fires + writes the row; the unfiltered drain re-listed it). Red
// under the round-4 mock, green with the result-write landing on rows.
func TestScheduler_RoutineFireExecutesOncePerTick(t *testing.T) {
	store := newMockSchedulerStore()
	wsPtr := "ws-once"
	due := time.Now().UTC().Add(-5 * time.Second)
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-once", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceCron, WorkspaceID: &wsPtr,
		SourceConfig: json.RawMessage(`{"expr":"* * * * *","tz":"UTC"}`),
		Prompt:       "ACK", AutoDisableAfter: 10, NextFireAt: &due,
	}}
	ex := &countingExecutor{}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, 1, ex.calls, "claim-executed fire must not re-execute on the same tick's drain")
	require.Len(t, store.fires, 1)
	assert.Equal(t, "delivered", store.fires[0].Status)
}

type countingExecutor struct{ calls int }

func (e *countingExecutor) Execute(_ context.Context, _, _ string, _ *NodeExecRequest) (*NodeExecResponse, error) {
	e.calls++
	return &NodeExecResponse{Output: json.RawMessage(`{"response":"ACK"}`)}, nil
}

// --- #1457: session-create failures join the transient retry class ---

// Classifier unit: agentd's session_create_failed wrapping an opencode
// 500/502/503 (provider blip on the create leg) is retried — the same
// transient class as script_failed wraps. Red until
// retryableAgentdFailure learns the new code (#1457).
func TestExecuteWithRetry_SessionCreateFailedTransient5xxRecovers(t *testing.T) {
	for _, status := range []string{"500", "502", "503"} {
		ex := &scriptedExecutor{results: []struct {
			resp *NodeExecResponse
			err  error
		}{
			{resp: &NodeExecResponse{ErrorCode: "session_create_failed", Detail: "opencode returned " + status}},
			{resp: &NodeExecResponse{Output: json.RawMessage(`{}`)}},
		}}
		_, err := executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
		require.NoError(t, err)
		assert.Equal(t, 2, ex.calls, "opencode %s on session create must retry once", status)
	}
}

// Classifier unit: the NON-transient session shapes are never retried
// — the transport-to-opencode detail (consistent with the message leg,
// where opencode transport errors are not retried), deterministic 4xx
// creates, and a genuine missing session (session_not_found).
func TestExecuteWithRetry_SessionCreateFailedNonTransientNotRetried(t *testing.T) {
	for name, resp := range map[string]*NodeExecResponse{
		"transport detail":   {ErrorCode: "session_create_failed", Detail: `opencode session create: Post "http://localhost:4096/session": dial tcp: connection refused`},
		"opencode 400 wrap":  {ErrorCode: "session_create_failed", Detail: "opencode returned 400"},
		"genuine missing":    {ErrorCode: "session_not_found", Detail: "session ses_x not found"},
		"missing, blip text": {ErrorCode: "session_not_found", Detail: "opencode returned 500"},
	} {
		ex := &scriptedExecutor{results: []struct {
			resp *NodeExecResponse
			err  error
		}{{resp: resp}}}
		_, _ = executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
		assert.Equal(t, 1, ex.calls, "%s must not retry", name)
	}
}

// Wiring pin (#1457): a routine fire whose agent leg hits a transient
// opencode 5xx on SESSION CREATE delivers after exactly one retry and
// burns NO failure budget. Reverting the session_create_failed arm of
// retryableAgentdFailure leaves this red (fire fails, one failure
// burned — the pre-fix collapse mapped the blip to
// session_not_found, outside the retry class).
func TestScheduler_RoutineFireRetriesSessionCreate5xx(t *testing.T) {
	store := newMockSchedulerStore()
	wsPtr := "ws-ses5xx"
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-ses5xx", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceWebhook, WorkspaceID: &wsPtr,
		Prompt: "ACK", AutoDisableAfter: 10,
	}}
	store.overridePending = []*wf.TriggerFireRow{{
		ID: "fire-ses5xx", TriggerID: "trig-ses5xx", SourceType: "webhook",
		ActionType: "routine", Status: "fired", FiredAt: time.Now().UTC(),
	}}
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "session_create_failed", Detail: "opencode returned 503"}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ACK"}`)}},
	}}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, "delivered", store.statuses["fire-ses5xx"], "a transient session-create 5xx must not fail the fire")
	assert.Equal(t, 2, ex.calls, "exactly one retry")
	assert.Equal(t, 0, store.triggerFail["trig-ses5xx"], "no consecutiveFailures burned")
	assert.False(t, store.disabled["trig-ses5xx"])
}

// Wiring guard (#1457): a GENUINE missing session (message-leg 404 →
// session_not_found) is deterministic — one attempt, fire failed,
// exactly ONE failure burned. Retrying it would triple-burn the
// auto-disable budget on a permanently-broken session.
func TestScheduler_RoutineFireSessionNotFoundNotRetried(t *testing.T) {
	store := newMockSchedulerStore()
	wsPtr := "ws-snf"
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-snf", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceWebhook, WorkspaceID: &wsPtr,
		Prompt: "ACK", AutoDisableAfter: 10,
	}}
	store.overridePending = []*wf.TriggerFireRow{{
		ID: "fire-snf", TriggerID: "trig-snf", SourceType: "webhook",
		ActionType: "routine", Status: "fired", FiredAt: time.Now().UTC(),
	}}
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "session_not_found", Detail: "session ses_x not found"}},
	}}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, "failed", store.statuses["fire-snf"], "genuine missing session fails the fire")
	assert.Equal(t, 1, ex.calls, "no retry for session_not_found")
	assert.Equal(t, 1, store.triggerFail["trig-snf"], "exactly ONE failure burned")
}

// --- #1458: the ScriptPath pre-script leg rides the retry ---

// Wiring pin (#1458): a transient agentd transport 5xx on the
// PRE-SCRIPT leg (ScriptPath set) is retried — the fire delivers.
// Reverting the executeWithRetry call site in the ScriptPath branch
// (back to a direct AgentdClient.Execute) leaves this red: the first
// 502 fails the fire after one call.
func TestScheduler_RoutineFireScriptLegRetriesTransient5xx(t *testing.T) {
	store := newMockSchedulerStore()
	wsPtr := "ws-scr5xx"
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-scr5xx", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceWebhook, WorkspaceID: &wsPtr,
		ScriptPath: "/workspace/pre.py", Prompt: "ACK {{.scriptResult}}", AutoDisableAfter: 10,
	}}
	store.overridePending = []*wf.TriggerFireRow{{
		ID: "fire-scr5xx", TriggerID: "trig-scr5xx", SourceType: "webhook",
		ActionType: "routine", Status: "fired", FiredAt: time.Now().UTC(),
	}}
	// Call order: script leg (transient 502 → retry → success), then
	// the agent leg (success).
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{err: fmt.Errorf("agentd node execute returned 502: bad gateway")},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"stdout":"PRE"}`)}},
		{resp: &NodeExecResponse{Output: json.RawMessage(`{"response":"ACK"}`)}},
	}}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, "delivered", store.statuses["fire-scr5xx"], "a transient 5xx on the pre-script leg must not fail the fire")
	assert.Equal(t, 3, ex.calls, "script leg retried once (2 calls) + agent leg (1 call)")
	assert.Equal(t, 0, store.triggerFail["trig-scr5xx"], "no consecutiveFailures burned")
	assert.False(t, store.disabled["trig-scr5xx"])
}

// Wiring guard (#1458): deterministic failures on the pre-script leg
// are NOT retried — a 4xx transport error fails the fire after one
// attempt and burns exactly ONE failure. Guards the retry class from
// widening into everything-shaped.
func TestScheduler_RoutineFireScriptLegDeterministic4xxNoRetry(t *testing.T) {
	store := newMockSchedulerStore()
	wsPtr := "ws-scr4xx"
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-scr4xx", OwnerType: "user", OwnerID: "u1", Enabled: true,
		SourceType: types.TriggerSourceWebhook, WorkspaceID: &wsPtr,
		ScriptPath: "/workspace/pre.py", Prompt: "ACK", AutoDisableAfter: 10,
	}}
	store.overridePending = []*wf.TriggerFireRow{{
		ID: "fire-scr4xx", TriggerID: "trig-scr4xx", SourceType: "webhook",
		ActionType: "routine", Status: "fired", FiredAt: time.Now().UTC(),
	}}
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{err: fmt.Errorf("agentd node execute returned 400: bad request")},
	}}
	sched := &Scheduler{
		Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second,
		AgentdClient: ex, Activator: &mockActivator{},
	}
	sched.tick(context.Background(), noopLogger{}, 10)

	assert.Equal(t, "failed", store.statuses["fire-scr4xx"], "deterministic 4xx on the pre-script leg fails the fire")
	assert.Equal(t, 1, ex.calls, "no retry on deterministic failure")
	assert.Equal(t, 1, store.triggerFail["trig-scr4xx"], "exactly ONE failure burned")
}

// #1455 guard: agentd's script_env_unavailable (scratch-sidecar: no
// /tmp, no interpreters) is deterministic — an environment does not
// heal within the retry backoff — and must stay outside the retry
// class (one attempt, like every other non-transient node failure).
func TestExecuteWithRetry_ScriptEnvUnavailableNotRetried(t *testing.T) {
	ex := &scriptedExecutor{results: []struct {
		resp *NodeExecResponse
		err  error
	}{
		{resp: &NodeExecResponse{ErrorCode: "script_env_unavailable", Detail: "script node execution environment unavailable in this container: no writable temp dir"}},
	}}
	resp, _ := executeWithRetry(context.Background(), ex, "ws", "ip", &NodeExecRequest{})
	assert.Equal(t, 1, ex.calls, "script_env_unavailable must not retry")
	assert.Equal(t, "script_env_unavailable", resp.ErrorCode)
}

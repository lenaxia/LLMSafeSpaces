// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1729 integration: a routine trigger created through the pod-automation
// surface with targetWorkspaceId FIREs in the TARGET workspace. The chain
// under test is the real one end to end — HTTP create (authn → seam
// scoping → delegated create + ownership gate → store), then the REAL
// workflow Scheduler ticking the created row (activator + agentd dispatch
// keyed by the row's workspace). The only fakes are the scheduler's store
// (handing back the API-created row at its due slot) and the pod-adjacent
// activator/agentd transports, which RECORD which workspace they were
// asked to reach.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// crossWSSchedulerStore hands the API-created trigger row to the REAL
// scheduler as due (the cron slot arrives), records the fire lifecycle,
// and answers the routine path's memory reads with "no history".
type crossWSSchedulerStore struct {
	trigger  *wf.TriggerRow
	fires    []*wf.TriggerFireRow
	statuses map[string]string
	origins  []*wf.SessionOriginRow
	failures map[string]int
}

func (s *crossWSSchedulerStore) ClaimDueCronTriggers(_ context.Context, _ time.Time, _ int, _ func(*wf.TriggerRow) time.Time) ([]*wf.TriggerRow, error) {
	// The store's due-claim in production compares next_fire_at <= now;
	// simulate the schedule's slot arriving by stamping the row due
	// (5s ago — inside the tick window, so it fires rather than skips).
	due := *s.trigger
	slot := time.Now().UTC().Add(-5 * time.Second)
	due.NextFireAt = &slot
	return []*wf.TriggerRow{&due}, nil
}

func (s *crossWSSchedulerStore) ListPendingRoutineFires(_ context.Context, _ int) ([]*wf.TriggerFireRow, error) {
	return nil, nil
}
func (s *crossWSSchedulerStore) GetTriggerByID(_ context.Context, _ string) (*wf.TriggerRow, error) {
	return nil, wf.ErrNotFound
}
func (s *crossWSSchedulerStore) GetWorkflow(_ context.Context, _, _, _ string) (*wf.WorkflowRow, error) {
	return nil, wf.ErrNotFound
}
func (s *crossWSSchedulerStore) CreateWorkflowRunWithFire(_ context.Context, _ *wf.TriggerFireRow, _ *wf.WorkflowRunRow) error {
	return nil
}
func (s *crossWSSchedulerStore) CreateTriggerFire(_ context.Context, row *wf.TriggerFireRow) error {
	s.fires = append(s.fires, row)
	return nil
}
func (s *crossWSSchedulerStore) UpdateTriggerFireResult(_ context.Context, fireID string, result json.RawMessage, status string) error {
	if s.statuses == nil {
		s.statuses = map[string]string{}
	}
	s.statuses[fireID] = status
	for _, f := range s.fires {
		if f.ID == fireID {
			f.Result, f.Status = result, status
		}
	}
	return nil
}
func (s *crossWSSchedulerStore) GetLastRoutineResult(_ context.Context, _ string) (json.RawMessage, error) {
	return nil, nil
}
func (s *crossWSSchedulerStore) GetRecentRoutineResults(_ context.Context, _ string, _ int) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *crossWSSchedulerStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
	if s.failures == nil {
		s.failures = map[string]int{}
	}
	s.failures[triggerID]++
	return s.failures[triggerID], nil
}
func (s *crossWSSchedulerStore) ResetTriggerFailures(_ context.Context, _ string) error { return nil }
func (s *crossWSSchedulerStore) DisableTrigger(_ context.Context, _ string) error       { return nil }
func (s *crossWSSchedulerStore) UpdateTriggerFireAction(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}
func (s *crossWSSchedulerStore) RecordSessionOrigin(_ context.Context, row *wf.SessionOriginRow) error {
	s.origins = append(s.origins, row)
	return nil
}

// crossWSActivator / crossWSAgentd record which workspace the REAL engine
// reached for.
type crossWSActivator struct{ activated []string }

func (a *crossWSActivator) EnsureActive(_ context.Context, workspaceID string, _ time.Duration) (string, error) {
	a.activated = append(a.activated, workspaceID)
	return "10.0.0.9", nil
}

type crossWSAgentd struct {
	executed []execution
}

type execution struct {
	workspaceID string
	nodeID      string
}

func (e *crossWSAgentd) Execute(_ context.Context, workspaceID, _ string, req *apiwf.NodeExecRequest) (*apiwf.NodeExecResponse, error) {
	e.executed = append(e.executed, execution{workspaceID: workspaceID, nodeID: req.NodeID})
	if req.NodeType == "script" {
		return &apiwf.NodeExecResponse{}, nil
	}
	return &apiwf.NodeExecResponse{Output: json.RawMessage(`{"response":"ran"}`)}, nil
}

// Create via the automation surface targeting ws-2 (owned by the same
// user as the creating pod's ws-1), then tick the REAL scheduler over
// the created row: the routine must activate and dispatch in ws-2 —
// never the creating workspace.
func TestPodAutomation_CrossWorkspaceRoutineFiresInTargetWorkspace(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-2",
		"name":"deploy-resume-waker",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"wake up and resume"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	require.Len(t, trigStore.triggers, 1)
	var created *wf.TriggerRow
	for _, row := range trigStore.triggers {
		created = row
	}
	require.NotNil(t, created.WorkspaceID)
	require.Equal(t, "ws-2", *created.WorkspaceID, "API created the routine in the target workspace")

	// Fire the created row through the REAL engine. Start runs one
	// synchronous tick before its loop; the bounded context ends Start
	// after that first tick has completed.
	store := &crossWSSchedulerStore{trigger: created}
	activator := &crossWSActivator{}
	agentd := &crossWSAgentd{}
	sched := &apiwf.Scheduler{
		Store: store, Activator: activator, AgentdClient: agentd,
		TickInterval: 30 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = sched.Start(ctx)

	require.Len(t, store.fires, 1, "exactly one fire")
	assert.Equal(t, "routine", store.fires[0].ActionType)
	assert.Equal(t, "delivered", store.fires[0].Status, "fire delivered in the target workspace")

	require.Len(t, activator.activated, 1, "one activation")
	assert.Equal(t, "ws-2", activator.activated[0], "activation keyed to the TARGET workspace")
	assert.NotContains(t, activator.activated, "ws-1", "creating workspace never activated")

	require.NotEmpty(t, agentd.executed, "agent dispatched")
	for _, ex := range agentd.executed {
		assert.Equal(t, "ws-2", ex.workspaceID, "agent turn dispatched in the TARGET workspace (node %s)", ex.nodeID)
	}
}

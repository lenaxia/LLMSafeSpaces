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

func (m *mockSchedulerStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
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

	if len(store.fires) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(store.fires))
	}
	if store.fires[0].Status != "fired" {
		t.Errorf("expected fired, got %s", store.fires[0].Status)
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

	// Trigger with a very old next_fire_at (controller was down).
	old := time.Now().UTC().Add(-2 * time.Hour)
	trigger := &wf.TriggerRow{
		ID: "trig-old", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
		TargetType:   types.TriggerTargetRunWorkflow,
		TargetConfig: json.RawMessage(`{"workflowId":"wf-1"}`),
		NextFireAt:   &old,
	}
	store.triggers = []*wf.TriggerRow{trigger}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 {
		t.Fatalf("expected 1 fire (skipped), got %d", len(store.fires))
	}
	if store.fires[0].Status != "skipped" {
		t.Errorf("expected skipped, got %s", store.fires[0].Status)
	}
}

func TestScheduler_RunScriptTarget(t *testing.T) {
	store := newMockSchedulerStore()
	now := time.Now().UTC().Add(-5 * time.Second)
	store.triggers = []*wf.TriggerRow{
		{
			ID: "trig-script", OwnerType: "user", OwnerID: "u1",
			Enabled: true, SourceType: types.TriggerSourceCron,
			SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
			TargetType:   types.TriggerTargetRunScript,
			TargetConfig: json.RawMessage(`{"workspaceId":"ws-1","path":"/scripts/backup.sh"}`),
			NextFireAt:   &now,
		},
	}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if len(store.fires) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(store.fires))
	}
	if store.fires[0].Status != "delivered" {
		t.Errorf("expected delivered, got %s", store.fires[0].Status)
	}
}

func TestScheduler_AdvancesNextFireAt(t *testing.T) {
	store := newMockSchedulerStore()
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtr("ws-1"),
	}
	store.triggers = []*wf.TriggerRow{makeDueTrigger("trig-1", "wf-1", "ws-1")}

	sched := &Scheduler{Store: store, Logger: noopLogger{}}
	sched.tick(context.Background(), noopLogger{}, 10)

	next, ok := store.nextFires["trig-1"]
	if !ok {
		t.Fatal("expected next_fire_at to be set")
	}
	if !next.After(time.Now()) {
		t.Error("next_fire_at should be in the future")
	}
}

func TestComputeNextFire(t *testing.T) {
	trigger := &wf.TriggerRow{
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *"}`),
	}
	now := time.Now().UTC()
	next := computeNextFire(trigger, now)
	if !next.After(now) {
		t.Error("next fire should be after now")
	}
}

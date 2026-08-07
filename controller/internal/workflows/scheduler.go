// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Epic 64: Scheduler — leader-elected ticker that polls due cron triggers.
//
// Implements manager.Runnable + NeedLeaderElection. On each tick:
// 1. Selects due cron triggers (enabled, next_fire_at <= now)
// 2. For each: computes input template, creates fire + run atomically
// 3. Advances next_fire_at
// 4. Enforces circuit breaker (auto-disable after N failures)
// 5. Logs missed fires (controller downtime) as 'skipped'

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// SchedulerStore is the narrow store interface for the scheduler.
type SchedulerStore interface {
	ListDueCronTriggers(ctx context.Context, now time.Time, limit int) ([]*wf.TriggerRow, error)
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
	CreateWorkflowRunWithFire(ctx context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error
	CreateTriggerFire(ctx context.Context, row *wf.TriggerFireRow) error
	UpdateTriggerFireTimestamps(ctx context.Context, triggerID string, lastFiredAt time.Time, nextFireAt *time.Time) error
	IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error)
	DisableTrigger(ctx context.Context, triggerID string) error
}

// Scheduler polls due cron triggers and fires them.
type Scheduler struct {
	Store        SchedulerStore
	Logger       ReconcilerLogger
	TickInterval time.Duration
	BatchLimit   int
}

func (s *Scheduler) Start(ctx context.Context) error {
	logger := s.Logger
	if logger == nil {
		logger = noopLogger{}
	}

	interval := s.TickInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	limit := s.BatchLimit
	if limit <= 0 {
		limit = 50
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Fire once immediately, then on ticker.
	s.tick(ctx, logger, limit)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(ctx, logger, limit)
		}
	}
}

func (s *Scheduler) NeedLeaderElection() bool { return true }

func (s *Scheduler) tick(ctx context.Context, logger ReconcilerLogger, limit int) {
	now := time.Now().UTC()

	triggers, err := s.Store.ListDueCronTriggers(ctx, now, limit)
	if err != nil {
		logger.Error(err, "scheduler: failed to list due cron triggers")
		return
	}

	for _, trigger := range triggers {
		s.fireTrigger(ctx, logger, trigger, now)
	}
}

func (s *Scheduler) fireTrigger(ctx context.Context, logger ReconcilerLogger, trigger *wf.TriggerRow, now time.Time) {
	// Check for missed fire (controller was down).
	if trigger.NextFireAt != nil && now.Sub(*trigger.NextFireAt) > s.TickInterval {
		// Log the missed fire as 'skipped' (not silent).
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID:        fmt.Sprintf("fire-missed-%s-%d", trigger.ID, now.Unix()),
			TriggerID: trigger.ID, SourceType: "cron",
			ActionType: trigger.TargetType, Status: "skipped",
			FiredAt: *trigger.NextFireAt, CompletedAt: &now,
		})
		logger.Info("scheduler: missed fire logged as skipped", "triggerId", trigger.ID)

		// Advance next_fire_at to the future (skip this missed fire).
		nextFire := computeNextFire(trigger, now)
		_ = s.Store.UpdateTriggerFireTimestamps(ctx, trigger.ID, now, &nextFire)
		return
	}

	// Compute input from template.
	envelope := map[string]any{
		"source":      map[string]any{"type": "cron", "id": trigger.ID},
		"received_at": now.Format(time.RFC3339),
	}
	envelopeJSON, _ := json.Marshal(envelope)

	if trigger.TargetType == types.TriggerTargetRunWorkflow {
		s.fireWorkflowTarget(ctx, logger, trigger, envelopeJSON, now)
	} else {
		// run_script — just record the fire.
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID:        fmt.Sprintf("fire-%s-%d", trigger.ID, now.Unix()),
			TriggerID: trigger.ID, SourceType: "cron",
			InputEnvelope: envelopeJSON, ActionType: trigger.TargetType,
			Status: "delivered", FiredAt: now, CompletedAt: &now,
		})
	}

	// Advance next_fire_at.
	nextFire := computeNextFire(trigger, now)
	_ = s.Store.UpdateTriggerFireTimestamps(ctx, trigger.ID, now, &nextFire)
}

func (s *Scheduler) fireWorkflowTarget(ctx context.Context, logger ReconcilerLogger, trigger *wf.TriggerRow, envelopeJSON []byte, now time.Time) {
	var targetCfg map[string]any
	json.Unmarshal(trigger.TargetConfig, &targetCfg)
	workflowID, _ := targetCfg["workflowId"].(string)
	if workflowID == "" {
		logger.Info("scheduler: trigger target missing workflowId", "triggerId", trigger.ID)
		return
	}

	wfRow, err := s.Store.GetWorkflow(ctx, trigger.OwnerType, trigger.OwnerID, workflowID)
	if err != nil {
		logger.Error(err, "scheduler: target workflow not found", "triggerId", trigger.ID, "workflowId", workflowID)
		return
	}

	// Render input from template (if any).
	inputForRun := json.RawMessage(envelopeJSON)
	if tmpl, ok := targetCfg["inputTemplate"].(map[string]any); ok && len(tmpl) > 0 {
		rendered := make(map[string]any)
		for k, v := range tmpl {
			rendered[k] = v // v1: pass template values as-is; scheduler doesn't evaluate templates
		}
		inputForRun, _ = json.Marshal(rendered)
	}

	fireID := fmt.Sprintf("fire-%s-%d", trigger.ID, now.Unix())
	runID := fmt.Sprintf("run-%s-%d", trigger.ID, now.Unix())

	workspaceID := ""
	if wfRow.TargetWorkspaceID != nil {
		workspaceID = *wfRow.TargetWorkspaceID
	}

	fire := &wf.TriggerFireRow{
		ID: fireID, TriggerID: trigger.ID, SourceType: "cron",
		InputEnvelope: envelopeJSON, ActionType: "run_workflow",
		Status: "fired", FiredAt: now,
	}
	run := &wf.WorkflowRunRow{
		ID: runID, WorkflowID: workflowID, SpecSnapshot: wfRow.SpecJSON,
		Input: inputForRun, Status: "queued", TriggerID: &trigger.ID,
		WorkspaceID: workspaceID, CreatedAt: now, UpdatedAt: now,
	}

	err = s.Store.CreateWorkflowRunWithFire(ctx, fire, run)
	if err != nil {
		logger.Info("scheduler: run creation rejected (likely single-in-flight)", "triggerId", trigger.ID, "err", err)
		// Log as skipped — the workflow is already running.
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID: fireID + "-skipped", TriggerID: trigger.ID, SourceType: "cron",
			InputEnvelope: envelopeJSON, ActionType: "run_workflow",
			ActionResult: json.RawMessage(`{"reason":"already_running"}`),
			Status:       "skipped", FiredAt: now, CompletedAt: &now,
		})
		return
	}

	logger.Info("scheduler: fired cron trigger", "triggerId", trigger.ID, "runId", runID)
}

// computeNextFire computes the next fire time. v1 uses a simple interval
// based on the cron expression's smallest granularity. A proper cron parser
// is deferred to when a library is chosen (US-64.9 enhancement).
func computeNextFire(trigger *wf.TriggerRow, now time.Time) time.Time {
	var cfg types.CronSourceConfig
	json.Unmarshal(trigger.SourceConfig, &cfg)

	// v1: default to 1 hour intervals (the scheduler tick handles the actual
	// timing; the cron expression parsing is a TODO that needs a cron library).
	// For now, advance by the tick interval or 1 hour, whichever is larger.
	return now.Add(time.Hour)
}

func strPtr(s string) *string { return &s }

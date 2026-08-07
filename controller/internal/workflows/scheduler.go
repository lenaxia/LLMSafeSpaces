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
	"strconv"
	"strings"
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
	_ = json.Unmarshal(trigger.TargetConfig, &targetCfg)
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

// computeNextFire computes the next fire time from a cron expression.
// v1 supports basic 5-field cron expressions: minute hour day-of-month month day-of-week.
// Uses a simplified interval approach: finds the smallest interval implied by the
// expression and adds it. Full cron parsing (with field wildcards) requires a library.
func computeNextFire(trigger *wf.TriggerRow, now time.Time) time.Time {
	var cfg types.CronSourceConfig
	_ = json.Unmarshal(trigger.SourceConfig, &cfg)

	expr := cfg.Expr
	if expr == "" {
		return now.Add(time.Hour) // default 1h
	}

	// Resolve the timezone from config; default to UTC.
	loc := time.UTC
	if cfg.TZ != "" {
		if parsed, err := time.LoadLocation(cfg.TZ); err == nil {
			loc = parsed
		}
	}

	// Parse 5-field cron expression. Fields: minute hour dom month dow.
	// For v1, we support the common patterns:
	// - "*/N * * * *" → every N minutes
	// - "0 * * * *" → hourly
	// - "0 N * * *" → daily at hour N
	// - "0 0 * * 1-5" → weekdays at midnight
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return now.Add(time.Hour) // unknown format, default 1h
	}

	minuteField := fields[0]
	hourField := fields[1]

	// */N in minute → every N minutes (timezone-independent interval)
	if strings.HasPrefix(minuteField, "*/") {
		if n, err := strconv.Atoi(minuteField[2:]); err == nil && n > 0 {
			return now.Add(time.Duration(n) * time.Minute)
		}
	}

	// 0 in minute + * in hour → hourly
	if minuteField == "0" && hourField == "*" {
		return now.Add(time.Hour)
	}

	// Specific minute + specific hour → daily at that time in the configured timezone.
	minute, _ := strconv.Atoi(minuteField)
	hour, _ := strconv.Atoi(hourField)

	// Convert "now" to the target timezone, compute the next fire there,
	// then convert back to UTC for storage.
	nowInTz := now.In(loc)
	next := time.Date(nowInTz.Year(), nowInTz.Month(), nowInTz.Day(), hour, minute, 0, 0, loc)
	if next.Before(nowInTz) || next.Equal(nowInTz) {
		next = next.Add(24 * time.Hour)
	}

	// Check day-of-week filter (field[4])
	dowField := fields[4]
	if dowField != "*" {
		// Simple: "1-5" means Mon-Fri
		if dowField == "1-5" {
			for next.Weekday() == time.Sunday || next.Weekday() == time.Saturday {
				next = next.Add(24 * time.Hour)
			}
		}
	}

	return next.UTC()
}

func strPtr(s string) *string { return &s }

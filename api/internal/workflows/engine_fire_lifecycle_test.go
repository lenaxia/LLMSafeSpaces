// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// #1454/#1473: the routine-fire lifecycle pins. The #1454 consolidation
// pinned these seams' behavior as-is (characterization); #1473 flips
// the two drain-gap pins to the FIXED expectations (targetless drains
// account + auto-disable with the unified trigger_has_no_target
// payload; transient fetch errors leave the fire pending for re-drive)
// and keeps the accounting call-count pins for the extraction seams.

// Drain targetless leg (#1473 gap 1): the EXISTING fire row is updated
// (never a second row minted) with the SAME trigger_has_no_target
// payload the cron route's #1440 guard mints, and the failure is
// accounted — increment once, honor auto-disable. The silent-zombie
// door is closed on both routes.
func TestProcessPendingRoutineFire_TargetlessDrain_AccountsAndDisables(t *testing.T) {
	for _, tc := range []struct {
		name             string
		ws               *string
		autoDisableAfter int
		wantDisabled     bool
	}{
		{"nil workspace id, below threshold", nil, 2, false},
		{"empty workspace id, below threshold", strPtr(""), 2, false},
		{"at threshold, disarms", nil, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockSchedulerStore()
			store.triggers = []*wf.TriggerRow{{
				ID: "trig-drain", OwnerType: "user", OwnerID: "u1",
				Enabled: true, SourceType: types.TriggerSourceCron,
				WorkspaceID: tc.ws, AutoDisableAfter: tc.autoDisableAfter,
			}}
			fire := &wf.TriggerFireRow{ID: "fire-drain", TriggerID: "trig-drain", InputEnvelope: json.RawMessage(`{}`)}

			sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
			sched.processPendingRoutineFire(context.Background(), noopLogger{}, fire)

			if store.statuses["fire-drain"] != "failed" {
				t.Fatalf("expected failed, got %s", store.statuses["fire-drain"])
			}
			want := `{"hint":"workflow deleted (FK set null) or routine missing workspace_id","reason":"trigger_has_no_target"}`
			if got := string(store.results["fire-drain"]); got != want {
				t.Errorf("drain payload must match the cron guard's:\n got: %s\nwant: %s", got, want)
			}
			if len(store.fires) != 0 {
				t.Errorf("drain must UPDATE the existing row, never mint one; fires=%d", len(store.fires))
			}
			if store.increments["trig-drain"] != 1 {
				t.Errorf("targetless drain must account exactly once, got %d", store.increments["trig-drain"])
			}
			if store.disabled["trig-drain"] != tc.wantDisabled {
				t.Errorf("disabled=%v, want %v (autoDisableAfter=%d)", store.disabled["trig-drain"], tc.wantDisabled, tc.autoDisableAfter)
			}
		})
	}
}

// Drain fetch split (#1473 gap 2): a TRANSIENT store error is the
// platform's problem, not the fire's — the fire stays PENDING for the
// next tick's re-drive, nothing is written, nothing is counted. (The
// webhook already 202'd; killing the fire on a pool blip was the bug.)
func TestProcessPendingRoutineFire_TransientFetchError_LeavesPending(t *testing.T) {
	for _, drive := range []struct {
		name string
		fn   func(sched *Scheduler, fire *wf.TriggerFireRow)
	}{
		{"direct", func(s *Scheduler, f *wf.TriggerFireRow) {
			s.processPendingRoutineFire(context.Background(), noopLogger{}, f)
		}},
		{"tick drain", func(s *Scheduler, f *wf.TriggerFireRow) {
			store := s.Store.(*mockSchedulerStore)
			store.overridePending = []*wf.TriggerFireRow{f}
			s.tick(context.Background(), noopLogger{}, 10)
		}},
	} {
		t.Run(drive.name, func(t *testing.T) {
			store := newMockSchedulerStore()
			store.getTriggerByIDErr = fmt.Errorf("pool exhausted")
			fire := &wf.TriggerFireRow{ID: "fire-blip", TriggerID: "trig-live", InputEnvelope: json.RawMessage(`{}`)}

			sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
			drive.fn(sched, fire)

			if _, written := store.statuses["fire-blip"]; written {
				t.Errorf("transient fetch error must not write the fire, got status %s", store.statuses["fire-blip"])
			}
			if store.results["fire-blip"] != nil {
				t.Errorf("transient fetch error must not write a result, got %s", store.results["fire-blip"])
			}
			if store.increments["trig-live"] != 0 {
				t.Errorf("transient fetch error must not account, got %d increments", store.increments["trig-live"])
			}
			if store.disabled["trig-live"] {
				t.Error("transient fetch error must never disable")
			}
		})
	}
}

// Drain fetch split (#1473 gap 2): trigger GONE (wf.ErrNotFound). In
// production this is only the mid-tick race window — trigger_fires is
// ON DELETE CASCADE (migration 000016:349), so a deleted trigger's
// fires are gone with it and IncrementTriggerFailures has no row to
// hold a counter. The leg therefore fails the fire best-effort with
// the cause payload and NO accounting (schema-excluded; see the
// correction comment on #1473).
func TestProcessPendingRoutineFire_TriggerDeleted_FailsLoudUnaccounted(t *testing.T) {
	store := newMockSchedulerStore()
	fire := &wf.TriggerFireRow{ID: "fire-orphan", TriggerID: "nonexistent", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
	sched.processPendingRoutineFire(context.Background(), noopLogger{}, fire)

	if store.statuses["fire-orphan"] != "failed" {
		t.Fatalf("expected failed, got %s", store.statuses["fire-orphan"])
	}
	if got := string(store.results["fire-orphan"]); got != `{"error":"trigger not found"}` {
		t.Errorf("not-found payload must stay byte-identical, got %s", got)
	}
	if store.increments["nonexistent"] != 0 {
		t.Errorf("not-found leg must not account (no trigger row exists), got %d", store.increments["nonexistent"])
	}
}

// Account-exactly-once per outcome: every failure leg increments
// exactly once and never resets; a delivered fire resets exactly once
// and never increments. (#1454 mutation seam, retained.)
func TestExecuteRoutine_AccountingCallCounts(t *testing.T) {
	newTrigger := func() *wf.TriggerRow {
		wsID := "ws-1"
		return &wf.TriggerRow{
			ID: "trig-acc", WorkspaceID: &wsID, Prompt: "p",
			CaptureMode: types.CaptureFull, AutoDisableAfter: 100,
		}
	}
	newFire := func(i int) *wf.TriggerFireRow {
		return &wf.TriggerFireRow{
			ID: fmt.Sprintf("fire-acc-%d", i), TriggerID: "trig-acc",
			InputEnvelope: json.RawMessage(`{}`),
		}
	}

	t.Run("activation failure increments once, never resets", func(t *testing.T) {
		store := newMockSchedulerStore()
		sched := &Scheduler{Store: store, Activator: &mockActivator{fail: true}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
		sched.executeRoutine(context.Background(), noopLogger{}, newTrigger(), newFire(0))
		if store.increments["trig-acc"] != 1 || store.resets["trig-acc"] != 0 {
			t.Errorf("want 1 increment / 0 resets, got %d/%d", store.increments["trig-acc"], store.resets["trig-acc"])
		}
	})

	t.Run("script failure increments once, never resets", func(t *testing.T) {
		store := newMockSchedulerStore()
		agentd := newMockAgentd()
		agentd.errors["routine-script"] = fmt.Errorf("script crashed")
		trig := newTrigger()
		trig.ScriptPath = "/scripts/run.sh"
		sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
		sched.executeRoutine(context.Background(), noopLogger{}, trig, newFire(1))
		if store.increments["trig-acc"] != 1 || store.resets["trig-acc"] != 0 {
			t.Errorf("want 1 increment / 0 resets, got %d/%d", store.increments["trig-acc"], store.resets["trig-acc"])
		}
	})

	t.Run("agent error increments once, never resets", func(t *testing.T) {
		store := newMockSchedulerStore()
		agentd := newMockAgentd()
		agentd.errors["routine-agent"] = fmt.Errorf("agent down")
		sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
		sched.executeRoutine(context.Background(), noopLogger{}, newTrigger(), newFire(2))
		if store.increments["trig-acc"] != 1 || store.resets["trig-acc"] != 0 {
			t.Errorf("want 1 increment / 0 resets, got %d/%d", store.increments["trig-acc"], store.resets["trig-acc"])
		}
	})

	t.Run("agent error-code increments once, never resets", func(t *testing.T) {
		store := newMockSchedulerStore()
		agentd := newMockAgentd()
		agentd.errCodes["routine-agent"] = "script_failed"
		sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
		sched.executeRoutine(context.Background(), noopLogger{}, newTrigger(), newFire(3))
		if store.increments["trig-acc"] != 1 || store.resets["trig-acc"] != 0 {
			t.Errorf("want 1 increment / 0 resets, got %d/%d", store.increments["trig-acc"], store.resets["trig-acc"])
		}
	})

	t.Run("delivered resets once, never increments", func(t *testing.T) {
		store := newMockSchedulerStore()
		agentd := newMockAgentd()
		agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"done","session_id":"ses_1","tokens":{"input":1,"output":1,"total":2},"prompt":"p","parts":[]}`)
		sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
		sched.executeRoutine(context.Background(), noopLogger{}, newTrigger(), newFire(4))
		if store.resets["trig-acc"] != 1 || store.increments["trig-acc"] != 0 {
			t.Errorf("want 0 increments / 1 reset, got %d/%d", store.increments["trig-acc"], store.resets["trig-acc"])
		}
	})
}

// Cron-route targetless guard, payload pinned byte-identical (key
// order included) so the two routes' payloads cannot drift apart
// again. Accounting/auto-disable legs are already pinned by
// TestScheduler_TargetlessTriggerFailsLoudly / AutoDisables.
func TestFireRoutineTarget_Targetless_ExactPayloadAndCounts(t *testing.T) {
	store := newMockSchedulerStore()
	trigger := &wf.TriggerRow{
		ID: "trig-cron-ghost", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		WorkflowID: nil, WorkspaceID: nil, AutoDisableAfter: 5,
	}

	sched := &Scheduler{Store: store, Logger: noopLogger{}}
	sched.fireRoutineTarget(context.Background(), noopLogger{}, trigger, json.RawMessage(`{}`), time.Now().UTC())

	if len(store.fires) != 1 || store.fires[0].Status != "failed" {
		t.Fatalf("targetless cron fire must mint exactly one FAILED fire, got %+v", store.fires)
	}
	want := `{"hint":"workflow deleted (FK set null) or routine missing workspace_id","reason":"trigger_has_no_target"}`
	if got := string(store.fires[0].ActionResult); got != want {
		t.Errorf("cron payload must stay byte-identical:\n got: %s\nwant: %s", got, want)
	}
	if store.increments["trig-cron-ghost"] != 1 {
		t.Errorf("want exactly 1 increment, got %d", store.increments["trig-cron-ghost"])
	}
	if store.disabled["trig-cron-ghost"] {
		t.Error("AutoDisableAfter=5 must not disarm on the first failure")
	}
}

// Tick-level drain wiring for the targetless fix: a pending fire whose
// trigger lost its target drains through the normal scheduler tick and
// comes out failed + accounted + (at threshold) disabled.
func TestScheduler_PendingDrainTick_TargetlessTrigger_Accounts(t *testing.T) {
	store := newMockSchedulerStore()
	store.triggers = []*wf.TriggerRow{{
		ID: "trig-drain-tick", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceWebhook,
		WorkspaceID: nil, AutoDisableAfter: 1,
	}}
	fire := &wf.TriggerFireRow{ID: "fire-tick", TriggerID: "trig-drain-tick", ActionType: "routine", Status: "fired", InputEnvelope: json.RawMessage(`{}`)}
	store.overridePending = []*wf.TriggerFireRow{fire}

	sched := &Scheduler{Store: store, Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if store.statuses["fire-tick"] != "failed" {
		t.Fatalf("expected failed via tick drain, got %s", store.statuses["fire-tick"])
	}
	if store.increments["trig-drain-tick"] != 1 {
		t.Errorf("tick drain must account exactly once, got %d", store.increments["trig-drain-tick"])
	}
	if !store.disabled["trig-drain-tick"] {
		t.Error("AutoDisableAfter=1 must disarm the targetless zombie at the threshold")
	}
}

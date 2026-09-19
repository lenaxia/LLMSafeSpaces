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

// #1454: behavior-identical consolidation pins. These CHARACTERIZE the
// current semantics of the routine-fire lifecycle seams the dedup
// touches (drain guards, per-leg accounting, the success reset leg) so
// the refactor provably changes nothing. Where current behavior is a
// known gap (the drain's unaccounted targetless leg; any-error fetch
// handling) the pin documents it deliberately — fixing those is a
// behavior-change follow-up, out of this consolidation's scope.

// Drain targetless leg, preserved exactly: the EXISTING fire row is
// updated (never a second row minted), with the drain path's own
// payload, and ZERO failure accounting — the trigger keeps ticking.
// This is the pre-#1440 silent-zombie shape through the drain door,
// deliberately pinned as-is (consolidation-only scope).
func TestProcessPendingRoutineFire_TargetlessDrain_PreservedBehavior(t *testing.T) {
	for _, tc := range []struct {
		name string
		ws   *string
	}{
		{"nil workspace id", nil},
		{"empty workspace id", strPtr("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockSchedulerStore()
			store.triggers = []*wf.TriggerRow{{
				ID: "trig-drain", OwnerType: "user", OwnerID: "u1",
				Enabled: true, SourceType: types.TriggerSourceCron,
				WorkspaceID: tc.ws, AutoDisableAfter: 2,
			}}
			fire := &wf.TriggerFireRow{ID: "fire-drain", TriggerID: "trig-drain", InputEnvelope: json.RawMessage(`{}`)}

			sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
			sched.processPendingRoutineFire(context.Background(), noopLogger{}, fire)

			if store.statuses["fire-drain"] != "failed" {
				t.Fatalf("expected failed, got %s", store.statuses["fire-drain"])
			}
			if got := string(store.results["fire-drain"]); got != `{"error":"trigger has no workspace_id"}` {
				t.Errorf("drain payload must stay byte-identical, got %s", got)
			}
			if len(store.fires) != 0 {
				t.Errorf("drain must UPDATE the existing row, never mint one; fires=%d", len(store.fires))
			}
			if store.increments["trig-drain"] != 0 {
				t.Errorf("preserved behavior: drain targetless leg does NOT account, got %d increments", store.increments["trig-drain"])
			}
			if store.disabled["trig-drain"] {
				t.Error("preserved behavior: drain targetless leg never auto-disables")
			}
		})
	}
}

// Drain fetch-error leg, preserved exactly: ANY GetTriggerByID error
// (transient or not-found alike) fails the fire with the generic
// trigger-not-found payload and zero accounting. The transient-vs-
// not-found split the orphaned #1454 report proposed is a behavior
// change — pinned as-is here, follow-up material.
func TestProcessPendingRoutineFire_FetchError_PreservedBehavior(t *testing.T) {
	store := newMockSchedulerStore()
	fire := &wf.TriggerFireRow{ID: "fire-orphan", TriggerID: "nonexistent", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: newMockAgentd(), Logger: noopLogger{}}
	sched.processPendingRoutineFire(context.Background(), noopLogger{}, fire)

	if store.statuses["fire-orphan"] != "failed" {
		t.Fatalf("expected failed, got %s", store.statuses["fire-orphan"])
	}
	if got := string(store.results["fire-orphan"]); got != `{"error":"trigger not found"}` {
		t.Errorf("fetch-error payload must stay byte-identical, got %s", got)
	}
	if store.increments["nonexistent"] != 0 {
		t.Errorf("preserved behavior: fetch-error leg does NOT account, got %d increments", store.increments["nonexistent"])
	}
}

// Account-exactly-once per outcome: every failure leg increments
// exactly once and never resets; a delivered fire resets exactly once
// and never increments. These call-count pins are the mutation seam for
// the accountTriggerFailure consolidation (a botched extraction that
// double-accounts, accounts on success, or resets on failure trips
// them).
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
// order included) so the failTargetless consolidation cannot drift the
// two routes' payloads into each other. Accounting/auto-disable legs
// are already pinned by TestScheduler_TargetlessTriggerFailsLoudly /
// AutoDisables; this adds the exact-bytes and count-once assertions.
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

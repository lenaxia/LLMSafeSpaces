// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// engine_routine_outage_recovery_test.go — the #1834 routine-path
// wedge pins. The 2026-10-21 outage (01:20–22:54Z, multi-window: fast
// script_failed/"opencode returned 500" fails, then 05:00Z and
// 08:20–09:40Z slow-fail timeouts) left every trigger-fire leg of the
// headless routine path failing on the wedged opencode invocation
// surface while the platform's own circuit breaker — auto-disable —
// failed to stop the hammering for webhook-sourced triggers: pr-
// monitor-sweep accumulated ~30 failed fires with consecutiveFailures
// at 0 (the pre-#1885 ErrorCode-blind legs) and, once accounting was
// fixed, would still have kept EXECUTING every signed delivery because
// neither the webhook receiver nor the pending-fire drain consults
// trigger.Enabled. Only the cron claim filters enabled — the disable
// was decorative on the webhook route.
//
// These pins hold the wedge class to its recovery contract:
//
//  1. the outage's exact envelope (script_failed wrapping "opencode
//     returned 500", every attempt) fails the fire LOUD and counts
//     toward auto-disable exactly once (the #1441/#1878 accounting
//     family, now pinned for the retry-exhausted agent-leg shape);
//  2. auto-disable actually DISARMS the webhook route: a disabled
//     trigger's pending fire is skipped by the drain — never executed,
//     never accounted (the gate-closed discipline: absorbing is not
//     failing);
//  3. the receiver-side twin lives in webhook_receiver_test.go.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// The outage shape verbatim: every agent-leg attempt answers the
// HTTP-200 error envelope script_failed/"opencode returned 500" (the
// classifier's retryable class — 3 attempts, 2s+4s backoff), and the
// fire must land failed + accounted exactly once. Pre-#1885 the
// pre-script leg laundered this class (delivered, cf=0 — pr-monitor's
// ~30 silent failures); this pin holds the LAST leg (the agent leg)
// to the same fail-loud contract after retry exhaustion.
func TestExecuteRoutine_AgentLegOpencode500Envelope_FailsLoudAndCounts(t *testing.T) {
	store := newMockSchedulerStore()
	agentd := newMockAgentd()
	agentd.errCodes["routine-agent"] = "script_failed"
	agentd.errDetails["routine-agent"] = "opencode returned 500"
	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-1834-500", WorkspaceID: &wsID,
		Prompt:      "sweep the PRs and summarize",
		CaptureMode: types.CaptureErrorsOnly, PreserveSession: types.PreserveNever,
		AutoDisableAfter: 10,
	}
	fire := &wf.TriggerFireRow{ID: "fire-1834-500", TriggerID: "trig-1834-500", InputEnvelope: json.RawMessage(`{}`)}
	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	if store.statuses["fire-1834-500"] != "failed" {
		t.Fatalf("the retry-exhausted opencode-500 outage shape must fail the fire, got %q", store.statuses["fire-1834-500"])
	}
	if got := string(store.results["fire-1834-500"]); !strings.Contains(got, `"code":"script_failed"`) || !strings.Contains(got, "opencode returned 500") {
		t.Errorf("the fire audit must carry the envelope's code+detail verbatim, got %s", got)
	}
	if store.triggerFail["trig-1834-500"] != 1 {
		t.Fatalf("a persistent 500-class outage must count toward auto-disable exactly once per fire, got %d", store.triggerFail["trig-1834-500"])
	}
	if store.resets != nil && store.resets["trig-1834-500"] != 0 {
		t.Errorf("a failed fire must never reset the failure counter, got %d resets", store.resets["trig-1834-500"])
	}
	dispatches := 0
	for _, n := range agentd.callNodeIDs() {
		if n == "routine-agent" {
			dispatches++
		}
	}
	if dispatches != 3 {
		t.Errorf("the bounded transient retry must make exactly 3 attempts (outage cost ≈ 3×15-25s), got %d", dispatches)
	}
}

// Auto-disable must disarm BOTH entry routes (#1834 finding 3's
// circuit breaker): the cron claim filters enabled=false, but a fire
// minted by the webhook receiver (which never checked Enabled) rode
// this drain straight into executeRoutine — the disable was decorative
// for webhook triggers, and a persistent 500-class outage hammered the
// wedged headless path on every signed delivery forever.
func TestProcessPendingRoutineFire_DisabledTrigger_SkipsExecution(t *testing.T) {
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
			wsID := "ws-1"
			store.triggers = []*wf.TriggerRow{{
				ID: "trig-1834-off", OwnerType: "user", OwnerID: "u1",
				Enabled: false, SourceType: types.TriggerSourceWebhook,
				WorkspaceID: &wsID, AutoDisableAfter: 2,
				Prompt: "sweep the PRs",
			}}
			fire := &wf.TriggerFireRow{
				ID: "fire-1834-off", TriggerID: "trig-1834-off",
				ActionType: "routine", Status: "fired",
				InputEnvelope: json.RawMessage(`{}`),
			}
			agentd := newMockAgentd()
			sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
			drive.fn(sched, fire)

			if store.statuses["fire-1834-off"] != "skipped" {
				t.Fatalf("a disabled trigger's pending fire must be skipped, got %q", store.statuses["fire-1834-off"])
			}
			if got := string(store.results["fire-1834-off"]); !strings.Contains(got, "trigger_disabled") {
				t.Errorf("the skip must self-explain in the audit, got %s", got)
			}
			if calls := agentd.callNodeIDs(); len(calls) != 0 {
				t.Errorf("a disabled trigger must NEVER dispatch the headless leg, dispatched %v", calls)
			}
			if store.increments != nil && store.increments["trig-1834-off"] != 0 {
				t.Errorf("absorbing a delivery on a disabled trigger is not a failure, got %d increments", store.increments["trig-1834-off"])
			}
			if store.resets != nil && store.resets["trig-1834-off"] != 0 {
				t.Errorf("a skipped fire must not reset the counter either, got %d resets", store.resets["trig-1834-off"])
			}
		})
	}
}

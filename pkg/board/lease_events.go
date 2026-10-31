// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// lease_events.go — the seamless lease's observation plane (design
// 0072 S4): the ladder's DECISIONS — which rung renewed a lease, which
// evidence wedged a claim, which lapse re-queued a row — projected to
// an optional sink the app wiring points at the workspace event bus.
//
// The engine stays harness- and transport-agnostic (the same
// construction as NudgeSender): it reports WHAT it decided; the sink
// owns where the report goes. The nudge rung is deliberately absent —
// the app-side nudger IS the nudge publisher (design 0072 §9.3: it
// publishes lease.{task}.nudged onto the bus with a v1 point-to-point
// fallback, so a sentinel's bus event means its delivery is the
// consumer's, and a v1 send means the bus could not be trusted);
// emitting nudged here too would double-report every sentinel.
//
// Emissions are POST-COMMIT and fail-open by contract: the sink is
// observability, never a dependency — a sink failure logs at the sink
// and changes no board state. Sources and reasons are flat strings (a
// convention, not an enum — the bus vocabulary's rule): the launch set
// is pinned by the wiring tests and consumed by agentd's lease group.

import "context"

// LeaseEvent kinds — the arm of lease.{task_id}.{kind} on the bus.
const (
	// LeaseEventRenewed: the ladder answered ALIVE at one of its rungs
	// and extended the lease (bus topic lease.{task}.renewed).
	LeaseEventRenewed = "renewed"
	// LeaseEventWedged: the sentinel was positioned in the session's
	// event order and nothing followed it — the machine verdict (bus
	// topic lease.{task}.wedge_verdict).
	LeaseEventWedged = "wedge_verdict"
	// LeaseEventReclaimed: the claim lapsed and the row was re-queued
	// (or escalated) — the reclaim the 2026-10-30 incident could not
	// see (bus topic lease.{task}.reclaimed).
	LeaseEventReclaimed = "reclaimed"
)

// Renewal sources — WHICH rung answered alive (LeaseEventRenewed).
const (
	RenewSourceHeartbeat = "heartbeat_response"  // the worker answered the sentinel on the response channel
	RenewSourceMovement  = "movement"            // observed delta since last sweep (frame stream / index)
	RenewSourceReaction  = "post_nudge_reaction" // events landed after the sentinel
	RenewSourceBusy      = "busy_gate"           // derived-busy: mid-turn, silent but alive (the 2026-10-29 busy-gate)
)

// Reclaim reasons — WHY the claim was re-queued (LeaseEventReclaimed).
const (
	ReclaimReasonLapse          = "lease_lapse"            // quiet past the sentinel's measurement window
	ReclaimReasonDeadEvidence   = "dead_evidence"          // the session row is soft-deleted — provably gone
	ReclaimReasonDeliveryFailed = "nudge_delivery_failed"  // the sentinel could not be attempted; honest wall-clock path
	ReclaimReasonNoAssignee     = "lapse_no_assignee"      // no identity to observe or nudge: the wall-clock path
	ReclaimReasonPersonaLapse   = "persona_lapse"          // stage-persona (in_review) lease lapse
	ReclaimReasonPersonaCleared = "persona_config_cleared" // persona config removed mid-flight; claim converged away
)

// LeaseEvent is one ladder decision, projected post-commit. The bus
// payload contract (pkg/eventbus's lease topics) is built by the app
// sink; the engine's fields are the decision's own facts.
type LeaseEvent struct {
	Kind        string // LeaseEventRenewed | LeaseEventWedged | LeaseEventReclaimed
	TaskID      string
	TaskShortID string
	ProjectID   string
	// Assignee is the claim's session ref at the decision ("" when the
	// row carried none — the no-identity reclaim path).
	Assignee string
	// NudgeID is the sentinel the decision measured against (nudged /
	// wedged; "" otherwise).
	NudgeID string
	// Source is the renewal's rung (LeaseEventRenewed).
	Source string
	// Evidence is the wedge verdict's evidence summary (the human-
	// readable "why this is a machine verdict, not a guess").
	Evidence string
	// Reason is the reclaim's reason (LeaseEventReclaimed).
	Reason string
	// FailureCount is the row's consecutive-failure count AFTER the
	// reclaim's increment (0 for renewals).
	FailureCount int32
}

// LeaseEventSink receives the ladder's post-commit decisions. Installed
// via WithLeaseEventSink; nil (the default) is the inert pre-S4
// posture — the ladder's behavior is byte-identical, only the
// projection is absent.
type LeaseEventSink func(ctx context.Context, ev LeaseEvent)

// emitLease projects one decision to the installed sink. Fail-open:
// a nil sink is a no-op; the sink's own contract keeps a slow or
// failing bus from stalling the sweep (the app adapter bounds every
// publish).
func (s *Service) emitLease(ctx context.Context, ev LeaseEvent) {
	if s.leaseEvents != nil {
		s.leaseEvents(ctx, ev)
	}
}

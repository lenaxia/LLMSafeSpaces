// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import "context"

// AssigneeLivenessSource is the seamless lease's harness-agnostic
// observation seam (reports/specs/seamless-lease-design.md,
// owner-approved 2026-10-28).
//
// The engine NEVER imports harness internals: everything it knows about an
// assignee session's state arrives through AssigneeLivenessSource, an
// injectable seam in the turnEvidence/WorkerMessenger house pattern — Nil
// = inert, and every engine guarantee holds at Nil (the existing suite
// passes unchanged; that inert contract is pinned).
//
// Three facts per session, deliberately POOR (poverty is the portability):
//
//   - MovedSinceLastSweep: the harness observed the session do ANYTHING
//     since the previous observation of this row. A same-source delta —
//     never a wall-clock recency comparison, which is skew-fragile (a
//     cached observation or a 90s clock offset would renew dead workers).
//   - Exists: false = dead-evidence — the session provably no longer
//     exists (reaped, deleted). Absence of evidence NEVER decodes as
//     death: an unobservable session is no-evidence, and no-evidence
//     degrades to the honest wall-clock path, never a fast reclaim.
//   - EventsAfterNudge: the ordinal wedge query — are there any session
//     events AFTER the transcript position of the message carrying the
//     row's latest nudge marker? Answered in the harness's totally
//     ordered event log; valid only when the row carries a nudge_id.
//     Position, not time: the sentinel exists precisely so no timestamp
//     is ever compared across processes. Even a thinking model emits a
//     turn-start; silence after a positioned sentinel is a machine
//     wedge verdict, not a heuristic.
//
// The wedge predicate is THREE-legged (item 6 amendment): nudged ∧ no
// post-sentinel events ∧ no post-nudge HEARTBEAT ⇒ wedge. The
// heartbeat (board_heartbeat, the nudge-response channel) is a strong
// ALIVE signal OR'd in at EVERY rung — its arrival is recorded on the
// row at server clock (last_heartbeat_at), so a response later than
// nudged_at saves the worker whatever the observation seam reports. At
// the no-evidence rung (Nil source, degraded adapter, unindexed
// session) the heartbeat is the ONLY wedge detector: deliver-a-message
// + call-a-tool is the minimal harness capability — the
// harness-agnostic wedge floor.
//
// Call hygiene: implementations must be cheap (~2s budget per call — the
// reconciler sweeps on a cadence and stalls here stall reclaims GLOBALLY,
// which would break the honest-lapse property for every row). Failure to
// answer = no-evidence, loudly. K consecutive failures should
// circuit-break the source to inert until recovery — and the degraded
// state must be VISIBLE (a gauge/counter), because a silently dead source
// regresses the whole fleet to heartbeat semantics and the churn returns
// wearing a different hat.
//
// Wake capability is NOT per-session: board assignees are spawner-minted
// top-level sessions by construction (claims issue only to them — pinned),
// so "can the platform wake this session" is a wiring fact (source +
// messenger installed), not an observation.
type AssigneeLivenessSource interface {
	ObserveAssignees(sess SessionObservations) SessionObservationResults
}

// AssigneeRef is one row's observation request: the assignee session and,
// when the row carries a live nudge, the marker to anchor the ordinal
// query against ("" = skip the wedge arm).
type AssigneeRef struct {
	SessionID string
	NudgeID   string
}

// AssigneeObservation is the per-session answer. The zero value is the
// safest possible answer: no movement claimed, existence unknown, no
// post-nudge events claimed — the engine renews nothing and reclaims
// nothing on the strength of a zero value.
type AssigneeObservation struct {
	// Observed is false when the source could not answer for this
	// session at all (timeout, degraded, unknown session to THIS
	// harness — distinct from Exists=false, which is positive
	// dead-evidence).
	Observed bool
	// MovedSinceLastSweep: renewal evidence (delta since the source's
	// previous answer for this session).
	MovedSinceLastSweep bool
	// Exists: false = the session provably does not exist. Only
	// meaningful when Observed.
	Exists bool
	// EventsAfterNudge: any session events after the latest nudge
	// marker's transcript position. Only meaningful when Observed and
	// the request carried a NudgeID.
	EventsAfterNudge bool
}

// SessionObservations batches one sweep's questions: one call per sweep,
// not one round trip per row (the seam is the reconciler's hot path).
type SessionObservations struct {
	Refs []AssigneeRef
}

// SessionObservationResults is the batched answer keyed by session id.
type SessionObservationResults map[string]AssigneeObservation

// NilLiveness is the inert source: every question answered with the zero
// observation. The engine degrades honestly — with no nudge seam either,
// pure wall-clock lease + clean arbitration (today's semantics, correct,
// merely less seamless); with a nudge seam, the ladder still runs its
// nudge → heartbeat-or-wedge floor (the response channel needs no
// observation). Correctness never depends on integration depth.
type NilLiveness struct{}

func (NilLiveness) ObserveAssignees(sess SessionObservations) SessionObservationResults {
	out := make(SessionObservationResults, len(sess.Refs))
	for _, ref := range sess.Refs {
		out[ref.SessionID] = AssigneeObservation{Observed: false}
	}
	return out
}

// NudgePayload is one lease-nudge sentinel: the id the engine minted and
// stored on the row (nudge_id — the ordinal anchor the next observation
// sweep's EventsAfterNudge answers against), the task it concerns, and
// the directive prose contract (item 6: RESPOND first via
// board_heartbeat, then continue working, advance the stage, or declare
// failure — never restart). The harness-side sender composes the
// agentmessage-v1 sentinel + the surgical prose and delivers through
// the session-message machinery.
type NudgePayload struct {
	NudgeID     string
	TaskShortID string
	TaskID      string
}

// NudgeSender delivers one sentinel nudge. An error means delivery
// could not be attempted (bounce/transport) — the engine then treats
// the nudge arm as unavailable this sweep and falls to wall-clock; a
// delivered-but-ignored nudge is NOT this function's concern (the
// ordinal sweep question answers it).
type NudgeSender func(ctx context.Context, sessionID string, p NudgePayload) error

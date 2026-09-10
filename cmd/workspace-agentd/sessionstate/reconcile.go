// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"sort"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// reconcile.go — epic-71 / 1b (#1311): ledger store-evidence convergence.
// The ledger is platform truth for delivery lifecycle, but its non-terminal
// rows are LEASES against the harness store, not facts: rows land in
// LEDGERED/ADMITTED/STALLED and stay there when the events that would
// advance them are lost (harness death mid-turn, agentd restart, promotion
// event dropped). The reconcile pass re-derives every unresolved row from
// store evidence — never assumes, never errors on absence — and re-derives
// BUSY from the same evidence (the ledger side supplies "no un-promoted
// admissions" to #1310's status re-derivation).
//
// Invariants served: S7 (ledger ⊆ evidence past no deadline), S8 (reopen +
// reseed satisfies S7 with no operator action — the sweep runs inside every
// Reseed, so deploying it auto-heals wedged sessions), L4/L5 (bounded
// convergence on the ReconcileCadence ticker).

// LeaseConvergenceBound is the epic-71 lease clock: the shared convergence
// bound for every lease this authority holds (#1311's ledger deadlines and
// #1310's pending-input lease — ONE constant, proposed 30s per the stress
// matrix, tune before freezing).
const LeaseConvergenceBound = 30 * time.Second

// ReconcileCadence is the background reconcile-pass interval: half the
// convergence bound, so one missed tick still converges inside it.
const ReconcileCadence = LeaseConvergenceBound / 2

// defaultAdmissionDeadline bounds the LEDGERED state: a row that has not
// admitted within this window (retry envelope ~6s + boot replay) is swept
// to FAILED — re-armable at attempt+1 by the outbox.
const defaultAdmissionDeadline = 1 * time.Minute

// ReconcileStats reports one reconcile pass's outcome (S7's observable
// surface: what the sweep advanced, what it could not).
type ReconcileStats struct {
	// Promoted / TurnEnded / Failed count rows the pass advanced.
	Promoted  int
	TurnEnded int
	Failed    int
	// BusyCleared counts sessions whose BUSY view was re-derived idle
	// (evidence idle + no unresolved rows remain).
	BusyCleared int
	// EvidenceFailures counts store-evidence reads that errored — rows
	// were left untouched (never an authoritative empty) and retry on the
	// next pass.
	EvidenceFailures int
}

// Reconcile runs one store-evidence convergence pass over the ledger and
// the BUSY projection. Serialized against Reseed (reseedMu); per-session
// row advancement runs under the session's single-flight lock so it can
// never interleave with an in-flight admission. Store I/O happens outside
// every authority lock (M3.1: no synchronous harness call on a hot path —
// this is a background/ cadence pass, never request-scoped).
func (a *Authority) Reconcile(ctx context.Context) ReconcileStats {
	a.reseedMu.Lock()
	defer a.reseedMu.Unlock()
	return a.reconcileLocked(ctx)
}

// reconcileLocked is Reconcile's core; reseedMu must be held (Reseed calls
// it with seeds it already read).
func (a *Authority) reconcileLocked(ctx context.Context) ReconcileStats {
	if a.ledger == nil || a.cfg.Store == nil {
		return ReconcileStats{}
	}
	rows := a.ledger.unresolvedBySession()
	busy := a.busySessions()

	// Cheap no-op: nothing unresolved, nothing wedged busy. The pass must
	// stay pod-local-free when there is nothing to converge.
	if len(rows) == 0 && len(busy) == 0 {
		return ReconcileStats{}
	}

	seeds, err := a.cfg.Store.SessionStates(ctx)
	if err != nil {
		a.logger.Warn("sessionstate reconcile: store evidence read failed — rows untouched",
			zap.Error(err))
		stats := ReconcileStats{EvidenceFailures: 1}
		a.recordReconcile(stats)
		return stats
	}
	return a.sweepAgainstEvidence(ctx, seeds)
}

// sweepAgainstEvidence applies the reconciliation matrix per session:
//
//	LEDGERED  past admission deadline               → FAILED (re-armable)
//	ADMITTED/STALLED + message present in store     → PROMOTED
//	ADMITTED/STALLED + turn ended (idle/absent)     → TURN_ENDED
//	ADMITTED/STALLED + busy + message absent        → stays (turn running)
//
// Message-absence is only consulted from a SUCCESSFUL read: an evidence
// error skips that session's rows (counted), never treats them as absent.
// BUSY is re-derived on the same pass: a session whose evidence is idle
// and whose rows all resolved clears busy/status/in-flight parts.
func (a *Authority) sweepAgainstEvidence(ctx context.Context, seeds map[string]SessionSeed) ReconcileStats {
	var stats ReconcileStats
	if a.ledger == nil {
		return stats
	}

	sessions := make(map[string]struct{}, len(seeds))
	for sid := range a.ledger.unresolvedBySession() {
		sessions[sid] = struct{}{}
	}
	for _, sid := range a.busySessions() {
		sessions[sid] = struct{}{}
	}
	ordered := make([]string, 0, len(sessions))
	for sid := range sessions {
		ordered = append(ordered, sid)
	}
	sort.Strings(ordered)

	for _, sid := range ordered {
		if ctx.Err() != nil {
			return stats
		}
		seed, inStore := seeds[sid]
		turnEnded := !inStore || seed.Status == abiv1.SessionStatus_SESSION_STATUS_IDLE || seed.Status == abiv1.SessionStatus_SESSION_STATUS_ERROR

		rows := a.ledger.rowsForSweep(sid)
		need := make([]string, 0, len(rows))
		for _, r := range rows {
			if r.MessageID != "" {
				need = append(need, r.MessageID)
			}
		}
		var present map[string]bool
		if len(need) > 0 {
			p, err := a.cfg.Store.MessagePresence(ctx, sid, need)
			if err != nil {
				// No usable message evidence — but status evidence stands:
				// the turn-ended arm still applies (a nil present map is
				// safe in the matrix; rows in busy sessions fall through
				// untouched either way). Counted, retried next pass.
				stats.EvidenceFailures++
				a.logger.Warn("sessionstate reconcile: message evidence read failed — continuing on status evidence",
					zap.String("session", sid), zap.Error(err))
			} else {
				present = p
			}
		}

		lock := a.sessionLock(sid)
		lock.Lock()
		promoted, ended, failed := a.ledger.sweepSession(sid, present, turnEnded, time.Now())
		lock.Unlock()
		stats.Promoted += promoted
		stats.TurnEnded += ended
		stats.Failed += failed

		// Busy re-derivation (L4): busy/idle ground truth is the harness
		// (#1312 ownership table — busy iff a turn runs). A LEDGERED row
		// is queued-not-running, so evidence-idle clears busy regardless
		// of queued entries; queued admissions re-mark busy via their
		// MESSAGE_START/PART_START folds when the turn actually starts.
		if turnEnded {
			evStatus := abiv1.SessionStatus_SESSION_STATUS_IDLE
			if inStore {
				evStatus = seed.Status
			}
			stats.BusyCleared += a.clearBusyFromEvidence(sid, evStatus)
		}
	}
	// Single recording site: every sweep's outcomes (cadence pass AND the
	// reseed-embedded boot heal) land in the cumulative Metrics counters.
	a.recordReconcile(stats)
	return stats
}

// recordReconcile folds one pass's outcomes into the cumulative Metrics
// counters (S7's observable surface).
func (a *Authority) recordReconcile(stats ReconcileStats) {
	if stats == (ReconcileStats{}) {
		return
	}
	a.mu.Lock()
	a.reconPromoted += int64(stats.Promoted)
	a.reconTurnEnded += int64(stats.TurnEnded)
	a.reconFailed += int64(stats.Failed)
	a.reconBusyCleared += int64(stats.BusyCleared)
	a.reconEvidenceFails += int64(stats.EvidenceFailures)
	a.mu.Unlock()
}

// clearBusyFromEvidence re-derives one session's BUSY view from sweep
// evidence: the harness is not running a turn, so busy must not survive
// (L4). evStatus carries the evidence's own status (IDLE for an absent
// session; ERROR keeps the error visible). Returns 1 when the view was
// cleared. The a.mu hold is tiny (no I/O) — projection reads/writes only.
func (a *Authority) clearBusyFromEvidence(sid string, evStatus abiv1.SessionStatus) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.sessions[sid]
	if !ok || !rec.busy {
		return 0
	}
	rec.busy = false
	rec.status = evStatus
	rec.inFly = nil
	return 1
}

// busySessions snapshots the session IDs whose view is BUSY (the wedge
// candidates for re-derivation).
func (a *Authority) busySessions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.sessions))
	for sid, rec := range a.sessions {
		if rec.busy {
			out = append(out, sid)
		}
	}
	return out
}

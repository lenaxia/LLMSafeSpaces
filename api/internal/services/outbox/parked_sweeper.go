// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/obs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// parked_sweeper.go — #1316 (epic 71 / 0b): the outbox's parked-error
// recovery. The 2026-09-10 incident left status:error entries parked on
// honest timeouts while the agentd ledger still held them admitted; the
// only automatic re-arm path matched one lastError string and ran only
// on the Active phase transition. Nothing reconciled parked entries
// against the ledger's truth — recovery meant Valkey surgery (#1308).
//
// Two halves:
//   - the sweeper: periodic + on-Active-transition re-verification of
//     status:error entries against the ledger for their last attempt —
//     admitted-or-later completes (the #1308 manual procedure,
//     automated), failed-with-budget re-arms, failed-and-exhausted stays
//     (terminal; the user-facing retry/dismiss path owns it);
//   - the park guard: the error-park write consults the ledger first —
//     a terminus timeout with the row still LEDGERED/ADMITTED never
//     parks (agentd owns admission; the fuel-removal half of the
//     ses_f73747f8 error-pill → retry → re-admission cycle).

// Ledger state constants — the frozen ABI enum names (pkg/abi/v1
// LedgerState_*). Kept local so the outbox keeps zero generated-code
// coupling (same principle as the terminus's hand-rolled transport);
// a handler-side test pins them against the generated enum.
const (
	LedgerStateLedgered  = "LEDGER_STATE_LEDGERED"
	LedgerStateAdmitted  = "LEDGER_STATE_ADMITTED"
	LedgerStatePromoted  = "LEDGER_STATE_PROMOTED"
	LedgerStateTurnEnded = "LEDGER_STATE_TURN_ENDED"
	LedgerStateStalled   = "LEDGER_STATE_STALLED"
	LedgerStateFailed    = "LEDGER_STATE_FAILED"
)

// ledgerStateCompletes mirrors the terminus's I10 mapping: exactly the
// admission-implying states complete. Unknown states never complete.
func ledgerStateCompletes(state string) bool {
	switch state {
	case LedgerStateAdmitted, LedgerStatePromoted, LedgerStateTurnEnded, LedgerStateStalled:
		return true
	}
	return false
}

// LedgerProbe reports the agentd delivery ledger's state for
// (entry, attempt). Contract:
//   - ("", nil): no row exists for that (entry, attempt)
//   - (state, nil): the row's current state
//   - (_, err): the probe failed (pod unreachable) — indeterminate
//
// Read-only and idempotent; wired iff the agentd terminus deliverer is
// the delivery regime (adapter mode leaves it nil and the sweeper is a
// no-op, preserving legacy behavior).
type LedgerProbe func(ctx context.Context, workspaceID, sessionID, entryID string, attempt uint32) (state string, err error)

// Sweep cadence and guard tunables (vars for tests).
var (
	// ParkedSweepInterval bounds parked-entry indeterminacy: L9
	// (#1312/#1316 proposed budget) caps parked → {completed, re-armed,
	// confirmed-terminal} at ≤5min; a 60s sweep leaves four re-tries of
	// headroom for unreachable pods. This is an API-side recovery bound,
	// distinct from the agentd lease clock (#1319: LeaseConvergenceBound
	// 30s / ReconcileCadence 15s governs lease expiry, not parked-pill
	// recovery); coherence between the two families is pinned in the
	// #1312 budget table (proposal on #1314).
	ParkedSweepInterval = 60 * time.Second
	// ownsAdmissionRePollBackoff gates the re-poll of a guard-held
	// (still delivering) entry. The poll itself blocks up to the
	// terminus inline window, so this only paces re-entry.
	ownsAdmissionRePollBackoff = 15 * time.Second
	// probeTimeout bounds one ledger probe (park guard + sweeper): a
	// status lookup is pod-local HTTP; a hung pod fails fast and the
	// caller treats the outcome as indeterminate.
	probeTimeout = 5 * time.Second
)

// sweepLockRetryEvery/sweepLockRetryBudget bound the sweep's wait on a
// contended per-session delivery lock before deferring the session to
// the next pass (vars for tests).
var (
	sweepLockRetryEvery  = 20 * time.Millisecond
	sweepLockRetryBudget = 2 * time.Second
)

// 0b observability (#1312 canary inputs): sweep outcomes and a last-run
// gauge — every periodic loop must be detectably alive.
var (
	parkedSweepOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_outbox_parked_sweeper_outcomes_total",
		Help: "Parked-error sweeper outcomes (#1316): verified (probed), completed (ledger admitted-or-later), rearmed (failed with budget), stayed (ledgered/terminal), indeterminate (probe failed — next pass).",
	}, []string{"outcome"})
)

// SetLedgerProbe wires the ledger truth source. Call before Run.
func (s *Service) SetLedgerProbe(p LedgerProbe) { s.ledgerProbe = p }

// SweepWorkspaceParkedErrors re-verifies one workspace's parked error
// entries against the ledger. Returns the number recovered (completed
// or re-armed). Best-effort per session — a missed entry retries on the
// next sweep; idempotent on already-swept state.
func (s *Service) SweepWorkspaceParkedErrors(ctx context.Context, workspaceID string) (int, error) {
	return s.sweepParkedErrors(ctx, workspaceID)
}

// sweepParkedErrors implements the #1316 decision table for every
// status:error entry of workspaceID ("" = all sessions — the Run loop's
// periodic pass). Each session's pass runs under the per-session
// delivery lock: the sweep mutates lists on a periodic cadence, so its
// LRange→LRem/LSet window would otherwise race a concurrent deliverOne
// (or a peer API replica's sweep) on the same session — an index-based
// LSet against a mutated list overwrites the wrong entry. The lock is
// briefly contended: a same-tick deliverOne routinely acquires it first
// (parked entries are never that worker's payload), so the sweep waits
// out short deliveries and only defers the session to the next pass
// when a real (turn-length) delivery holds it.
func (s *Service) sweepParkedErrors(ctx context.Context, workspaceID string) (int, error) {
	if s.ledgerProbe == nil {
		return 0, nil
	}
	recovered := 0
	now := time.Now().UTC()
	for _, p := range s.sessions(ctx) {
		ws, ses := p[0], p[1]
		if workspaceID != "" && ws != workspaceID {
			continue
		}
		token, ok := s.acquireLockWithRetry(ctx, ws, ses)
		if !ok {
			continue
		}
		n := s.sweepSessionParked(ctx, ws, ses, now)
		s.releaseLockDetached(ctx, ws, ses, token)
		recovered += n
	}
	return recovered, nil
}

// acquireLockWithRetry waits out short lock holds (same-tick delivery
// bookkeeping) before deferring: parked entries are never mid-delivery
// themselves, so only a live turn (DeliveryTimeout-class) exceeds the
// retry budget.
func (s *Service) acquireLockWithRetry(ctx context.Context, ws, ses string) (string, bool) {
	deadline := time.Now().Add(sweepLockRetryBudget)
	for {
		token, ok := s.acquireLock(ctx, ws, ses)
		if ok {
			return token, true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(sweepLockRetryEvery):
		}
	}
}

// Note (r2 robustness): the session lock is held across probes (up to
// 2 x probeTimeout per entry, bounded by the pass budget), so a
// black-holed pod's sessions consume sweep-pass budget and can starve
// other sessions' sweeps for that cycle — delivery is blocked only for
// the held session, and L9 retains headroom at the 60s cadence.
func (s *Service) sweepSessionParked(ctx context.Context, ws, ses string, now time.Time) int {
	qk := qKey(ws, ses)
	vals, err := s.client.LRange(ctx, qk, 0, -1).Result()
	if err != nil {
		return 0
	}
	recovered := 0
	// Descending: a completed entry's LRem shifts every later index
	// down, so an ascending LSet against the snapshot would overwrite
	// the WRONG entry — silently destroying an innocent neighbor (S3).
	// Mutations at index i only ever affect indices > i; iterating from
	// the tail keeps every remaining snapshot index valid.
	for i := len(vals) - 1; i >= 0; i-- {
		v := vals[i]
		var e Entry
		if json.Unmarshal([]byte(v), &e) != nil || e.Status != StatusError {
			continue
		}
		outcome := s.dispositionParked(ctx, ws, ses, &e, now)
		parkedSweepOutcomes.WithLabelValues(outcome).Inc()
		switch outcome {
		case "completed":
			// Cross-list exactly-once claim: only the winner fires
			// (the multi-replica storm double-fire, main-red twice).
			if s.claimDelivered(ctx, ws, ses, v, true) > 0 {
				s.fireOnDelivered(ws, ses, e)
				recovered++
			}
		case "rearmed":
			raw, merr := json.Marshal(e)
			if merr != nil || s.client.LSet(ctx, qk, int64(i), string(raw)).Err() != nil {
				continue
			}
			recovered++
		}
	}
	return recovered
}

// dispositionParked decides one parked entry's fate against the ledger,
// mutating e for the re-arm case. Probes the last attempt first
// (attemptOf(e.Attempts) — each recorded failure corresponds to a
// driven attempt), then ALWAYS the ambiguous in-flight attempt+1 row:
// a FAILED row at Attempts must not mask an ADMITTED row at Attempts+1
// (the unverifiable class's forever-stay shape from the r3 review).
func (s *Service) dispositionParked(ctx context.Context, ws, ses string, e *Entry, now time.Time) string {
	state, err := s.probeLedger(ctx, ws, ses, e.ID, e.Attempts)
	if err != nil {
		return "indeterminate"
	}
	parkedSweepOutcomes.WithLabelValues("verified").Inc()
	if ledgerStateCompletes(state) {
		return "completed"
	}
	next, nerr := s.probeLedger(ctx, ws, ses, e.ID, e.Attempts+1)
	if nerr != nil {
		return "indeterminate"
	}
	if ledgerStateCompletes(next) {
		return "completed"
	}
	if state == LedgerStateLedgered || next == LedgerStateLedgered {
		// agentd owns admission; its state deadlines (#1311) resolve the
		// row and the next sweep acts on the terminal state.
		return "stayed"
	}
	attempts := e.Attempts
	if next == LedgerStateFailed {
		// Adopt the in-flight row's observed failure as the recorded
		// attempt count — the same bookkeeping the terminus performs
		// when it observes a failure in-band, keeping the next POST's
		// attempt numbering truthful. (A FAILED row at Attempts with no
		// row at Attempts+1 already has truthful numbering.)
		attempts = e.Attempts + 1
	}
	// evidence is definitive-or-absent (FAILED rows / no rows at all):
	if e.LastError == lastErrUnverifiable {
		// The unverifiable class re-arms to VERIFYING only
		// (SweepWorkspaceUnverifiable) — a pending re-arm would blind
		// re-POST an entry whose transcript fate is unknown (#987).
		return "stayed"
	}
	if attempts >= MaxAttempts {
		return "stayed"
	}
	e.Status = StatusPending
	e.Attempts = attempts
	e.LastError = ""
	e.TransientFails = 0
	e.NextAttemptAt = now
	return "rearmed"
}

// probeLedger bounds one probe call (probeTimeout); attempt is the int
// counter from the entry.
func (s *Service) probeLedger(ctx context.Context, ws, ses, entryID string, attempt int) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return s.ledgerProbe(pctx, ws, ses, entryID, uint32(attempt)) //nolint:gosec // G115: bounded by retry policy
}

// parkGuard is the never-park-while-ledger-holds check at the error-park
// write. Returns (completes, holds):
//   - completes: the ledger shows admitted-or-later — the entry is done,
//     complete it instead of parking (the automated #1308 procedure)
//   - holds: the row is LEDGERED — agentd owns admission; keep the entry
//     delivering on a bounded re-poll, never park
//   - neither: park as before (failed row, no row, or indeterminate probe)
//
// Only consulted at a park threshold (attempt or transient budget);
// below it the failure branch re-arms to pending exactly as before, and
// the terminus's prior-attempt resolution already prevents re-POSTing a
// live row. Probes both real attempt rows (Attempts and Attempts+1 —
// unconditionally: a FAILED row at Attempts must not mask an ADMITTED
// row at Attempts+1) so counter alignment can never hide an admission.
func (s *Service) parkGuard(ctx context.Context, ws, ses string, e Entry) (completes, holds bool) {
	if s.ledgerProbe == nil {
		return false, false
	}
	state, err := s.probeLedger(ctx, ws, ses, e.ID, e.Attempts)
	if err != nil {
		return false, false
	}
	if ledgerStateCompletes(state) {
		return true, false
	}
	if state == LedgerStateLedgered {
		return false, true
	}
	next, nerr := s.probeLedger(ctx, ws, ses, e.ID, e.Attempts+1)
	if nerr == nil {
		if ledgerStateCompletes(next) {
			return true, false
		}
		return false, next == LedgerStateLedgered
	}
	return false, false
}

// applyParkGuardDisposition executes a guard override at a park
// threshold: completes → finish the entry (staging drain + the single
// confirmed-delivery seam); holds → stay delivering on a bounded re-poll.
func (s *Service) applyParkGuardDisposition(completes bool, ctx context.Context, ws, ses, qk, dk string, idx int, staged []byte, e Entry, now time.Time) {
	if completes {
		// Cross-list exactly-once claim, same as the sweeper.
		if s.claimDelivered(ctx, ws, ses, string(staged), true) > 0 {
			s.fireOnDelivered(ws, ses, e)
		}
		return
	}
	e.Status = StatusDelivering
	e.NextAttemptAt = now.Add(ownsAdmissionRePollBackoff)
	s.restoreStaged(ctx, qk, dk, idx, staged, e)
}

// stampLoopLiveness records the periodic loop's completed pass on the
// shared epic-71 family. pkg/obs owns the family's ONLY registration
// (0c part 2: two per-package promauto registrations of the same name
// panic at init). Call sites: the Run loop only — the on-transition
// sweep share is NOT loop liveness (stamping there would keep a dead
// loop looking fresh under transition churn).
func (s *Service) stampLoopLiveness() {
	obs.StampLoopLastRun(obs.LoopOutboxParkedSweeper)
}

// ledgerProbeForTest reports whether a probe is wired (regime assertions).
func (s *Service) ledgerProbeForTest() LedgerProbe { return s.ledgerProbe }

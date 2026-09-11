// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// lease.go — epic-71 / 2a (#1310 slice B): pending inputs are leases.
// An opencode ask exists exactly as long as a tool call is blocked on it;
// the harness drops it with no lifecycle event when the turn aborts.
// The projection therefore re-verifies its pending set against store
// truth (the /question + /permission live registries via the Store seam)
// on every snapshot serve and on the reconcile cadence. Diff semantics:
// projected−live resolves (InputResolved emitted — browsers clear);
// live−projected appears (InputRequest emitted); a gather failure never
// mutates the projection (never an authoritative empty). BUSY re-derives
// from harness status on the same pass, gated by the lease window so a
// racing status read cannot clear a fresh busy-mark.
//
// Invariants: S5 (projection ⊆ truth modulo the lease window), S8 (the
// reseed already clears the pending set; the lease pass keeps it true
// under the lease model), L3 (convergence within one ReconcileCadence
// tick of divergence), L4 (status convergence on the same bound).

// leaseBound returns the busy-lease window (test-overridable).
func (a *Authority) leaseBound() time.Duration {
	if a.leaseBoundOverride != 0 {
		return a.leaseBoundOverride
	}
	return LeaseConvergenceBound
}

// SetLeaseBoundForTest overrides the busy-lease expiry bound (≤0 forces
// immediate expiry; 0 restores the default).
func (a *Authority) SetLeaseBoundForTest(d time.Duration) { a.leaseBoundOverride = d }

// knownSessionCount counts projected session records (the lease pass's
// cadence gate: known sessions stay on the gather cadence).
func (a *Authority) knownSessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

// pendingSessions lists sessions with a non-empty projected pending set.
func (a *Authority) pendingSessions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for sid, rec := range a.sessions {
		if rec != nil && len(rec.pending) > 0 {
			out = append(out, sid)
		}
	}
	return out
}

// diffPendingLeases reconciles projected pending sets and BUSY state
// against store truth. Sessions mid-admission are skipped (TryLock, the
// 1b concurrency shape); the next tick converges them. Lock order:
// session single-flight, then a.mu — the acyclic order resolveByAbsence
// established.
func (a *Authority) diffPendingLeases(seeds map[string]SessionSeed) (resolved, appeared int) {
	scope := map[string]bool{}
	for sid := range seeds {
		scope[sid] = true
	}
	a.mu.Lock()
	for sid, rec := range a.sessions {
		if rec != nil && (len(rec.pending) > 0 || rec.busy) {
			scope[sid] = true
		}
	}
	a.mu.Unlock()

	for sid := range scope {
		lock := a.sessionLock(sid)
		if !lock.TryLock() {
			continue // a live admission owns the session; next tick converges
		}
		r, ap := a.diffSessionLease(sid, seeds[sid])
		resolved += r
		appeared += ap
		lock.Unlock()
	}
	return resolved, appeared
}

// diffSessionLease applies the lease diff for one session; the session
// single-flight must be held.
func (a *Authority) diffSessionLease(sid string, seed SessionSeed) (resolved, appeared int) {
	live := map[string]*abiv1.InputRequest{}
	for _, in := range seed.PendingInputs {
		if in != nil && in.GetId() != "" {
			live[in.GetId()] = in
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.sessions[sid]
	if rec == nil {
		if len(live) == 0 {
			return 0, 0
		}
		// Live asks for a session the projection never saw (restart
		// window): materialize the record through the event fold.
		for _, in := range seed.PendingInputs {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, Input: in})
			appeared++
		}
		return 0, appeared
	}

	for id := range rec.pending {
		if _, stillLive := live[id]; !stillLive {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, Input: &abiv1.InputRequest{Id: id}})
			resolved++
		}
	}
	for _, in := range seed.PendingInputs {
		if in == nil || in.GetId() == "" {
			continue
		}
		if _, projected := rec.pending[in.GetId()]; !projected {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, Input: in})
			appeared++
		}
	}
	// Note: rec.pending mutates through applyLocked above; the appeared
	// loop re-reads via rec.pending each iteration — safe under a.mu.

	a.rederiveStatusLocked(sid, rec, seed.Status)
	return resolved, appeared
}

// rederiveStatusLocked converges BUSY/IDLE from harness status, gated by
// the lease window: a busy-mark inside the window holds (its status event
// may simply be late); past it, harness truth wins. a.mu must be held.
func (a *Authority) rederiveStatusLocked(sid string, rec *sessionRecord, truth abiv1.SessionStatus) {
	switch truth {
	case abiv1.SessionStatus_SESSION_STATUS_BUSY:
		if !rec.busy {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
		}
	case abiv1.SessionStatus_SESSION_STATUS_IDLE, abiv1.SessionStatus_SESSION_STATUS_ERROR:
		if rec.busy && time.Since(rec.busySince) > a.leaseBound() {
			st := truth
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: st})
		}
	}
}

// refreshSessionLeaseOnServe refreshes one session's pending lease before
// a snapshot is built (browser refresh is exactly when humans notice
// staleness). One store gather per serve — pod-local, bounded by the
// caller's context; a gather failure serves the projection degraded.
func (a *Authority) refreshSessionLeaseOnServe(ctx context.Context, sid string) {
	if a.cfg.Store == nil {
		return
	}
	seeds, err := a.cfg.Store.SessionStates(ctx)
	if err != nil {
		a.logger.Warn("sessionstate: lease gather on serve failed — serving projection degraded",
			zap.String("session", sid), zap.Error(err))
		return
	}
	lock := a.sessionLock(sid)
	if !lock.TryLock() {
		return // an admission owns the session; the snapshot serves current projection
	}
	defer lock.Unlock()
	a.diffSessionLease(sid, seeds[sid])
}

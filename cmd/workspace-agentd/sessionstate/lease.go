// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"sync"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// lease.go — epic-71 / 2a (#1310 slice B): pending inputs are leases.
// An opencode ask exists exactly as long as a tool call is blocked on it;
// the harness drops it with no lifecycle event when the turn aborts.
// The projection therefore re-verifies its pending set against the live
// ask registries (Store.PendingInputs — STRICT failure semantics: an
// endpoint failure is indeterminate, never empty) on every snapshot serve
// and on the reconcile cadence. Diff semantics: projected−live resolves
// (InputResolved emitted — browsers clear); live−projected appears
// (InputRequest emitted); a gather failure never mutates the projection.
//
// BUSY re-derivation runs on the cadence pass from SessionStates status.
// In the LEDGER-WIRED topology (production), 1b's evidence sweep is the
// authoritative busy-clear (seq-gated against the evidence read — a
// busy-mark newer than the evidence is never cleared); the lease window
// below governs the ledger-less topology and is a backstop behind the
// sweep's faster seq gate.
//
// Invariants: S5 (projection ⊆ truth modulo the lease window), S8 (the
// reseed clears the pending set; the lease pass keeps it true), L3 (one
// ReconcileCadence tick), L4 (the same pass).

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

// leaseGather is the serve-path singleflight: concurrent snapshot serves
// coalesce onto one in-flight gather, and a fresh result (gatherTTL) is
// reused — a refresh storm costs one gather per TTL window, not per
// serve (leg 9).
const gatherTTL = 500 * time.Millisecond

// serveGatherTimeout bounds ONE serve's lease gather: tighter than the
// reconcile pass budget — a snapshot serve must degrade fast, never hold
// a browser refresh hostage to a hung store (r1 finding 2).
const serveGatherTimeout = 2 * time.Second

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

// knownSessionCount counts projected session records (the lease pass's
// cadence gate: known sessions stay on the gather cadence).
func (a *Authority) knownSessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

// diffPendingLeases reconciles the projected pending set against the
// live ask registries. Sessions mid-admission are skipped (TryLock, the
// 1b concurrency shape); the next tick converges them. Lock order:
// session single-flight, then a.mu — the acyclic order resolveByAbsence
// established. A PendingInputs failure skips the pending diff entirely
// (statuses still converge — their gather has its own error path).
func (a *Authority) diffPendingLeases(ctx context.Context) (resolved, appeared int, err error) {
	live, err := a.cfg.Store.PendingInputs(ctx)
	if err != nil {
		a.logger.Warn("sessionstate lease: pending gather failed — projection untouched",
			zap.Error(err))
		return 0, 0, err
	}
	scope := map[string]bool{}
	for sid := range live {
		scope[sid] = true
	}
	a.mu.Lock()
	for sid, rec := range a.sessions {
		if rec != nil && len(rec.pending) > 0 {
			scope[sid] = true
		}
	}
	a.mu.Unlock()

	for sid := range scope {
		lock := a.sessionLock(sid)
		if !lock.TryLock() {
			continue // a live admission owns the session; next tick converges
		}
		r, ap := a.diffSessionLease(sid, live[sid])
		resolved += r
		appeared += ap
		lock.Unlock()
	}
	return resolved, appeared, nil
}

// diffSessionLease applies the lease diff for one session; the session
// single-flight must be held.
func (a *Authority) diffSessionLease(sid string, liveIn []*abiv1.InputRequest) (resolved, appeared int) {
	live := map[string]*abiv1.InputRequest{}
	for _, in := range liveIn {
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
		// window, lost events): materialize through the event fold. The
		// nil/empty-ID filter matches the live map — a malformed seed
		// entry consumes no seq and emits no garbage frame.
		for _, in := range liveIn {
			if in == nil || in.GetId() == "" {
				continue
			}
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
	for _, in := range liveIn {
		if in == nil || in.GetId() == "" {
			continue
		}
		if _, projected := rec.pending[in.GetId()]; !projected {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, Input: in})
			appeared++
		}
	}
	return resolved, appeared
}

// rederiveStatusLocked converges BUSY/IDLE from harness status, gated by
// the lease window: a busy-mark inside the window holds (its status event
// may simply be late); past it, harness truth wins. In the ledger-wired
// topology 1b's evidence sweep clears busy first (seq-gated); this is the
// ledger-less governing rule and the sweep's backstop. a.mu must be held.
func (a *Authority) rederiveStatusLocked(sid string, rec *sessionRecord, truth abiv1.SessionStatus) {
	switch truth {
	case abiv1.SessionStatus_SESSION_STATUS_BUSY:
		if !rec.busy {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
		}
	case abiv1.SessionStatus_SESSION_STATUS_IDLE, abiv1.SessionStatus_SESSION_STATUS_ERROR:
		if rec.busy && time.Since(rec.busySince) > a.leaseBound() {
			a.applyLocked(&abiv1.Event{SessionId: sid, Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: truth})
		}
	}
}

// rederiveStatuses applies status re-derivation for every session in the
// gathered seeds (the cadence pass's status half).
func (a *Authority) rederiveStatuses(seeds map[string]SessionSeed) {
	for sid, seed := range seeds {
		lock := a.sessionLock(sid)
		if !lock.TryLock() {
			continue
		}
		a.mu.Lock()
		if rec := a.sessions[sid]; rec != nil {
			a.rederiveStatusLocked(sid, rec, seed.Status)
		}
		a.mu.Unlock()
		lock.Unlock()
	}
}

// serveGather coalesces serve-path pending gathers: one in-flight gather
// at a time, results reused within gatherTTL. A refresh storm costs one
// gather per TTL window (leg 9).
type serveGather struct {
	mu         sync.Mutex
	inFlight   bool
	pending    map[string][]*abiv1.InputRequest
	gatheredAt time.Time
}

// refreshSessionLeaseOnServe refreshes one session's pending lease before
// a snapshot is built (browser refresh is exactly when humans notice
// staleness). The gather is deadline-bounded (a hung store must not wedge
// the serve — the same discipline as the reconcile pass) and coalesced
// across concurrent serves; failure serves the projection degraded.
func (a *Authority) refreshSessionLeaseOnServe(ctx context.Context, sid string) {
	if a.cfg.Store == nil {
		return
	}
	a.serveGathersMu.Lock()
	g, ok := a.serveGathers[sid]
	if !ok {
		g = &serveGather{}
		a.serveGathers[sid] = g
	}
	a.serveGathersMu.Unlock()

	g.mu.Lock()
	if g.inFlight {
		g.mu.Unlock()
		return // a concurrent serve is gathering; this serve reads the projection as-is
	}
	if time.Since(g.gatheredAt) < gatherTTL && g.pending != nil {
		live := g.pending
		g.mu.Unlock()
		a.applyServeDiff(sid, live)
		return
	}
	g.inFlight = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight = false
		g.mu.Unlock()
	}()

	gctx, cancel := context.WithTimeout(ctx, serveGatherTimeout)
	defer cancel()
	live, err := a.cfg.Store.PendingInputs(gctx)
	if err != nil {
		a.logger.Warn("sessionstate: lease gather on serve failed — serving projection degraded",
			zap.String("session", sid), zap.Error(err))
		return
	}
	g.mu.Lock()
	g.pending = live
	g.gatheredAt = time.Now()
	g.mu.Unlock()
	a.applyServeDiff(sid, live)
}

func (a *Authority) applyServeDiff(sid string, live map[string][]*abiv1.InputRequest) {
	lock := a.sessionLock(sid)
	if !lock.TryLock() {
		return // an admission owns the session; the snapshot serves current projection
	}
	defer lock.Unlock()
	a.diffSessionLease(sid, live[sid])
}

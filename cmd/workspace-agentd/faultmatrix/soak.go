// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package faultmatrix

import (
	"context"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// soak.go — the #1312 soak shape in-process: N sessions × fault rate λ
// for a duration, sampled continuously — every breach lands in the
// Violations counters and every convergence span in the ConvergenceLog.
// The CI self-test runs minutes-scale with λ high; the kind row runs
// the same driver hours-scale. Zero violations + convergence within
// bounds is the soak gate.

// SoakConfig shapes one soak run.
type SoakConfig struct {
	// Sessions is the session count (N).
	Sessions int
	// FaultsPerTick is the fault rate λ: the probability per tick that
	// a fault lands somewhere in the fleet.
	FaultsPerTick float64
	// Tick is the fault-application + reconcile cadence.
	Tick time.Duration
	// Duration bounds the run.
	Duration time.Duration
	// Rand seeds the fault stream (deterministic soaks for CI).
	Rand *rand.Rand
}

// soakConvergenceBound mirrors the lease window (L3-class); the
// per-fault convergence wait uses it.
const soakConvergenceBound = sessionstate.LeaseConvergenceBound

const soakPoll = 5 * time.Millisecond

func soakSessionID(i int) string { return "ses-soak-" + strconv.Itoa(i) }
func soakInputID(i int) string   { return "in-soak-" + strconv.Itoa(i) }

// applyFault lands one randomly chosen fault on a random session and
// returns the affected session.
func applyFault(store *EvidenceStore, rng *rand.Rand, sessions []string, i int) string {
	sid := sessions[rng.Intn(len(sessions))]
	switch rng.Intn(3) {
	case 0: // silent ask drop (leg 1): truth forgets any ask, no event
		store.SetState(sid, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	case 1: // lost ask event (leg 3): a truth-side ask the projection missed
		store.SetState(sid, abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
			Id: soakInputID(i), Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "soak?",
		})
	default: // silent turn end (leg 5): truth idle, status event lost
		store.SetState(sid, abiv1.SessionStatus_SESSION_STATUS_IDLE)
	}
	return sid
}

func (s *EvidenceStore) livePendingIDs(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool, len(s.states[sessionID].PendingInputs))
	ids := make([]string, 0, len(s.states[sessionID].PendingInputs))
	for _, in := range s.states[sessionID].PendingInputs {
		if in != nil && in.GetId() != "" { // the lease diff filters nil/empty AND map-dedups; mirror both
			if !seen[in.GetId()] {
				seen[in.GetId()] = true
				ids = append(ids, in.GetId())
			}
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00") // NUL-separated: injective up to NUL-free ids
}

func projectedPendingIDs(ctx context.Context, a *sessionstate.Authority, sessionID string) string {
	res, err := a.GetSnapshot(ctx, connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: sessionID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return "" // an unprojected session holds no pending set (S5 vacuous)
		}
		return "\x00error:" + err.Error()
	}
	seen := make(map[string]bool, len(res.Msg.GetPendingInputs()))
	ids := make([]string, 0, len(res.Msg.GetPendingInputs()))
	for _, in := range res.Msg.GetPendingInputs() {
		if in != nil && in.GetId() != "" { // the lease diff filters nil/empty AND map-dedups; mirror both
			if !seen[in.GetId()] {
				seen[in.GetId()] = true
				ids = append(ids, in.GetId())
			}
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00") // NUL-separated: injective up to NUL-free ids
}

// ErrSoakConfig reports a misshapen SoakConfig (the exported seam the
// kind row drives must fail loudly, not panic or silently no-op).
type ErrSoakConfig string

func (e ErrSoakConfig) Error() string { return "soak config: " + string(e) }

func validateSoakConfig(cfg SoakConfig) error {
	switch {
	case cfg.Sessions < 1:
		return ErrSoakConfig("Sessions must be >= 1")
	case !(cfg.FaultsPerTick >= 0 && cfg.FaultsPerTick <= 1):
		// The negated conjunction is load-bearing: NaN fails BOTH raw
		// comparisons and would validate — then `rng.Float64() < NaN`
		// never fires, a vacuously green soak (r2 F1).
		return ErrSoakConfig("FaultsPerTick is a probability in [0,1]")
	case cfg.Tick <= 0:
		return ErrSoakConfig("Tick must be > 0")
	case cfg.Duration <= 0:
		return ErrSoakConfig("Duration must be > 0")
	}
	return nil
}

// RunSoak drives the authority under the fault stream for cfg.Duration.
// After each fault, the affected session's projection must converge to
// the truth's pending-IDENTITY (sorted ID sets, not counts) inside the
// lease window (ticking Reconcile, the cadence contract); every breach
// is a violation, every span a sample — except ctx teardown, which
// aborts WITHOUT phantom violations (the hours-scale kind run is
// wall-clock canceled; a teardown must not poison the gate). The
// returned counters and log are the soak's gate.
func RunSoak(ctx context.Context, a *sessionstate.Authority, store *EvidenceStore, cfg SoakConfig) (*Violations, *ConvergenceLog, error) {
	if err := validateSoakConfig(cfg); err != nil {
		return nil, nil, err
	}
	v := NewViolations()
	log := NewConvergenceLog()
	rng := cfg.Rand
	if rng == nil {
		//nolint:gosec // a DETERMINISTIC fault stream is the requirement (CI reproducibility); this is not cryptographic
		rng = rand.New(rand.NewSource(1))
	}
	// OWNERSHIP: RunSoak takes the store — it resets every ses-soak-*
	// session to IDLE at start (its fault stream owns these sessions'
	// truth from here on; pre-seeded pending for soak sessions is
	// clobbered by design).
	sessions := make([]string, cfg.Sessions)
	for i := range sessions {
		sessions[i] = soakSessionID(i)
		store.SetState(sessions[i], abiv1.SessionStatus_SESSION_STATUS_IDLE)
	}

	deadline := time.Now().Add(cfg.Duration)
	tick := time.NewTicker(cfg.Tick)
	defer tick.Stop()
	for i := 0; time.Now().Before(deadline); i++ {
		if rng.Float64() < cfg.FaultsPerTick {
			sid := applyFault(store, rng, sessions, i)
			// Convergence is against CURRENT truth, by pending IDENTITY
			// (sorted ID sets — counts would read converged while the
			// projection holds a stale ask and misses a live one, the
			// exact S5 false-green the soak exists to catch).
			elapsed, ok := WaitConverges(ctx, soakConvergenceBound, soakPoll, func() bool {
				a.Reconcile(ctx)
				return PendingShapesMatch(ctx, a, store, sid)
			})
			log.Record("L3", elapsed)
			if !ok && ctx.Err() == nil {
				v.Add("L3") // a real breach; ctx teardown aborts clean
			}
			// Disclosed teardown blind window (r2): a wait in flight at
			// cancel is suppressed AND its aborted span still lands in
			// the histogram — a wall-clock-canceled hours run carries
			// ~one bounded unverifiable window and teardown noise in
			// the samples. Direction-safe (no phantom positives).
		}
		select {
		case <-ctx.Done():
			return v, log, nil
		case <-tick.C:
		}
	}
	return v, log, nil
}

// PendingShapesMatch is the soak gate's core predicate: the projection's
// pending ID set equals truth's. Delegates to idsMatch — the pure,
// wall-clock-free comparison the regression pin drives directly (a pin
// through GetSnapshot carries the serve-gather TTL as a hidden bound).
func PendingShapesMatch(ctx context.Context, a *sessionstate.Authority, store *EvidenceStore, sessionID string) bool {
	return idsMatch(projectedPendingIDs(ctx, a, sessionID), store.livePendingIDs(sessionID))
}

// idsMatch compares two already-joined ID-set encodings. The join uses
// a NUL separator, making it injective up to NUL-free IDs (harness ids
// are; a NUL inside one would still be safer than any comma join).
func idsMatch(projected, live string) bool {
	return projected == live
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package faultmatrix

import (
	"context"
	"math/rand"
	"strconv"
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

func (s *EvidenceStore) livePendingCount(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.states[sessionID].PendingInputs)
}

func projectedPendingCount(ctx context.Context, a *sessionstate.Authority, sessionID string) (int, error) {
	res, err := a.GetSnapshot(ctx, connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: sessionID}))
	if err != nil {
		// An unprojected session holds no pending set — S5 holds
		// vacuously; treat it as zero rather than a perpetual error
		// (the diff will surface the session when truth gives it asks).
		if connect.CodeOf(err) == connect.CodeNotFound {
			return 0, nil
		}
		return 0, err
	}
	return len(res.Msg.GetPendingInputs()), nil
}

// RunSoak drives the authority under the fault stream for cfg.Duration.
// After each fault, the affected session's projection must converge to
// the truth's pending-shape inside the lease window (ticking Reconcile,
// the cadence contract); every breach is a violation, every span a
// sample. The returned counters and log are the soak's gate.
func RunSoak(ctx context.Context, a *sessionstate.Authority, store *EvidenceStore, cfg SoakConfig) (*Violations, *ConvergenceLog) {
	v := NewViolations()
	log := NewConvergenceLog()
	rng := cfg.Rand
	if rng == nil {
		//nolint:gosec // a DETERMINISTIC fault stream is the requirement (CI reproducibility); this is not cryptographic
		rng = rand.New(rand.NewSource(1))
	}
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
			// Convergence is against CURRENT truth — the fault stream
			// keeps moving it (a mid-wait fault on the same session
			// changes the target; chasing a stale snapshot of `want`
			// would breach spuriously).
			elapsed, ok := WaitConverges(ctx, soakConvergenceBound, soakPoll, func() bool {
				a.Reconcile(ctx)
				got, err := projectedPendingCount(ctx, a, sid)
				return err == nil && got == store.livePendingCount(sid)
			})
			log.Record("L3", elapsed)
			if !ok {
				v.Add("L3")
			}
		}
		select {
		case <-ctx.Done():
			return v, log
		case <-tick.C:
		}
	}
	return v, log
}

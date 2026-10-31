// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// stamp.go — the assignee stamp's hardening (RC-B, the 2026-10-30
// incident's S1 remediation).
//
// The assignee stamp (tasks.assignee_session, written when a spawned
// worker's session id becomes known — engine.go spawnSync) gates EVERY
// downstream consumer: the lease ladder's observations key on it
// (rc1Reclaims builds refs only for rows that carry it), the nudger
// delivers through it, the badge and the Workers folder classify on it.
// A single-shot write that dies silently makes a fully-working worker
// observationally invisible — the incident's three anonymous orphans ran
// 49m/73m/75m first turns against an observation plane that had nothing
// to key on.
//
// Three belts land here:
//
//   - RETRY WITH BACKOFF on the stamp write: a transient DB failure
//     retries a bounded number of times with doubling backoff instead of
//     dropping the identity on the floor.
//   - BACK-FILL: the stamp is eventually-true, never write-once — the
//     next claim-token-authenticated evidence of the claim (a worker
//     turn's board_heartbeat carrying the live token + the calling
//     session's identity) re-stamps a row whose stamp was lost. The
//     claim token IS the evidence: only the session the charter was
//     dispatched to holds it.
//   - OBSERVABILITY: every landed stamp (spawn or back-fill) emits
//     stamp_lag — the span from the claim mint (stage_entered_at, set
//     by every claim-path write) to the stamp landing — as a histogram,
//     and every FAILED write attempt increments a failure counter and
//     logs at ERROR. A lagging or lost stamp is a metric and a log
//     line, never silence.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Stamp metric names (the llmsafespaces_ family convention, pkg/obs).
const (
	StampLagMetricName = "llmsafespaces_board_stamp_lag_seconds"
	// StampLagHelp documents the claim→stamp span semantics.
	StampLagHelp = "Seconds from the claim mint (stage_entered_at) to the assignee stamp landing (RC-B S1: a lagging stamp is observable, never silent). The `origin` label names the landing path: spawn (the post-spawn stamp write) or backfill (re-stamped from subsequent claim evidence)."
	// StampWriteFailuresMetricName counts failed stamp write attempts.
	StampWriteFailuresMetricName = "llmsafespaces_board_stamp_write_failures_total"
	// StampWriteFailuresHelp documents the failure counter.
	StampWriteFailuresHelp = "Failed assignee-stamp write attempts (RC-B S1: every failed attempt is counted here and logged at ERROR — a lost stamp is loud, never swallowed)."
)

// stampLagBuckets spans the honest range: sub-second happy-path stamps
// (post-#1986 verified live at turn-end+19ms) through the incident's
// 49–75 minute orphans.
var stampLagBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// stampLag and stampWriteFailures are the family's single registration
// (one per binary — pkg/board is linked once; the pkg/obs
// two-collectors-under-one-name hazard does not arise for a family
// registered in exactly one package).
var (
	stampLag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    StampLagMetricName,
		Help:    StampLagHelp,
		Buckets: stampLagBuckets,
	}, []string{"origin"})

	stampWriteFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: StampWriteFailuresMetricName,
		Help: StampWriteFailuresHelp,
	}, []string{"origin"})
)

// Stamp origin labels: the two landing paths.
const (
	stampOriginSpawn    = "spawn"
	stampOriginBackfill = "backfill"
)

// StampEvent is the stamp's event projection (design 0072 S6): every
// LANDED stamp — the row's assignee identity appearing on the
// observation plane — is an event, emitted post-write alongside the
// stamp_lag observation. The stamp writes no board_events row (it is
// a guarded row write, not a caller action), so the bus entry is its
// only event surface.
type StampEvent struct {
	TaskID      string
	TaskShortID string
	ProjectID   string
	Assignee    string
	Origin      string // stampOriginSpawn | stampOriginBackfill
	LagSeconds  float64
}

// StampEventSink receives landed stamp events. Post-write, fail-open:
// a sink failure is logged at the sink and never changes board state.
// Nil = inert (the default — nothing is projected).
type StampEventSink func(ctx context.Context, ev StampEvent)

// emitStamp hands one landed stamp to the projection. The contract is
// the EventPublisher's: the sink must be fast and never block the
// write path — and a sink that panics anyway is CONTAINED here (a
// broken projection must never take the stamp or its caller down;
// loud, then on).
func (s *Service) emitStamp(ctx context.Context, ev StampEvent) {
	if s.stampEvents == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "board: stamp event sink panicked — contained (the stamp itself landed)",
				"task_id", ev.TaskID, "origin", ev.Origin, "panic", r)
		}
	}()
	s.stampEvents(ctx, ev)
}

// StampLag exposes the lag histogram for tests and admin surfaces
// (production code writes; the obs.LoopLastRun precedent).
func StampLag() *prometheus.HistogramVec { return stampLag }

// StampWriteFailures exposes the failure counter for tests.
func StampWriteFailures() *prometheus.CounterVec { return stampWriteFailures }

// Stamp write retry policy: bounded attempts, doubling backoff. The
// write rides the spawn's goroutine (async mode) or the claim path
// (sync mode) — worst-case added latency on total failure is the
// backoff sum (~700ms), paid only when the database is failing.
const (
	stampWriteAttempts = 4
	stampBackoffBase   = 100 * time.Millisecond
	stampBackoffCap    = 400 * time.Millisecond
)

// fpStampWrite is the fault seam for the stamp write (kill-test
// injections: an armed failpoint fails the attempt like a DB error —
// the retry loop must land the stamp once it clears).
const fpStampWrite = "stamp.write"

// stampBackoff returns the pre-attempt-n sleep (base<<(n-1), capped).
func stampBackoff(attempt int) time.Duration {
	d := stampBackoffBase << (attempt - 1)
	if d > stampBackoffCap || d <= 0 {
		return stampBackoffCap
	}
	return d
}

// writeAssigneeStamp lands the guarded assignee stamp with bounded
// retry: assignee_session=sessionID + the lease re-arm, guarded by the
// live claim token and stage (the merged design's spawn-stamp guard).
//
//	landed=true  — the stamp is on the row (claim.Assignee follows).
//	landed=false — the guard lost the race (deleted, lapsed, or
//	               re-claimed while the spawn was in flight): the
//	               just-spawned session holds a dead charter, and the
//	               CALLER aborts it (spawnSync's existing semantics).
//
// A failed attempt is NEVER swallowed: each one increments the failure
// counter and logs at ERROR; exhaustion logs loud and returns
// landed=false (the back-fill belt owns recovery — the stamp becomes
// true when the claim's next evidence arrives).
func (s *Service) writeAssigneeStamp(ctx context.Context, claim *Claim, sessionID, stage string) bool {
	taskID, claimToken := claim.TaskID, claim.ClaimToken
	var lastErr error
	for attempt := 1; attempt <= stampWriteAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				// The retry window died with the context: loud, then
				// out (the back-fill belt owns the recovery).
				slog.ErrorContext(ctx, "board: assignee stamp write abandoned — context canceled mid-retry (back-fill owns recovery)",
					"task_id", taskID, "session_id", sessionID, "attempts", attempt-1, "err", ctx.Err().Error())
				stampWriteFailures.WithLabelValues(stampOriginSpawn).Inc()
				return false
			case <-time.After(stampBackoff(attempt - 1)):
			}
		}
		// The fault seam (kill-tests): an armed failpoint fails the
		// attempt exactly like a DB error.
		if err := s.failAt(ctx, fpStampWrite); err != nil {
			lastErr = err
		} else {
			var lagSeconds float64
			// stamp_lag is measured SERVER-side (now() - stage_entered_at,
			// one clock domain): stage_entered_at is written by every
			// claim-path mint (grant, manual start, re-arm, persona
			// claim, bounce), so it is the claim-time anchor.
			err = s.q().QueryRow(ctx, `
				UPDATE tasks SET assignee_session=$2,
					lease_expires_at=now()+make_interval(secs=>$5::float8)
				WHERE id=$1 AND claim_token=$3 AND stage=$4
				RETURNING extract(epoch FROM (now() - stage_entered_at))`,
				taskID, sessionID, claimToken, stage, s.cfg.LeaseDuration.Seconds()).Scan(&lagSeconds)
			if err == nil {
				stampLag.WithLabelValues(stampOriginSpawn).Observe(lagSeconds)
				s.emitStamp(ctx, StampEvent{
					TaskID: taskID, TaskShortID: claim.ShortID, ProjectID: claim.ProjectID,
					Assignee: sessionID, Origin: stampOriginSpawn, LagSeconds: lagSeconds,
				})
				return true
			}
			if errors.Is(err, pgx.ErrNoRows) {
				// The guarded UPDATE matched nothing: the claim moved —
				// retrying cannot help and stamping would be wrong.
				return false
			}
			lastErr = err
		}
		stampWriteFailures.WithLabelValues(stampOriginSpawn).Inc()
		slog.ErrorContext(ctx, "board: assignee stamp write FAILED — retrying",
			"task_id", taskID, "session_id", sessionID, "attempt", attempt,
			"of", stampWriteAttempts, "err", lastErr.Error())
	}
	slog.ErrorContext(ctx, "board: assignee stamp write EXHAUSTED retries — the stamp is lost for now (the back-fill belt re-stamps on the claim's next evidence; observation is blind until then)",
		"task_id", taskID, "session_id", sessionID, "attempts", stampWriteAttempts, "err", lastErr.Error())
	return false
}

// backfillAssigneeStamp re-stamps a claim whose stamp was lost, from
// subsequent evidence of the claim: a caller that (a) carries the row's
// LIVE claim token — only the session the charter was dispatched to
// holds it — and (b) presents a session identity. The guard re-checks
// the token at write time (the evidence must still be live when the
// stamp lands) and only ever FILLS a missing stamp — an existing stamp
// is never overwritten (the spawned identity wins; the write-once
// hazard is fixed by filling the gap, not by churning identities).
//
// Best-effort by design: a failure logs at ERROR and counts, but never
// fails the caller's own write — the evidence channel (the heartbeat)
// must not become a hostage to the stamp. The next evidence retries.
func (s *Service) backfillAssigneeStamp(ctx context.Context, t *Task, actor Actor) {
	if t == nil || t.AssigneeSession != "" || actor.SessionID == "" || actor.ClaimToken == "" || t.ClaimToken == "" || actor.ClaimToken != t.ClaimToken {
		return
	}
	if t.Stage != StageInProgress && t.Stage != StageInReview {
		return
	}
	var lagSeconds float64
	err := s.q().QueryRow(ctx, `
		UPDATE tasks SET assignee_session=$2, updated_at=now()
		WHERE id=$1 AND claim_token=$3 AND assignee_session IS NULL
		  AND stage IN ('in_progress','in_review')
		RETURNING extract(epoch FROM (now() - stage_entered_at))`,
		t.ID, actor.SessionID, actor.ClaimToken).Scan(&lagSeconds)
	switch {
	case err == nil:
		// The identity-only back-fill: the lease is untouched (the
		// heartbeat's own arm decides renewal); only the observation
		// plane's key was missing.
		stampLag.WithLabelValues(stampOriginBackfill).Observe(lagSeconds)
		s.emitStamp(ctx, StampEvent{
			TaskID: t.ID, TaskShortID: t.ShortID, ProjectID: t.ProjectID,
			Assignee: actor.SessionID, Origin: stampOriginBackfill, LagSeconds: lagSeconds,
		})
		slog.InfoContext(ctx, "board: assignee stamp back-filled from claim evidence — the observation plane sees the claim again",
			"task_id", t.ID, "session_id", actor.SessionID, "lag_seconds", lagSeconds)
	case errors.Is(err, pgx.ErrNoRows):
		// The claim moved, or the stamp landed concurrently (spawn
		// retry, another evidence channel): converged elsewhere — not
		// a failure.
		return
	default:
		stampWriteFailures.WithLabelValues(stampOriginBackfill).Inc()
		slog.ErrorContext(ctx, "board: assignee stamp BACK-FILL write failed — the claim stays observationally invisible until the next evidence",
			"task_id", t.ID, "session_id", actor.SessionID, "err", err.Error())
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// sessionIndexEvents counts the write path's outcomes (#754 fold-in via
// #1340): queue drops were previously Warn-log-only — not queryable.
var sessionIndexEvents = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "session_index_events_total",
	Help: "Session-index write outcomes by kind (queued, applied, db_error, dropped_queue_full).",
}, []string{"outcome"})

// reconcileOutcomes counts the convergence pass's dispositions (#1340):
// reaped (guard-admitted deletion), kept_for_operator (history-bearing
// absent — the triage guard held it), delete_failed.
var reconcileOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "session_index_reconcile_outcomes_total",
	Help: "Session-index reconciliation dispositions (reaped, kept_for_operator, delete_failed).",
}, []string{"outcome"})

// ReconcileOutcome records one convergence disposition (handler-side).
func ReconcileOutcome(outcome string) {
	reconcileOutcomes.WithLabelValues(outcome).Inc()
}

// ReconcileOutcomeForTest reads a disposition counter (test assertions).
func ReconcileOutcomeForTest(outcome string) prometheus.Counter {
	return reconcileOutcomes.WithLabelValues(outcome)
}

// Service manages the session_index table with non-blocking writes.
type Service struct {
	db     interfaces.DatabaseService
	logger *logger.Logger
	queue  chan recordEvent
	closeC chan struct{}
	wg     sync.WaitGroup
}

type recordEvent struct {
	workspaceID string
	sessionID   string
	title       string
	at          time.Time
}

// New creates a new SessionIndexService.
func New(db interfaces.DatabaseService, log *logger.Logger) *Service {
	return &Service{
		db:     db,
		logger: log,
		queue:  make(chan recordEvent, 1024),
		closeC: make(chan struct{}),
	}
}

// Start begins the background drainer goroutine.
func (s *Service) Start() error {
	s.wg.Add(1)
	go s.drain()
	if s.logger != nil {
		s.logger.Info("SessionIndexService started")
	}
	return nil
}

// Stop signals the drainer to stop and waits for it to finish.
func (s *Service) Stop() error {
	close(s.closeC)
	s.wg.Wait()
	if s.logger != nil {
		s.logger.Info("SessionIndexService stopped")
	}
	return nil
}

// RecordMessage is non-blocking: pushes to a bounded channel.
// If the channel is full, the oldest event is dropped.
func (s *Service) RecordMessage(workspaceID, sessionID, title string, at time.Time) {
	sessionIndexEvents.WithLabelValues("queued").Inc()
	select {
	case s.queue <- recordEvent{workspaceID: workspaceID, sessionID: sessionID, title: title, at: at}:
	default:
		if s.logger != nil {
			s.logger.Warn("session_index: channel full, dropping oldest event",
				"workspaceID", workspaceID, "sessionID", sessionID,
				"queueSize", len(s.queue))
		}
		select {
		case <-s.queue:
		default:
		}
		sessionIndexEvents.WithLabelValues("dropped_queue_full").Inc()
		s.queue <- recordEvent{workspaceID: workspaceID, sessionID: sessionID, title: title, at: at}
	}
}

// ListByWorkspace returns session metadata ordered by last_message_at DESC.
func (s *Service) ListByWorkspace(ctx context.Context, workspaceID string) ([]types.SessionListItem, error) {
	return s.db.ListSessionIndex(ctx, workspaceID)
}

// DeleteByWorkspace removes all session index entries for a workspace.
func (s *Service) DeleteByWorkspace(ctx context.Context, workspaceID string) error {
	return s.db.DeleteSessionIndex(ctx, workspaceID)
}

// DeleteSession removes a single session and its descendants from the session index.
func (s *Service) DeleteSession(ctx context.Context, workspaceID, sessionID string) error {
	return s.db.DeleteSessionTree(ctx, workspaceID, sessionID)
}

// UpsertTitle updates just the title for a session.
func (s *Service) UpsertTitle(ctx context.Context, workspaceID, sessionID, title string) error {
	return s.db.UpsertSessionTitle(ctx, workspaceID, sessionID, title)
}

// UpsertParent records the parent session of a (sub)session. Used by the
// proxy to mirror opencode's session.parentID into the sidebar's
// session_index so the sidebar can render hierarchy without round-tripping
// the agent. Empty parentID is allowed (top-level session) but uncommon —
// callers typically only invoke this when they have observed a non-empty
// parentID on a session.
func (s *Service) UpsertParent(ctx context.Context, workspaceID, sessionID, parentID string) error {
	return s.db.UpsertSessionParent(ctx, workspaceID, sessionID, parentID)
}

// UpsertContextUsed persists the prompt token count from the most recent LLM
// step for this session. Called synchronously from the proxy's onRawEvent
// handler on every adapter-declared usage event. Low frequency (at most
// once per LLM call), so a direct synchronous write is appropriate.
func (s *Service) UpsertContextUsed(ctx context.Context, workspaceID, sessionID string, contextUsed int64) error {
	return s.db.UpsertSessionContextUsed(ctx, workspaceID, sessionID, contextUsed)
}

func (s *Service) UpdateLastSeen(ctx context.Context, workspaceID, sessionID string) error {
	return s.db.UpdateSessionLastSeen(ctx, workspaceID, sessionID)
}

// PlanReconciliation is the #1340 convergence decision (S5b): given the
// index's session IDs, a harness-list provider, and the previous
// consecutive-miss counters, it returns which rows to DELETE now and the
// next counters. Pure — orchestration (adapter call, counter storage,
// deletion) lives with the proxy handler; this function IS the
// invariant:
//
//   - harness present → keep, counter cleared
//   - harness absent  → counter+1; removal at threshold consecutive
//     misses (N=2 survives one mid-restart snapshot miss)
//   - harness call error → keep everything, counters frozen (an
//     unreachable pod is NOT evidence of deletion)
//   - counters for rows no longer indexed are dropped (bounded state)
//
// The lister returning an error models the entire check failing: no
// session is treated as absent on a failed check.
//
// ReapGuard is the #1340 triage safety rule: auto-deletion only removes
// rows the guard admits (message_count == 0 — the never-persisted /
// probe-shell class). Rows the harness reports gone but that carry
// message history are returned as keptForOperator instead: a vanished
// session WITH history may indicate a harness store reset, so the row
// is surfaced (the orchestration layer records the
// session_index_reconcile_outcomes_total metric + a Warn log) rather
// than silently destroyed. threshold <= 1 removes on the first absence;
// callers pass 2. keptForOperator rows carry no miss counter here —
// the orchestration layer pins theirs at the threshold so the signal
// repeats consistently.
func PlanReconciliation(indexRows []types.SessionListItem, harnessList func() (map[string]bool, error), prevMiss map[string]int, threshold int, reap ReapGuard) (remove, keptForOperator []string, nextMiss map[string]int) {
	nextMiss = map[string]int{}
	present, err := harnessList()
	if err != nil {
		for _, r := range indexRows {
			if n, ok := prevMiss[r.ID]; ok {
				nextMiss[r.ID] = n
			}
		}
		return nil, nil, nextMiss
	}
	for _, row := range indexRows {
		if present[row.ID] {
			continue
		}
		n := prevMiss[row.ID] + 1
		if n < threshold {
			nextMiss[row.ID] = n
			continue
		}
		if reap(row) {
			remove = append(remove, row.ID)
			continue
		}
		keptForOperator = append(keptForOperator, row.ID)
	}
	return remove, keptForOperator, nextMiss
}

// ReapGuard admits an index row for auto-deletion. The default guard
// (#1340 triage): only rows with no message history (message_count == 0).
type ReapGuard func(types.SessionListItem) bool

// DefaultReapGuard is the message_count == 0 safety guard.
func DefaultReapGuard(row types.SessionListItem) bool {
	return row.MessageCount == 0
}

func (s *Service) drain() {
	defer s.wg.Done()
	for {
		select {
		case ev := <-s.queue:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.db.UpsertSessionMessage(ctx, ev.workspaceID, ev.sessionID, ev.at); err != nil {
				sessionIndexEvents.WithLabelValues("db_error").Inc()
				if s.logger != nil {
					s.logger.Error("session index upsert failed", err)
				}
			} else {
				sessionIndexEvents.WithLabelValues("applied").Inc()
			}
			cancel()
		case <-s.closeC:
			// Drain remaining
			for len(s.queue) > 0 {
				ev := <-s.queue
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := s.db.UpsertSessionMessage(ctx, ev.workspaceID, ev.sessionID, ev.at); err != nil {
					if s.logger != nil {
						s.logger.Warn("Failed to flush session message on shutdown", "error", err, "workspaceID", ev.workspaceID, "sessionID", ev.sessionID)
					}
				}
				cancel()
			}
			return
		}
	}
}

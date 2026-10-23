// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"container/list"
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
// reaped (guard-admitted deletion), reap_vetoed_live (#1850 via #1865:
// the by-ID probe found the session alive — the list gap alone never
// reaps), reap_deferred_probe (#1850 via #1865: the probe was
// indeterminate — deferred, retried next pass), kept_for_operator
// (history-bearing absent — the triage guard held it), kept_spawn_window
// (#1850 via #1881: fresh count-zero rows sheltered by the reap grace —
// not history-bearing, a distinct population from kept_for_operator,
// split so the metric never conflates them), delete_failed,
// reaped_carrier (#1882: the reaped row was session_origins-marked — a
// platform routine carrier admitted past the history exception and the
// spawn-window grace; a carrier's absence is platform policy, not
// harness-list lag).
var reconcileOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "session_index_reconcile_outcomes_total",
	Help: "Session-index reconciliation dispositions (reaped, reap_vetoed_live, reap_deferred_probe, kept_for_operator, kept_spawn_window, delete_failed, reaped_carrier, count_rebuilt, count_unchanged, count_walk_error).",
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

	// registerLRU is the known-registered (workspace, session) cache
	// (design 0066 §4.2): the hot per-message row-creating paths call
	// Register before every upsert; after first sight the pair is
	// LRU-resident and the seam costs one map lookup. Bounded; a miss
	// is never a correctness event (registration is idempotent).
	registerMu  sync.Mutex
	registerLRU *list.List // front = most recent
	registerMap map[string]*list.Element
	registerCap int
}

// registeredPair is one LRU entry.
type registeredPair struct {
	key     string
	shortID string
}

// registerLRUCap bounds the known-registered set. Far above any sane
// workspace's live-session cardinality; small enough that the memory
// is noise.
const registerLRUCap = 8192

func (s *Service) knownRegistered(workspaceID, sessionID string) (string, bool) {
	key := workspaceID + "\x00" + sessionID
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if el, ok := s.registerMap[key]; ok {
		s.registerLRU.MoveToFront(el)
		return el.Value.(*registeredPair).shortID, true
	}
	return "", false
}

func (s *Service) markRegistered(workspaceID, sessionID, shortID string) {
	key := workspaceID + "\x00" + sessionID
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if s.registerMap == nil {
		s.registerMap = map[string]*list.Element{}
		s.registerLRU = list.New()
		s.registerCap = registerLRUCap
	}
	if el, ok := s.registerMap[key]; ok {
		el.Value.(*registeredPair).shortID = shortID
		s.registerLRU.MoveToFront(el)
		return
	}
	el := s.registerLRU.PushFront(&registeredPair{key: key, shortID: shortID})
	s.registerMap[key] = el
	for s.registerLRU.Len() > s.registerCap {
		oldest := s.registerLRU.Back()
		if oldest == nil {
			break
		}
		s.registerLRU.Remove(oldest)
		delete(s.registerMap, oldest.Value.(*registeredPair).key)
	}
}

// ResolveShortID is the #1817 piece-3 READ-ONLY seam: it returns the
// short id a session is registered to WITHOUT minting a new row (unlike
// Register, which EnsureSessionRegisters). It consults only the
// known-registered LRU (no DB write, no DB read). Today's caller is the
// delete tail (translateSessionIDToPhraseNoMint), where a mint would
// RESURRECT a deleted session's row: DeleteSession does not evict the LRU,
// so a minting translate would re-INSERT a ghost row and the `deleted`
// clear would carry a new phrase the frontend does not recognize. Returns
// "" when unknown (the clear then passes the harness id through — never
// worse; the frontend removes the session by identity either way).
func (s *Service) ResolveShortID(workspaceID, sessionID string) string {
	if s.db == nil || workspaceID == "" || sessionID == "" {
		return ""
	}
	if shortID, ok := s.knownRegistered(workspaceID, sessionID); ok {
		return shortID
	}
	return ""
}

// Register is the registration seam's wrapper (design 0066 §4.2):
// EnsureSessionRegistered behind the known-registered LRU, best-effort
// by contract — a failed registration degrades to a short-id-less row
// that the convergence sweep backfills; it must never fail the
// caller's actual write. Returns the short id, "" when degraded.
func (s *Service) Register(ctx context.Context, workspaceID, sessionID, harnessType string) string {
	if s.db == nil || workspaceID == "" || sessionID == "" {
		return ""
	}
	if shortID, ok := s.knownRegistered(workspaceID, sessionID); ok {
		return shortID
	}
	regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	shortID, err := s.db.EnsureSessionRegistered(regCtx, workspaceID, sessionID, harnessType)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("session_index: short-id registration failed; backfill sweep will converge",
				"workspaceID", workspaceID, "sessionID", sessionID, "error", err.Error())
		}
		return ""
	}
	s.markRegistered(workspaceID, sessionID, shortID)
	return shortID
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

// ListByWorkspace returns the workspace's complete session listing
// (#1886): message-recency head, created_at DESC through the #1880
// NULL-key pool, session_id as the total-order tiebreaker — no row
// cap, so live unarchived sessions survive past any row count.
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

// MarkSessionDeleted stamps the #1882 durable death mark (session row
// + descendants) BEFORE the row delete — the read-path filter's
// signal. A row whose delete fails stays hidden instead of rendering
// as a ghost.
func (s *Service) MarkSessionDeleted(ctx context.Context, workspaceID, sessionID string) error {
	return s.db.MarkSessionIndexDeleted(ctx, workspaceID, sessionID)
}

// UpsertTitle updates just the title for a session.
func (s *Service) UpsertTitle(ctx context.Context, workspaceID, sessionID, title string) error {
	return s.db.UpsertSessionTitle(ctx, workspaceID, sessionID, title)
}

// RebuildMessageCount sets a session's message_count absolutely from the
// harness-walked ground truth (#1481) — the reconcile-side repair for
// the incremental path's duplicate-event double-count. The DB-level
// DISTINCT guard keeps converged workspaces write-free.
func (s *Service) RebuildMessageCount(ctx context.Context, workspaceID, sessionID string, count int) error {
	return s.db.UpsertSessionMessageCount(ctx, workspaceID, sessionID, count)
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

// SetArchived flips the platform-level archived marker (#1627). The
// marker is llmsafespaces-only: nothing inside the agent changes.
func (s *Service) SetArchived(ctx context.Context, workspaceID, sessionID string, archived bool) error {
	return s.db.SetSessionArchivedStatus(ctx, workspaceID, sessionID, archived)
}

// IsArchived reports the platform-level archived marker. Absent index
// rows read as not-archived (see the DB layer).
func (s *Service) IsArchived(ctx context.Context, workspaceID, sessionID string) (bool, error) {
	return s.db.IsSessionArchived(ctx, workspaceID, sessionID)
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
		// Keyed by InternalID like every other path below: a minted
		// row's ID is the phrase — carrying miss counters under the
		// phrase would drop them on a transient list error and delay
		// reap convergence (found by the #1756 post-union verify).
		for _, r := range indexRows {
			if n, ok := prevMiss[r.InternalID]; ok {
				nextMiss[r.InternalID] = n
			}
		}
		return nil, nil, nextMiss
	}
	for _, row := range indexRows {
		if present[row.InternalID] {
			continue
		}
		n := prevMiss[row.InternalID] + 1
		if n < threshold {
			nextMiss[row.InternalID] = n
			continue
		}
		if reap(row) {
			remove = append(remove, row.InternalID)
			continue
		}
		keptForOperator = append(keptForOperator, row.InternalID)
	}
	return remove, keptForOperator, nextMiss
}

// ReapGuard admits an index row for auto-deletion. The default guard
// (#1340 triage): only rows with no message history (message_count == 0).
type ReapGuard func(types.SessionListItem) bool

// reapGraceWindow is the #1850 spawn-window protection: a row younger
// than this is never reaped, even at message_count == 0. A freshly
// created create_session worker keeps count 0 through its whole first
// turn (the prompt delivery is a pod-local POST the API never sees),
// so without the grace two harness-list misses inside that window
// reaped the LIVE row and orphaned the phrase minted at creation —
// the re-phrase defect's five reproductions. Aged count-zero rows
// keep the #1340 ghost-hygiene verdict (they converge one grace
// period later; stillborn rows are the accepted, documented cost).
const reapGraceWindow = 15 * time.Minute

// CountServeEligible is the #1880 listing serve-through gate: whether
// this row's message_count is KNOWN-untrustworthy on the platform side
// (the never-counted / never-traversed drift class) and therefore worth
// one bounded harness count walk on the listing read path. SKELETON —
// the #1880 red pins commit stores the seam unimplemented; the
// implementation commit fills the rule (drift signature + spawn-grace
// age gate + archived exclusion).
func CountServeEligible(row types.SessionListItem) bool {
	return false
}

// DefaultReapGuard is the message_count == 0 safety guard, extended
// by the #1850 spawn-window grace: a count-zero row inside
// reapGraceWindow of its creation is NOT admitted (a fresh session is
// not a ghost — harness-list lag must never orphan a minted phrase).
// Rows without creation-time information keep the pre-#1850 verdict.
func DefaultReapGuard(row types.SessionListItem) bool {
	if row.MessageCount == 0 {
		if row.CreatedAt != nil && time.Since(*row.CreatedAt) < reapGraceWindow {
			return false
		}
		return true
	}
	return false
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

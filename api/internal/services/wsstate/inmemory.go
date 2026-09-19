// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package wsstate

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Compile-time assertion that InMemoryStore implements Store.
var _ Store = (*InMemoryStore)(nil)

// deletedSessionsHighWater bounds the deleted-session tombstone set.
// When the set size exceeds this number, a batch of entries is evicted.
// The value matches the prior ProxyHandler behavior exactly so this
// refactor introduces no behavioral drift.
const deletedSessionsHighWater = 500

const deletedSessionsEvictBatch = 250

// InMemoryStore is the process-local implementation of Store. It is the
// ONLY implementation in this story; subsequent Epic 45 stories will add
// a Redis-backed implementation that multi-replica deployments can share.
//
// All fields are private; access is via the Store methods only. The
// internal mutexes are granular per data type so a hot operation on one
// (e.g. active-session check) does not block unrelated operations (e.g.
// password cache reads).
type InMemoryStore struct {
	// activeSess: workspace ID -> set of active session IDs.
	activeSess   map[string]map[string]bool
	activeSessMu sync.RWMutex

	// deletedSessions: "workspaceID/sessionID" -> tombstone. Keyed as a
	// flat string so the underlying map can be scanned by workspace
	// prefix during ClearDeletedSessions without holding a nested map.
	deletedSessions   map[string]struct{}
	deletedSessionsMu sync.RWMutex

	// pwCache: workspace ID -> opencode password. Cache-only; the K8s
	// Secret fetch stays in ProxyHandler.getPassword so the store has no
	// I/O dependencies.
	pwCache   map[string]string
	pwCacheMu sync.RWMutex

	// wsConfig: workspace ID -> spec-derived config cache.
	wsConfig   map[string]Config
	wsConfigMu sync.RWMutex

	// priorPhase: workspace ID -> last-observed phase.
	priorPhase   map[string]string
	priorPhaseMu sync.RWMutex

	// parentBackfilled: workspace ID -> backfill marker.
	parentBackfilled   map[string]struct{}
	parentBackfilledMu sync.RWMutex

	// reconcileMisses: workspace ID -> session ID -> consecutive-absent
	// counter (#1340 S5b convergence). Whole-map replace on write.
	reconcileMisses   map[string]map[string]int
	reconcileMissesMu sync.RWMutex

	// reconcileTurn: workspace ID -> turn-claim expiry (#1340). An
	// expired entry is an unclaimed window.
	reconcileTurn   map[string]time.Time
	reconcileTurnMu sync.Mutex
}

// NewInMemoryStore returns a Store backed by process-local maps. The
// returned store is safe for concurrent use.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		activeSess:       make(map[string]map[string]bool),
		deletedSessions:  make(map[string]struct{}),
		pwCache:          make(map[string]string),
		wsConfig:         make(map[string]Config),
		priorPhase:       make(map[string]string),
		parentBackfilled: make(map[string]struct{}),
	}
}

// --- Active session tracking ---

func (s *InMemoryStore) CheckAndAddActiveSession(ctx context.Context, workspaceID, sessionID string, maxSessions int) bool {
	s.activeSessMu.Lock()
	defer s.activeSessMu.Unlock()

	if s.activeSess[workspaceID] == nil {
		s.activeSess[workspaceID] = make(map[string]bool)
	}

	if s.activeSess[workspaceID][sessionID] {
		return true
	}

	if len(s.activeSess[workspaceID]) >= maxSessions {
		return false
	}

	s.activeSess[workspaceID][sessionID] = true
	return true
}

func (s *InMemoryStore) RemoveActiveSession(ctx context.Context, workspaceID, sessionID string) {
	s.activeSessMu.Lock()
	defer s.activeSessMu.Unlock()
	sessions, ok := s.activeSess[workspaceID]
	if !ok {
		return
	}
	delete(sessions, sessionID)
	if len(sessions) == 0 {
		delete(s.activeSess, workspaceID)
	}
}

func (s *InMemoryStore) IsSessionActive(ctx context.Context, workspaceID, sessionID string) bool {
	s.activeSessMu.RLock()
	defer s.activeSessMu.RUnlock()
	sessions, ok := s.activeSess[workspaceID]
	if !ok {
		return false
	}
	return sessions[sessionID]
}

func (s *InMemoryStore) ActiveSessionCount(ctx context.Context, workspaceID string) int {
	s.activeSessMu.RLock()
	defer s.activeSessMu.RUnlock()
	return len(s.activeSess[workspaceID])
}

func (s *InMemoryStore) GetActiveSessions(ctx context.Context, workspaceID string) []string {
	s.activeSessMu.RLock()
	defer s.activeSessMu.RUnlock()
	sessions := s.activeSess[workspaceID]
	if len(sessions) == 0 {
		return nil
	}
	result := make([]string, 0, len(sessions))
	for sid := range sessions {
		result = append(result, sid)
	}
	return result
}

func (s *InMemoryStore) ClearActiveSessions(ctx context.Context, workspaceID string) {
	s.activeSessMu.Lock()
	defer s.activeSessMu.Unlock()
	delete(s.activeSess, workspaceID)
}

// TouchActiveSessions is a no-op for the InMemoryStore: in-memory entries
// have no TTL (they persist until explicitly removed). The Redis
// implementation refreshes the key TTL; see redis.go TouchActiveSessions.
func (s *InMemoryStore) TouchActiveSessions(ctx context.Context, workspaceID string) {
	// Intentionally empty — no TTL to refresh.
}

// --- Deleted-session tombstones ---

func (s *InMemoryStore) MarkSessionDeleted(ctx context.Context, workspaceID, sessionID string) {
	s.deletedSessionsMu.Lock()
	defer s.deletedSessionsMu.Unlock()
	s.deletedSessions[workspaceID+"/"+sessionID] = struct{}{}
	// Bounded: if the set grows beyond high-water, evict a batch. The
	// eviction is map-iteration-order (Go intentionally randomizes map
	// iteration), which is acceptable for a tombstone cache whose only
	// invariant is "recently deleted sessions are recognized". The bound
	// matches the prior ProxyHandler behavior exactly.
	if len(s.deletedSessions) > deletedSessionsHighWater {
		count := 0
		for k := range s.deletedSessions {
			delete(s.deletedSessions, k)
			count++
			if count >= deletedSessionsEvictBatch {
				break
			}
		}
	}
}

func (s *InMemoryStore) IsSessionDeleted(ctx context.Context, workspaceID, sessionID string) bool {
	s.deletedSessionsMu.RLock()
	defer s.deletedSessionsMu.RUnlock()
	_, ok := s.deletedSessions[workspaceID+"/"+sessionID]
	return ok
}

func (s *InMemoryStore) ClearDeletedSessions(ctx context.Context, workspaceID string) {
	s.deletedSessionsMu.Lock()
	defer s.deletedSessionsMu.Unlock()
	prefix := workspaceID + "/"
	for k := range s.deletedSessions {
		if strings.HasPrefix(k, prefix) {
			delete(s.deletedSessions, k)
		}
	}
}

// --- Workspace password cache ---

func (s *InMemoryStore) GetCachedPassword(ctx context.Context, workspaceID string) (string, bool) {
	s.pwCacheMu.RLock()
	defer s.pwCacheMu.RUnlock()
	pw, ok := s.pwCache[workspaceID]
	return pw, ok
}

func (s *InMemoryStore) SetCachedPassword(ctx context.Context, workspaceID, password string) {
	s.pwCacheMu.Lock()
	defer s.pwCacheMu.Unlock()
	s.pwCache[workspaceID] = password
}

func (s *InMemoryStore) InvalidatePassword(ctx context.Context, workspaceID string) {
	s.pwCacheMu.Lock()
	defer s.pwCacheMu.Unlock()
	delete(s.pwCache, workspaceID)
}

// --- Workspace config cache ---

func (s *InMemoryStore) GetWorkspaceConfig(ctx context.Context, workspaceID string) (Config, bool) {
	s.wsConfigMu.RLock()
	defer s.wsConfigMu.RUnlock()
	cfg, ok := s.wsConfig[workspaceID]
	return cfg, ok
}

func (s *InMemoryStore) SetWorkspaceConfig(ctx context.Context, workspaceID string, cfg Config) {
	s.wsConfigMu.Lock()
	defer s.wsConfigMu.Unlock()
	s.wsConfig[workspaceID] = cfg
}

func (s *InMemoryStore) InvalidateWorkspaceConfig(ctx context.Context, workspaceID string) {
	s.wsConfigMu.Lock()
	defer s.wsConfigMu.Unlock()
	delete(s.wsConfig, workspaceID)
}

// --- Prior phase tracking ---

func (s *InMemoryStore) GetPriorPhase(ctx context.Context, workspaceID string) (string, bool) {
	s.priorPhaseMu.RLock()
	defer s.priorPhaseMu.RUnlock()
	phase, ok := s.priorPhase[workspaceID]
	return phase, ok
}

func (s *InMemoryStore) SetPriorPhase(ctx context.Context, workspaceID, phase string) {
	s.priorPhaseMu.Lock()
	defer s.priorPhaseMu.Unlock()
	s.priorPhase[workspaceID] = phase
}

func (s *InMemoryStore) DeletePriorPhase(ctx context.Context, workspaceID string) {
	s.priorPhaseMu.Lock()
	defer s.priorPhaseMu.Unlock()
	delete(s.priorPhase, workspaceID)
}

// --- Parent-backfill marker ---

func (s *InMemoryStore) GetParentBackfilled(ctx context.Context, workspaceID string) bool {
	s.parentBackfilledMu.RLock()
	defer s.parentBackfilledMu.RUnlock()
	_, ok := s.parentBackfilled[workspaceID]
	return ok
}

func (s *InMemoryStore) SetParentBackfilled(ctx context.Context, workspaceID string) {
	s.parentBackfilledMu.Lock()
	defer s.parentBackfilledMu.Unlock()
	s.parentBackfilled[workspaceID] = struct{}{}
}

func (s *InMemoryStore) DeleteParentBackfilled(ctx context.Context, workspaceID string) {
	s.parentBackfilledMu.Lock()
	defer s.parentBackfilledMu.Unlock()
	delete(s.parentBackfilled, workspaceID)
}

// --- Session-index reconciliation counters (#1340) ---

func (s *InMemoryStore) GetReconcileMisses(_ context.Context, workspaceID string) map[string]int {
	s.reconcileMissesMu.RLock()
	defer s.reconcileMissesMu.RUnlock()
	counters, ok := s.reconcileMisses[workspaceID]
	if !ok || len(counters) == 0 {
		// Nil for absent (Redis-parity: no key reads nil).
		return nil
	}
	out := make(map[string]int, len(counters))
	for k, v := range counters {
		out[k] = v
	}
	return out
}

func (s *InMemoryStore) ClaimReconcileTurn(_ context.Context, workspaceID string, cadence time.Duration) bool {
	s.reconcileTurnMu.Lock()
	defer s.reconcileTurnMu.Unlock()
	now := time.Now()
	if s.reconcileTurn == nil {
		s.reconcileTurn = map[string]time.Time{}
	}
	if exp, ok := s.reconcileTurn[workspaceID]; ok && now.Before(exp) {
		return false
	}
	s.reconcileTurn[workspaceID] = now.Add(cadence)
	return true
}

func (s *InMemoryStore) SetReconcileMisses(_ context.Context, workspaceID string, misses map[string]int) {
	s.reconcileMissesMu.Lock()
	defer s.reconcileMissesMu.Unlock()
	if len(misses) == 0 {
		delete(s.reconcileMisses, workspaceID)
		return
	}
	if s.reconcileMisses == nil {
		s.reconcileMisses = map[string]map[string]int{}
	}
	cp := make(map[string]int, len(misses))
	for k, v := range misses {
		cp[k] = v
	}
	s.reconcileMisses[workspaceID] = cp
}

// --- Bulk invalidation ---

func (s *InMemoryStore) InvalidateAll(ctx context.Context, workspaceID string) {
	// Order matters for clarity, not correctness — each operation takes
	// its own lock and they are independent. Documenting the order so a
	// future reader can correlate with the original invalidateCaches().
	//
	// priorPhase is INTENTIONALLY NOT cleared here. The onPhaseChange
	// handler relies on priorPhase surviving invalidation so it can
	// distinguish "first invocation" (prior empty/unknown) from
	// "subsequent Active→Active reconcile" (prior == Active). Only the
	// Terminate/Terminating branch in proxy_events.go explicitly deletes
	// priorPhase — matching the original invalidateCaches behavior
	// exactly. Clearing it here would cause every redundant Active
	// watch event to wipe active sessions, deleted tombstones, and
	// password cache, breaking 429 enforcement and resurrecting
	// zombie sessions.
	s.ClearActiveSessions(ctx, workspaceID)
	s.ClearDeletedSessions(ctx, workspaceID)
	s.InvalidatePassword(ctx, workspaceID)
	s.InvalidateWorkspaceConfig(ctx, workspaceID)
	s.DeleteParentBackfilled(ctx, workspaceID)
	s.SetReconcileMisses(ctx, workspaceID, nil)
	s.reconcileTurnMu.Lock()
	delete(s.reconcileTurn, workspaceID)
	s.reconcileTurnMu.Unlock()
}

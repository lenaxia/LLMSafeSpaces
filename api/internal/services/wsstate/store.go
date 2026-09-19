// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wsstate holds the per-workspace state that ProxyHandler
// previously kept in process-local maps. Externalizing it to a Store
// abstraction is the foundation for sharing the state across replicas
// via a Redis backend in subsequent stories — eliminating the
// per-replica drift that caused the 2026-06-16 stuck-session class of
// bugs.
//
// All Store methods MUST be safe for concurrent use. Callers may invoke
// them from any goroutine (request handlers, watcher callbacks, background
// timers).
//
// connCount (HTTP connections per workspace) is intentionally NOT in this
// interface — it represents a per-replica resource (file descriptors,
// memory) and must remain local even after the Redis migration.
package wsstate

import (
	"context"
	"time"
)

// Config is the cached view of a workspace's spec-derived configuration
// (formerly ProxyHandler.workspaceConfig). It is populated from the
// Workspace CRD on first access and invalidated on phase transitions.
type Config struct {
	MaxActiveSessions      int
	AutoApprovePermissions bool
}

// Store is the per-workspace state contract used by ProxyHandler.
//
// The interface groups six logically distinct pieces of state that share
// the same lifecycle (per-workspace, invalidated together on phase
// change). They are grouped rather than split into separate interfaces
// because:
//   - the current consumer (ProxyHandler) uses all six together;
//   - the Redis key namespace is shared (`ws:{workspace_id}:*`);
//   - the invalidation semantics are shared (InvalidateAll).
//
// Future consumers that need only a subset should define a narrower
// interface at the call site (Go's structural typing makes this free).
//
// Method naming follows the existing ProxyHandler method names where
// possible to minimize churn at the call sites.
type Store interface {
	// --- Active session tracking (formerly activeSess) ---

	// CheckAndAddActiveSession atomically adds sessionID to the
	// workspace's active set if it is not already present AND the set
	// size is below maxSessions. Returns true if the session is now
	// active (newly added OR already present), false if the maxSessions
	// limit blocked the add. Atomicity is required so that two
	// concurrent calls for different sessions cannot both observe
	// size == maxSessions and both succeed (which would exceed the
	// limit by one). The InMemoryStore implements this with a mutex;
	// a future Redis implementation will use a Lua script for the
	// same atomicity guarantee.
	CheckAndAddActiveSession(ctx context.Context, workspaceID, sessionID string, maxSessions int) bool

	// RemoveActiveSession removes sessionID from the workspace's active
	// set. No-op if not present. Cleans up the per-workspace map entry
	// when the set becomes empty to keep memory bounded.
	RemoveActiveSession(ctx context.Context, workspaceID, sessionID string)

	// IsSessionActive reports whether sessionID is in the workspace's
	// active set.
	IsSessionActive(ctx context.Context, workspaceID, sessionID string) bool

	// ActiveSessionCount returns the number of sessions currently in
	// the workspace's active set. Returns 0 if the workspace has no
	// active set (no sessions ever added).
	ActiveSessionCount(ctx context.Context, workspaceID string) int

	// GetActiveSessions returns the IDs of all sessions currently in
	// the workspace's active set. Returns nil for an empty/unknown
	// workspace. Order is unspecified; callers must not rely on it.
	GetActiveSessions(ctx context.Context, workspaceID string) []string

	// ClearActiveSessions removes the workspace's entire active set.
	// Called by InvalidateAll on phase transitions.
	ClearActiveSessions(ctx context.Context, workspaceID string)

	// TouchActiveSessions refreshes the TTL of the workspace's active
	// session set without adding or removing any session. Called on SSE
	// activity (worklog 371 C3) so that a multi-hour agentic turn — which
	// emits session.status=busy once at turn start and no further session
	// events until completion — does not let the 30-minute TTL expire and
	// admit a concurrent turn that would corrupt opencode's SQLite session
	// history. For InMemoryStore this is a no-op (no TTL); for RedisStore
	// it runs EXPIRE on the active-set key.
	TouchActiveSessions(ctx context.Context, workspaceID string)

	// --- Deleted-session tombstones (formerly deletedSessions) ---

	// MarkSessionDeleted records that sessionID in workspaceID was
	// explicitly deleted via the API, so late SSE events arriving after
	// deletion are suppressed (preventing zombie sessions in
	// session_index).
	MarkSessionDeleted(ctx context.Context, workspaceID, sessionID string)

	// IsSessionDeleted reports whether the session was recently deleted.
	// Implementations may age out tombstones (the InMemoryStore bounds
	// the set to 500 entries with batch eviction); callers must treat a
	// false response as "not recently deleted" rather than "never
	// deleted".
	IsSessionDeleted(ctx context.Context, workspaceID, sessionID string) bool

	// ClearDeletedSessions removes all tombstones for the workspace.
	// Called by InvalidateAll.
	ClearDeletedSessions(ctx context.Context, workspaceID string)

	// --- Workspace password cache (formerly pwCache) ---

	// GetCachedPassword returns the cached password for the workspace,
	// if present. Cache-only — does NOT fall back to the K8s Secret
	// fetch. The fallback stays in ProxyHandler.getPassword so the
	// store remains pure-state (no I/O dependencies).
	GetCachedPassword(ctx context.Context, workspaceID string) (string, bool)

	// SetCachedPassword populates the password cache for the workspace.
	SetCachedPassword(ctx context.Context, workspaceID, password string)

	// InvalidatePassword clears the cached password for the workspace.
	// Called on 401 from upstream and on phase transitions.
	InvalidatePassword(ctx context.Context, workspaceID string)

	// --- Workspace config cache (formerly wsConfig) ---

	// GetWorkspaceConfig returns the cached config for the workspace, if
	// present. Cache-only — ProxyHandler.shouldAutoApprovePermissions
	// falls back to fetching the Workspace CRD on miss.
	GetWorkspaceConfig(ctx context.Context, workspaceID string) (Config, bool)

	// SetWorkspaceConfig populates the config cache for the workspace.
	SetWorkspaceConfig(ctx context.Context, workspaceID string, cfg Config)

	// InvalidateWorkspaceConfig clears the cached config.
	InvalidateWorkspaceConfig(ctx context.Context, workspaceID string)

	// --- Prior phase tracking (formerly priorPhase) ---

	// GetPriorPhase returns the workspace's last-observed phase, if any.
	// Used by onPhaseChange to detect real transitions vs no-op events.
	GetPriorPhase(ctx context.Context, workspaceID string) (string, bool)

	// SetPriorPhase records the workspace's current phase as the prior
	// phase for the next onPhaseChange invocation.
	SetPriorPhase(ctx context.Context, workspaceID, phase string)

	// DeletePriorPhase removes the prior-phase entry. Called on
	// terminate so the workspace starts fresh if ever re-created with
	// the same name. NOT called by InvalidateAll — see the contract
	// doc on InvalidateAll for why.
	DeletePriorPhase(ctx context.Context, workspaceID string)

	// --- Parent-backfill marker (formerly parentBackfilled) ---

	// GetParentBackfilled reports whether the workspace's session-parent
	// backfill has already run. The marker is per-replica today; a
	// future Redis-backed implementation will move it to a shared key
	// so only one replica performs the backfill.
	GetParentBackfilled(ctx context.Context, workspaceID string) bool

	// SetParentBackfilled marks the workspace's backfill as done.
	SetParentBackfilled(ctx context.Context, workspaceID string)

	// DeleteParentBackfilled clears the marker, allowing the backfill
	// to re-run on the next opportunity. Called on backfill failure so
	// it can be retried, and on workspace terminate.
	DeleteParentBackfilled(ctx context.Context, workspaceID string)

	// --- Session-index reconciliation counters (#1340) ---

	// GetReconcileMisses returns the per-session consecutive-absent
	// counters for the workspace's session-index reconciliation pass
	// (S5b convergence). Counters are shared across replicas (Redis
	// TTL decays stale state); nil/empty means no pending misses.
	GetReconcileMisses(ctx context.Context, workspaceID string) map[string]int

	// SetReconcileMisses replaces the workspace's miss counters
	// wholesale (PlanReconciliation returns the complete next map).
	// Redis implementations set a TTL so abandoned counters decay.
	SetReconcileMisses(ctx context.Context, workspaceID string, misses map[string]int)

	// ClaimReconcileTurn atomically claims the workspace's
	// reconciliation turn for the cadence window: true exactly once per
	// window across ALL replicas (Redis SET-NX-EX; in-memory per
	// process). This single-writer claim is what makes the miss
	// counters' read-modify-write safe — the claim holder is the only
	// writer, so N counts checks, never concurrent repetitions.
	ClaimReconcileTurn(ctx context.Context, workspaceID string, cadence time.Duration) bool

	// --- Bulk invalidation ---

	// InvalidateAll clears the workspace state that becomes stale on a
	// phase transition: active sessions, deleted markers, cached
	// password, cached config, parent-backfill marker, and
	// session-index reconciliation counters. Does NOT
	// affect connCount (which is not in this Store) and does NOT affect
	// prior phase — the onPhaseChange handler relies on prior phase
	// surviving invalidation to distinguish first-invocation from
	// Active→Active reconcile. Terminate/Terminating explicitly calls
	// DeletePriorPhase when the workspace is truly gone.
	InvalidateAll(ctx context.Context, workspaceID string)
}

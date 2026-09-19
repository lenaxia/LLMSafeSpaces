// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package wsstate

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// Compile-time assertion that RedisStore implements Store.
var _ Store = (*RedisStore)(nil)

// DefaultActiveSessTTL is the auto-recovery TTL for stuck active-session
// entries. If a session is added but never removed (process crash,
// network partition), the entry expires after this duration so the
// workspace doesn't stay stuck — the multi-replica fix for the
// 2026-06-16 incident. 30 minutes matches the design spec.
const DefaultActiveSessTTL = 30 * time.Minute

// DefaultDeletedTTL is the TTL for per-session tombstones. Each
// tombstone expires independently (per-key TTL, not a shared SET TTL).
// After expiry, late SSE events for that session are no longer
// suppressed — but by then the session has been gone long enough that
// any late event is extremely unlikely. 30 minutes matches the design
// spec and is the same duration as activeSess TTL.
const DefaultDeletedTTL = 30 * time.Minute

// DefaultPasswordTTL is the TTL for cached workspace passwords.
// Passwords are stable (only change on workspace recreate), so the TTL
// can be longer than for active sessions or tombstones. 1 hour matches
// the design spec. After TTL expiry, the next request re-fetches from
// K8s (password may have rotated).
const DefaultPasswordTTL = 1 * time.Hour

// DefaultConfigTTL is the TTL for cached workspace config. Shorter than
// password TTL because config (MaxActiveSessions, AutoApprovePermissions)
// can change via CRD updates. 5 minutes matches the design spec.
const DefaultConfigTTL = 5 * time.Minute

// DefaultPriorPhaseTTL is the TTL for prior-phase tracking. 24 hours —
// long enough to survive API replica restarts and watch reconnects.
const DefaultPriorPhaseTTL = 24 * time.Hour

// DefaultBackfilledTTL is the TTL for the parent-backfill marker. 24
// hours — backfill is idempotent, so re-running after TTL expiry is safe.
const DefaultBackfilledTTL = 24 * time.Hour

// DefaultReconcileMissesTTL is the TTL for session-index reconciliation
// miss counters (#1340). 15 minutes — far longer than the 30s
// reconciliation cadence (so counters persist across cadence gaps and
// replica churn), short enough that abandoned counters decay instead of
// accumulating for deleted workspaces.
const DefaultReconcileMissesTTL = 15 * time.Minute

// checkAndAddScript atomically checks the active-session set size and
// adds the session ID if there's room. The atomicity is what makes this
// safe across replicas: two concurrent calls cannot both observe
// size == maxSessions and both succeed. Lua scripts run as a single
// indivisible command in Redis, so the SISMEMBER+SCARD+SADD sequence
// is race-free.
//
// Returns 1 if added OR already present (idempotent); 0 if blocked by
// the maxSessions limit.
var checkAndAddScript = redis.NewScript(`
-- KEYS[1] = "ws:{workspace_id}:active"
-- ARGV[1] = sessionID
-- ARGV[2] = maxSessions
-- ARGV[3] = ttlSeconds
-- Returns: 1 if added/already-present, 0 if rejected by limit

local key = KEYS[1]
local sessionID = ARGV[1]
local maxSessions = tonumber(ARGV[2])
local ttlSeconds = tonumber(ARGV[3])

if redis.call('SISMEMBER', key, sessionID) == 1 then
    redis.call('EXPIRE', key, ttlSeconds)
    return 1
end

local count = redis.call('SCARD', key)
if count >= maxSessions then
    return 0
end

redis.call('SADD', key, sessionID)
redis.call('EXPIRE', key, ttlSeconds)
return 1
`)

// RedisStore is the multi-replica-safe implementation of Store. All
// six state sections (activeSess, deletedSessions, pwCache, wsConfig,
// priorPhase, parentBackfilled) are backed by Redis. The RedisStore is
// the sole production path — the InMemoryStore exists only as the
// default for ProxyHandler when no Redis client is configured (unit
// tests, local dev without Redis).
type RedisStore struct {
	// client is borrowed — its lifecycle is managed by the caller
	// (typically the cache service). RedisStore does not close it.
	client *redis.Client

	// activeSessTTL is the auto-recovery TTL for stuck active-session
	// entries. Refreshed on every successful CheckAndAddActiveSession.
	activeSessTTL time.Duration

	// deletedTTL is the per-key TTL for session tombstones. Each
	// tombstone expires independently.
	deletedTTL time.Duration

	// passwordTTL is the TTL for cached workspace passwords. Longer than
	// activeSessTTL/deletedTTL because passwords are stable.
	passwordTTL time.Duration

	// configTTL is the TTL for cached workspace config. Shorter than
	// passwordTTL because config can change via CRD updates.
	configTTL time.Duration

	// priorPhaseTTL is the TTL for prior-phase tracking.
	priorPhaseTTL time.Duration

	// backfilledTTL is the TTL for the parent-backfill marker.
	backfilledTTL time.Duration

	// logger records fail-open events. Optional — if nil, errors are
	// surfaced only via Prometheus metrics.
	logger pkginterfaces.LoggerInterface

	// Prometheus metrics required by US-45.2.
	opDuration          *prometheus.HistogramVec
	errorsTotal         *prometheus.CounterVec
	activeSessionsGauge *prometheus.GaugeVec
}

// NewRedisStore returns a Store backed by Redis for active sessions and
// by InMemoryStore for the remaining (not-yet-migrated) sections. The
// active-session TTL is set to DefaultActiveSessTTL.
func NewRedisStore(client *redis.Client, activeSessTTL time.Duration) *RedisStore {
	return NewRedisStoreWithLogger(client, activeSessTTL, nil)
}

// NewRedisStoreWithLogger is like NewRedisStore but also accepts a
// logger for fail-open event recording. The logger may be nil —
// Prometheus metrics are recorded regardless.
func NewRedisStoreWithLogger(client *redis.Client, activeSessTTL time.Duration, logger pkginterfaces.LoggerInterface) *RedisStore {
	if activeSessTTL <= 0 {
		activeSessTTL = DefaultActiveSessTTL
	}
	registerMetrics()
	return &RedisStore{
		client:              client,
		activeSessTTL:       activeSessTTL,
		deletedTTL:          DefaultDeletedTTL,
		passwordTTL:         DefaultPasswordTTL,
		configTTL:           DefaultConfigTTL,
		priorPhaseTTL:       DefaultPriorPhaseTTL,
		backfilledTTL:       DefaultBackfilledTTL,
		logger:              logger,
		opDuration:          pkgOpDuration,
		errorsTotal:         pkgErrorsTotal,
		activeSessionsGauge: pkgActiveSessionsGauge,
	}
}

// Package-level Prometheus metrics. Registered once via sync.Once
// because the Prometheus default registry rejects duplicate
// registrations — each test creates a fresh RedisStore, so per-store
// metric fields would panic on the second construction.
var (
	metricsOnce sync.Once

	pkgOpDuration          *prometheus.HistogramVec
	pkgErrorsTotal         *prometheus.CounterVec
	pkgActiveSessionsGauge *prometheus.GaugeVec

	// pkgIsSessionDeletedFailClosedTotal (worklog 371 M1) counts the number
	// of times IsSessionDeleted returned TRUE because of a Redis error
	// (fail-closed), as opposed to an actual tombstone. During a Redis
	// outage, every IsSessionDeleted call fail-closes → ALL session-event
	// processing (title persistence, context-token recording, queue drain,
	// sessionIndex.RecordMessage) is silently suppressed. This counter makes
	// that silent suppression alertable: alert on
	// rate(ws_state_is_session_deleted_fail_closed_total[5m]) > 0.
	pkgIsSessionDeletedFailClosedTotal prometheus.Counter
)

func registerMetrics() {
	metricsOnce.Do(func() {
		pkgOpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ws_state_op_duration_seconds",
			Help:    "wsstate Store operation latency by operation and result",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 12),
		}, []string{"op", "result"})

		pkgErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "ws_state_errors_total",
			Help: "wsstate Store operation errors by operation",
		}, []string{"op"})

		pkgActiveSessionsGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ws_state_active_sessions",
			Help: "wsstate active session count per workspace (sampled on writes)",
		}, []string{"workspace_id"})

		pkgIsSessionDeletedFailClosedTotal = promauto.NewCounter(prometheus.CounterOpts{
			Name: "ws_state_is_session_deleted_fail_closed_total",
			Help: "Total times IsSessionDeleted returned true due to Redis error (fail-closed), suppressing session-event processing. Alert if > 0: a Redis outage makes all sessions look deleted.",
		})
	})
}

// observeOp records an operation's duration and result. Callers must
// pass a non-zero start time; we avoid time.Since ambiguity by taking
// the start as a parameter.
func (s *RedisStore) observeOp(op, result string, start time.Time) {
	if s.opDuration == nil {
		return
	}
	s.opDuration.WithLabelValues(op, result).Observe(time.Since(start).Seconds())
}

func (s *RedisStore) recordError(op string) {
	if s.errorsTotal == nil {
		return
	}
	s.errorsTotal.WithLabelValues(op).Inc()
}

// activeKey returns the canonical Redis key for a workspace's active
// session set. The {workspace_id} hash tag forces all keys for a
// workspace to land on the same Redis shard — enables future cluster
// migration with zero code change.
func activeSessKey(workspaceID string) string {
	return fmt.Sprintf("ws:{%s}:active", workspaceID)
}

// --- Active session tracking (Redis-backed) ---

// CheckAndAddActiveSession atomically adds sessionID to the workspace's
// active set if there's room. Fail-open: if Redis is unreachable,
// returns true and records the error. The rationale (per design):
// better to allow a request than block legit traffic when Redis hiccups.
func (s *RedisStore) CheckAndAddActiveSession(ctx context.Context, workspaceID, sessionID string, maxSessions int) bool {
	const op = "check_and_add_active_session"
	start := time.Now()

	res, err := checkAndAddScript.Run(ctx, s.client,
		[]string{activeSessKey(workspaceID)},
		sessionID, maxSessions, int(s.activeSessTTL.Seconds())).Result()
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis CheckAndAddActiveSession failed, failing OPEN",
				"error", err, "workspace_id", workspaceID, "session_id", sessionID)
		}
		return true
	}

	allowed, ok := res.(int64)
	if !ok {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Error("wsstate: unexpected CheckAndAdd result type", fmt.Errorf("got %T", res),
				"workspace_id", workspaceID, "session_id", sessionID)
		}
		return true
	}

	if allowed == 1 {
		s.observeOp(op, "allowed", start)
		if s.activeSessionsGauge != nil {
			// Sample the gauge on every successful write. Cheaper than a
			// separate polling loop, and the value is fresh.
			n := s.ActiveSessionCount(ctx, workspaceID)
			s.activeSessionsGauge.WithLabelValues(workspaceID).Set(float64(n))
		}
		return true
	}
	s.observeOp(op, "rejected", start)
	return false
}

// RemoveActiveSession removes sessionID from the workspace's active set.
// If the set becomes empty, the Redis key is deleted so it does not
// linger as an orphan with TTL countdown.
//
// The SREM, SCARD-check, and conditional DEL run inside a single Lua
// script so the entire operation is atomic. Without atomicity a race
// could exist: between SREM and a separate DEL-on-empty check, another
// replica could SADD a new session; the subsequent DEL would erase it.
//
// On transition to empty the Prometheus gauge label is cleaned up via
// DeleteLabelValues — without this, workspaces that churn through
// create/suspend/terminate cycles would accumulate orphan time series
// forever (workspace_id is a UUID, so cardinality is unbounded).
func (s *RedisStore) RemoveActiveSession(ctx context.Context, workspaceID, sessionID string) {
	const op = "remove_active_session"
	start := time.Now()
	key := activeSessKey(workspaceID)

	res, err := removeActiveScript.Run(ctx, s.client,
		[]string{key}, sessionID).Result()
	if err != nil && err != redis.Nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis RemoveActiveSession failed",
				"error", err, "workspace_id", workspaceID, "session_id", sessionID)
		}
		return
	}
	s.observeOp(op, "ok", start)

	// Clean up the Prometheus gauge label when the workspace's set
	// becomes empty. removeActiveScript returns the remaining size
	// (0 if the key was deleted). This bounds metric cardinality.
	remaining, _ := res.(int64)
	if remaining == 0 && s.activeSessionsGauge != nil {
		s.activeSessionsGauge.DeleteLabelValues(workspaceID)
	}
}

// removeActiveScript atomically removes a session and deletes the key
// if the set is now empty. Returns the remaining size (0 if key deleted).
var removeActiveScript = redis.NewScript(`
-- KEYS[1] = "ws:{workspace_id}:active"
-- ARGV[1] = sessionID
local key = KEYS[1]
redis.call('SREM', key, ARGV[1])
if redis.call('SCARD', key) == 0 then
    redis.call('DEL', key)
    return 0
end
return redis.call('SCARD', key)
`)

// IsSessionActive reports whether sessionID is in the workspace's
// active set. Returns false on Redis error (do not trap the user in 409
// based on possibly-stale state).
func (s *RedisStore) IsSessionActive(ctx context.Context, workspaceID, sessionID string) bool {
	const op = "is_session_active"
	start := time.Now()
	n, err := s.client.SIsMember(ctx, activeSessKey(workspaceID), sessionID).Result()
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return false
	}
	s.observeOp(op, "ok", start)
	return n
}

// ActiveSessionCount returns the number of sessions currently in the
// workspace's active set. Returns 0 on Redis error.
func (s *RedisStore) ActiveSessionCount(ctx context.Context, workspaceID string) int {
	const op = "active_session_count"
	start := time.Now()
	n, err := s.client.SCard(ctx, activeSessKey(workspaceID)).Result()
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return 0
	}
	s.observeOp(op, "ok", start)
	return int(n)
}

// GetActiveSessions returns the IDs of all sessions currently in the
// workspace's active set. Returns nil on Redis error or empty set.
func (s *RedisStore) GetActiveSessions(ctx context.Context, workspaceID string) []string {
	const op = "get_active_sessions"
	start := time.Now()
	members, err := s.client.SMembers(ctx, activeSessKey(workspaceID)).Result()
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return nil
	}
	s.observeOp(op, "ok", start)
	if len(members) == 0 {
		return nil
	}
	return members
}

// ClearActiveSessions deletes the workspace's entire active set,
// removing the Redis key entirely so no orphan TTL countdown lingers.
// Also cleans up the Prometheus gauge label to bound cardinality.
func (s *RedisStore) ClearActiveSessions(ctx context.Context, workspaceID string) {
	const op = "clear_active_sessions"
	start := time.Now()
	if err := s.client.Del(ctx, activeSessKey(workspaceID)).Err(); err != nil && err != redis.Nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis ClearActiveSessions failed",
				"error", err, "workspace_id", workspaceID)
		}
		return
	}
	s.observeOp(op, "ok", start)
	if s.activeSessionsGauge != nil {
		s.activeSessionsGauge.DeleteLabelValues(workspaceID)
	}
}

// TouchActiveSessions refreshes the TTL of the workspace's active
// session set (worklog 371 C3). Called on SSE activity so a multi-hour
// agentic turn does not let the 30-minute TTL expire mid-turn and admit
// a concurrent request that corrupts opencode's SQLite session history.
//
// EXPIRE on a non-existent key is a no-op (returns 0, no error), so it
// is safe to call unconditionally on every SSE event even when the
// workspace has no active sessions.
func (s *RedisStore) TouchActiveSessions(ctx context.Context, workspaceID string) {
	const op = "touch_active_sessions"
	start := time.Now()
	if err := s.client.Expire(ctx, activeSessKey(workspaceID), s.activeSessTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis TouchActiveSessions failed",
				"error", err, "workspace_id", workspaceID)
		}
		return
	}
	s.observeOp(op, "ok", start)
}

// --- InvalidateAll ---

// InvalidateAll clears all Redis-backed state (active sessions, deleted
// tombstones, password cache, config cache, parent backfill) for the
// workspace. priorPhase is INTENTIONALLY PRESERVED — onPhaseChange
// relies on it to distinguish first-invocation from Active→Active
// reconcile (per US-45.1 contract). Terminate/Terminating calls
// DeletePriorPhase explicitly when the workspace is truly gone.
func (s *RedisStore) InvalidateAll(ctx context.Context, workspaceID string) {
	s.ClearActiveSessions(ctx, workspaceID)
	s.ClearDeletedSessions(ctx, workspaceID)
	s.InvalidatePassword(ctx, workspaceID)
	s.InvalidateWorkspaceConfig(ctx, workspaceID)
	s.DeleteParentBackfilled(ctx, workspaceID)
	s.SetReconcileMisses(ctx, workspaceID, nil)
}

// --- Deleted-session tombstones (Redis-backed, US-45.3) ---
//
// Tombstones prevent late SSE events from opencode from re-inserting a
// deleted session into session_index. Moving them to Redis ensures the
// suppression is cluster-wide.
//
// Fail-CLOSED policy (intentional opposite of activeSess fail-OPEN): if
// Redis is unreachable, IsSessionDeleted returns TRUE (assume deleted to
// prevent zombie resurrection). The rationale (per design): "If we can't
// verify, assume deleted; user can recreate session" — data integrity >
// availability here.

func deletedSessKey(workspaceID, sessionID string) string {
	return fmt.Sprintf("ws:{%s}:deleted:%s", workspaceID, sessionID)
}

// MarkSessionDeleted records a per-session tombstone in Redis with TTL.
// Silently fails on Redis error — the tombstone is not recorded, but the
// system continues. When Redis recovers, the session can be re-deleted.
func (s *RedisStore) MarkSessionDeleted(ctx context.Context, workspaceID, sessionID string) {
	const op = "mark_session_deleted"
	start := time.Now()
	key := deletedSessKey(workspaceID, sessionID)
	if err := s.client.Set(ctx, key, "1", s.deletedTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis MarkSessionDeleted failed",
				"error", err, "workspace_id", workspaceID, "session_id", sessionID)
		}
		return
	}
	s.observeOp(op, "ok", start)
}

// IsSessionDeleted reports whether the session was recently deleted via
// the API. Fail-CLOSED: returns TRUE on Redis error (assume deleted to
// prevent zombie session resurrection).
//
// Worklog 371 M1: each fail-closed return increments
// ws_state_is_session_deleted_fail_closed_total so operators can alert on
// the silent suppression of session-event processing during a Redis
// outage (title persistence, context-token recording, queue drain, and
// sessionIndex.RecordMessage are all gated on !isSessionDeleted).
func (s *RedisStore) IsSessionDeleted(ctx context.Context, workspaceID, sessionID string) bool {
	const op = "is_session_deleted"
	start := time.Now()
	n, err := s.client.Exists(ctx, deletedSessKey(workspaceID, sessionID)).Result()
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if pkgIsSessionDeletedFailClosedTotal != nil {
			pkgIsSessionDeletedFailClosedTotal.Inc()
		}
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis IsSessionDeleted failed — fail-closing (treating as deleted)",
				"error", err, "workspace_id", workspaceID, "session_id", sessionID)
		}
		return true
	}
	s.observeOp(op, "ok", start)
	return n > 0
}

// ClearDeletedSessions removes all tombstones for the workspace. Uses
// SCAN to find keys matching `ws:{workspace_id}:deleted:*` and DELs them
// in batches. No-op on Redis error (the tombstones will expire via TTL).
func (s *RedisStore) ClearDeletedSessions(ctx context.Context, workspaceID string) {
	const op = "clear_deleted_sessions"
	start := time.Now()
	pattern := fmt.Sprintf("ws:{%s}:deleted:*", workspaceID)
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			s.recordError(op)
			s.observeOp(op, "error", start)
			if s.logger != nil {
				s.logger.Warn("wsstate: Redis ClearDeletedSessions scan failed",
					"error", err, "workspace_id", workspaceID)
			}
			return
		}
		if len(keys) > 0 {
			if err := s.client.Del(ctx, keys...).Err(); err != nil {
				s.recordError(op)
				s.observeOp(op, "error", start)
				if s.logger != nil {
					s.logger.Warn("wsstate: Redis ClearDeletedSessions del failed",
						"error", err, "workspace_id", workspaceID)
				}
				return
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	s.observeOp(op, "ok", start)
}

// --- Workspace password cache (Redis-backed, US-45.4) ---
//
// Passwords are stable (only change on workspace recreate). Moving them
// to Redis eliminates per-replica staleness on phase changes: a 401 on
// replica A that invalidates the cache is now visible to all replicas.
//
// Fail-through-to-K8s policy: Redis is a cache, the source of truth is
// the K8s Secret. On Redis error, GetCachedPassword returns (empty, false)
// so the caller (ProxyHandler.getPassword) falls back to fetching the
// K8s Secret directly. This is NOT fail-closed (no false data) and NOT
// fail-open (no return true) — it is "fail-through" to the source of truth.
//
// The K8s Secret fetch stays in ProxyHandler.getPassword so the store
// remains pure-state (no I/O dependencies). The store's SetCachedPassword
// is called only after a successful K8s fetch to populate the shared cache.

func passwordCacheKey(workspaceID string) string {
	return fmt.Sprintf("ws:{%s}:pw", workspaceID)
}

// GetCachedPassword returns the cached password for the workspace, if
// present. Cache-only — never returns false data on Redis error. Returns
// ("", false) on miss OR on Redis error so the caller falls through to
// the K8s Secret fetch.
func (s *RedisStore) GetCachedPassword(ctx context.Context, workspaceID string) (string, bool) {
	const op = "get_cached_password"
	start := time.Now()
	pw, err := s.client.Get(ctx, passwordCacheKey(workspaceID)).Result()
	if err != nil {
		// redis.Nil = key not found (cache miss) — not an error.
		if err != redis.Nil {
			s.recordError(op)
			s.observeOp(op, "error", start)
			if s.logger != nil {
				s.logger.Warn("wsstate: Redis GetCachedPassword failed — falling through to K8s",
					"error", err, "workspace_id", workspaceID)
			}
		} else {
			s.observeOp(op, "miss", start)
		}
		return "", false
	}
	s.observeOp(op, "hit", start)
	return pw, true
}

// SetCachedPassword populates the password cache for the workspace.
// Silently fails on Redis error — the next read returns a miss and
// falls through to K8s. Idempotent: re-setting the same password
// refreshes the TTL.
//
// H3 (worklog 371): the password is stored in PLAINTEXT in Redis. This is
// intentional: the API needs the plaintext to set Basic-Auth on every proxied
// request to opencode, so hashing (which would prevent plaintext retrieval)
// is not viable — every cache hit would fall through to the K8s Secret fetch,
// defeating the cache. Production deployments MUST configure Redis with:
//   - TLS in-transit (rediss:// or a TLS sidecar) so the plaintext is not
//     exposed on the internal network.
//   - At-rest encryption (Redis 7 ACL + encryption, or disk-level encryption
//     on the Redis PVC) so RDB/AOF dumps and backups do not expose it.
//   - NetworkPolicy restricting ingress to the API pods only.
//
// These are deployment responsibilities, not code-level controls — see the
// chart's redis section in values.yaml and the production runbook. The
// passwords are per-workspace generated credentials (not user passwords),
// bounded by the 1h TTL, and the source of truth is the K8s Secret (also
// encrypted at rest).
func (s *RedisStore) SetCachedPassword(ctx context.Context, workspaceID, password string) {
	const op = "set_cached_password"
	start := time.Now()
	if err := s.client.Set(ctx, passwordCacheKey(workspaceID), password, s.passwordTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis SetCachedPassword failed",
				"error", err, "workspace_id", workspaceID)
		}
		return
	}
	s.observeOp(op, "ok", start)
}

// InvalidatePassword clears the cached password for the workspace.
// DEL is the single source of truth — replicas hitting Redis on miss
// fall through to K8s. No pubsub needed (per design: replicas hit Redis
// on every request anyway).
func (s *RedisStore) InvalidatePassword(ctx context.Context, workspaceID string) {
	const op = "invalidate_password"
	start := time.Now()
	if err := s.client.Del(ctx, passwordCacheKey(workspaceID)).Err(); err != nil && err != redis.Nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis InvalidatePassword failed",
				"error", err, "workspace_id", workspaceID)
		}
		return
	}
	s.observeOp(op, "ok", start)
}

// --- Workspace config cache (Redis-backed, US-45.6) ---
//
// Config (MaxActiveSessions + AutoApprovePermissions) is fetched from
// the Workspace CRD on first access and cached. Moving to Redis ensures
// all replicas share the same config view.
//
// Same fail-through pattern as pwCache: Redis is a cache, the source of
// truth is the Workspace CRD. On Redis error, GetWorkspaceConfig returns
// (zero, false) so the caller (shouldAutoApprovePermissions) falls back
// to fetching the CRD.

func configCacheKey(workspaceID string) string {
	return fmt.Sprintf("ws:{%s}:config", workspaceID)
}

func (s *RedisStore) GetWorkspaceConfig(ctx context.Context, workspaceID string) (Config, bool) {
	const op = "get_workspace_config"
	start := time.Now()
	raw, err := s.client.Get(ctx, configCacheKey(workspaceID)).Bytes()
	if err != nil {
		if err != redis.Nil {
			s.recordError(op)
			s.observeOp(op, "error", start)
			if s.logger != nil {
				s.logger.Warn("wsstate: Redis GetWorkspaceConfig failed — falling through to CRD",
					"error", err, "workspace_id", workspaceID)
			}
		} else {
			s.observeOp(op, "miss", start)
		}
		return Config{}, false
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: cached config JSON corrupt — falling through to CRD",
				"error", err, "workspace_id", workspaceID)
		}
		return Config{}, false
	}
	s.observeOp(op, "hit", start)
	return cfg, true
}

func (s *RedisStore) SetWorkspaceConfig(ctx context.Context, workspaceID string, cfg Config) {
	const op = "set_workspace_config"
	start := time.Now()
	data, err := json.Marshal(cfg)
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Error("wsstate: failed to marshal Config for Redis cache", err,
				"workspace_id", workspaceID)
		}
		return
	}
	if err := s.client.Set(ctx, configCacheKey(workspaceID), data, s.configTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis SetWorkspaceConfig failed",
				"error", err, "workspace_id", workspaceID)
		}
		return
	}
	s.observeOp(op, "ok", start)
}

func (s *RedisStore) InvalidateWorkspaceConfig(ctx context.Context, workspaceID string) {
	const op = "invalidate_workspace_config"
	start := time.Now()
	if err := s.client.Del(ctx, configCacheKey(workspaceID)).Err(); err != nil && err != redis.Nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		if s.logger != nil {
			s.logger.Warn("wsstate: Redis InvalidateWorkspaceConfig failed",
				"error", err, "workspace_id", workspaceID)
		}
		return
	}
	s.observeOp(op, "ok", start)
}

// --- Prior phase tracking (Redis-backed, US-45.7) ---

func priorPhaseCacheKey(workspaceID string) string {
	return fmt.Sprintf("ws:{%s}:phase", workspaceID)
}

func (s *RedisStore) GetPriorPhase(ctx context.Context, workspaceID string) (string, bool) {
	const op = "get_prior_phase"
	start := time.Now()
	phase, err := s.client.Get(ctx, priorPhaseCacheKey(workspaceID)).Result()
	if err != nil {
		if err != redis.Nil {
			s.recordError(op)
			s.observeOp(op, "error", start)
			if s.logger != nil {
				s.logger.Warn("wsstate: Redis GetPriorPhase failed — assuming Active→Active to avoid mass cache wipe",
					"error", err, "workspace_id", workspaceID)
			}
			// C4 (worklog 371): returning ("", false) on Redis error causes
			// onPhaseChange to treat it as first-invocation and call
			// invalidateCaches, which wipes activeSess + deletedSessions +
			// pwCache + wsConfig across all replicas. A transient Redis blip
			// during a CRD watcher reconnect would silently recreate the
			// data-loss class Epic 45 exists to prevent. Instead assume the
			// common case (Active→Active) so only WorkspaceConfig is
			// invalidated, not the full cache. The Creating→Active edge case
			// (rare: requires Redis outage exactly when a workspace
			// transitions to Active) misses the SSE subscription restart,
			// but the proxy path's EnsureWatching on the next request and
			// the controller's periodic reconcile recover it.
			return "Active", true
		}
		s.observeOp(op, "miss", start)
		return "", false
	}
	s.observeOp(op, "hit", start)
	return phase, true
}

func (s *RedisStore) SetPriorPhase(ctx context.Context, workspaceID, phase string) {
	const op = "set_prior_phase"
	start := time.Now()
	if err := s.client.Set(ctx, priorPhaseCacheKey(workspaceID), phase, s.priorPhaseTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return
	}
	s.observeOp(op, "ok", start)
}

func (s *RedisStore) DeletePriorPhase(ctx context.Context, workspaceID string) {
	const op = "delete_prior_phase"
	start := time.Now()
	if err := s.client.Del(ctx, priorPhaseCacheKey(workspaceID)).Err(); err != nil && err != redis.Nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return
	}
	s.observeOp(op, "ok", start)
}

// --- Parent-backfill marker (Redis-backed, US-45.8) ---

func backfilledCacheKey(workspaceID string) string {
	return fmt.Sprintf("ws:{%s}:backfilled", workspaceID)
}

func (s *RedisStore) GetParentBackfilled(ctx context.Context, workspaceID string) bool {
	const op = "get_parent_backfilled"
	start := time.Now()
	n, err := s.client.Exists(ctx, backfilledCacheKey(workspaceID)).Result()
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return false
	}
	s.observeOp(op, "ok", start)
	return n > 0
}

func (s *RedisStore) SetParentBackfilled(ctx context.Context, workspaceID string) {
	const op = "set_parent_backfilled"
	start := time.Now()
	if err := s.client.Set(ctx, backfilledCacheKey(workspaceID), "1", s.backfilledTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return
	}
	s.observeOp(op, "ok", start)
}

func (s *RedisStore) DeleteParentBackfilled(ctx context.Context, workspaceID string) {
	const op = "delete_parent_backfilled"
	start := time.Now()
	if err := s.client.Del(ctx, backfilledCacheKey(workspaceID)).Err(); err != nil && err != redis.Nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return
	}
	s.observeOp(op, "ok", start)
}

// --- Session-index reconciliation counters (#1340, Redis-backed) ---

func reconcileMissesKey(workspaceID string) string {
	return fmt.Sprintf("ws:{%s}:reconcile-misses", workspaceID)
}

func (s *RedisStore) GetReconcileMisses(ctx context.Context, workspaceID string) map[string]int {
	const op = "get_reconcile_misses"
	start := time.Now()
	raw, err := s.client.Get(ctx, reconcileMissesKey(workspaceID)).Bytes()
	if err != nil {
		if err != redis.Nil {
			s.recordError(op)
			s.observeOp(op, "error", start)
		}
		return nil
	}
	var misses map[string]int
	if err := json.Unmarshal(raw, &misses); err != nil || misses == nil {
		s.observeOp(op, "ok", start)
		return nil
	}
	s.observeOp(op, "ok", start)
	return misses
}

func (s *RedisStore) SetReconcileMisses(ctx context.Context, workspaceID string, misses map[string]int) {
	const op = "set_reconcile_misses"
	start := time.Now()
	if len(misses) == 0 {
		if err := s.client.Del(ctx, reconcileMissesKey(workspaceID)).Err(); err != nil && err != redis.Nil {
			s.recordError(op)
			s.observeOp(op, "error", start)
		}
		s.observeOp(op, "ok", start)
		return
	}
	payload, err := json.Marshal(misses)
	if err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return
	}
	if err := s.client.Set(ctx, reconcileMissesKey(workspaceID), payload, DefaultReconcileMissesTTL).Err(); err != nil {
		s.recordError(op)
		s.observeOp(op, "error", start)
		return
	}
	s.observeOp(op, "ok", start)
}

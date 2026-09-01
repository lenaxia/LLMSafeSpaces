// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgLoggerInterfacePkg "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// pkgLoggerInterface aliases the shared LoggerInterface so the local
// captureLogger's With() return signature matches without a wide
// import at the call sites.
type pkgLoggerInterface = pkgLoggerInterfacePkg.LoggerInterface

// getCachedDEKFixture wires KeyService, mockJWTSessionStore, and a fake
// DEKCache together for GetCachedDEKForUser tests. Deterministic clock
// via store.now.
type getCachedDEKFixture struct {
	svc     *KeyService
	store   *mockJWTSessionStore
	cache   *fakeDEKCache
	userID  string
	baseTs  time.Time
	realDEK []byte
}

func newGetCachedDEKFixture(t *testing.T) *getCachedDEKFixture {
	t.Helper()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := newMockJWTSessionStore()
	store.now = base
	cache := &fakeDEKCache{data: map[string][]byte{}}
	svc := &KeyService{
		cache:       cache,
		jwtSessions: store,
	}
	svc.setClock(func() time.Time { return base })
	return &getCachedDEKFixture{
		svc:     svc,
		store:   store,
		cache:   cache,
		userID:  "user-1",
		baseTs:  base,
		realDEK: []byte("this-is-a-32-byte-user-dek-abcde"),
	}
}

// addSession creates a jwt_sessions row with arbitrary wrap bytes —
// post-US-70.5 the walk never unwraps, so only jti + expiry matter.
func (f *getCachedDEKFixture) addSession(t *testing.T, createdAt, expiresAt time.Time) *JWTSession {
	t.Helper()
	row := &JWTSession{
		JTI:        uuid.New(),
		UserID:     f.userID,
		WrappedDEK: []byte("historical-wrap-bytes"),
		KEKSalt:    []byte("historical-salt"),
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}
	f.store.mu.Lock()
	f.store.rows[row.JTI] = row
	f.store.mu.Unlock()
	return row
}

// TestGetCachedDEKForUser_NoActiveSessions is the primary "no live
// user" case: the user has never logged in on this API, or all their
// sessions have been pruned. Must surface as ErrDEKUnavailable so
// callers handle it as the no-source outcome (keyrewrap:
// unwrappable_no_source).
func TestGetCachedDEKForUser_NoActiveSessions(t *testing.T) {
	f := newGetCachedDEKFixture(t)

	dek, _, err := f.svc.GetCachedDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable,
		"absent session must surface as ErrDEKUnavailable — the same "+
			"sentinel every other 'no live DEK material' case uses")
}

// TestGetCachedDEKForUser_WarmCacheHit covers the recovery contract:
// an enumerated session's jti has the DEK in the Redis cache (login's
// K1 write) → the walk returns it with that jti.
func TestGetCachedDEKForUser_WarmCacheHit(t *testing.T) {
	f := newGetCachedDEKFixture(t)
	row := f.addSession(t, f.baseTs.Add(-30*time.Minute), f.baseTs.Add(24*time.Hour))
	require.NoError(t, f.cache.CacheDEK(context.Background(), row.JTI.String(), f.realDEK, time.Hour))

	dek, jti, err := f.svc.GetCachedDEKForUser(context.Background(), f.userID)
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, dek)
	assert.Equal(t, row.JTI.String(), jti, "the returned jti is the cache key the DEK was found under")
}

// TestGetCachedDEKForUser_MostRecentRowFirst pins the walk order: the
// newest session's cache entry wins over an older session's.
func TestGetCachedDEKForUser_MostRecentRowFirst(t *testing.T) {
	f := newGetCachedDEKFixture(t)
	oldRow := f.addSession(t, f.baseTs.Add(-3*time.Hour), f.baseTs.Add(1*time.Hour))
	newRow := f.addSession(t, f.baseTs.Add(-1*time.Hour), f.baseTs.Add(23*time.Hour))
	require.NoError(t, f.cache.CacheDEK(context.Background(), oldRow.JTI.String(), []byte("old-dek"), time.Hour))
	require.NoError(t, f.cache.CacheDEK(context.Background(), newRow.JTI.String(), f.realDEK, time.Hour))

	dek, jti, err := f.svc.GetCachedDEKForUser(context.Background(), f.userID)
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, dek)
	assert.Equal(t, newRow.JTI.String(), jti, "most-recent row's cache entry must be consulted first")
}

// TestGetCachedDEKForUser_CacheMissNeverUnwraps is the honest US-70.5
// pin: rows exist that COULD be unwrapped by historically-known keys,
// but the method has no unwrap path — a cold cache on every enumerated
// jti surfaces ErrDEKUnavailable, never a re-derived DEK.
func TestGetCachedDEKForUser_CacheMissNeverUnwraps(t *testing.T) {
	f := newGetCachedDEKFixture(t)
	f.addSession(t, f.baseTs.Add(-30*time.Minute), f.baseTs.Add(24*time.Hour))
	// No cache entries seeded.

	dek, _, err := f.svc.GetCachedDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable,
		"a durable row without a warm cache entry is NOT a source — "+
			"US-70.5 deleted the unwrap walk")
}

// TestGetCachedDEKForUser_CacheGetErrorLoggedAndTreatedAsMiss proves
// the Redis-outage observability contract: the error is logged (so an
// operator debugging a slow recovery can see the cache fault) AND the
// walk continues to older rows before giving up — no unwrap fallback.
func TestGetCachedDEKForUser_CacheGetErrorLoggedAndTreatedAsMiss(t *testing.T) {
	f := newGetCachedDEKFixture(t)
	f.addSession(t, f.baseTs.Add(-30*time.Minute), f.baseTs.Add(24*time.Hour))
	log := newCaptureLogger()
	f.svc.logger = log
	f.cache.getErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")

	dek, _, err := f.svc.GetCachedDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable,
		"a Redis outage must degrade to the no-source sentinel, not fail "+
			"with the raw cache error and not unwrap")
	assert.True(t, log.hasWarn("GetCachedDEKForUser: Redis DEK lookup failed"),
		"cache Get error MUST log a Warn — otherwise a Redis outage is silent")
}

// TestGetCachedDEKForUser_ListErrorPropagates proves errors from the
// store bubble up, not silently converted to ErrDEKUnavailable — a
// genuine PG outage should be observable by the caller.
func TestGetCachedDEKForUser_ListErrorPropagates(t *testing.T) {
	f := newGetCachedDEKFixture(t)
	f.store.listErr = errors.New("connection refused")

	dek, _, err := f.svc.GetCachedDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDEKUnavailable,
		"genuine DB errors should NOT be flattened to ErrDEKUnavailable")
}

// TestGetCachedDEKForUser_NoStoreConfiguredIsDEKUnavailable covers
// tests / dev configs that construct a KeyService without wiring the
// JWTSessionStore: no panic, same sentinel.
func TestGetCachedDEKForUser_NoStoreConfiguredIsDEKUnavailable(t *testing.T) {
	svc := &KeyService{cache: &fakeDEKCache{data: map[string][]byte{}}}

	dek, _, err := svc.GetCachedDEKForUser(context.Background(), "user-1")
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable)
}

// fakeDEKCache is a minimal in-memory DEKCache for these tests.
// Supports injected errors on Get for adversarial tests.
type fakeDEKCache struct {
	data     map[string][]byte
	getErr   error
	writeErr error
}

func (f *fakeDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	cp := make([]byte, len(dek))
	copy(cp, dek)
	f.data[sessionID] = cp
	return nil
}

func (f *fakeDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.data[sessionID]
	if !ok {
		return nil, nil
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

func (f *fakeDEKCache) EvictDEK(_ context.Context, sessionID string) error {
	delete(f.data, sessionID)
	return nil
}

// captureLogger records Warn calls so tests can assert on log content.
type captureLogger struct {
	warns []string
}

func newCaptureLogger() *captureLogger { return &captureLogger{} }

func (c *captureLogger) Debug(_ string, _ ...interface{})          {}
func (c *captureLogger) Info(_ string, _ ...interface{})           {}
func (c *captureLogger) Warn(msg string, _ ...interface{})         { c.warns = append(c.warns, msg) }
func (c *captureLogger) Error(_ string, _ error, _ ...interface{}) {}
func (c *captureLogger) Fatal(_ string, _ error, _ ...interface{}) {}
func (c *captureLogger) With(_ ...interface{}) pkgLoggerInterface  { return c }
func (c *captureLogger) Sync() error                               { return nil }

func (c *captureLogger) hasWarn(prefix string) bool {
	for _, w := range c.warns {
		if len(w) >= len(prefix) && w[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

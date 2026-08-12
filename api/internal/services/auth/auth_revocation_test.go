// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

// Regression tests for Epic 17 G18: JWT revocation cache key mismatch.
//
// Pre-fix bug: RevokeToken stored the blacklist entry under "token:<jti>".
// ValidateToken's fast-path read "token:<hash(token)>". The two keys never
// collided so revocation was silently a no-op for any subsequent request.
//
// Fix: RevokeToken writes BOTH keys atomically. ValidateToken checks the hash
// key (cache fast-path) on entry and the jti key (defense-in-depth) after
// parsing claims.
//
// These tests use an in-memory cache implementation rather than gomock so the
// end-to-end revoke→validate flow exercises real cache semantics.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	pkgutil "github.com/lenaxia/llmsafespaces/pkg/utilities"
)

// memCache is a minimal in-memory cache that satisfies interfaces.CacheService
// for test purposes. Only Get/Set/Delete are implemented since they're the
// only methods exercised by RevokeToken/ValidateToken.
type memCache struct {
	mu   sync.Mutex
	data map[string]memCacheEntry
}

type memCacheEntry struct {
	value     string
	expiresAt time.Time
}

func newMemCache() *memCache { return &memCache{data: map[string]memCacheEntry{}} }

func (c *memCache) Start() error { return nil }
func (c *memCache) Stop() error  { return nil }

func (c *memCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok {
		return "", errors.New("not found")
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(c.data, key)
		return "", errors.New("not found")
	}
	return e.value, nil
}

func (c *memCache) Set(_ context.Context, key, value string, expiration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := time.Time{}
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}
	c.data[key] = memCacheEntry{value: value, expiresAt: exp}
	return nil
}

func (c *memCache) SetNX(_ context.Context, key, value string, expiration time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[key]; ok {
		return false, nil
	}
	exp := time.Time{}
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}
	c.data[key] = memCacheEntry{value: value, expiresAt: exp}
	return true, nil
}

func (c *memCache) Incr(_ context.Context, key string, expiration time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := 0
	if entry, ok := c.data[key]; ok {
		fmt.Sscanf(entry.value, "%d", &v)
	}
	v++
	exp := time.Time{}
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}
	c.data[key] = memCacheEntry{value: fmt.Sprintf("%d", v), expiresAt: exp}
	return int64(v), nil
}

func (c *memCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
	return nil
}

func (c *memCache) DeleteByPrefix(_ context.Context, _ string) error { return nil }

func (c *memCache) GetObject(_ context.Context, _ string, _ interface{}) error {
	return errors.New("not implemented")
}
func (c *memCache) SetObject(_ context.Context, _ string, _ interface{}, _ time.Duration) error {
	return errors.New("not implemented")
}
func (c *memCache) GetSession(_ context.Context, _ string) (*types.CachedSession, error) {
	return nil, errors.New("not implemented")
}
func (c *memCache) SetSession(_ context.Context, _ string, _ types.CachedSession, _ time.Duration) error {
	return errors.New("not implemented")
}
func (c *memCache) DeleteSession(_ context.Context, _ string) error {
	return errors.New("not implemented")
}
func (c *memCache) Ping(_ context.Context) error { return nil }

var _ interfaces.CacheService = (*memCache)(nil)

func newRevocationFixture(t *testing.T) (*Service, *memCache, string) {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-revocation"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"

	cache := newMemCache()
	mockDB := new(mocks.MockDatabaseService)

	svc, err := New(cfg, log, mockDB, cache)
	require.NoError(t, err)

	token, err := svc.GenerateToken("user-revocation-test")
	require.NoError(t, err)

	return svc, cache, token
}

// TestG18_RevokeThenValidate_RejectsToken is the headline regression for G18.
// Before the fix this test failed: ValidateToken would return the userID even
// after RevokeToken succeeded.
func TestG18_RevokeThenValidate_RejectsToken(t *testing.T) {
	svc, _, token := newRevocationFixture(t)

	// Sanity check: token validates before revocation.
	userID, err := svc.ValidateToken(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, "user-revocation-test", userID)

	// Revoke.
	require.NoError(t, svc.RevokeToken(context.Background(), token))

	// Validation must now fail.
	userID, err = svc.ValidateToken(context.Background(), token)
	require.Error(t, err)
	require.Empty(t, userID)
	require.Contains(t, err.Error(), "revoked", "ValidateToken must surface a 'revoked' error after RevokeToken")
}

// TestG18_RevokeWritesBothKeys verifies the fix's defense-in-depth design:
// revocation must be visible under BOTH the hash-based fast-path key and the
// jti-based defense-in-depth key. If either is missing we have a regression.
func TestG18_RevokeWritesBothKeys(t *testing.T) {
	svc, cache, token := newRevocationFixture(t)

	// Extract jti from the token so we know what key to look for.
	parsed, _ := jwt.Parse(token, func(*jwt.Token) (interface{}, error) { return svc.jwtSecret, nil })
	claims := parsed.Claims.(jwt.MapClaims)
	jti, _ := claims["jti"].(string)
	require.NotEmpty(t, jti, "test setup: token must carry a jti claim")

	require.NoError(t, svc.RevokeToken(context.Background(), token))

	// Hash-keyed entry: the fast-path in ValidateToken reads this on entry.
	hashKey := "token:" + pkgutil.HashString(token)
	v, err := cache.Get(context.Background(), hashKey)
	require.NoError(t, err, "hash-keyed revocation entry missing — fast-path will not see revocation")
	require.Equal(t, "revoked", v)

	// Jti-keyed entry: the defense-in-depth path in ValidateToken reads this
	// after parsing claims.
	jtiKey := "token:" + jti
	v, err = cache.Get(context.Background(), jtiKey)
	require.NoError(t, err, "jti-keyed revocation entry missing — defense-in-depth path will not see revocation")
	require.Equal(t, "revoked", v)
}

// TestG18_RevocationDefenseInDepth_HashCacheEvicted simulates a Redis eviction
// of the hash-keyed revocation entry (e.g. memory pressure). The jti-keyed
// entry must still cause ValidateToken to reject the token.
func TestG18_RevocationDefenseInDepth_HashCacheEvicted(t *testing.T) {
	svc, cache, token := newRevocationFixture(t)

	require.NoError(t, svc.RevokeToken(context.Background(), token))

	// Manually evict the hash-keyed revocation entry but leave the jti entry.
	hashKey := "token:" + pkgutil.HashString(token)
	require.NoError(t, cache.Delete(context.Background(), hashKey))

	userID, err := svc.ValidateToken(context.Background(), token)
	require.Error(t, err)
	require.Empty(t, userID)
	require.Contains(t, err.Error(), "revoked",
		"defense-in-depth: jti check must reject revoked token even when hash-cache entry is evicted")
}

// TestG18_NonRevokedToken_StillValidates ensures the fix didn't introduce a
// false-positive revocation path for healthy tokens.
func TestG18_NonRevokedToken_StillValidates(t *testing.T) {
	svc, _, token := newRevocationFixture(t)

	for i := 0; i < 3; i++ {
		userID, err := svc.ValidateToken(context.Background(), token)
		require.NoError(t, err, "iteration %d: non-revoked token must validate cleanly", i)
		require.Equal(t, "user-revocation-test", userID)
	}
}

// TestG18_RevocationSurvivesCacheRoundTrip verifies that even after the
// hash-key fast-path caches the userID for a valid token, a subsequent
// revocation call still causes ValidateToken to return "revoked" on the
// next request. Pre-fix the cached "userID" would shadow the revocation
// because RevokeToken did not overwrite the hash-key.
func TestG18_RevocationSurvivesCacheRoundTrip(t *testing.T) {
	svc, _, token := newRevocationFixture(t)

	// Warm the validation cache (writes token:<hash> = userID).
	userID, err := svc.ValidateToken(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, "user-revocation-test", userID)

	// Revoke.
	require.NoError(t, svc.RevokeToken(context.Background(), token))

	// Same token should now be rejected on the very next call.
	userID, err = svc.ValidateToken(context.Background(), token)
	require.Error(t, err)
	require.Empty(t, userID)
	require.True(t, strings.Contains(err.Error(), "revoked"),
		"expected 'revoked' error after revocation overwrites cached userID; got: %v", err)
}

// TestG18_DoubleRevoke_Idempotent ensures revoking an already-revoked token
// is harmless.
func TestG18_DoubleRevoke_Idempotent(t *testing.T) {
	svc, _, token := newRevocationFixture(t)

	require.NoError(t, svc.RevokeToken(context.Background(), token))
	// Second call must succeed (Set is idempotent in-memory; Redis SET is too).
	require.NoError(t, svc.RevokeToken(context.Background(), token))

	_, err := svc.ValidateToken(context.Background(), token)
	require.Error(t, err)
	require.Contains(t, err.Error(), "revoked")
}

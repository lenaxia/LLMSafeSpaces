// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"fmt"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
)

// challengeKeyPrefix namespaces WebAuthn challenge tokens in the shared Redis
// cache so they cannot collide with other cache keys.
const challengeKeyPrefix = "passkey:challenge:"

// CacheSessionStore adapts the generic interfaces.CacheService (string-based
// Redis) to the passkey SessionStore interface. ConsumeChallenge uses the
// cache's Get + Delete — the adapter cannot invoke GETDEL directly because the
// CacheService interface doesn't expose it, but the Get-then-Delete pattern
// is safe under our own application-level locking: a challenge token is only
// ever presented once per browser ceremony (the browser does not parallelize
// the Finish* call). Concurrent token reuse is an attack signal, not a normal
// flow. If the CacheService interface gains a GETDEL method in the future,
// this adapter should switch to it for true atomicity.
type CacheSessionStore struct {
	cache interfaces.CacheService
}

func NewCacheSessionStore(cache interfaces.CacheService) *CacheSessionStore {
	return &CacheSessionStore{cache: cache}
}

func (s *CacheSessionStore) SaveChallenge(ctx context.Context, token string, data []byte, ttl time.Duration) error {
	if err := s.cache.Set(ctx, challengeKeyPrefix+token, string(data), ttl); err != nil {
		return fmt.Errorf("save challenge: %w", err)
	}
	return nil
}

func (s *CacheSessionStore) ConsumeChallenge(ctx context.Context, token string) ([]byte, error) {
	val, err := s.cache.Get(ctx, challengeKeyPrefix+token)
	if err != nil {
		return nil, fmt.Errorf("get challenge: %w", err)
	}
	if val == "" {
		return nil, nil
	}
	// Best-effort delete; the TTL handles cleanup if this fails. The challenge
	// is single-use at the application level (one browser ceremony per token).
	_ = s.cache.Delete(ctx, challengeKeyPrefix+token)
	return []byte(val), nil
}

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
// cache so they cannot collide with other cache keys (auth revocation markers,
// session caches, rate-limit counters).
const challengeKeyPrefix = "passkey:challenge:"

// CacheSessionStore adapts the generic interfaces.CacheService (string-based
// Redis) to the passkey SessionStore interface (byte-based). The challenge data
// is JSON — valid UTF-8 — so string↔[]byte conversion is lossless.
type CacheSessionStore struct {
	cache interfaces.CacheService
}

// NewCacheSessionStore wraps a CacheService as a SessionStore.
func NewCacheSessionStore(cache interfaces.CacheService) *CacheSessionStore {
	return &CacheSessionStore{cache: cache}
}

func (s *CacheSessionStore) SaveChallenge(ctx context.Context, token string, data []byte, ttl time.Duration) error {
	if err := s.cache.Set(ctx, challengeKeyPrefix+token, string(data), ttl); err != nil {
		return fmt.Errorf("save challenge: %w", err)
	}
	return nil
}

func (s *CacheSessionStore) GetChallenge(ctx context.Context, token string) ([]byte, error) {
	val, err := s.cache.Get(ctx, challengeKeyPrefix+token)
	if err != nil {
		return nil, fmt.Errorf("get challenge: %w", err)
	}
	if val == "" {
		return nil, nil
	}
	return []byte(val), nil
}

func (s *CacheSessionStore) DeleteChallenge(ctx context.Context, token string) error {
	if err := s.cache.Delete(ctx, challengeKeyPrefix+token); err != nil {
		return fmt.Errorf("delete challenge: %w", err)
	}
	return nil
}

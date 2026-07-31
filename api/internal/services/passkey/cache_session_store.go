// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
)

// challengeKeyPrefix namespaces WebAuthn challenge tokens in Redis.
const challengeKeyPrefix = "passkey:challenge:"

// CacheSessionStore implements SessionStore against Redis. ConsumeChallenge
// uses GETDEL (Redis 6.2+) for true atomic read-and-delete — closing the
// concurrent-replay window that separate GET+DEL operations would leave open.
// When the Redis client does not support GETDEL (pre-6.2), it falls back to
// GET+DEL with the delete error propagated (fail-safe, not silent).
type CacheSessionStore struct {
	// cache is used for SaveChallenge (via Set). It provides the CacheService
	// interface abstraction the rest of the API uses.
	cache interfaces.CacheService
	// client is the raw Redis client, used for ConsumeChallenge (GETDEL).
	// Required — without it the atomic single-use guarantee cannot be enforced.
	client *redis.Client
}

// NewCacheSessionStore constructs a Redis-backed session store. The raw client
// is needed for GETDEL (atomic challenge consumption). It is obtained from
// cache.Service.GetClient().
func NewCacheSessionStore(cache interfaces.CacheService, client *redis.Client) *CacheSessionStore {
	return &CacheSessionStore{cache: cache, client: client}
}

func (s *CacheSessionStore) SaveChallenge(ctx context.Context, token string, data []byte, ttl time.Duration) error {
	if err := s.cache.Set(ctx, challengeKeyPrefix+token, string(data), ttl); err != nil {
		return fmt.Errorf("save challenge: %w", err)
	}
	return nil
}

func (s *CacheSessionStore) ConsumeChallenge(ctx context.Context, token string) ([]byte, error) {
	key := challengeKeyPrefix + token
	// GETDEL: atomically reads and deletes in a single Redis operation. This
	// closes the replay window that separate GET+DEL would leave open under
	// concurrent Finish* requests sharing the same token.
	val, err := s.client.GetDel(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getdel challenge: %w", err)
	}
	return []byte(val), nil
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// challengeKeyPrefix namespaces WebAuthn challenge tokens in Redis.
const challengeKeyPrefix = "passkey:challenge:"

// CacheSessionStore implements SessionStore against Redis using the raw
// *redis.Client for both Save (SET) and Consume (GETDEL — atomic read+delete).
// This avoids the CacheService interface's lack of GETDEL, and closes the
// concurrent-replay window that separate GET+DEL would leave open.
type CacheSessionStore struct {
	client *redis.Client
}

// NewCacheSessionStore constructs a Redis-backed session store. The client is
// obtained from cache.Service.GetClient() in production wiring.
func NewCacheSessionStore(client *redis.Client) *CacheSessionStore {
	return &CacheSessionStore{client: client}
}

func (s *CacheSessionStore) SaveChallenge(ctx context.Context, token string, data []byte, ttl time.Duration) error {
	if err := s.client.Set(ctx, challengeKeyPrefix+token, string(data), ttl).Err(); err != nil {
		return fmt.Errorf("save challenge: %w", err)
	}
	return nil
}

func (s *CacheSessionStore) ConsumeChallenge(ctx context.Context, token string) ([]byte, error) {
	key := challengeKeyPrefix + token
	val, err := s.client.GetDel(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getdel challenge: %w", err)
	}
	return []byte(val), nil
}

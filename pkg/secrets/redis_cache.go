// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// DEKCachePrefix is the Redis key prefix for cached DEKs (dek:<session>).
// The rotate-kek and migrate-kek CLIs import it so their post-rotation cache
// flush deletes exactly these keys — never the rate-limit, session, or
// revocation keys sharing the Redis instance.
const DEKCachePrefix = "dek:"

// RedisDEKCache implements DEKCache using Redis.
// If a master key is provided, DEKs are encrypted before storage in Redis.
type RedisDEKCache struct {
	client    *redis.Client
	masterKey []byte // optional: if set, DEKs are wrapped before caching
}

// NewRedisDEKCache creates a new Redis-backed DEK cache.
// masterKey is optional — if nil or empty, DEKs are stored as plain hex.
func NewRedisDEKCache(client *redis.Client, masterKey ...[]byte) *RedisDEKCache {
	var mk []byte
	if len(masterKey) > 0 && len(masterKey[0]) == 32 {
		mk = masterKey[0]
	}
	return &RedisDEKCache{client: client, masterKey: mk}
}

func (c *RedisDEKCache) CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error {
	key := DEKCachePrefix + sessionID
	var val string
	if c.masterKey != nil {
		wrapped, err := EncryptSecret(c.masterKey, dek)
		if err != nil {
			return fmt.Errorf("wrap DEK for cache: %w", err)
		}
		val = hex.EncodeToString(wrapped)
	} else {
		val = hex.EncodeToString(dek)
	}
	return c.client.Set(ctx, key, val, ttl).Err()
}

func (c *RedisDEKCache) GetDEK(ctx context.Context, sessionID string) ([]byte, error) {
	key := DEKCachePrefix + sessionID
	val, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis get DEK: %w", err)
	}
	raw, err := hex.DecodeString(val)
	if err != nil {
		return nil, fmt.Errorf("hex decode DEK: %w", err)
	}
	if c.masterKey != nil {
		dek, err := DecryptSecret(c.masterKey, raw)
		if err != nil {
			return nil, fmt.Errorf("unwrap DEK from cache: %w", err)
		}
		return dek, nil
	}
	return raw, nil
}

func (c *RedisDEKCache) EvictDEK(ctx context.Context, sessionID string) error {
	key := DEKCachePrefix + sessionID
	return c.client.Del(ctx, key).Err()
}

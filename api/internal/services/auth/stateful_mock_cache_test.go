// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// statefulMockCache is a CacheService implementation backed by a
// thread-safe in-memory map. Unlike mockCache (which is uniformly
// no-op), it persists Set/SetObject writes so cache-dependent code paths
// — JWT revocation (token:<hash>, token:<jti>), user-sessions:<id>
// tracking, rate-limit counters — can be exercised end-to-end without a
// real Redis.
//
// It does NOT honor TTLs: entries live until Delete is called. That is
// sufficient for the e2e tests that use it, which run in-process and
// complete in seconds. Adding TTL expiry would require a goroutine and a
// heap of edge cases; not worth it for test infra.
type statefulMockCache struct {
	mu      sync.RWMutex
	strs    map[string]string
	objects map[string][]byte
	sess    map[string]types.CachedSession
}

func newStatefulMockCache() *statefulMockCache {
	return &statefulMockCache{
		strs:    make(map[string]string),
		objects: make(map[string][]byte),
		sess:    make(map[string]types.CachedSession),
	}
}

func (c *statefulMockCache) Get(_ context.Context, key string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.strs[key]; ok {
		return v, nil
	}
	return "", nil // cache miss == empty string, nil error (matches go-redis wrapper)
}

func (c *statefulMockCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strs[key] = value
	return nil
}

func (c *statefulMockCache) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.strs[key]; exists {
		return false, nil
	}
	c.strs[key] = value
	return true, nil
}

func (c *statefulMockCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.strs, key)
	delete(c.objects, key)
	return nil
}

func (c *statefulMockCache) DeleteByPrefix(_ context.Context, prefix string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.strs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.strs, k)
		}
	}
	for k := range c.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.objects, k)
		}
	}
	return nil
}

func (c *statefulMockCache) GetObject(_ context.Context, key string, out interface{}) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	raw, ok := c.objects[key]
	if !ok {
		return nil // miss
	}
	return json.Unmarshal(raw, out)
}

func (c *statefulMockCache) SetObject(_ context.Context, key string, value interface{}, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.objects[key] = raw
	return nil
}

func (c *statefulMockCache) GetSession(_ context.Context, sessionID string) (*types.CachedSession, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.sess[sessionID]; ok {
		cp := s
		return &cp, nil
	}
	return nil, nil
}

func (c *statefulMockCache) SetSession(_ context.Context, sessionID string, session types.CachedSession, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess[sessionID] = session
	return nil
}

func (c *statefulMockCache) DeleteSession(_ context.Context, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sess, sessionID)
	return nil
}

func (c *statefulMockCache) Ping(context.Context) error { return nil }
func (c *statefulMockCache) Start() error               { return nil }
func (c *statefulMockCache) Stop() error                { return nil }

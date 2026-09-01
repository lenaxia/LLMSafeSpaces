// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
)

const (
	keyPrefix           = "rl:"
	defaultBucketTTL    = 10 * time.Minute
	defaultCleanupEvery = time.Minute
)

type Service struct {
	logger          *logger.Logger
	backend         Backend
	localBuckets    map[string]*bucket
	mu              sync.Mutex
	bucketTTL       time.Duration
	cleanupInterval time.Duration
	lastCleanup     time.Time
}

type bucket struct {
	tokens     float64
	lastTime   time.Time
	lastAccess time.Time
}

var _ interfaces.RateLimiterService = (*Service)(nil)

type Option func(*Service)

func WithCleanup(ttl, interval time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.bucketTTL = ttl
		}
		if interval > 0 {
			s.cleanupInterval = interval
		}
	}
}

func NewWithBackend(log *logger.Logger, backend Backend, opts ...Option) *Service {
	s := &Service{
		logger:          log,
		backend:         backend,
		localBuckets:    make(map[string]*bucket),
		bucketTTL:       defaultBucketTTL,
		cleanupInterval: defaultCleanupEvery,
		lastCleanup:     time.Now(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func NewWithMemory(log *logger.Logger, opts ...Option) *Service {
	return NewWithBackend(log, NewMemoryBackend(), opts...)
}

func NewWithRedisClient(log *logger.Logger, client *redis.Client, opts ...Option) *Service {
	return NewWithBackend(log, NewRedisBackend(client), opts...)
}

func (s *Service) Start() error { return nil }

func (s *Service) Stop() error {
	if s == nil {
		return nil
	}
	if s.backend != nil {
		return s.backend.Close()
	}
	return nil
}

func (s *Service) Increment(ctx context.Context, key string, value int64, expiration time.Duration) (int64, error) {
	return s.backend.IncrExpire(ctx, keyPrefix+key, value, expiration)
}

func (s *Service) AddToWindow(ctx context.Context, key string, timestamp int64, member string, expiration time.Duration) error {
	return s.backend.WindowAdd(ctx, keyPrefix+key, timestamp, member, expiration)
}

func (s *Service) RemoveFromWindow(ctx context.Context, key string, cutoff int64) error {
	return s.backend.WindowRemove(ctx, keyPrefix+key, cutoff)
}

func (s *Service) CountInWindow(ctx context.Context, key string, min, max int64) (int, error) {
	return s.backend.WindowCount(ctx, keyPrefix+key, min, max)
}

func (s *Service) GetWindowEntries(ctx context.Context, key string, start, stop int) ([]string, error) {
	return s.backend.WindowMembers(ctx, keyPrefix+key, int64(start), int64(stop))
}

func (s *Service) GetTTL(ctx context.Context, key string) (time.Duration, error) {
	return s.backend.TTL(ctx, keyPrefix+key)
}

func (s *Service) Allow(key string, rate float64, burst int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.maybeCleanup(now)

	b, exists := s.localBuckets[key]
	if !exists {
		b = &bucket{tokens: float64(burst), lastTime: now}
		s.localBuckets[key] = b
	}
	b.lastAccess = now

	elapsed := now.Sub(b.lastTime).Seconds()
	b.tokens += elapsed * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.lastTime = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (s *Service) maybeCleanup(now time.Time) {
	if now.Sub(s.lastCleanup) < s.cleanupInterval {
		return
	}
	s.lastCleanup = now
	for k, b := range s.localBuckets {
		if now.Sub(b.lastAccess) > s.bucketTTL {
			delete(s.localBuckets, k)
		}
	}
}

func (s *Service) LocalBucketCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.localBuckets)
}

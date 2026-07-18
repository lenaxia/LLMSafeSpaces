// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Service handles Redis cache operations
type Service struct {
	logger *logger.Logger
	config *config.Config
	client *redis.Client
}

// Ensure Service implements the CacheService interface
var _ interfaces.CacheService = (*Service)(nil) // Compile-time interface check

// New creates a new cache service
func New(cfg *config.Config, log *logger.Logger) (*Service, error) {
	// Create Redis client. When cfg.Redis.TLS is set (#465), the client
	// uses TLS-in-transit — required for AWS ElastiCache
	// (TransitEncryptionEnabled), GCP Memorystore with TLS, and any
	// self-hosted Redis with TLS. InsecureSkipVerify is exposed for the
	// self-signed-cert dev case; production should leave it false and
	// use a CA-signed cert.
	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		PoolSize: cfg.Redis.PoolSize,
	}
	if cfg.Redis.TLS {
		opts.TLSConfig = &tls.Config{
			ServerName: cfg.Redis.Host,
			//nolint:gosec // G402: operator-controlled escape hatch for self-signed dev certs (cfg.Redis.InsecureSkipVerify). Production should leave this false and use a CA-signed cert — see worklog 0639 and helm/values.yaml redis.insecureSkipVerify docs.
			InsecureSkipVerify: cfg.Redis.InsecureSkipVerify,
			MinVersion:         tls.VersionTLS12, // Go crypto/tls default since 1.18; explicit for clarity
		}
	}
	client := redis.NewClient(opts)

	// Attach the metrics hook so every command emits the
	// llmsafespaces_redis_command_duration_seconds histogram and the
	// llmsafespaces_redis_errors_total counter. Reused by callers that
	// share this client via GetClient() (rate limiter, message queue,
	// wsstate store).
	client.AddHook(newMetricsHook())

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Service{
		logger: log,
		config: cfg,
		client: client,
	}, nil
}

// Start starts the cache service
func (s *Service) Start() error {
	s.logger.Info("Cache service started")
	return nil
}

// Stop stops the cache service
func (s *Service) Stop() error {
	s.logger.Info("Stopping cache service")
	return s.client.Close()
}

// Ping checks the Redis connection
func (s *Service) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// GetClient returns the underlying Redis client so other services can reuse
// the same connection pool instead of opening a duplicate client to the same
// Redis instance (e.g. the rate limiter). The client remains owned by the
// cache service and is closed when the cache service stops; callers must not
// close it themselves.
func (s *Service) GetClient() *redis.Client {
	return s.client
}

// Get gets a value from the cache
func (s *Service) Get(ctx context.Context, key string) (string, error) {
	val, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("failed to get value from cache: %w", err)
	}
	return val, nil
}

// Set sets a value in the cache
func (s *Service) Set(ctx context.Context, key string, value string, expiration time.Duration) error {
	err := s.client.Set(ctx, key, value, expiration).Err()
	if err != nil {
		return fmt.Errorf("failed to set value in cache: %w", err)
	}
	return nil
}

// SetNX atomically sets a key only if it does not already exist.
// Returns true if the key was set, false if it already existed.
func (s *Service) SetNX(ctx context.Context, key string, value string, expiration time.Duration) (bool, error) {
	ok, err := s.client.SetNX(ctx, key, value, expiration).Result()
	if err != nil {
		return false, fmt.Errorf("failed to setnx in cache: %w", err)
	}
	return ok, nil
}

// Delete deletes a value from the cache
func (s *Service) Delete(ctx context.Context, key string) error {
	err := s.client.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("failed to delete value from cache: %w", err)
	}
	return nil
}

// DeleteByPrefix deletes all keys matching the given prefix using SCAN + DEL.
// Uses UNLINK for non-blocking deletion when available (Redis 4.0+).
func (s *Service) DeleteByPrefix(ctx context.Context, prefix string) error {
	var cursor uint64
	for {
		var keys []string
		var err error
		keys, cursor, err = s.client.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return fmt.Errorf("failed to scan cache keys with prefix %s: %w", prefix, err)
		}
		if len(keys) > 0 {
			if err := s.client.Unlink(ctx, keys...).Err(); err != nil {
				_ = s.client.Del(ctx, keys...).Err()
			}
		}
		if cursor == 0 {
			break
		}
	}
	return nil
}

// GetObject gets an object from the cache and unmarshals it into the provided value
func (s *Service) GetObject(ctx context.Context, key string, value interface{}) error {
	data, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to get object from cache: %w", err)
	}

	err = json.Unmarshal(data, value)
	if err != nil {
		return fmt.Errorf("failed to unmarshal object from cache: %w", err)
	}

	return nil
}

// SetObject sets an object in the cache
func (s *Service) SetObject(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal object for cache: %w", err)
	}

	err = s.client.Set(ctx, key, data, expiration).Err()
	if err != nil {
		return fmt.Errorf("failed to set object in cache: %w", err)
	}

	return nil
}

// GetSession retrieves a typed session from the cache.
// Returns nil, nil when the key does not exist.
func (s *Service) GetSession(ctx context.Context, sessionID string) (*types.CachedSession, error) {
	data, err := s.client.Get(ctx, fmt.Sprintf("session:%s", sessionID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session from cache: %w", err)
	}
	var session types.CachedSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}
	return &session, nil
}

// SetSession stores a typed session in the cache.
func (s *Service) SetSession(ctx context.Context, sessionID string, session types.CachedSession, expiration time.Duration) error {
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}
	if err := s.client.Set(ctx, fmt.Sprintf("session:%s", sessionID), data, expiration).Err(); err != nil {
		return fmt.Errorf("failed to set session in cache: %w", err)
	}
	return nil
}

// DeleteSession deletes a session from the cache
func (s *Service) DeleteSession(ctx context.Context, sessionID string) error {
	err := s.Delete(ctx, fmt.Sprintf("session:%s", sessionID))
	if err != nil {
		return fmt.Errorf("failed to delete session from cache: %w", err)
	}
	return nil
}

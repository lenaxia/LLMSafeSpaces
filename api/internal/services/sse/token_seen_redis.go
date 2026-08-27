// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DefaultTokenSeenTTL bounds how long per-session dedup state survives
// without activity. It must exceed any realistic billing-emitting
// session lifetime including suspend/resume gaps: opencode reports
// cumulative tokens, so an expired entry re-bills the session's input on
// the next event. 30 days covers month-long workspaces.
const DefaultTokenSeenTTL = 30 * 24 * time.Hour

var metricTokenSeenStoreErrors = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "llmsafespaces_sse_token_seen_store_errors_total",
		Help: "Token-seen store operation failures (#759 dedup persistence)",
	},
	[]string{"op"},
)

// RedisTokenSeenStore implements TokenSeenStore on Redis, shared by all
// API replicas so any replica (or a restarted one) resumes a session's
// dedup state instead of re-billing cumulative input (#759).
type RedisTokenSeenStore struct {
	client *redis.Client
	ttl    time.Duration
}

func NewRedisTokenSeenStore(client *redis.Client, ttl time.Duration) *RedisTokenSeenStore {
	if ttl <= 0 {
		ttl = DefaultTokenSeenTTL
	}
	return &RedisTokenSeenStore{client: client, ttl: ttl}
}

func tokenSeenKey(workspaceID, sessionID string) string {
	return fmt.Sprintf("metering:tseen:%s:%s", workspaceID, sessionID)
}

type tokenSeenValue struct {
	Output int64   `json:"output"`
	Cost   float64 `json:"cost"`
}

func (s *RedisTokenSeenStore) GetSessionUsage(ctx context.Context, workspaceID, sessionID string) (int64, float64, bool, error) {
	raw, err := s.client.Get(ctx, tokenSeenKey(workspaceID, sessionID)).Bytes()
	if err == redis.Nil {
		return 0, 0, false, nil
	}
	if err != nil {
		metricTokenSeenStoreErrors.WithLabelValues("get").Inc()
		return 0, 0, false, fmt.Errorf("token-seen get %s/%s: %w", workspaceID, sessionID, err)
	}
	var v tokenSeenValue
	if err := json.Unmarshal(raw, &v); err != nil {
		// Corrupt entry: treat as absent rather than failing the event.
		return 0, 0, false, nil
	}
	return v.Output, v.Cost, true, nil
}

func (s *RedisTokenSeenStore) SetSessionUsage(ctx context.Context, workspaceID, sessionID string, output int64, cost float64) error {
	raw, err := json.Marshal(tokenSeenValue{Output: output, Cost: cost})
	if err != nil {
		metricTokenSeenStoreErrors.WithLabelValues("marshal").Inc()
		return fmt.Errorf("token-seen marshal %s/%s: %w", workspaceID, sessionID, err)
	}
	if err := s.client.Set(ctx, tokenSeenKey(workspaceID, sessionID), raw, s.ttl).Err(); err != nil {
		metricTokenSeenStoreErrors.WithLabelValues("set").Inc()
		return fmt.Errorf("token-seen set %s/%s: %w", workspaceID, sessionID, err)
	}
	return nil
}

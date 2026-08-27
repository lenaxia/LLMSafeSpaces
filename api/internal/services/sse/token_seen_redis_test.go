// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTokenSeenRedis(t *testing.T) (*RedisTokenSeenStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisTokenSeenStore(client, time.Hour), mr
}

func TestRedisTokenSeenStore_RoundTrip(t *testing.T) {
	store, _ := newTokenSeenRedis(t)
	ctx := context.Background()

	_, _, ok, err := store.GetSessionUsage(ctx, "ws-1", "ses-1")
	require.NoError(t, err)
	assert.False(t, ok, "absent entry must report not-found, not error")

	require.NoError(t, store.SetSessionUsage(ctx, "ws-1", "ses-1", 900, 1.25))

	out, cost, ok, err := store.GetSessionUsage(ctx, "ws-1", "ses-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(900), out)
	assert.InDelta(t, 1.25, cost, 1e-9)
}

func TestRedisTokenSeenStore_TTLSet(t *testing.T) {
	store, mr := newTokenSeenRedis(t)
	require.NoError(t, store.SetSessionUsage(context.Background(), "ws-1", "ses-1", 5, 0.1))
	assert.True(t, mr.Exists(tokenSeenKey("ws-1", "ses-1")))
	mr.FastForward(2 * time.Hour)
	assert.False(t, mr.Exists(tokenSeenKey("ws-1", "ses-1")),
		"entry must expire with the configured TTL")
}

func TestRedisTokenSeenStore_CorruptEntry_TreatedAsAbsent(t *testing.T) {
	store, mr := newTokenSeenRedis(t)
	require.NoError(t, mr.Set(tokenSeenKey("ws-1", "ses-1"), "not-json"))

	_, _, ok, err := store.GetSessionUsage(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err, "corrupt entry must not fail the event path")
	assert.False(t, ok)
}

func TestRedisTokenSeenStore_RedisDown_ReturnsError(t *testing.T) {
	store, mr := newTokenSeenRedis(t)
	mr.Close() // simulate outage

	_, _, _, err := store.GetSessionUsage(context.Background(), "ws-1", "ses-1")
	require.Error(t, err)
	assert.Error(t, store.SetSessionUsage(context.Background(), "ws-1", "ses-1", 5, 0.1))
}

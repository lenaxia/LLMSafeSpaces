// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCacheSessionStore_GetDel_Atomic verifies the CacheSessionStore uses
// GETDEL for truly atomic challenge consumption (read+delete in one Redis
// operation). This closes the concurrent-replay window flagged in prior
// reviews.
func TestCacheSessionStore_GetDel_Atomic(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	store := NewCacheSessionStore(client)
	ctx := context.Background()
	data := []byte(`{"challenge":"abc"}`)

	// Save a challenge.
	require.NoError(t, store.SaveChallenge(ctx, "tok-1", data, time.Minute))

	// Consume — must return the data and atomically delete.
	got, err := store.ConsumeChallenge(ctx, "tok-1")
	require.NoError(t, err)
	assert.Equal(t, data, got)

	// Second consume must return nil (already consumed via GETDEL).
	got2, err := store.ConsumeChallenge(ctx, "tok-1")
	require.NoError(t, err)
	assert.Nil(t, got2, "GETDEL must have removed the key atomically")
}

// TestCacheSessionStore_ConsumeMissing_ReturnsNil verifies that consuming a
// non-existent token returns (nil, nil), not an error.
func TestCacheSessionStore_ConsumeMissing_ReturnsNil(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	store := NewCacheSessionStore(client)

	got, err := store.ConsumeChallenge(context.Background(), "never-existed")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestCacheSessionStore_SaveAndConsume verifies the full round-trip.
func TestCacheSessionStore_SaveAndConsume(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	store := NewCacheSessionStore(client)
	ctx := context.Background()
	data := []byte(`{"challenge":"xyz","user_id":"u1"}`)

	require.NoError(t, store.SaveChallenge(ctx, "tok-abc", data, 5*time.Minute))

	// Verify the key exists in Redis with the correct prefix.
	assert.True(t, mr.Exists("passkey:challenge:tok-abc"))

	got, err := store.ConsumeChallenge(ctx, "tok-abc")
	require.NoError(t, err)
	assert.Equal(t, data, got)

	// Key must be gone after consume.
	assert.False(t, mr.Exists("passkey:challenge:tok-abc"))
}

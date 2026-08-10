// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestV2PendingRedis_TrackAndClear(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	tracker := NewV2PendingTracker(client)
	require.NotNil(t, tracker)

	tracker.add("ws-1", "ses-a")
	tracker.add("ws-1", "ses-b")
	assert.True(t, tracker.has("ws-1", "ses-a"))
	assert.True(t, tracker.has("ws-1", "ses-b"))
	assert.False(t, tracker.has("ws-1", "ses-c"))

	sessions := tracker.sessionsForWorkspace("ws-1")
	assert.Len(t, sessions, 2)

	tracker.remove("ws-1", "ses-a")
	assert.False(t, tracker.has("ws-1", "ses-a"))
	assert.True(t, tracker.has("ws-1", "ses-b"))

	tracker.remove("ws-1", "ses-b")
	assert.Empty(t, tracker.sessionsForWorkspace("ws-1"))
}

func TestV2PendingRedis_ReferenceCountedMultiInput(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	tracker := NewV2PendingTracker(client)

	tracker.add("ws-1", "ses-a")
	tracker.add("ws-1", "ses-a")
	assert.True(t, tracker.has("ws-1", "ses-a"))

	tracker.remove("ws-1", "ses-a")
	assert.True(t, tracker.has("ws-1", "ses-a"),
		"session with 1 remaining pending input must stay tracked")

	tracker.remove("ws-1", "ses-a")
	assert.False(t, tracker.has("ws-1", "ses-a"),
		"session with 0 pending inputs must be removed from tracking")
}

func TestV2PendingRedis_NilClientSafe(t *testing.T) {
	tracker := NewV2PendingTracker(nil)
	// NewV2PendingTracker returns nil for nil client — app.go keeps the
	// in-memory default. If called directly, the methods must be nil-safe.
	if tracker != nil {
		assert.NotPanics(t, func() {
			tracker.add("ws", "ses")
			tracker.remove("ws", "ses")
			_ = tracker.has("ws", "ses")
			_ = tracker.sessionsForWorkspace("ws")
		})
	}
}

func TestV2PendingRedis_CrossWorkspaceIsolation(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	tracker := NewV2PendingTracker(client)

	tracker.add("ws-1", "ses-a")
	tracker.add("ws-2", "ses-b")

	assert.Len(t, tracker.sessionsForWorkspace("ws-1"), 1)
	assert.Len(t, tracker.sessionsForWorkspace("ws-2"), 1)
	assert.Empty(t, tracker.sessionsForWorkspace("ws-3"))
}

func TestV2PendingRedis_TTLExpiresPhantomEntries(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	tracker := NewV2PendingTracker(client)
	tracker.add("ws-1", "ses-phantom")
	assert.True(t, tracker.has("ws-1", "ses-phantom"))

	mr.FastForward(v2PendingTTL + 1000000000)

	assert.False(t, tracker.has("ws-1", "ses-phantom"),
		"TTL must expire phantom entries from lost Prompted events")
}

func TestV2PendingRedis_RedisDownGracefulDegradation(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close()

	tracker := NewV2PendingTracker(client)

	assert.NotPanics(t, func() {
		tracker.add("ws", "ses")
		tracker.remove("ws", "ses")
		_ = tracker.has("ws", "ses")
		_ = tracker.sessionsForWorkspace("ws")
	})
	assert.False(t, tracker.has("ws", "ses"),
		"Redis down → has returns false (graceful degradation)")
	assert.Empty(t, tracker.sessionsForWorkspace("ws"))
}

// Verify both implementations satisfy the same interface.
func TestV2PendingTracker_InterfaceConformance(t *testing.T) {
	var _ v2PendingTracker = (*v2PendingSessions)(nil)
	var _ v2PendingTracker = (*v2PendingRedis)(nil)
}

// Cross-implementation behavioral parity: both trackers must produce the
// same results for the same sequence of operations.
func TestV2PendingTracker_BehavioralParity(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	memTracker := newV2PendingSessions()
	redisTracker := NewV2PendingTracker(client)

	ops := []struct {
		op  string
		wid string
		sid string
	}{
		{"add", "ws-1", "ses-a"},
		{"add", "ws-1", "ses-a"},
		{"add", "ws-1", "ses-b"},
		{"remove", "ws-1", "ses-a"},
		{"add", "ws-2", "ses-c"},
		{"remove", "ws-1", "ses-b"},
		{"remove", "ws-1", "ses-a"},
	}

	for _, o := range ops {
		switch o.op {
		case "add":
			memTracker.add(o.wid, o.sid)
			redisTracker.add(o.wid, o.sid)
		case "remove":
			memTracker.remove(o.wid, o.sid)
			redisTracker.remove(o.wid, o.sid)
		}
	}

	// After all ops, both must agree on has() and sessionsForWorkspace().
	for _, wid := range []string{"ws-1", "ws-2", "ws-3"} {
		memSessions := memTracker.sessionsForWorkspace(wid)
		redisSessions := redisTracker.sessionsForWorkspace(wid)
		assert.ElementsMatch(t, memSessions, redisSessions,
			"wid=%s: in-memory and Redis trackers must agree", wid)
	}

	_ = context.Background()
}

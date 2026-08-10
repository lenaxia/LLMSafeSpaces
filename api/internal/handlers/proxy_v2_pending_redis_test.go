// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// NewV2PendingTracker(nil) returns nil interface — app.go keeps the
	// in-memory default via SetV2PendingTracker's nil guard.
	assert.Nil(t, tracker, "NewV2PendingTracker(nil) must return nil")
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

// Integration test: wire the Redis-backed tracker through the real handler
// and verify enqueueV2 → v2Pending.add → sessionsForWorkspace returns the
// session, then bridgeV2Prompted → v2Pending.remove → session cleared.
// This is the regression test for the multi-replica divergence (the in-memory
// tracker would not be shared across replicas; the Redis tracker is).
func TestV2PendingRedis_HandlerIntegration(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, handler := newV2TestHandler(t, srv)
	handler.SetV2PendingTracker(NewV2PendingTracker(client))

	// Enqueue a message via the handler (goes through gin → enqueueV2 →
	// v2Pending.add).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello"}`))
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)

	// The Redis tracker must now show the session as pending.
	assert.True(t, handler.v2Pending.has("ws-1", "ses-1"),
		"Redis tracker must show session as pending after enqueueV2")

	// Simulate the V2 Prompted event (fires when opencode promotes/drains).
	handler.onRawEvent("ws-1", "session.next.prompted",
		`{"id":"e1","type":"session.next.prompted","properties":{"messageID":"msg_v2_1","sessionID":"ses-1","delivery":"queue"}}`)

	// The Redis tracker must now show the session as drained.
	assert.False(t, handler.v2Pending.has("ws-1", "ses-1"),
		"Redis tracker must clear session after Prompted event")
}

// Regression test for the TOCTOU race: concurrent add + remove must not
// lose tracking. With the old HDel approach, a concurrent add between
// HINCRBY -1 and HDel would be clobbered. The no-HDel fix ensures the
// count is always consistent.
func TestV2PendingRedis_ConcurrentAddRemove(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	tracker := NewV2PendingTracker(client)

	// Add 100 times and remove 99 times concurrently. Final count must
	// be 1 (100 adds - 99 removes = 1 pending). If the race existed,
	// some adds could be clobbered by HDel, leaving count < 1.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			tracker.add("ws-race", "ses-race")
		}
		close(done)
	}()
	for i := 0; i < 99; i++ {
		tracker.remove("ws-race", "ses-race")
	}
	<-done

	// The count should be 1 (100 - 99). miniredis is single-threaded so
	// operations serialize, but the test validates the logic: no HDel
	// means no lost increments.
	assert.True(t, tracker.has("ws-race", "ses-race"),
		"after 100 adds + 99 removes, session must still be tracked (count=1)")
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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestV2QueueShadow_AddListRemove(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	shadow := NewV2QueueShadow(client)
	require.NotNil(t, shadow)

	ctx := context.Background()

	// Add two messages.
	shadow.Add(ctx, "ws-1", "ses-1", "msg_a", "hello")
	shadow.Add(ctx, "ws-1", "ses-1", "msg_b", "world")

	// List: should have both.
	entries := shadow.List(ctx, "ws-1", "ses-1")
	assert.Len(t, entries, 2)

	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.ID] = true
	}
	assert.True(t, ids["msg_a"])
	assert.True(t, ids["msg_b"])

	// Verify text is stored.
	for _, e := range entries {
		if e.ID == "msg_a" {
			assert.Equal(t, "hello", e.Text)
		}
	}

	// Remove one.
	shadow.Remove(ctx, "ws-1", "ses-1", "msg_a")
	entries = shadow.List(ctx, "ws-1", "ses-1")
	assert.Len(t, entries, 1)
	assert.Equal(t, "msg_b", entries[0].ID)

	// ClearAll.
	shadow.ClearAll(ctx, "ws-1", "ses-1")
	entries = shadow.List(ctx, "ws-1", "ses-1")
	assert.Empty(t, entries)
}

func TestV2QueueShadow_NilClientSafe(t *testing.T) {
	// NewV2QueueShadow(nil) returns nil. All methods must be nil-safe.
	shadow := NewV2QueueShadow(nil)
	assert.Nil(t, shadow)

	// Calling methods on nil should not panic.
	assert.NotPanics(t, func() {
		shadow.Add(context.Background(), "ws", "ses", "msg", "text")
		shadow.Remove(context.Background(), "ws", "ses", "msg")
		_ = shadow.List(context.Background(), "ws", "ses")
		shadow.ClearAll(context.Background(), "ws", "ses")
	})
}

func TestV2QueueShadow_CrossSessionIsolation(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	shadow := NewV2QueueShadow(client)
	ctx := context.Background()

	shadow.Add(ctx, "ws-1", "ses-a", "msg1", "a1")
	shadow.Add(ctx, "ws-1", "ses-b", "msg2", "b1")

	assert.Len(t, shadow.List(ctx, "ws-1", "ses-a"), 1)
	assert.Len(t, shadow.List(ctx, "ws-1", "ses-b"), 1)
	assert.Empty(t, shadow.List(ctx, "ws-1", "ses-c"))
}

func TestV2QueueShadow_LostPromptedSelfHealsViaTTL(t *testing.T) {
	// If the Prompted event is lost (SSE disconnect, replica crash), the
	// phantom pill must not persist forever. The TTL on the Redis key
	// ensures it expires. This test verifies the TTL is set.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	shadow := NewV2QueueShadow(client)
	ctx := context.Background()

	shadow.Add(ctx, "ws-1", "ses-1", "msg_phantom", "ghost")
	assert.Len(t, shadow.List(ctx, "ws-1", "ses-1"), 1)

	// Fast-forward miniredis past the TTL.
	mr.FastForward(v2ShadowTTL + time.Second)

	assert.Empty(t, shadow.List(ctx, "ws-1", "ses-1"),
		"TTL must expire phantom pills from lost Prompted events")
}

func TestV2QueueShadow_MalformedHashDataIgnored(t *testing.T) {
	// Corrupted hash data (not valid v2ShadowEntry JSON) must be silently
	// skipped by List — not cause a panic or error.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	shadow := NewV2QueueShadow(client)
	ctx := context.Background()

	// Seed a valid entry + a corrupted one directly via Redis.
	shadow.Add(ctx, "ws-1", "ses-1", "msg_good", "hello")
	_ = client.HSet(ctx, shadowKey("ws-1", "ses-1"), "msg_bad", "not-valid-json{").Err()

	entries := shadow.List(ctx, "ws-1", "ses-1")
	assert.Len(t, entries, 1, "malformed entry must be silently skipped")
	assert.Equal(t, "msg_good", entries[0].ID)
}

func TestV2QueueShadow_RedisDownGracefulDegradation(t *testing.T) {
	// If Redis is unreachable, all methods must return zero-values, not panic.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // kill Redis

	shadow := NewV2QueueShadow(client)
	ctx := context.Background()

	assert.NotPanics(t, func() {
		shadow.Add(ctx, "ws", "ses", "msg", "text")
		shadow.Remove(ctx, "ws", "ses", "msg")
		_ = shadow.List(ctx, "ws", "ses")
		shadow.ClearAll(ctx, "ws", "ses")
	})
	assert.Empty(t, shadow.List(ctx, "ws", "ses"),
		"Redis down → List returns empty (graceful degradation)")
}

func TestDeleteQueueMessageV2_RemovesFromShadow(t *testing.T) {
	// US-63.10: DeleteQueueMessage under V2 must clear the shadow so
	// dismissed messages don't reappear on fresh load.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	gin.SetMode(gin.TestMode)
	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "127.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetV2SessionQueueEnabled(true)
	handler.SetV2QueueShadow(NewV2QueueShadow(client))

	handler.v2Shadow.Add(context.Background(), "ws-1", "ses-1", "msg_del", "bye")

	router := gin.New()
	router.DELETE("/:id/sessions/:sessionId/queue/:messageId", handler.DeleteQueueMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/ws-1/sessions/ses-1/queue/msg_del", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, handler.v2Shadow.List(context.Background(), "ws-1", "ses-1"),
		"dismissed message must be removed from shadow")
}

func TestListQueueV2_ShadowReturnsPills(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	gin.SetMode(gin.TestMode)
	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "127.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetV2SessionQueueEnabled(true)
	handler.SetV2QueueShadow(NewV2QueueShadow(client))

	// Seed two messages in the shadow.
	handler.v2Shadow.Add(context.Background(), "ws-1", "ses-1", "msg_x", "hello")
	handler.v2Shadow.Add(context.Background(), "ws-1", "ses-1", "msg_y", "world")

	// GET /queue via gin router with ListQueue registered.
	router := gin.New()
	router.GET("/:id/sessions/:sessionId/queue", handler.ListQueue)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ws-1/sessions/ses-1/queue", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Messages, 2, "fresh-load must show both queued pills")

	// Verify message IDs are present.
	ids := map[string]bool{}
	for _, m := range resp.Messages {
		ids[m["id"].(string)] = true
	}
	assert.True(t, ids["msg_x"])
	assert.True(t, ids["msg_y"])
}

func TestListQueueV2_ShadowClearedOnPrompted(t *testing.T) {
	// When the Prompted event fires, the message is removed from the shadow.
	// A subsequent GET /queue must not show it.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	_, handler := newV2TestHandler(t, srv)
	handler.SetV2QueueShadow(NewV2QueueShadow(client))

	// Register ListQueue route for GET requests.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/:id/sessions/:sessionId/queue", handler.ListQueue)

	// Seed a message in the shadow via the SSE bridge (PromptAdmitted).
	handler.onRawEvent("ws-1", "session.next.prompt.admitted",
		`{"id":"e1","type":"session.next.prompt.admitted","properties":{"messageID":"msg_z","sessionID":"ses-1","delivery":"queue","prompt":{"text":"hi"}}}`)

	// Verify it's visible via ListQueue.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ws-1/sessions/ses-1/queue", nil)
	router.ServeHTTP(w, req)
	var resp struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Messages, 1)

	// Fire Prompted — message is promoted/drained.
	handler.onRawEvent("ws-1", "session.next.prompted",
		`{"id":"e2","type":"session.next.prompted","properties":{"messageID":"msg_z","sessionID":"ses-1","delivery":"queue"}}`)

	// Give the async Remove a moment.
	time.Sleep(100 * time.Millisecond)

	// ListQueue must now return empty.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/ws-1/sessions/ses-1/queue", nil)
	router.ServeHTTP(w2, req2)
	var resp2 struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp2))
	assert.Empty(t, resp2.Messages, "Prompted must clear the pill from the shadow")
}

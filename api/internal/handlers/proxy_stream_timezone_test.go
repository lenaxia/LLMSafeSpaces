package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tzGinContext(t *testing.T, userID string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/session-events", nil)
	if userID != "" {
		c.Set("userID", userID)
	}
	return c
}

type fakeTimezonePusher struct {
	got chan struct {
		userID, workspaceID, tz string
	}
}

func newFakeTimezonePusher() *fakeTimezonePusher {
	return &fakeTimezonePusher{got: make(chan struct {
		userID, workspaceID, tz string
	}, 4)}
}

func (f *fakeTimezonePusher) PushUserTimezone(_ context.Context, userID, workspaceID, tz string) error {
	f.got <- struct {
		userID, workspaceID, tz string
	}{userID, workspaceID, tz}
	return nil
}

type fakeTimezoneReader struct{ tz string }

func (f fakeTimezoneReader) GetString(_ context.Context, _, _ string) (string, error) {
	return f.tz, nil
}

// The SSE-connect path pushes the user's stored zone to the pod's
// agentd, async, without gating the stream.
func TestPushUserTimezoneOnConnect(t *testing.T) {
	pusher := newFakeTimezonePusher()
	h := &ProxyHandler{
		timezonePusher: pusher,
		userTimezones:  fakeTimezoneReader{tz: "Europe/Berlin"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := tzGinContext(t, "user-9")
	c.Request = c.Request.WithContext(ctx)
	h.pushUserTimezoneOnConnect(c, "ws-1")

	select {
	case got := <-pusher.got:
		assert.Equal(t, "user-9", got.userID)
		assert.Equal(t, "ws-1", got.workspaceID)
		assert.Equal(t, "Europe/Berlin", got.tz)
	case <-time.After(2 * time.Second):
		t.Fatal("push never fired")
	}
}

// No stored zone → no push (nothing to deliver).
func TestPushUserTimezoneOnConnect_EmptyZone(t *testing.T) {
	pusher := newFakeTimezonePusher()
	h := &ProxyHandler{
		timezonePusher: pusher,
		userTimezones:  fakeTimezoneReader{tz: ""},
	}
	h.pushUserTimezoneOnConnect(tzGinContext(t, "user-9"), "ws-1")
	select {
	case <-pusher.got:
		t.Fatal("no push without a stored zone")
	case <-time.After(300 * time.Millisecond):
	}
}

// Unwired (nil seams — e.g. unit-test ProxyHandlers) → no-op, no panic.
func TestPushUserTimezoneOnConnect_Unwired(t *testing.T) {
	h := &ProxyHandler{}
	require.NotPanics(t, func() {
		h.pushUserTimezoneOnConnect(tzGinContext(t, "u"), "ws-1")
	})
}

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	llmv1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
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

// Call-site pin (the TestStreamEvents_ArmsUsageGateOnOpen precedent):
// opening the workspace stream must fire the timezone push — deleting
// the StreamEvents call site fails this test.
func TestStreamEvents_FiresTimezonePushOnOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	pusher := newFakeTimezonePusher()
	env.handler.SetTimezonePush(pusher, fakeTimezoneReader{tz: "America/New_York"})

	// The push reads the authenticated userID from the gin context; the
	// bare test router has no auth middleware. Build the router with the
	// middleware BEFORE route registration (gin only applies Use to
	// routes registered after it).
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "u-1"); c.Next() })
	r.GET("/api/v1/workspaces/:id/events", env.handler.StreamEvents)

	cancel, body, _, _ := doStreamingRequest(r, "/api/v1/workspaces/ws-1/events")
	defer body.Close()

	select {
	case got := <-pusher.got:
		assert.Equal(t, "u-1", got.userID, "the push carries the authenticated user")
		assert.Equal(t, "ws-1", got.workspaceID)
		assert.Equal(t, "America/New_York", got.tz)
	case <-time.After(3 * time.Second):
		t.Fatal("stream open never fired the timezone push")
	}
	cancel()
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- US-23.4: copyResponseHeaders strips dangerous headers ---

func TestCopyResponseHeaders_StripsWWWAuthenticate(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("WWW-Authenticate", "Basic realm=\"opencode\"")
	src.Set("Content-Length", "42")

	dst := http.Header{}
	copyResponseHeaders(src, dst)

	assert.Equal(t, "application/json", dst.Get("Content-Type"))
	assert.Equal(t, "42", dst.Get("Content-Length"))
	assert.Empty(t, dst.Get("WWW-Authenticate"), "WWW-Authenticate must be stripped")
}

func TestCopyResponseHeaders_StripsProxyAuthenticate(t *testing.T) {
	src := http.Header{}
	src.Set("Proxy-Authenticate", "Basic")
	src.Set("Content-Type", "text/plain")

	dst := http.Header{}
	copyResponseHeaders(src, dst)

	assert.Empty(t, dst.Get("Proxy-Authenticate"))
	assert.Equal(t, "text/plain", dst.Get("Content-Type"))
}

func TestCopyResponseHeaders_StripsSetCookie(t *testing.T) {
	src := http.Header{}
	src.Set("Set-Cookie", "session=abc123")
	src.Set("ETag", "\"abc\"")

	dst := http.Header{}
	copyResponseHeaders(src, dst)

	assert.Empty(t, dst.Get("Set-Cookie"))
	assert.Equal(t, "\"abc\"", dst.Get("ETag"))
}

func TestCopyResponseHeaders_PreservesSafeHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Cache-Control", "no-cache")
	src.Set("X-Custom-Header", "value")
	src.Set("X-Accel-Buffering", "no")

	dst := http.Header{}
	copyResponseHeaders(src, dst)

	assert.Equal(t, "application/json", dst.Get("Content-Type"))
	assert.Equal(t, "no-cache", dst.Get("Cache-Control"))
	assert.Equal(t, "value", dst.Get("X-Custom-Header"))
	assert.Equal(t, "no", dst.Get("X-Accel-Buffering"))
}

// --- US-23.4: Upstream 401 → 502 conversion ---

func TestProxy_Upstream401_Returns502(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"opencode\"")
		w.WriteHeader(http.StatusUnauthorized)
	})
	env.setupWorkspacePodWithT(t, "ws-auth-fail", "10.0.0.1", "Active", "")
	env.setupPasswordWithT(t, "ws-auth-fail", "stale-password")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-auth-fail/sessions", nil)

	assert.Equal(t, http.StatusBadGateway, w.Code,
		"upstream 401 must be converted to 502")
	assert.Empty(t, w.Header().Get("WWW-Authenticate"),
		"WWW-Authenticate must NOT be forwarded to browser")
	assert.Contains(t, w.Body.String(), "upstream authentication failed")
}

func TestProxy_Upstream401_InvalidatesPasswordCache(t *testing.T) {
	callCount := 0
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("WWW-Authenticate", "Basic realm=\"opencode\"")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	env.setupWorkspacePodWithT(t, "ws-cache-inv", "10.0.0.1", "Active", "")
	env.setupPasswordWithT(t, "ws-cache-inv", "test-password")

	// First request: 401 → cache invalidated
	w1 := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-cache-inv/sessions", nil)
	assert.Equal(t, http.StatusBadGateway, w1.Code)

	// Verify cache was invalidated
	_, cached := env.handler.GetCachedPasswordForTest("ws-cache-inv")
	assert.False(t, cached, "password cache must be invalidated after 401")
}

// --- US-23.4: onPhaseChange cache invalidation ---

func TestOnPhaseChange_Failed_InvalidatesPwCache(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-fail", "test-password")

	// Prime the cache
	env.handler.SetCachedPasswordForTest("ws-fail", "cached-password")

	// Simulate phase change to Failed
	ws := &v1.Workspace{}
	ws.Name = "ws-fail"
	ws.Status.Phase = v1.WorkspacePhaseFailed
	env.handler.onPhaseChange(ws)

	_, cached := env.handler.GetCachedPasswordForTest("ws-fail")
	assert.False(t, cached, "pwCache must be invalidated on Failed transition")
}

func TestOnPhaseChange_ActiveFromNonActive_InvalidatesPwCache(t *testing.T) {
	env := newTestEnv(t)

	// Prime the cache and set prior phase to Creating
	env.handler.SetCachedPasswordForTest("ws-recover", "old-password")
	env.handler.SetPriorPhaseForTest("ws-recover", "Creating")

	// Simulate phase change to Active (from Creating)
	ws := &v1.Workspace{}
	ws.Name = "ws-recover"
	ws.Status.Phase = v1.WorkspacePhaseActive
	env.handler.onPhaseChange(ws)

	_, cached := env.handler.GetCachedPasswordForTest("ws-recover")
	assert.False(t, cached, "pwCache must be invalidated on Active-from-non-Active")
}

func TestOnPhaseChange_ActiveFromActive_DoesNotInvalidatePwCache(t *testing.T) {
	env := newTestEnv(t)

	// Prime the cache and set prior phase to Active
	env.handler.SetCachedPasswordForTest("ws-stable", "good-password")
	env.handler.SetPriorPhaseForTest("ws-stable", "Active")

	// Simulate Active→Active reconcile
	ws := &v1.Workspace{}
	ws.Name = "ws-stable"
	ws.Status.Phase = v1.WorkspacePhaseActive
	env.handler.onPhaseChange(ws)

	pw, cached := env.handler.GetCachedPasswordForTest("ws-stable")
	assert.True(t, cached, "pwCache must NOT be invalidated on Active→Active")
	assert.Equal(t, "good-password", pw)
}

func TestOnPhaseChange_Terminated_CleansUpPriorPhase(t *testing.T) {
	env := newTestEnv(t)

	env.handler.SetPriorPhaseForTest("ws-term", "Active")

	ws := &v1.Workspace{}
	ws.Name = "ws-term"
	ws.Status.Phase = v1.WorkspacePhaseTerminated
	env.handler.onPhaseChange(ws)

	_, exists := env.handler.GetPriorPhaseForTest("ws-term")
	assert.False(t, exists, "priorPhase must be cleaned up on Terminated")
}

// --- Regression: existing phase transitions still work ---

func TestOnPhaseChange_Suspending_StillInvalidates(t *testing.T) {
	env := newTestEnv(t)

	env.handler.SetCachedPasswordForTest("ws-susp", "pw")

	ws := &v1.Workspace{}
	ws.Name = "ws-susp"
	ws.Status.Phase = v1.WorkspacePhaseSuspending
	env.handler.onPhaseChange(ws)

	_, cached := env.handler.GetCachedPasswordForTest("ws-susp")
	assert.False(t, cached)
}

func TestProxy_Upstream401_DoesNotPanic_WithoutWorkspaceIDInContext(t *testing.T) {
	// Edge case: if workspaceID is somehow empty in context, the 401
	// handler should still not panic.
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Basic")
		w.WriteHeader(http.StatusUnauthorized)
	})
	env.setupWorkspacePodWithT(t, "ws-edge", "10.0.0.1", "Active", "")
	env.setupPasswordWithT(t, "ws-edge", "pw")

	require.NotPanics(t, func() {
		env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-edge/sessions", nil)
	})
}

// --- US-45.4: Redis-backed pwCache integration ---
//
// Verifies the E2E wiring: a RedisStore injected via SetStateStore is
// used by ProxyHandler.getPassword for cache reads/writes, AND that the
// existing 401→invalidate path correctly DELs the Redis key (not just
// the in-memory map). README-LLM.md "E2E Wiring Verification" requires
// this kind of integration test — unit tests on the store alone are
// insufficient evidence of wiring.

// TestProxy_Upstream401_InvalidatesRedisPasswordCache exercises the
// full 401→invalidate path through a Redis-backed state store. After
// the 401, the Redis key for the workspace's password must be deleted
// (not just an in-memory entry) so the next request on any replica
// falls through to K8s.
func TestProxy_Upstream401_InvalidatesRedisPasswordCache(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = redisClient.Close() }()

	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"opencode\"")
		w.WriteHeader(http.StatusUnauthorized)
	})
	// Swap the default InMemoryStore for a RedisStore so the test
	// exercises the Redis code path end-to-end.
	env.handler.SetStateStore(wsstate.NewRedisStore(redisClient, wsstate.DefaultActiveSessTTL))

	env.setupWorkspacePodWithT(t, "ws-redis-inv", "10.0.0.1", "Active", "")
	env.setupPasswordWithT(t, "ws-redis-inv", "test-password")

	// First request: triggers getPassword (cache miss → K8s fetch →
	// SetCachedPassword → Redis SET with TTL) then 401 from upstream
	// → invalidateCaches → InvalidatePassword (Redis DEL).
	w1 := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-redis-inv/sessions", nil)
	assert.Equal(t, http.StatusBadGateway, w1.Code)

	// Verify the Redis key for the cached password is gone — the 401
	// invalidation reached the Redis layer, not just an in-memory map.
	pwKey := "ws:{ws-redis-inv}:pw"
	assert.False(t, mr.Exists(pwKey),
		"Redis password cache key must be DELeted after 401 — multi-replica invalidation")

	// Verify via the store interface too (defense-in-depth).
	_, cached := env.handler.GetCachedPasswordForTest("ws-redis-inv")
	assert.False(t, cached, "GetCachedPassword must return miss after Redis DEL")
}

// --- #902: usage-gate re-arm on Active events (US-69.11 port) ---
//
// The tracker's EnsureWatching is retired; the equivalent surface is the
// busy-gated usage stream (h.UsageStream().Open/Close). The same #902
// regression shapes hold: arm on EVERY Active event (even prior==Active),
// a fresh gate on real transitions, teardown on suspend, and reconciler
// healing for gates the seed missed.

func TestOnPhaseChange_ActiveUpdateRearmsUsageGate(t *testing.T) {
	env := newTestEnv(t)
	consumer, fc, resolved := newRecordingGateConsumer(nil)
	t.Cleanup(injectUsageStream(consumer))
	env.handler.SetPriorPhaseForTest("ws-902", "Active")

	ws := &v1.Workspace{}
	ws.Name = "ws-902"
	ws.Status.Phase = v1.WorkspacePhaseActive
	env.handler.onPhaseChange(ws)

	requireGateConnect(t, fc, "Active→Active update must arm the usage gate")
	require.Contains(t, resolved(), "ws-902",
		"#902: Redis-persisted prior-phase makes post-restart seeds look exactly like this — the gate must still arm")
}

// A real transition (prior != Active) must still Close+Open — a fresh
// gate, since the old one targets the previous pod.
func TestOnPhaseChange_ActiveTransitionFreshGate(t *testing.T) {
	env := newTestEnv(t)
	consumer, fc, _ := newRecordingGateConsumer(nil)
	t.Cleanup(injectUsageStream(consumer))
	env.handler.SetPriorPhaseForTest("ws-902b", "Resuming")

	// Simulate an existing (stale) gate from the previous pod: armed
	// before the transition.
	env.handler.UsageStream().Open("ws-902b")
	requireGateConnect(t, fc, "pre-transition gate must arm")

	ws := &v1.Workspace{}
	ws.Name = "ws-902b"
	ws.Status.Phase = v1.WorkspacePhaseActive
	env.handler.onPhaseChange(ws)

	// The transition tears the old gate down and arms a fresh one: a
	// second connection must appear.
	requireGateConnect(t, fc, "transition into Active must re-arm a fresh gate")
	require.Equal(t, 1, env.handler.UsageStream().Gates(),
		"transition must leave exactly one gate armed")
}

func TestOnPhaseChange_SuspendedClosesGate(t *testing.T) {
	env := newTestEnv(t)
	consumer, _, _ := newRecordingGateConsumer(nil)
	t.Cleanup(injectUsageStream(consumer))
	env.handler.SetPriorPhaseForTest("ws-902c", "Active")

	env.handler.UsageStream().Open("ws-902c")
	require.Equal(t, 1, env.handler.UsageStream().Gates())

	ws := &v1.Workspace{}
	ws.Name = "ws-902c"
	ws.Status.Phase = v1.WorkspacePhaseSuspended
	env.handler.onPhaseChange(ws)

	assert.Equal(t, 0, env.handler.UsageStream().Gates(),
		"Suspended must close the usage gate (no pod stream to a deleted pod)")
}

// TestStateReconciler_HealsMissingGate (#902): the reconciler must arm
// gates for Active workspaces the seed missed — converting permanent
// event-blindness into at most one reconcile interval.
func TestStateReconciler_HealsMissingGate(t *testing.T) {
	env := newTestEnv(t)
	consumer, _, resolved := newRecordingGateConsumer(nil)
	t.Cleanup(injectUsageStream(consumer))

	orig := stateReconcileInterval
	stateReconcileInterval = 20 * time.Millisecond
	t.Cleanup(func() { stateReconcileInterval = orig })

	stopCh := make(chan struct{})
	env.handler.stopCh = stopCh
	reconDone := make(chan struct{})

	// A phase source with an Active workspace no gate covers (the
	// seed-skip shape), plus non-Active controls.
	env.handler.phaseSource = fakePhaseSource{
		"ws-missed":  "Active",
		"ws-susp":    "Suspended",
		"ws-created": "Creating",
	}
	go func() { env.handler.stateReconciler(stateReconcileInterval); close(reconDone) }()
	// Quiesce BEFORE the injected consumer's CloseAll cleanup runs
	// (LIFO): a reconciler mid-tick could otherwise open a gate after
	// CloseAll and leak it into later tests.
	t.Cleanup(func() {
		close(stopCh)
		<-reconDone
	})

	require.Eventually(t, func() bool {
		for _, id := range resolved() {
			if id == "ws-missed" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		"reconciler must arm the missed Active gate")

	for _, id := range resolved() {
		assert.NotEqual(t, "ws-susp", id, "only Active phases are armed")
		assert.NotEqual(t, "ws-created", id, "only Active phases are armed")
	}
}

type fakePhaseSource map[string]string

func (f fakePhaseSource) GetAllKnownPhases() map[string]string { return f }

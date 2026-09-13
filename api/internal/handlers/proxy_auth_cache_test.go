// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestProxy_Upstream401_Returns502 was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_Upstream401_InvalidatesPasswordCache was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

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
		env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-edge/legacy-read/s1", nil)
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

// TestProxy_Upstream401_InvalidatesRedisPasswordCache was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

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

	requireGateSilence(t, fc, "an activity-driven status update must not open a pod stream")
	require.Empty(t, resolved(), "no gate may arm without turn activity")
}

// A real transition into Active must not open a gate either (the
// previous pod's gate is closed by the transition; a new one opens only
// on activity).
func TestOnPhaseChange_ActiveTransitionOpensNoGate(t *testing.T) {
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

	// The transition closes the stale gate and opens nothing: gates are
	// activity-gated, and a phase is not activity.
	assert.Equal(t, 0, env.handler.UsageStream().Gates(),
		"a real transition into Active must not arm a gate — only turn activity does")
	requireGateSilence(t, fc, "no fresh connection may open without activity")
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

// TestStateReconciler_NoArmOnIdleFleet (D1-B / rolling_deploy_no_fanin_storm):
// the reconciler must NOT arm gates for Active workspaces — usage gates
// are activity-gated, so an idle fleet (or a freshly deployed API over
// one) holds ZERO pod streams. Non-Active gates are torn down.
func TestStateReconciler_NoArmOnIdleFleet(t *testing.T) {
	env := newTestEnv(t)
	consumer, fc, resolved := newRecordingGateConsumer(nil)
	t.Cleanup(injectUsageStream(consumer))

	orig := stateReconcileInterval
	stateReconcileInterval = 20 * time.Millisecond
	t.Cleanup(func() { stateReconcileInterval = orig })

	stopCh := make(chan struct{})
	env.handler.stopCh = stopCh
	reconDone := make(chan struct{})
	_ = fc

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

	// Let at least two ticks fire: no gate may ever connect (the
	// fan-in-storm guarantee — phase-Active is not activity).
	requireGateSilence(t, fc, "the reconciler must not arm gates on an idle fleet")
	assert.Empty(t, resolved(), "no gate may arm without turn activity")
}

type fakePhaseSource map[string]string

func (f fakePhaseSource) GetAllKnownPhases() map[string]string { return f }

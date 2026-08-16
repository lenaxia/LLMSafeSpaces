// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Design 0050 D4 (#892): /v1/readyz answers from agentd liveness + a
// kernel-level TCP-listener check on opencode's port — never from opencode
// responsiveness. Starvation must not flap readiness; boot must hold it.

func newReadyzDeps(t *testing.T) serverDeps {
	t.Helper()
	return serverDeps{
		client:          &OpenCodeClient{password: "test"},
		cache:           &providerCache{},
		sseTracker:      newSessionStatusTracker(),
		pressureMonitor: newMemoryPressureMonitor(),
		healthCache:     newHealthzCache(),
		gr:              newGateRecorder(time.Now(), agentdGateDurationSeconds, testLogger()),
		startedAt:       time.Now(),
	}
}

func doReadyz(t *testing.T, deps serverDeps, ready func() bool) (int, agentd.ReadyzResponse) {
	t.Helper()
	srv := httptest.NewServer(buildReadyzHandler(deps, ready))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	var body agentd.ReadyzResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp.StatusCode, body
}

func TestReadyz_TCPListenerReady_EvenWhenHTTPWouldStarve(t *testing.T) {
	withTestLogger(t)
	deps := newReadyzDeps(t)
	// Mark the cache initialized + healthy (agentd saw opencode once).
	deps.healthCache.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, Version: "vtest"})

	// A listener whose application NEVER accepts: the kernel still
	// completes handshakes from the backlog. This is exactly the
	// starved-opencode shape — HTTP would time out; TCP succeeds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	code, body := doReadyz(t, deps, func() bool {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	assert.Equal(t, http.StatusOK, code, "a TCP-accepting opencode is ready even if its HTTP loop is starved")
	assert.True(t, body.Ready)
}

func TestReadyz_RefusedPortNotReady_HoldsBootWindow(t *testing.T) {
	withTestLogger(t)
	deps := newReadyzDeps(t)
	deps.healthCache.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listening now

	code, body := doReadyz(t, deps, func() bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	assert.Equal(t, http.StatusServiceUnavailable, code, "booting/dead opencode (refused) must hold readiness")
	assert.False(t, body.Ready)
}

func TestReadyz_UninitializedCacheNotReady(t *testing.T) {
	withTestLogger(t)
	deps := newReadyzDeps(t) // cache never initialized

	code, _ := doReadyz(t, deps, func() bool { return true })
	assert.Equal(t, http.StatusServiceUnavailable, code,
		"agentd must observe its first refresh cycle before reporting ready")
}

func TestReadyz_NilCheckerPreservesLegacySemantics(t *testing.T) {
	withTestLogger(t)
	deps := newReadyzDeps(t)
	deps.healthCache.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true})

	code, body := doReadyz(t, deps, nil)
	assert.Equal(t, http.StatusOK, code, "nil checker = legacy semantics (Initialized && Healthy)")
	assert.True(t, body.Ready)
}

func TestReadyz_NoSynchronousOpencodeFetch(t *testing.T) {
	withTestLogger(t)
	deps := newReadyzDeps(t)
	deps.healthCache.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true})
	// A hanging opencode: if readyz ever dialed opencode HTTP, the request
	// would stall past the test's client timeout. With D4 it must never
	// be contacted at all.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {} //nolint:staticcheck // deliberate hang
	}))
	t.Cleanup(hang.Close)
	orig := getAgentAddr()
	setAgentAddr(hang.URL)
	t.Cleanup(func() { setAgentAddr(orig) })

	code, _ := doReadyz(t, deps, func() bool { return true })
	assert.Equal(t, http.StatusOK, code, "readyz must answer without any opencode HTTP round-trip")
}

func TestProviderCache_LastKnownNoFetch(t *testing.T) {
	withTestLogger(t)
	c := &providerCache{}
	c.connected = []string{"prov-a"}
	c.configured = 2

	connected, configured := c.lastKnown()
	assert.Equal(t, []string{"prov-a"}, connected)
	assert.Equal(t, 2, configured)
}

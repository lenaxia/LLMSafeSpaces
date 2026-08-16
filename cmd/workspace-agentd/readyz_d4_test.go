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

// TestOpencodeTCPReady_ProductionAddrForm (review round 1 on #895,
// ship-blocker regression): the production checker dials a raw
// host:port, NOT the URL-form getAgentAddr() — net.Dial("tcp",
// "http://...") fails on address form regardless of listeners, which
// made readyz 503 forever (a deterministic startup-probe kill loop).
func TestOpencodeTCPReady_ProductionAddrForm(t *testing.T) {
	withTestLogger(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	// The production URL form is set as the global agent addr, exactly
	// like main() does — the checker must IGNORE it.
	orig := getAgentAddr()
	setAgentAddr("http://" + host + ":" + portStr)
	t.Cleanup(func() { setAgentAddr(orig) })

	// The production wiring form (host:port, no scheme) against a live
	// listener: must be ready.
	ready := opencodeTCPReady(net.JoinHostPort(host, portStr))
	assert.True(t, ready(), "host:port form against a live listener must be ready")

	// And the URL form itself — what the pre-fix code dialed — can never
	// work as a dial address, proving why the checker must not consult
	// getAgentAddr().
	conn, err := net.DialTimeout("tcp", getAgentAddr(), 2*time.Second)
	if err == nil {
		_ = conn.Close()
	}
	assert.Error(t, err, "URL-form addr must fail as a raw dial target (sanity: the pre-fix bug class)")
}

// TestOpencodeTCPReady_RefusedPort: nothing listening → not ready (the
// boot window stays closed).
func TestOpencodeTCPReady_RefusedPort(t *testing.T) {
	withTestLogger(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	ready := opencodeTCPReady(addr)
	assert.False(t, ready())
}

// TestReadyz_AdminServerWiring (review round 1: the admin-server wiring
// was untested): requireBearerToken + buildReadyzHandler +
// opencodeTCPReady composed as production registers them. Auth is
// enforced; an authorized request against a live opencode-port listener
// returns 200 with Ready=true.
func TestReadyz_AdminServerWiring(t *testing.T) {
	withTestLogger(t)
	// "opencode": a listener that never accepts — kernel answers
	// handshakes from the backlog regardless (the starved-opencode
	// shape).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	deps := newReadyzDeps(t)
	deps.healthCache.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, Version: "vtest"})

	mux := http.NewServeMux()
	mux.Handle("/v1/readyz", requireBearerToken("tok", buildReadyzHandler(deps, opencodeTCPReady(ln.Addr().String()))))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(auth string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/readyz", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusUnauthorized, get(""), "admin endpoints enforce bearer auth")
	assert.Equal(t, http.StatusUnauthorized, get("wrong"), "admin endpoints reject bad tokens")
	assert.Equal(t, http.StatusOK, get("tok"), "authorized request with live opencode port: ready")
}

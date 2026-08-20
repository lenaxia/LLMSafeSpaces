// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// control_client_test.go — US-2 (design 0051): the sidecar's control-socket
// client. Appendix A is one-JSON-per-connection; the client must round-trip
// every v1 method against the REAL server, surface protocol errors as typed
// controlError values, and tolerate transport failure (supervisor restarting)
// as a Go error, never a panic.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestControlClient_RoundTrips covers every v1 method against the real
// server backed by the fake proc.
func TestControlClient_RoundTrips(t *testing.T) {
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	cc := newControlClient(srv.addr())
	ctx := context.Background()

	hello, cerr := cc.Hello(ctx)
	require.Nil(t, cerr)
	require.Equal(t, "supervise-opencode", hello.Supervisor)
	require.Equal(t, "stopped", hello.ChildState)

	st, cerr := cc.Status(ctx)
	require.Nil(t, cerr)
	require.Equal(t, "stopped", st.ChildState)

	rres, cerr := cc.Restart(ctx, "manual", 5)
	require.Nil(t, cerr)
	require.True(t, rres.Restarted)
	require.False(t, rres.InProgress)
	require.Equal(t, int64(1), proc.restarts.Load())

	m, cerr := cc.Metrics(ctx)
	require.Nil(t, cerr)
	require.Equal(t, int64(0), m.MemoryCurrentBytes, "no metrics source wired → zero envelope")

	serr := cc.SpawnEnv(ctx, map[string]string{"K": "v"})
	require.Nil(t, serr)
	require.Equal(t, map[string]string{"K": "v"}, *proc.lastEnv.Load())
}

// TestControlClient_TypedStatusParsesRFC3339 pins the LastRestartAt parse:
// the vitals gatherer's boot-grace logic depends on a real time.Time.
func TestControlClient_TypedStatusParsesRFC3339(t *testing.T) {
	proc := &fakeRestartProc{}
	ts := time.Now().UTC().Add(-time.Minute)
	proc.overrideState.Store(&procStateOverride{pid: 42, state: "running", restarts: 3, lastRestartAt: ts})
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	cc := newControlClient(srv.addr())

	st, cerr := cc.Status(context.Background())
	require.Nil(t, cerr)
	require.Equal(t, 42, st.ChildPID)
	require.Equal(t, "running", st.ChildState)
	require.Equal(t, 3, st.Restarts)
	require.WithinDuration(t, ts, st.LastRestartAt, time.Second)
}

// TestControlClient_ProtocolErrors maps wire errors to typed controlError.
func TestControlClient_ProtocolErrors(t *testing.T) {
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	go srv.serve()
	ctx := context.Background()

	cc := newControlClient(srv.addr())
	_, cerr := cc.Restart(ctx, "not_a_reason", 5)
	require.NotNil(t, cerr)
	var badReq *controlClientError
	require.ErrorAs(t, cerr, &badReq)
	require.Equal(t, "bad_request", badReq.Code())

	// A v2-speaking client gets version_unsupported (A.1: hello with an
	// incompatible v is how mismatch is DETECTED).
	bad := &controlClient{addr: srv.addr(), timeout: 2 * time.Second, version: 2}
	_, cerr = bad.Hello(ctx)
	require.NotNil(t, cerr)
	var verUnsup *controlClientError
	require.ErrorAs(t, cerr, &verUnsup)
	require.Equal(t, "version_unsupported", verUnsup.Code())
}

// TestControlClient_TransportFailure: a dead supervisor surfaces as a Go
// error (dial refused), not a panic — the sidecar must keep serving its
// own muxes while the supervisor is down.
func TestControlClient_TransportFailure(t *testing.T) {
	port := freeTCPPort(t)
	cc := newControlClient(fmt.Sprintf("127.0.0.1:%d", port))
	cc.timeout = 500 * time.Millisecond
	_, cerr := cc.Hello(context.Background())
	require.NotNil(t, cerr)
	require.Contains(t, cerr.Error(), "control socket")
}

// TestControlClient_IDsUniquePerRequest: one connection per request means
// each call mints a fresh id (A.1 correlation). The ids must strictly
// increase; the server-echo half is pinned by the golden wire tests.
func TestControlClient_IDsUniquePerRequest(t *testing.T) {
	cc := newControlClient("127.0.0.1:1")
	prev := int64(0)
	for i := 0; i < 3; i++ {
		id := cc.nextID()
		require.Greater(t, id, prev, "ids must strictly increase")
		require.Positive(t, id, "ids are 1-based")
		prev = id
	}
}

// TestControlClient_StatusEmptyTimestampIsZeroTime: a supervisor that has
// never restarted reports last_restart_at "" — the client must map that to
// the zero time, not a parse error (boot-grace logic reads it).
func TestControlClient_StatusEmptyTimestampIsZeroTime(t *testing.T) {
	proc := &fakeRestartProc{}
	proc.overrideState.Store(&procStateOverride{pid: 7, state: "running", emptyLastRestart: true})
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	cc := newControlClient(srv.addr())

	st, cerr := cc.Status(context.Background())
	require.Nil(t, cerr)
	require.True(t, st.LastRestartAt.IsZero())
}

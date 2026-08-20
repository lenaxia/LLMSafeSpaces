// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// fakeRestartProc: records behavior without spawning children.
type fakeRestartProc struct {
	restarts   atomic.Int64
	lastEnv    atomic.Pointer[map[string]string]
	lastReason atomic.Pointer[string]

	// block, when closed by a test, makes the NEXT Restart hang until
	// released — models a slow (real) restart so a second request can
	// genuinely overlap it.
	block   chan struct{}
	blocked atomic.Bool
}

func (f *fakeRestartProc) Restart(reason string, _ int) (bool, bool) {
	if f.block != nil && f.blocked.CompareAndSwap(false, true) {
		<-f.block // hold the first restart open
	}
	f.restarts.Add(1)
	f.lastReason.Store(&reason)
	return true, false
}

func (f *fakeRestartProc) State() (int, string, int, time.Time) {
	return 0, "stopped", int(f.restarts.Load()), time.Time{}
}

func (f *fakeRestartProc) SetSpawnEnv(env map[string]string) {
	stored := make(map[string]string, len(env))
	for k, v := range env {
		stored[k] = v
	}
	f.lastEnv.Store(&stored)
}

// newControlSocketServerForTest builds a server on addr (":0" for
// ephemeral) backed by the fake proc.
func newControlSocketServerForTest(t *testing.T, addr string) *controlSocketServer {
	t.Helper()
	return newControlSocketServerWithProc(t, addr, &fakeRestartProc{})
}

func newControlSocketServerWithProc(t *testing.T, addr string, proc supervisedProcIface) *controlSocketServer {
	t.Helper()
	srv, err := newControlSocketServer(addr, proc)
	if err != nil {
		t.Fatalf("control socket listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.close() })
	return srv
}

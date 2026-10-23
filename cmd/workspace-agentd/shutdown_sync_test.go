// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// flushSpy overrides the flushSync seam and records invocations.
type flushSpy struct {
	mu      sync.Mutex
	calls   int
	block   bool
	release chan struct{}
}

func newFlushSpy() *flushSpy {
	return &flushSpy{release: make(chan struct{})}
}

func (f *flushSpy) sync() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block {
		<-f.release // never released in bound tests
	}
}

func (f *flushSpy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func withFlushSeam(t *testing.T, spy *flushSpy, wait time.Duration) {
	t.Helper()
	prevSync, prevWait := flushSync, shutdownFlushWait
	flushSync, shutdownFlushWait = spy.sync, wait
	t.Cleanup(func() { flushSync, shutdownFlushWait = prevSync, prevWait })
}

// PIN (#1808 H1 belt): flushWorkspaceWrites triggers a filesystem-wide
// writeback flush and reports completion when the flush returns.
func TestFlushWorkspaceWrites_Completes(t *testing.T) {
	spy := newFlushSpy()
	withFlushSeam(t, spy, time.Second)

	if !flushWorkspaceWrites() {
		t.Fatal("unblocked flush must report completion")
	}
	if spy.count() != 1 {
		t.Fatalf("flushSync calls: want 1, got %d", spy.count())
	}
}

// PIN (the bound): a flush that never returns is ABANDONED after
// shutdownFlushWait — the shutdown path must never hang past the bound.
// The kernel completes the triggered writeback asynchronously; the
// process exiting early does not cancel it.
func TestFlushWorkspaceWrites_BoundedWait(t *testing.T) {
	spy := newFlushSpy()
	spy.block = true
	withFlushSeam(t, spy, 50*time.Millisecond)

	start := time.Now()
	if flushWorkspaceWrites() {
		t.Fatal("blocked flush must NOT report completion")
	}
	elapsed := time.Since(start)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("returned before the bound (%v) — the wait is not actually applied", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("returned at %v — the bound did not hold", elapsed)
	}
}

// PIN (wiring): runShutdown — the single-container shutdown sequence —
// calls the flush seam before exiting. The belt must be on the actual
// shutdown path, not just an exported helper nothing calls.
func TestRunShutdown_FlushesBeforeExit(t *testing.T) {
	spy := newFlushSpy()
	withFlushSeam(t, spy, time.Second)

	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bgWg sync.WaitGroup
	runShutdown(&http.Server{}, &http.Server{}, bgCancel, &bgWg, nil)

	if spy.count() != 1 {
		t.Fatalf("runShutdown must flush exactly once, got %d", spy.count())
	}
}

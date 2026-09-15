// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pending_apply_test.go — #1342 item 4: the pending-apply tracker holds
// the deferred credential apply for healthz surfacing.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingApplyTracker_BeginSnapshotClear(t *testing.T) {
	p := newPendingApplyTracker()

	assert.Nil(t, p.snapshot(), "nothing pending initially")

	p.begin(3)
	snap := p.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, pendingApplyReasonCredentialChange, snap.Reason)
	assert.Equal(t, 3, snap.BusySessions)
	assert.GreaterOrEqual(t, snap.WaitingSeconds, 0)

	p.refreshBusy(1)
	snap = p.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.BusySessions, "tick refresh keeps the busy count honest")

	p.clear()
	assert.Nil(t, p.snapshot(), "cleared tracker surfaces nothing")
}

func TestPendingApplyTracker_ConcurrentDefers_Refcounted(t *testing.T) {
	p := newPendingApplyTracker()

	// Two resyncs defer while sessions stay busy (two goroutines share
	// the tracker — applyMu is released before the decision).
	p.begin(2)
	p.begin(1)

	snap := p.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 2, snap.BusySessions, "first begin's count stands until refreshed")

	// First restart fires — the second deferral is STILL outstanding.
	p.clear()
	snap = p.snapshot()
	require.NotNil(t, snap, "one clear must not erase a sibling deferral's pending state")
	p.refreshBusy(1)
	assert.Equal(t, 1, p.snapshot().BusySessions)

	// Second fires — the surface drops.
	p.clear()
	assert.Nil(t, p.snapshot())
}

func TestPendingApplyTracker_ConcurrentSafe(t *testing.T) {
	p := newPendingApplyTracker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			p.begin(i)
			p.refreshBusy(i)
			_ = p.snapshot()
			p.clear()
		}
	}()
	<-done
}

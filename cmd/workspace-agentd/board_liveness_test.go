// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_liveness_test.go — the #1847 liveness snapshot pins: the
// tracker's per-session observability must render the busy/started/
// activity rows the engine classifies lapsed claims against.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLivenessSnapshot_ObservabilityRows(t *testing.T) {
	tr := newSessionStatusTracker()

	// A mid-turn session: busy, streaming tool output (activity), no
	// completed message yet — started nonetheless (its turn is open).
	tr.set("sess-open", "busy")
	tr.noteActivity("sess-open")

	// An idle session that DID work: message 20m ago, quiet since
	// (stamped directly — the SSE classifier's seam, not re-derived).
	tr.set("sess-idle", "idle")
	tr.noteMessage("sess-idle")
	tr.mu.Lock()
	tr.lastMessageAt["sess-idle"] = time.Now().Add(-20 * time.Minute)
	tr.mu.Unlock()

	// A busy session never observed emitting anything: the busy mark is
	// its earliest activity evidence.
	tr.set("sess-quietbusy", "busy")

	rows := map[string]livenessRow{}
	for _, r := range tr.livenessSnapshot() {
		rows[r.SessionID] = r
	}
	require.Len(t, rows, 3)

	open := rows["sess-open"]
	require.True(t, open.Busy)
	require.True(t, open.Started, "an open turn IS started (the charter message landed)")
	require.WithinDuration(t, time.Now(), open.LastActivityAt, 5*time.Second,
		"stream activity is the watermark")

	idle := rows["sess-idle"]
	require.False(t, idle.Busy)
	require.True(t, idle.Started, "messages>0: the session began")
	require.WithinDuration(t, time.Now().Add(-20*time.Minute), idle.LastActivityAt, 5*time.Second,
		"the old message stamp rides verbatim (freshness is the server's judgment)")

	quiet := rows["sess-quietbusy"]
	require.True(t, quiet.Busy)
	require.True(t, quiet.Started)
	require.WithinDuration(t, time.Now(), quiet.LastActivityAt, 5*time.Second,
		"a never-observed busy session falls back to its busy mark")
}

func TestLivenessSnapshot_EmptyPod(t *testing.T) {
	tr := newSessionStatusTracker()
	require.Empty(t, tr.livenessSnapshot(),
		"an idle pod pushes nothing; its stale rows age out server-side")
}

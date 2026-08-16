// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/require"
)

// Design 0050 D2: busy flags from a dead opencode generation are orphaned
// by definition and must clear at every generation change; promptTokens
// (context meters) are display state and survive.

func TestResetBusyFlags_ClearsBusyKeepsIdleAndTokens(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	tr.set("ses-busy", "busy")
	tr.set("ses-idle", "idle")
	tr.set("ses-busy2", "busy")
	tr.setPromptTokens("ses-busy", 1234)
	tr.setPromptTokens("ses-idle", 99)

	cleared := tr.resetBusyFlags()

	require.ElementsMatch(t, []string{"ses-busy", "ses-busy2"}, cleared)
	require.Equal(t, "idle", tr.get("ses-busy"))
	require.Equal(t, "idle", tr.get("ses-busy2"))
	require.Equal(t, "idle", tr.get("ses-idle"))
	// Entries and context meters survive the reset.
	require.Equal(t, int64(1234), tr.getPromptTokens("ses-busy"))
	require.Equal(t, int64(99), tr.getPromptTokens("ses-idle"))
	require.True(t, tr.hasPromptTokens("ses-busy"))
	// hasAnyBusy false — restart deferral is not blocked by ghosts.
	require.False(t, tr.hasAnyBusy())
}

func TestResetBusyFlags_EmptyTrackerNoop(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	require.Empty(t, tr.resetBusyFlags())
}

func TestResetBusyFlags_AllIdleNoop(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	tr.set("a", "idle")
	tr.set("b", "idle")
	require.Empty(t, tr.resetBusyFlags())
	require.True(t, tr.hasAnyData())
}

func TestOnOpencodeGenerationStart_HealsOrphans(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	tr.set("ses-orphan", "busy")
	tr.setPromptTokens("ses-orphan", 500)

	tr.onOpencodeGenerationStart()

	require.Equal(t, "idle", tr.get("ses-orphan"))
	require.Equal(t, int64(500), tr.getPromptTokens("ses-orphan"))
}

func TestOnOpencodeGenerationStart_ConcurrentWithSSEWrites(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			tr.set("ses-x", "busy")
			tr.set("ses-x", "idle")
			tr.setPromptTokens("ses-x", int64(i))
		}
	}()
	for i := 0; i < 100; i++ {
		tr.onOpencodeGenerationStart()
	}
	<-done
	// No deadlock/panic; final state is one of the written values.
	require.Contains(t, []string{"busy", "idle"}, tr.get("ses-x"))
}

func TestRecordTrackerBusyReset_Metric(t *testing.T) {
	reg := prometheus.NewRegistry()
	vec := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "workspace_tracker_busy_resets_total",
	}, []string{"workspace_id"})
	m := &opsMetrics{trackerBusyResets: vec}

	m.RecordTrackerBusyReset("ws-metric", 3)
	m.RecordTrackerBusyReset("ws-metric", 2)
	m.RecordTrackerBusyReset("", 1) // empty ID normalizes to "unknown"

	require.Equal(t, 5.0, testutil.ToFloat64(vec.WithLabelValues("ws-metric")))
	require.Equal(t, 1.0, testutil.ToFloat64(vec.WithLabelValues("unknown")))
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package obs

import (
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoopLivenessContract pins the family's wire identity: the name and
// label are the alerting contract shared by every epic-71 periodic loop
// across BOTH binaries — drift here orphans every staleness rule.
func TestLoopLivenessContract(t *testing.T) {
	assert.Equal(t, "llmsafespaces_loop_last_run_timestamp_seconds", LoopLivenessMetric)
	assert.Equal(t, "loop", LoopLivenessLabel)
	assert.NotEmpty(t, LoopLivenessHelp)
}

// TestStampLoopLastRun_RecordsPerLoopSeries: each loop label is its own
// series, stamps are immediate, and a second stamp advances the first.
func TestStampLoopLastRun_RecordsPerLoopSeries(t *testing.T) {
	StampLoopLastRun("test-loop-a")
	first := promtestutil.ToFloat64(LoopLastRun().WithLabelValues("test-loop-a"))
	require.Greater(t, first, float64(0), "a stamped loop reads as live")

	assert.Equal(t, float64(0), promtestutil.ToFloat64(LoopLastRun().WithLabelValues("test-loop-b")),
		"an unstamped loop is a distinct (zero) series — stamps never bleed across loops")

	StampLoopLastRun("test-loop-a")
	assert.Greater(t, promtestutil.ToFloat64(LoopLastRun().WithLabelValues("test-loop-a")), first,
		"every completed pass refreshes the stamp (SetToCurrentTime precision)")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// --- US-69.11: the SSE-tracker retirement's scale-to-zero observables.
// The two stream consumers expose lifecycle hooks; these tests pin the
// handler-side gauge behavior — set 1 while open, DELETE the series on
// close (an idle fleet scrapes empty, not a wall of zero series — the
// DeleteRequestBufferMetrics discipline). ---

func TestContractStreamUpstreamGauge(t *testing.T) {
	contractStreamUpstreams.Reset()
	defer contractStreamUpstreams.Reset()

	recordContractStreamUpstream("ws-gauge-cs", true)
	require.Equal(t, 1.0, testutil.ToFloat64(contractStreamUpstreams.WithLabelValues("ws-gauge-cs")),
		"upstream create sets 1")

	require.Equal(t, 1, testutil.CollectAndCount(contractStreamUpstreams, "llmsafespaces_contract_stream_upstreams"),
		"exactly one series while open")

	recordContractStreamUpstream("ws-gauge-cs", false)
	require.Zero(t, testutil.CollectAndCount(contractStreamUpstreams, "llmsafespaces_contract_stream_upstreams"),
		"last detach deletes the series (scale-to-zero scrapes empty)")
}

func TestUsageStreamGatesGauge(t *testing.T) {
	usageStreamGates.Reset()
	defer usageStreamGates.Reset()

	recordUsageStreamGate("ws-gauge-us", true)
	require.Equal(t, 1.0, testutil.ToFloat64(usageStreamGates.WithLabelValues("ws-gauge-us")),
		"gate open sets 1")

	recordUsageStreamGate("ws-gauge-us", false)
	require.Zero(t, testutil.CollectAndCount(usageStreamGates, "llmsafespaces_usage_stream_gates"),
		"gate close deletes the series (idle fleets scrape empty)")
}

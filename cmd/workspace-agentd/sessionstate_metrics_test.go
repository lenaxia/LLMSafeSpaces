// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
)

// --- US-69.12: the metrics wiring (gauge refresh + ABI-surface
// instrumentation) ---

// TestRecordSessionStateMetrics_SetsGauges: one Metrics() snapshot lands
// in the named gauges with the design's labels.
func TestRecordSessionStateMetrics_SetsGauges(t *testing.T) {
	a := sessionstateTestAuthority(t)
	recordSessionStateMetrics("ws-1", a)

	assert.GreaterOrEqual(t, testutil.ToFloat64(sessionStateMetrics.seqStall.WithLabelValues("ws-1")), 0.0, "seq stall gauge set")

	// No ledger wired in this authority: the stalled gauge reads zero —
	// not a scrape hole (every gauge is pre-registered at boot).
	assert.Equal(t, 0.0, testutil.ToFloat64(sessionStateMetrics.stalledEntries))
}

// TestInstrumentABISurface_BudgetOpsMeasured: Deliver + GetSnapshot are
// timed/sized; other procedures pass through untouched.
func TestInstrumentABISurface_BudgetOpsMeasured(t *testing.T) {
	var inner string
	h := instrumentABISurface(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = r.URL.Path
		_, _ = w.Write([]byte(`{"message":{"sessionId":"s1"}}`))
	}))

	for _, path := range []string{
		"/llmsafespaces.abi.v1.HarnessABIService/Deliver",
		"/llmsafespaces.abi.v1.HarnessABIService/GetSnapshot",
		"/llmsafespaces.abi.v1.HarnessABIService/Act",
	} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, 200, res.Code)
		assert.Equal(t, path, inner)
	}

	require.Equal(t, 1, testutil.CollectAndCount(sessionStateMetrics.deliveryLatency), "delivery 202 latency observed once")
	require.Equal(t, 1, testutil.CollectAndCount(sessionStateMetrics.snapshotLatency), "snapshot latency observed once")
	require.Equal(t, 1, testutil.CollectAndCount(sessionStateMetrics.snapshotSize), "snapshot size observed once")
}

// sessionstateTestAuthority builds a minimal authority (no ledger).
func sessionstateTestAuthority(t *testing.T) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      noopParser{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

type noopParser struct{}

func (noopParser) Parse(raw []byte) (*abiv1.Event, bool, error) { return nil, false, nil }

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/obs"
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

// TestSessionStateWatchdog_EndToEnd (US-69.12 review round): the
// production watchdog loop, at a test cadence — a row crossing the stall
// deadline gets stalled by the LOOP (not a direct CheckStalls call), the
// errored wake increments the real counter, and the gauge refresh carries
// the funnel + stalled subset to the registry.
func TestSessionStateWatchdog_EndToEnd(t *testing.T) {
	wsID := "ws-e2e"
	wakeFailed := false
	a, errA := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      noopParser{},
		Passwords:   []string{"pw"},
		Admitter:    instantAdmitter{},
		Wake: func(context.Context, string) error {
			wakeFailed = true
			return errors.New("store unreachable")
		},
		FastCursor: true,
	})
	require.NoError(t, errA)
	t.Cleanup(func() { _ = a.Close() })
	a.SetStallDeadlineForTest(time.Nanosecond)

	// Land an admitted row through the real Deliver op.
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/llmsafespaces.abi.v1.HarnessABIService/Deliver",
		strings.NewReader(`{"sessionId":"s1","entryId":"e-1","attempt":1,"parts":[{"text":"hi"}]}`))
	req.SetBasicAuth("opencode", "pw")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	require.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	}, 3*time.Second, 10*time.Millisecond, "row admitted")

	ctx, cancel := context.WithCancel(context.Background())
	go runSessionStateWatchdog(ctx, wsID, a, 5*time.Millisecond)
	t.Cleanup(cancel)

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(sessionStateMetrics.stalledEntries) == 1 &&
			testutil.ToFloat64(sessionStateMetrics.wakeFailures) >= 1
	}, 3*time.Second, 10*time.Millisecond, "the loop stalls the row, counts the failed wake, refreshes gauges")
	// epic-71 / 0c: the loop's last-run gauge advances on EVERY completed
	// pass — a boot-once stamp (or a dead loop) cannot satisfy this, and
	// staleness alerting consumes exactly this property (alerts themselves
	// are gated per the wave plan).
	first := testutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopReconcileWatchdog))
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopReconcileWatchdog)) > first
	}, 3*time.Second, 5*time.Millisecond, "the gauge advances with the loop (per-pass refresh)")
	assert.True(t, wakeFailed, "the configured wake fired")
	assert.Equal(t, 1.0, testutil.ToFloat64(sessionStateMetrics.ledgerDepth.WithLabelValues(wsID, "stalled")),
		"the funnel gauge carries the stalled state")
}

// TestMetricsScrape_Completeness (the issue's metrics_scrape_completeness
// in its in-repo form): every US-69.12 metric family is present on the
// ACTUAL :4098 scrape surface — registration is package-init, so a wiring
// typo would otherwise silently drop a metric.
func TestMetricsScrape_Completeness(t *testing.T) {
	// A *Vec with no children emits NO series in the text format — its
	// family is invisible on the scrape regardless of registration. This
	// test therefore materializes every vec family first (sentinel
	// labels), making it order-independent: previously it passed only
	// when earlier tests in the binary happened to create children, and
	// loop_last_run depended on the watchdog goroutine's timing.
	sessionStateMetrics.seqStall.WithLabelValues("seed")
	sessionStateMetrics.ledgerDepth.WithLabelValues("seed", "stalled")
	sessionStateMetrics.promotionStall.WithLabelValues("seed")
	sessionStateMetrics.reconciled.WithLabelValues("failed")
	obs.StampLoopLastRun(obs.LoopReconcileWatchdog)

	ts := httptest.NewServer(promhttp.Handler())
	t.Cleanup(ts.Close)
	res, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	for _, name := range []string{
		"llmsafespaces_seq_stall_seconds",
		"llmsafespaces_ledger_depth",
		"llmsafespaces_stalled_entries",
		"llmsafespaces_promotion_stall_seconds",
		"llmsafespaces_snapshot_size_bytes",
		"llmsafespaces_snapshot_latency_seconds",
		"llmsafespaces_delivery_202_latency_seconds",
		"llmsafespaces_wake_failures_total",
		"llmsafespaces_sessionstate_dropped_events",
		"llmsafespaces_sessionstate_parser_failures",
		"llmsafespaces_sessionstate_panics_contained",
		"llmsafespaces_sessionstate_subscribers",
		"llmsafespaces_loop_last_run_timestamp_seconds",
		// #1342 S12: the orphan-sweep counter must be on the scrape —
		// its absence would silently blind the fleet-wide orphan signal
		// (registration is package-init; a wiring typo is otherwise
		// invisible until an operator needs it).
		"llmsafespaces_orphan_parts_aborted_total",
	} {
		assert.Contains(t, string(body), name, "metric scrapes")
	}
}

// metricFunnelStore is the minimal store for the orphan-funnel test: one
// session the sweep can restore into.
type metricFunnelStore struct{}

func (metricFunnelStore) SessionStates(context.Context) (map[string]sessionstate.SessionSeed, error) {
	return map[string]sessionstate.SessionSeed{
		"ses_m": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}, nil
}

func (metricFunnelStore) MessagePresence(_ context.Context, _ string, messageIDs []string) (map[string]bool, error) {
	present := map[string]bool{}
	for _, id := range messageIDs {
		present[id] = false
	}
	return present, nil
}

func (metricFunnelStore) PendingInputs(context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

type metricFunnelParser struct{}

func (metricFunnelParser) Parse([]byte) (*abiv1.Event, bool, error) { return nil, false, nil }

// TestOrphanPartsMetric_FunnelAdvances pins the #1342 Prometheus bridge
// end-to-end (r3): a harness-restart sweep's cumulative counter must
// advance the REGISTERED counter through recordSessionStateMetrics —
// and only by the delta (a re-record without new sweeps must not
// double-count). Deleting the bridge block must fail this test.
func TestOrphanPartsMetric_FunnelAdvances(t *testing.T) {
	a, err := sessionstate.New(sessionstate.Config{
		Parser:      metricFunnelParser{},
		Store:       metricFunnelStore{},
		Passwords:   []string{"pw"},
		PlatformDir: t.TempDir(),
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_m",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{{
			Id:   "prt_run",
			Type: abiv1.PartType_PART_TYPE_TOOL,
			Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
				CallId: "call_1", Name: "bash",
				State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
			}},
		}}},
	})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))
	require.EqualValues(t, 1, a.Metrics().OrphanPartsAborted, "the sweep folded exactly one orphan")

	wsID := "ws-orphan-funnel"
	recordSessionStateMetrics(wsID, a)
	assert.Equal(t, 1.0, testutil.ToFloat64(sessionStateMetrics.orphanPartsAborted),
		"the funnel must advance the registered orphan-parts counter")

	// Delta discipline (customValveDelta's convention): re-recording the
	// same cumulative must not double-count.
	recordSessionStateMetrics(wsID, a)
	assert.Equal(t, 1.0, testutil.ToFloat64(sessionStateMetrics.orphanPartsAborted),
		"the delta bridge must not re-add the cumulative on every scrape")
}

// instantAdmitter admits synchronously (the watchdog e2e's ledger).
type instantAdmitter struct{}

func (instantAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	return "msg-1", nil
}

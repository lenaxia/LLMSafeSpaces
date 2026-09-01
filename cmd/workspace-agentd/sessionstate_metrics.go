// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sessionstate_metrics.go — US-69.12 (design 0055 R5 + §Rollout S4):
// seq advance, ledger states, promotion stalls, and snapshot/delivery
// costs become first-class observables. The sessionstate module stays
// prometheus-free (the seal): Authority.Metrics()/CheckStalls() are the
// bridge, and this wiring layer owns the registry.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
)

var sessionStateMetrics = struct {
	seqStall          *prometheus.GaugeVec
	ledgerDepth       *prometheus.GaugeVec
	stalledEntries    prometheus.Gauge
	promotionStall    *prometheus.GaugeVec
	snapshotSize      prometheus.Histogram
	snapshotLatency   prometheus.Histogram
	deliveryLatency   prometheus.Histogram
	wakeFailures      prometheus.Counter
	droppedEvents     prometheus.Gauge
	parserFailures    prometheus.Gauge
	panicsContained   prometheus.Gauge
	subscribers       prometheus.Gauge
	customValveEvents prometheus.Counter
}{
	seqStall: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmsafespaces_seq_stall_seconds",
		Help: "Seconds since the sessionstate projection last advanced (R5 starvation signal: seq stalled while the pod runs; a reseed counts as advance).",
	}, []string{"workspace_id"}),
	ledgerDepth: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmsafespaces_ledger_depth",
		Help: "Delivery-ledger row count per state (the funnel: ledgered -> admitted -> promoted/turn_ended, stalled and failed visible).",
	}, []string{"workspace_id", "state"}),
	stalledEntries: promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llmsafespaces_stalled_entries",
		Help: "Delivery-ledger rows in the stalled state (admitted past the promotion deadline; the #1119 silent-strand class, visible).",
	}),
	promotionStall: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmsafespaces_promotion_stall_seconds",
		Help: "Age of the oldest admitted-unpromoted ledger row (0 when none) — promotion stall before it crosses the stall deadline.",
	}, []string{"workspace_id"}),
	snapshotSize: promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmsafespaces_snapshot_size_bytes",
		Help:    "GetSnapshot response size in bytes (R9: O(in-flight state), never O(history)).",
		Buckets: []float64{1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20},
	}),
	snapshotLatency: promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmsafespaces_snapshot_latency_seconds",
		Help:    "GetSnapshot serving latency (the zero-harness-call read; US-69.4 budget).",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 13), // 1ms .. ~4s
	}),
	deliveryLatency: promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "llmsafespaces_delivery_202_latency_seconds",
		Help:    "Deliver ack latency — the 202 equivalent (idempotent fsync'd ledger ack; US-69.6/69.8 send-path budget).",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 13), // 1ms .. ~4s
	}),
	wakeFailures: promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_wake_failures_total",
		Help: "Stall-wake attempts that errored (wake-only recovery failing is the escalation signal).",
	}),
	droppedEvents: promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llmsafespaces_sessionstate_dropped_events",
		Help: "Cumulative harness events the projection dropped (seq-persist failures; integrity over availability).",
	}),
	parserFailures: promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llmsafespaces_sessionstate_parser_failures",
		Help: "Cumulative harness payloads that failed contract translation (the recover wall's containment count).",
	}),
	panicsContained: promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llmsafespaces_sessionstate_panics_contained",
		Help: "Cumulative parser panics contained by the recover wall.",
	}),
	subscribers: promauto.NewGauge(prometheus.GaugeOpts{
		Name: "llmsafespaces_sessionstate_subscribers",
		Help: "Active ABI stream subscribers (on-demand consumption health, D1-B).",
	}),
	customValveEvents: promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_custom_valve_events_total",
		Help: "Custom (PART_TYPE_CUSTOM) part applications folded into the projection — the unknown-taxonomy drift signal's agentd successor (US-69.11): growth means extension kinds the pinned taxonomy does not name are flowing through the valve.",
	}),
}

// customValveLast carries the last cumulative per-workspace snapshot so
// the prometheus counter advances by DELTAS — Metrics() exposes
// cumulative counts and the watchdog scrapes repeatedly; re-Adding the
// cumulative value each pass would over-count. A monotonicity break
// (authority recreated) resets the baseline instead.
var customValveLast = struct {
	mu sync.Mutex
	m  map[string]int64
}{m: map[string]int64{}}

func customValveDelta(workspaceID string, cumulative int64) int64 {
	customValveLast.mu.Lock()
	defer customValveLast.mu.Unlock()
	prev, seen := customValveLast.m[workspaceID]
	if !seen || cumulative < prev {
		customValveLast.m[workspaceID] = cumulative
		if !seen {
			return cumulative
		}
		return 0 // baseline reset after authority recreation: no double count
	}
	customValveLast.m[workspaceID] = cumulative
	return cumulative - prev
}

// recordSessionStateMetrics pulls one Metrics() snapshot into the gauges.
func recordSessionStateMetrics(workspaceID string, a *sessionstate.Authority) {
	if a == nil {
		return
	}
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m := a.Metrics()
	sessionStateMetrics.seqStall.WithLabelValues(workspaceID).Set(m.SecondsSinceSeqAdvance)
	sessionStateMetrics.promotionStall.WithLabelValues(workspaceID).Set(m.OldestPromotionStallSeconds)
	sessionStateMetrics.stalledEntries.Set(float64(m.StalledEntries))
	sessionStateMetrics.droppedEvents.Set(float64(m.DroppedEvents))
	sessionStateMetrics.parserFailures.Set(float64(m.ParserFailures))
	sessionStateMetrics.panicsContained.Set(float64(m.PanicsContained))
	sessionStateMetrics.subscribers.Set(float64(m.Subscribers))
	if d := customValveDelta(workspaceID, m.CustomValveEvents); d > 0 {
		sessionStateMetrics.customValveEvents.Add(float64(d))
	}
	// The funnel: reset-then-set so vanished states drop to zero instead
	// of lingering at their last value.
	for state, n := range m.LedgerDepths {
		sessionStateMetrics.ledgerDepth.WithLabelValues(workspaceID, state).Set(float64(n))
	}
}

// runSessionStateWatchdog drives the stall detector + gauge refresh: one
// pass per interval (production: 1m — the promotion deadline is 10m, so
// the cadence sees a stall within ~1m of crossing it; tests shrink it).
// Wake failures increment the counter per errored attempt (the
// escalation signal).
func runSessionStateWatchdog(ctx context.Context, workspaceID string, a *sessionstate.Authority, every time.Duration) {
	if a == nil {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := a.CheckStalls(ctx)
			if stats.WakeFailures > 0 {
				sessionStateMetrics.wakeFailures.Add(float64(stats.WakeFailures))
			}
			if stats.Stalled > 0 {
				log.Warn("agentd: sessionstate stall — admitted row crossed the promotion deadline (wake fired)",
					zap.Int("stalled", stats.Stalled), zap.Int("wakeFailures", stats.WakeFailures))
			}
			recordSessionStateMetrics(workspaceID, a)
		}
	}
}

// statusWriter counts response bytes (snapshot-size measurement).
type statusWriter struct {
	http.ResponseWriter
	bytes int64
}

func (w *statusWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// instrumentABISurface times + sizes the two budget-carrying ABI ops by
// procedure suffix: Deliver → the 202-equivalent ack latency; GetSnapshot
// → snapshot latency + size. Reads stay unmeasured (they are not budget
// carriers) and nothing here touches the harness.
func instrumentABISurface(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/Deliver"):
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			h.ServeHTTP(sw, r)
			sessionStateMetrics.deliveryLatency.Observe(time.Since(start).Seconds())
		case strings.HasSuffix(r.URL.Path, "/GetSnapshot"):
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			h.ServeHTTP(sw, r)
			sessionStateMetrics.snapshotLatency.Observe(time.Since(start).Seconds())
			sessionStateMetrics.snapshotSize.Observe(float64(sw.bytes))
		default:
			h.ServeHTTP(w, r)
		}
	})
}

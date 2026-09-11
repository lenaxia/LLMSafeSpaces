// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sessionstate_metrics.go — US-69.12 (design 0055 R5 + §Rollout S4):
// seq advance, ledger states, promotion stalls, and snapshot/delivery
// costs become first-class observables. The sessionstate module stays
// prometheus-free (the seal): Authority.Metrics()/CheckStalls() are the
// bridge, and this wiring layer owns the registry.

import (
	"github.com/lenaxia/llmsafespaces/pkg/obs"

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
	seqStall               *prometheus.GaugeVec
	ledgerDepth            *prometheus.GaugeVec
	stalledEntries         prometheus.Gauge
	promotionStall         *prometheus.GaugeVec
	snapshotSize           prometheus.Histogram
	snapshotLatency        prometheus.Histogram
	deliveryLatency        prometheus.Histogram
	wakeFailures           prometheus.Counter
	droppedEvents          prometheus.Gauge
	parserFailures         prometheus.Gauge
	panicsContained        prometheus.Gauge
	subscribers            prometheus.Gauge
	customValveEvents      prometheus.Counter
	reconciled             *prometheus.CounterVec
	reconcileEvidenceFails prometheus.Counter
	leaseGatherFails       prometheus.Counter
	loopLastRun            *prometheus.GaugeVec
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
		Help:    "GetSnapshot serving latency (the projection read plus ONE coalesced pod-local lease-refresh gather, #1310 slice B; US-69.4 budget unchanged).",
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
	reconciled: promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_ledger_reconciled_total",
		Help: "Convergence outcomes per reconcile pass (outcome: ledger rows promoted | turn_ended | failed | busy_cleared by the #1311 store-evidence sweep; input_resolved_by_absence | input_appeared_from_truth pending-input lease diffs by #1310 slice B) — S7/S5's observable surface.",
	}, []string{"outcome"}),
	leaseGatherFails: promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_lease_gather_failures_total",
		Help: "Cadence pending-gather failures (#1310 slice B): the lease truth source errored — the projection is untouched (skipped), and the flap-class outage is visible instead of silent.",
	}),
	reconcileEvidenceFails: promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_reconcile_evidence_failures_total",
		Help: "Store-evidence reads that errored during ledger reconciliation (#1311) — rows untouched, retried next pass; never an authoritative empty.",
	}),
	loopLastRun: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: obs.LoopLivenessMetric,
		Help: obs.LoopLivenessHelp,
	}, []string{obs.LoopLivenessLabel}),
}

// reconcileLast carries the last cumulative reconcile counters so the
// Prometheus counters advance by DELTAS (r2-2): boot/reseed-embedded
// sweeps record into the cumulative Metrics() counters only — the wiring
// bridge (recordSessionStateMetrics) is the single export path, so every
// sweep outcome (cadence pass AND the reseed-embedded boot heal) reaches
// the scrape surface. A monotonicity break (authority recreated) resets
// the baseline.
var reconcileLast = struct {
	mu sync.Mutex
	m  map[string]*sessionstate.Metrics
}{m: map[string]*sessionstate.Metrics{}}

var leaseLast = struct {
	mu       sync.Mutex
	resolved map[string]int64
	appeared map[string]int64
}{resolved: map[string]int64{}, appeared: map[string]int64{}}

func leaseResolvedDelta(workspaceID string, v int64) float64 {
	leaseLast.mu.Lock()
	defer leaseLast.mu.Unlock()
	prev, seen := leaseLast.resolved[workspaceID]
	leaseLast.resolved[workspaceID] = v
	if !seen || v < prev {
		// First scrape / monotonicity break: the FULL cumulative, per the
		// file's own convention (reconcileDeltas) — outcomes that
		// predate the watchdog's first tick still reach the series.
		return float64(v)
	}
	return float64(v - prev)
}

var leaseFailLast = struct {
	mu sync.Mutex
	m  map[string]int64
}{m: map[string]int64{}}

func leaseGatherFailDelta(workspaceID string, v int64) float64 {
	leaseFailLast.mu.Lock()
	defer leaseFailLast.mu.Unlock()
	prev, seen := leaseFailLast.m[workspaceID]
	leaseFailLast.m[workspaceID] = v
	if !seen || v < prev {
		return float64(v)
	}
	return float64(v - prev)
}

func leaseAppearedDelta(workspaceID string, v int64) float64 {
	leaseLast.mu.Lock()
	defer leaseLast.mu.Unlock()
	prev, seen := leaseLast.appeared[workspaceID]
	leaseLast.appeared[workspaceID] = v
	if !seen || v < prev {
		return float64(v)
	}
	return float64(v - prev)
}

func reconcileDeltas(workspaceID string, m *sessionstate.Metrics) (promoted, turnEnded, failed, busyCleared, evidenceFails float64) {
	reconcileLast.mu.Lock()
	defer reconcileLast.mu.Unlock()
	prev, seen := reconcileLast.m[workspaceID]
	if !seen || m.ReconcilePromoted < prev.ReconcilePromoted {
		reconcileLast.m[workspaceID] = &sessionstate.Metrics{
			ReconcilePromoted:      m.ReconcilePromoted,
			ReconcileTurnEnded:     m.ReconcileTurnEnded,
			ReconcileFailed:        m.ReconcileFailed,
			ReconcileBusyCleared:   m.ReconcileBusyCleared,
			ReconcileEvidenceFails: m.ReconcileEvidenceFails,
		}
		if seen {
			return 0, 0, 0, 0, 0 // baseline reset after authority recreation
		}
		// First scrape carries the full cumulative (customValveDelta's
		// convention): outcomes that predate the watchdog's first tick —
		// the boot-heal sweep — still reach the series.
		return float64(m.ReconcilePromoted), float64(m.ReconcileTurnEnded), float64(m.ReconcileFailed), float64(m.ReconcileBusyCleared), float64(m.ReconcileEvidenceFails)
	}
	promoted = float64(m.ReconcilePromoted - prev.ReconcilePromoted)
	turnEnded = float64(m.ReconcileTurnEnded - prev.ReconcileTurnEnded)
	failed = float64(m.ReconcileFailed - prev.ReconcileFailed)
	busyCleared = float64(m.ReconcileBusyCleared - prev.ReconcileBusyCleared)
	evidenceFails = float64(m.ReconcileEvidenceFails - prev.ReconcileEvidenceFails)
	reconcileLast.m[workspaceID] = &sessionstate.Metrics{
		ReconcilePromoted:      m.ReconcilePromoted,
		ReconcileTurnEnded:     m.ReconcileTurnEnded,
		ReconcileFailed:        m.ReconcileFailed,
		ReconcileBusyCleared:   m.ReconcileBusyCleared,
		ReconcileEvidenceFails: m.ReconcileEvidenceFails,
	}
	return promoted, turnEnded, failed, busyCleared, evidenceFails
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
	// r2-2: the single Prometheus export for reconcile outcomes — deltas
	// of the cumulative counters, so reseed-embedded sweeps (which never
	// pass through this loop's own Reconcile return) are counted too.
	if p, te, f, bc, ev := reconcileDeltas(workspaceID, &m); p+te+f+bc+ev > 0 {
		if p > 0 {
			sessionStateMetrics.reconciled.WithLabelValues("promoted").Add(p)
		}
		if te > 0 {
			sessionStateMetrics.reconciled.WithLabelValues("turn_ended").Add(te)
		}
		if f > 0 {
			sessionStateMetrics.reconciled.WithLabelValues("failed").Add(f)
		}
		if bc > 0 {
			sessionStateMetrics.reconciled.WithLabelValues("busy_cleared").Add(bc)
		}
		if ev > 0 {
			sessionStateMetrics.reconcileEvidenceFails.Add(ev)
		}
	}
	// #1310 slice B: lease outcomes ride the same convergence counter
	// family (resolved_by_absence / appeared_from_truth) so the incident
	// class's heal is as observable as every other one.
	if d := leaseResolvedDelta(workspaceID, m.LeaseResolved); d > 0 {
		sessionStateMetrics.reconciled.WithLabelValues("input_resolved_by_absence").Add(d)
	}
	if d := leaseAppearedDelta(workspaceID, m.LeaseAppeared); d > 0 {
		sessionStateMetrics.reconciled.WithLabelValues("input_appeared_from_truth").Add(d)
	}
	if d := leaseGatherFailDelta(workspaceID, m.LeaseGatherFails); d > 0 {
		sessionStateMetrics.leaseGatherFails.Add(d)
	}
	// The funnel: reset-then-set so vanished states drop to zero instead
	// of lingering at their last value.
	for state, n := range m.LedgerDepths {
		sessionStateMetrics.ledgerDepth.WithLabelValues(workspaceID, state).Set(float64(n))
	}
}

// runSessionStateWatchdog drives the ledger convergence pass + stall
// detector + gauge refresh: one pass per interval (production:
// sessionstate.ReconcileCadence — half the 30s lease-convergence bound, so
// one missed tick still converges inside L4/L5; tests shrink it). Wake
// failures increment the counter per errored attempt (the escalation
// signal). The reconcile pass is store-evidence-driven (#1311): stranded
// rows sweep, BUSY re-derives from harness truth, evidence failures are
// visible and never authoritative.
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
			rec := a.Reconcile(ctx)
			if rec.EvidenceFailures > 0 {
				log.Warn("agentd: sessionstate reconcile — store evidence read failed (rows untouched, retrying next pass)",
					zap.Int("evidenceFailures", rec.EvidenceFailures))
			}
			if advanced := rec.Promoted + rec.TurnEnded + rec.Failed + rec.BusyCleared; advanced > 0 {
				log.Info("agentd: sessionstate reconcile — converged stranded state",
					zap.Int("promoted", rec.Promoted), zap.Int("turnEnded", rec.TurnEnded),
					zap.Int("failed", rec.Failed), zap.Int("busyCleared", rec.BusyCleared))
			}
			if rec.LeaseResolved+rec.LeaseAppeared > 0 {
				log.Info("agentd: sessionstate reconcile — pending leases converged",
					zap.Int("resolvedByAbsence", rec.LeaseResolved), zap.Int("appearedFromTruth", rec.LeaseAppeared))
			}
			// Counter export rides recordSessionStateMetrics' delta
			// bridge below — the SINGLE export path, so reseed-embedded
			// sweeps (whose outcomes never appear in this loop's return)
			// are counted identically (r2-2).
			stats := a.CheckStalls(ctx)
			if stats.WakeFailures > 0 {
				sessionStateMetrics.wakeFailures.Add(float64(stats.WakeFailures))
			}
			if stats.Stalled > 0 {
				log.Warn("agentd: sessionstate stall — admitted row crossed the promotion deadline (wake fired)",
					zap.Int("stalled", stats.Stalled), zap.Int("wakeFailures", stats.WakeFailures))
			}
			recordSessionStateMetrics(workspaceID, a)
			// End-of-pass stamp: the Help contract says last COMPLETED
			// pass — a wedge anywhere in this tick (reconcile, stalls,
			// metrics) freezes the stamp at the previous pass (review r1).
			sessionStateMetrics.loopLastRun.WithLabelValues(obs.LoopReconcileWatchdog).Set(float64(time.Now().Unix()))
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

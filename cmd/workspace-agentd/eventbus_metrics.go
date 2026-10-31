// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// eventbus_metrics.go — the workspace event bus's observability
// surface (design 0072, S3): publish/consume counters, per-group lag
// and pending gauges, and the stalled-group detector. Silence on the
// bus is legitimate (a quiet workspace publishes nothing); silence
// PLUS a growing backlog is an outage this surface makes visible
// BEFORE anything else moves onto the bus (S4/S6).
//
// Same plumbing class as session_notifier_metrics.go: pkg/eventbus
// stays prometheus-free and speaks its own vocabulary (DeliveryEvent,
// StreamHealth); THIS file is the bridge — promauto on the default
// registry (the admin port :4098/metrics the PodMonitor scrapes), zap
// logs at the emission sites.
//
// The health model, stated once (docs/operator/monitoring.md carries
// the operator copy):
//
//	publishes_total{outcome=published}   rising — the producer leg lives
//	publishes_total{outcome=failed}     any rise — Redis down (the v1
//	                                    direct leg is the fallback;
//	                                    wake-ups still flow)
//	last_publish_timestamp_seconds      the silent-publisher signal:
//	                                    time() - ts >> any sane quiet
//	                                    period while sessions are busy
//	                                    means settles stopped publishing
//	deliveries_total{group,outcome}     delivered rising per group —
//	                                    consumers live; handler_error /
//	                                    poison rising — the group is
//	                                    failing its work, not stalling
//	group_lag / group_pending           the backlog shape: lag = never
//	                                    delivered (consumers dead/behind);
//	                                    pending = delivered un-acked
//	                                    (handlers wedged or consumers
//	                                    died mid-work)
//	group_oldest_pending_idle_seconds   far past the 60s reclaim
//	                                    threshold — NO consumer is
//	                                    reclaiming: the wedged-group
//	                                    signal
//	group_stalled                       the derived 0/1: the detector
//	                                    below, edge-logged on transition

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// eventbusHealthLoop defaults. Interval: the gauge freshness an
// operator needs vs the probe cost (XINFO + bounded XPENDING/XRANGE
// per group every tick — cheap). StallPendingIdle: a healthy consumer
// reclaims past MinIdle (60s); pending idle at FIVE MINUTES means no
// consumer in the group has reclaimed through five sweeps — wedged or
// dead. StallLag: a backlog of 100 undelivered entries on a stream
// whose consumers deliver in batches of 16 is ~7 stalled rounds deep —
// far past any transient burst.
const (
	eventbusHealthInterval   = 30 * time.Second
	eventbusStallPendingIdle = 5 * time.Minute
	eventbusStallLag         = int64(100)
)

var eventbusMetrics = struct {
	publishes    *prometheus.CounterVec
	deliveries   *prometheus.CounterVec
	redeliveries *prometheus.CounterVec

	streamEntries          *prometheus.GaugeVec
	lastPublishTimestamp   *prometheus.GaugeVec
	groupLag               *prometheus.GaugeVec
	groupPending           *prometheus.GaugeVec
	groupConsumers         *prometheus.GaugeVec
	groupOldestPendingIdle *prometheus.GaugeVec
	groupStalled           *prometheus.GaugeVec
}{
	publishes: promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "workspace_agentd_eventbus_publishes_total",
		Help: "Event bus publishes by outcome (published, failed — validation or transport; a failure falls back to the direct v1 delivery leg, wake-ups still flow). Design 0072 S3.",
	}, []string{"workspace_id", "topic_family", "outcome"}),
	deliveries: promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "workspace_agentd_eventbus_deliveries_total",
		Help: "Event bus consumer-group dispatch outcomes by group and outcome (delivered, filtered, poison, handler_error, ack_error). handler_error/poison rising means the group is failing its work while staying alive.",
	}, []string{"workspace_id", "group", "outcome"}),
	redeliveries: promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "workspace_agentd_eventbus_redeliveries_total",
		Help: "Event bus dispatches that were redeliveries (durable delivery count > 1) — the at-least-once machinery working; sustained rise without deliveries_total rising means work is being retried, not completed.",
	}, []string{"workspace_id", "group"}),

	streamEntries: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_stream_entries",
		Help: "Workspace event stream entry count (bounded by the publisher's ~10k MAXLEN; the retained-replay window).",
	}, []string{"workspace_id"}),
	lastPublishTimestamp: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_last_publish_timestamp_seconds",
		Help: "Unix timestamp of the newest bus entry's publish time (from its at field). The silent-publisher signal: alert on time() - this >> the workspace's quiet period while sessions are busy. Absent (no series) = the stream has no entries.",
	}, []string{"workspace_id"}),
	groupLag: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_group_lag",
		Help: "Consumer-group lag: stream entries never delivered to the group (capped at 10000). Consumers dead or falling behind.",
	}, []string{"workspace_id", "group"}),
	groupPending: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_group_pending",
		Help: "Consumer-group pending entries: delivered but un-acked (handlers failing, or consumers died mid-work — the reclaim sweep is the retry).",
	}, []string{"workspace_id", "group"}),
	groupConsumers: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_group_consumers",
		Help: "Consumer-group registered consumer count. 0 with lag > 0 = nobody is consuming (the dead-group shape; names persist until DELCONSUMER, so 0 means NO consumer ever joined or all were removed).",
	}, []string{"workspace_id", "group"}),
	groupOldestPendingIdle: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_group_oldest_pending_idle_seconds",
		Help: "Seconds the group's longest-idle pending entry has sat un-acked (0 = empty PEL). A healthy consumer reclaims past 60s; far past that means no consumer is reclaiming — the wedged-group signal.",
	}, []string{"workspace_id", "group"}),
	groupStalled: promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_agentd_eventbus_group_stalled",
		Help: "1 when the group is classified stalled (no consumers with a backlog, pending idle past the reclaim horizon, or undelivered backlog past the stall threshold), else 0. Edge-logged on transition by the health loop.",
	}, []string{"workspace_id", "group"}),
}

// topicFamily reduces a topic to its first segment (session, task,
// lease) — the publish counter's cardinality bound; per-session-id
// labels would be unbounded.
func topicFamily(topic string) string {
	for i := 0; i < len(topic); i++ {
		if topic[i] == '.' {
			return topic[:i]
		}
	}
	return topic
}

// eventbusPublishObserver is the bus's publish seam → counters. Every
// Publish outcome (success, validation failure, transport failure)
// ticks exactly one outcome.
func eventbusPublishObserver(ws string) func(eventbus.Event, string, error) {
	return func(ev eventbus.Event, _ string, err error) {
		outcome := "published"
		if err != nil {
			outcome = "failed"
		}
		eventbusMetrics.publishes.WithLabelValues(ws, topicFamily(ev.Topic), outcome).Inc()
	}
}

// eventbusDeliveryObserver is the consume seam → counters: one
// delivery-outcome counter per dispatch plus the redelivery counter
// when the durable attempt count says retry.
func eventbusDeliveryObserver(ws string) func(eventbus.DeliveryEvent) {
	return func(ev eventbus.DeliveryEvent) {
		eventbusMetrics.deliveries.WithLabelValues(ws, ev.Group, string(ev.Outcome)).Inc()
		if ev.Attempts > 1 {
			eventbusMetrics.redeliveries.WithLabelValues(ws, ev.Group).Inc()
		}
	}
}

// eventbusStallReason names why a group is stalled (the log line and
// the operator's triage entry); empty means healthy.
type eventbusStallReason string

const (
	stallNone         eventbusStallReason = ""
	stallNoConsumers  eventbusStallReason = "no_consumers_with_backlog"
	stallStalePending eventbusStallReason = "pending_idle_past_reclaim"
	stallBacklog      eventbusStallReason = "undelivered_backlog"
)

// classifyGroupStall applies the health model to one group. The
// thresholds are constants (eventbusStallPendingIdle /
// eventbusStallLag); the inputs are the raw gauges so the
// classification and the surfaced numbers can never disagree.
func classifyGroupStall(g eventbus.GroupHealth) (bool, eventbusStallReason) {
	switch {
	case g.Consumers == 0 && (g.Lag > 0 || g.Pending > 0):
		return true, stallNoConsumers
	case g.OldestPendingIdle >= eventbusStallPendingIdle:
		return true, stallStalePending
	case g.Lag >= eventbusStallLag:
		return true, stallBacklog
	}
	return false, stallNone
}

// eventbusHealthState carries the loop's edge-detection memory: the
// last stalled classification per group, so a stall LOGS when it
// begins and when it ends — not every tick.
type eventbusHealthState struct {
	mu      sync.Mutex
	stalled map[string]eventbusStallReason
}

func newEventbusHealthState() *eventbusHealthState {
	return &eventbusHealthState{stalled: map[string]eventbusStallReason{}}
}

// collectEventbusHealth snapshots the bus once and surfaces it: raw
// gauges for everything (stream entries, publish timestamp, per-group
// lag/pending/consumers/oldest-idle), the derived stalled gauge, and
// edge-triggered WARN/Info logs on stall transitions. Probe failures
// log at a bounded cadence (first failure, then every
// eventbusHealthErrEvery-th) — the gauges keep their last values,
// which is itself the signal (staleness).
const eventbusHealthErrEvery = 10

func collectEventbusHealth(ctx context.Context, bus *eventbus.RedisBus, ws string, logger *zap.Logger, state *eventbusHealthState, errStreak *int) {
	if bus == nil {
		return
	}
	h, err := bus.Health(ctx)
	if err != nil {
		*errStreak++
		if logger != nil && (*errStreak == 1 || *errStreak%eventbusHealthErrEvery == 0) {
			logger.Warn("event bus: health probe failed — gauges hold their last values",
				zap.String("stream", bus.Key()), zap.Int("streak", *errStreak), zap.Error(err))
		}
		return
	}
	*errStreak = 0

	eventbusMetrics.streamEntries.WithLabelValues(ws).Set(float64(h.Length))
	if !h.LastPublishAt.IsZero() {
		eventbusMetrics.lastPublishTimestamp.WithLabelValues(ws).Set(float64(h.LastPublishAt.Unix()))
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, g := range h.Groups {
		eventbusMetrics.groupLag.WithLabelValues(ws, g.Name).Set(float64(g.Lag))
		eventbusMetrics.groupPending.WithLabelValues(ws, g.Name).Set(float64(g.Pending))
		eventbusMetrics.groupConsumers.WithLabelValues(ws, g.Name).Set(float64(g.Consumers))
		eventbusMetrics.groupOldestPendingIdle.WithLabelValues(ws, g.Name).Set(g.OldestPendingIdle.Seconds())
		stalled, reason := classifyGroupStall(g)
		if stalled {
			eventbusMetrics.groupStalled.WithLabelValues(ws, g.Name).Set(1)
		} else {
			eventbusMetrics.groupStalled.WithLabelValues(ws, g.Name).Set(0)
		}
		prev := state.stalled[g.Name]
		if stalled && prev != reason {
			if logger != nil {
				logger.Warn("event bus: consumer group STALLED",
					zap.String("stream", bus.Key()),
					zap.String("group", g.Name),
					zap.String("reason", string(reason)),
					zap.Int64("lag", g.Lag),
					zap.Int64("pending", g.Pending),
					zap.Int64("consumers", g.Consumers),
					zap.Duration("oldest_pending_idle", g.OldestPendingIdle))
			}
			state.stalled[g.Name] = reason
		} else if !stalled && prev != stallNone {
			if logger != nil {
				logger.Info("event bus: consumer group recovered",
					zap.String("stream", bus.Key()),
					zap.String("group", g.Name),
					zap.String("previous_reason", string(prev)))
			}
			delete(state.stalled, g.Name)
		}
	}
}

// runEventbusHealthLoop is the periodic collector: one snapshot at
// startup (gauges exist from the first scrape) then one per interval.
// Joins wg for clean shutdown alongside the consumer groups.
func runEventbusHealthLoop(ctx context.Context, wg *sync.WaitGroup, bus *eventbus.RedisBus, logger *zap.Logger) {
	if bus == nil {
		return
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	ws := workspaceIDFromEnv()
	if ws == "" {
		ws = "unknown"
	}
	state := newEventbusHealthState()
	errStreak := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
		ticker := time.NewTicker(eventbusHealthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
			}
		}
	}()
}

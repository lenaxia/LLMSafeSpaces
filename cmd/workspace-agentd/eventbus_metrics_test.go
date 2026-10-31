// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// eventbus_metrics_test.go — the S3 surface pinned where it lives:
// the observer bridges (counter arithmetic), the stall classifier
// (every health-model row), and the collector over a REAL bus
// (miniredis) proving the two namesake failures are VISIBLE — a dead
// group's backlog and a wedged group's stale pending both flip the
// stalled gauge and fire the edge-triggered log line, and a recovery
// un-flips it. No sleeps: idle progression is miniredis clock
// arithmetic.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// newEventbusMetricsBus wires a real bus over miniredis for collector
// tests (fresh registry label set per workspace id keeps the shared
// promauto registry assertions clean). Returns the raw client too —
// the tests stage Redis-side state (groups) directly.
func newEventbusMetricsBus(t *testing.T, ws string) (*eventbus.RedisBus, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	bus, err := eventbus.NewRedisBus(context.Background(), client, eventbus.StreamKey(ws), 128)
	require.NoError(t, err)
	return bus, client, mr
}

func TestEventbusPublishObserverCountsOutcomes(t *testing.T) {
	obs := eventbusPublishObserver("ws-metrics-test")
	obs(eventbus.Event{Topic: "session.s1.idle"}, "1-1", nil)
	obs(eventbus.Event{Topic: "lease.tsk-1.nudged"}, "1-2", nil)
	obs(eventbus.Event{Topic: "task.t1.stage_changed"}, "1-3", nil)
	obs(eventbus.Event{Topic: "session.s2.error"}, "", errors.New("XADD down"))

	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.publishes.WithLabelValues("ws-metrics-test", "session", "published")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.publishes.WithLabelValues("ws-metrics-test", "lease", "published")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.publishes.WithLabelValues("ws-metrics-test", "task", "published")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.publishes.WithLabelValues("ws-metrics-test", "session", "failed")))
}

func TestEventbusDeliveryObserverCountsOutcomesAndRedeliveries(t *testing.T) {
	obs := eventbusDeliveryObserver("ws-metrics-test")
	obs(eventbus.DeliveryEvent{Group: "notifier", Outcome: eventbus.DeliveryDelivered, Attempts: 1})
	obs(eventbus.DeliveryEvent{Group: "notifier", Outcome: eventbus.DeliveryDelivered, Attempts: 3})
	obs(eventbus.DeliveryEvent{Group: "lease", Outcome: eventbus.DeliveryHandlerError, Attempts: 2})
	obs(eventbus.DeliveryEvent{Group: "lease", Outcome: eventbus.DeliveryFiltered, Attempts: 1})
	obs(eventbus.DeliveryEvent{Group: "lease", Outcome: eventbus.DeliveryPoison, Attempts: 6})

	require.Equal(t, 2.0, testutil.ToFloat64(eventbusMetrics.deliveries.WithLabelValues("ws-metrics-test", "notifier", "delivered")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.redeliveries.WithLabelValues("ws-metrics-test", "notifier")), "attempts>1 ticks the redelivery counter")
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.deliveries.WithLabelValues("ws-metrics-test", "lease", "handler_error")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.deliveries.WithLabelValues("ws-metrics-test", "lease", "filtered")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.deliveries.WithLabelValues("ws-metrics-test", "lease", "poison")))
	require.Equal(t, 2.0, testutil.ToFloat64(eventbusMetrics.redeliveries.WithLabelValues("ws-metrics-test", "lease")))
}

// TestClassifyGroupStall — every row of the health model's
// classification, including the healthy shapes that must NOT stall
// (quiet bus, transient backlog, fresh pending).
func TestClassifyGroupStall(t *testing.T) {
	cases := []struct {
		name   string
		g      eventbus.GroupHealth
		stall  bool
		reason eventbusStallReason
	}{
		{"healthy drained", eventbus.GroupHealth{Name: "g", Consumers: 1, Lag: 0, Pending: 0}, false, stallNone},
		{"healthy fresh pending", eventbus.GroupHealth{Name: "g", Consumers: 1, Pending: 3, OldestPendingIdle: 10 * time.Second}, false, stallNone},
		{"healthy transient backlog", eventbus.GroupHealth{Name: "g", Consumers: 1, Lag: 12}, false, stallNone},
		{"empty group on empty stream", eventbus.GroupHealth{Name: "g", Consumers: 0, Lag: 0, Pending: 0}, false, stallNone},
		{"dead group with backlog", eventbus.GroupHealth{Name: "g", Consumers: 0, Lag: 1}, true, stallNoConsumers},
		{"dead group with orphaned pending", eventbus.GroupHealth{Name: "g", Consumers: 0, Pending: 2}, true, stallNoConsumers},
		{"wedged group stale pending", eventbus.GroupHealth{Name: "g", Consumers: 1, Pending: 1, OldestPendingIdle: eventbusStallPendingIdle}, true, stallStalePending},
		{"deep backlog", eventbus.GroupHealth{Name: "g", Consumers: 1, Lag: eventbusStallLag}, true, stallBacklog},
	}
	for _, c := range cases {
		stall, reason := classifyGroupStall(c.g)
		require.Equal(t, c.stall, stall, c.name)
		require.Equal(t, c.reason, reason, c.name)
	}
}

// TestCollectEventbusHealth_DeadGroupVisible — the S3 namesake:
// a group that exists but never consumes accumulates lag, flips the
// stalled gauge, and fires the WARN exactly once (edge-triggered);
// draining the backlog recovers (stalled 0 + the recovery log).
func TestCollectEventbusHealth_DeadGroupVisible(t *testing.T) {
	ws := "ws-collect-dead"
	bus, client, _ := newEventbusMetricsBus(t, ws)
	ctx := context.Background()

	// Group exists (as Consume would create it), no consumer joins.
	require.NoError(t, client.XGroupCreateMkStream(ctx, bus.Key(), "notifier", "$").Err())
	for i := 0; i < 3; i++ {
		_, err := bus.Publish(ctx, eventbus.Event{Topic: "session.s" + string(rune('0'+i)) + ".idle"})
		require.NoError(t, err)
	}

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	state := newEventbusHealthState()
	errStreak := 0

	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 3.0, testutil.ToFloat64(eventbusMetrics.streamEntries.WithLabelValues(ws)))
	require.Equal(t, 3.0, testutil.ToFloat64(eventbusMetrics.groupLag.WithLabelValues(ws, "notifier")))
	require.Equal(t, 0.0, testutil.ToFloat64(eventbusMetrics.groupConsumers.WithLabelValues(ws, "notifier")))
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.groupStalled.WithLabelValues(ws, "notifier")), "the dead group must be stalled")
	warns := logs.FilterMessage("event bus: consumer group STALLED")
	require.Len(t, warns.All(), 1, "the stall must WARN once (edge-triggered)")
	require.Contains(t, warns.All()[0].ContextMap(), "reason")

	// Second collect with the SAME state: gauge holds, log does not repeat.
	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.groupStalled.WithLabelValues(ws, "notifier")))
	require.Len(t, logs.FilterMessage("event bus: consumer group STALLED").All(), 1)

	// A consumer drains the backlog: stalled recovers, Info logged.
	drained := make(chan struct{})
	close(drained)
	consumeCtx, cancel := context.WithCancel(ctx)
	go func() {
		_ = bus.Consume(consumeCtx, eventbus.ConsumeOptions{
			Group: "notifier", Consumer: "heir",
			Block: 10 * time.Millisecond, MinIdle: 30 * time.Millisecond,
			ReclaimTick: 20 * time.Millisecond, Batch: 8,
		}, func(context.Context, eventbus.Message) error { return nil })
	}()
	require.Eventually(t, func() bool {
		h, err := bus.Health(ctx)
		return err == nil && len(h.Groups) == 1 && h.Groups[0].Lag == 0
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-drained

	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 0.0, testutil.ToFloat64(eventbusMetrics.groupStalled.WithLabelValues(ws, "notifier")), "the drained group must recover")
	require.Len(t, logs.FilterMessage("event bus: consumer group recovered").All(), 1)
}

// TestCollectEventbusHealth_WedgedGroupPendingIdle — the second
// namesake: deliveries that never ack go stale; the pending-idle
// gauge crosses the reclaim horizon and classifies the group stalled
// EVEN WITH a live consumer name (a wedged loop reclaims nothing).
// The miniredis clock makes idle deterministic.
func TestCollectEventbusHealth_WedgedGroupPendingIdle(t *testing.T) {
	ws := "ws-collect-wedged"
	bus, client, mr := newEventbusMetricsBus(t, ws)
	mr.SetTime(time.Now().UTC())
	t.Cleanup(func() { mr.SetTime(time.Time{}) })
	ctx := context.Background()
	require.NoError(t, client.XGroupCreateMkStream(ctx, bus.Key(), "notifier", "$").Err())
	_, err := bus.Publish(ctx, eventbus.Event{Topic: "session.s1.idle"})
	require.NoError(t, err)

	// A consumer whose handler never succeeds: the entry is delivered
	// (cursor advances, lag 0) but never acks — pending.
	handlerDone := make(chan struct{})
	consumeCtx, cancel := context.WithCancel(ctx)
	go func() {
		_ = bus.Consume(consumeCtx, eventbus.ConsumeOptions{
			Group: "notifier", Consumer: "wedged",
			Block: 10 * time.Millisecond, MinIdle: 30 * time.Millisecond,
			ReclaimTick: 20 * time.Millisecond, Batch: 8, MaxAttempts: 1000,
		}, func(context.Context, eventbus.Message) error {
			select {
			case <-handlerDone:
			default:
				close(handlerDone)
			}
			return errors.New("wedged")
		})
	}()
	<-handlerDone
	// Let the redeliveries settle into the PEL, then stop the loop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	state := newEventbusHealthState()
	errStreak := 0

	// Fresh pending: visible, not yet stalled.
	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 0.0, testutil.ToFloat64(eventbusMetrics.groupStalled.WithLabelValues(ws, "notifier")))
	require.GreaterOrEqual(t, testutil.ToFloat64(eventbusMetrics.groupPending.WithLabelValues(ws, "notifier")), 1.0)

	// Advance the clock past the reclaim horizon: stalled, reason
	// named, log fired.
	mr.SetTime(time.Now().UTC().Add(eventbusStallPendingIdle + time.Minute))
	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.groupStalled.WithLabelValues(ws, "notifier")))
	idle := testutil.ToFloat64(eventbusMetrics.groupOldestPendingIdle.WithLabelValues(ws, "notifier"))
	require.GreaterOrEqual(t, idle, eventbusStallPendingIdle.Seconds())
	warns := logs.FilterMessage("event bus: consumer group STALLED").All()
	require.Len(t, warns, 1)
	require.Equal(t, string(stallStalePending), warns[0].ContextMap()["reason"])
}

// TestCollectEventbusHealth_SilentPublisherTimestamp — the publish
// side's liveness signal: the newest entry's publish time lands on
// the timestamp gauge (alert with time() - ts), absent on an empty
// stream.
func TestCollectEventbusHealth_SilentPublisherTimestamp(t *testing.T) {
	ws := "ws-collect-silent"
	bus, _, _ := newEventbusMetricsBus(t, ws)
	ctx := context.Background()
	state := newEventbusHealthState()
	errStreak := 0
	logger := zap.NewNop()

	// Empty stream: entries 0, no timestamp series.
	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 0.0, testutil.ToFloat64(eventbusMetrics.streamEntries.WithLabelValues(ws)))
	require.Equal(t, 0.0, testutil.ToFloat64(eventbusMetrics.lastPublishTimestamp.WithLabelValues(ws)), "no entry yet: the timestamp gauge stays zero/absent")

	at := time.Now().Add(-42 * time.Second).UTC()
	_, err := bus.Publish(ctx, eventbus.Event{Topic: "session.s1.idle", At: at})
	require.NoError(t, err)
	collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	require.Equal(t, 1.0, testutil.ToFloat64(eventbusMetrics.streamEntries.WithLabelValues(ws)))
	require.Equal(t, float64(at.Unix()), testutil.ToFloat64(eventbusMetrics.lastPublishTimestamp.WithLabelValues(ws)))
}

// TestCollectEventbusHealth_ProbeFailureLogsBounded — a dead Redis
// logs the FIRST failure and then only every Nth: visible, never a
// flood; the gauges keep their last values (staleness is the signal).
func TestCollectEventbusHealth_ProbeFailureLogsBounded(t *testing.T) {
	ws := "ws-collect-probe"
	bus, _, mr := newEventbusMetricsBus(t, ws)
	ctx := context.Background()
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	state := newEventbusHealthState()
	errStreak := 0

	mr.Close()
	for i := 0; i < eventbusHealthErrEvery+2; i++ {
		collectEventbusHealth(ctx, bus, ws, logger, state, &errStreak)
	}
	entries := logs.FilterMessageSnippet("event bus: health probe failed").All()
	require.Len(t, entries, 2, "streak 1 and streak %d fire, the rest stay quiet", eventbusHealthErrEvery)
}
